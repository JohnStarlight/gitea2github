// The relink command and the offer that follows a migration, which repoint local
// clones away from Gitea -- or, for a repository whose history was rewritten,
// hand the clone over to the rewritten copy.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/JohnStarlight/gitea2github/internal/creds"
	"github.com/JohnStarlight/gitea2github/internal/github"
	"github.com/JohnStarlight/gitea2github/internal/migrate"
	"github.com/JohnStarlight/gitea2github/internal/relink"
	"github.com/JohnStarlight/gitea2github/internal/tui"
	"github.com/JohnStarlight/gitea2github/internal/ui"
)

// cmdRelink repoints local working copies at GitHub.
func cmdRelink(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("relink", flag.ExitOnError)
	giteaURL := giteaFlags(fs)
	dryRun := fs.Bool("dry-run", false, "print the plan and stop, without asking anything")
	assumeYes := fs.Bool("yes", false, "skip the questions and the confirmation, using flags and defaults")
	oldName := fs.String("keep-as", "gitea", "name to give the existing Gitea remote (--push-to=github only)")
	verify := fs.Bool("verify", true, "confirm the GitHub repository exists before repointing")
	pushTo := fs.String("push-to", relink.ModeGitHub,
		"where relinked clones should push: github, both, or gitea")
	noTUI := fs.Bool("no-tui", false, "choose from numbered prompts instead of the full-screen selector")
	commitAs := fs.String("commit-as", "",
		"who new commits are made as in clones that will push to GitHub: "+
			"github (your GitHub name and no-reply address, in those clones only) or keep; asked when not given")
	var names stringList
	fs.Var(&names, "name",
		"the name a repository took on GitHub, as owner/repo=name, when it is not its own (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	named, err := parseNames(names)
	if err != nil {
		return err
	}
	if *commitAs != "" && *commitAs != "github" && *commitAs != "keep" {
		return fmt.Errorf("--commit-as must be github or keep (got %q)", *commitAs)
	}
	switch *pushTo {
	case relink.ModeGitHub, relink.ModeBoth, relink.ModeGitea:
	default:
		return fmt.Errorf("--push-to must be one of: %s, %s, %s (got %q)",
			relink.ModeGitHub, relink.ModeBoth, relink.ModeGitea, *pushTo)
	}

	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	prompt := newPrompter()

	// The directory defaults to the one you are standing in, which is where
	// clones almost always are and what the offer at the end of a migration
	// already assumes. Nothing is changed by the scan, and both the screen and
	// the plan name the directory they worked on, so a wrong guess is visible
	// before anything acts on it.
	root := fs.Arg(0)
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("no directory given and the current one cannot be read: %w", err)
		}
		root = cwd
	}

	parsed, err := url.Parse(*giteaURL)
	if err != nil {
		return fmt.Errorf("invalid --gitea-url %q: %w", *giteaURL, err)
	}

	ghCred, err := creds.GitHub()
	if err != nil {
		return err
	}
	ghClient := newGitHub(ghCred.Token)
	me, err := ghClient.Identity(ctx)
	if err != nil {
		return fmt.Errorf("identifying GitHub user: %w", err)
	}
	ghLogin := me.Login

	// Not fatal: without the list, each clone is matched by its own name,
	// which is right for every repository the migration did not rename --
	// and the check against GitHub's files catches the ones it did.
	targets, err := giteaTargets(ctx, *giteaURL)
	if err != nil {
		first, _, _ := strings.Cut(err.Error(), "\n")
		fmt.Printf("\nCould not read the repository list from Gitea (%s);\n"+
			"each clone is matched by its own name.\n", first)
	}
	// Names given with --name are the most specific thing said, so they win.
	targets = withNames(targets, named)

	// Where a clone should push is the one decision here with consequences, so
	// ask it outright rather than letting the default decide silently.
	// The screen needs to know what is out there before it can offer anything,
	// so the scan comes first and is reused as the plan afterwards.
	probeOptions := relink.Options{
		GitHub:        ghClient,
		Root:          root,
		GiteaHost:     parsed.Host,
		GitHubUser:    ghLogin,
		GitHubTok:     ghCred.Token,
		OldRemoteName: *oldName,
		Mode:          *pushTo,
		Verify:        *verify,
		DryRun:        true,
		Targets:       targets,
	}
	fmt.Printf("\nLooking for clones under %s...\n", shortenPath(root))
	probe, err := relink.Run(ctx, probeOptions)
	if err != nil {
		return err
	}
	if len(probe) == 0 {
		// Opening an empty screen would leave the user pressing q to find out
		// that nothing was there.
		fmt.Printf("No git working copies under %s.\n", shortenPath(root))
		fmt.Println("Give the directory that holds your clones: gitea2github relink <directory>")
		return nil
	}

	var only map[string]bool
	var modeFor map[string]string

	switch {
	case !*noTUI && !*dryRun && !*assumeYes && tui.Available():
		choice, screenErr := chooseRelink(ctx, probe, *pushTo, *oldName, root, ghLogin, probeOptions)
		if screenErr != nil {
			return screenErr
		}
		if choice.cancelled {
			fmt.Println("Cancelled; nothing was changed.")
			return nil
		}
		if len(choice.only) == 0 {
			fmt.Println("Nothing selected; nothing was changed.")
			return nil
		}
		only, modeFor = choice.only, choice.modes
		targets = withNames(targets, choice.renames)

		// The screen may have been pointed at another directory, or given
		// names: either way the scan the plan is built from has to be the one
		// the choices were made on, not the sweep this command started with.
		if newRoot := expandHome(choice.root); (newRoot != root && choice.root != "") || len(choice.renames) > 0 {
			if choice.root != "" {
				root = newRoot
			}
			probeOptions.Root, probeOptions.Targets = root, targets
			probe, err = relink.Run(ctx, probeOptions)
			if err != nil {
				return err
			}
		}

	case prompt.Interactive() && !*dryRun && !*assumeYes && !given["push-to"]:
		modes := []string{relink.ModeGitHub, relink.ModeBoth, relink.ModeGitea}
		choice := prompt.Choose("\nWhere should these clones push?", []ui.Option{
			{Label: "github", Help: "GitHub only; the Gitea remote is kept as \"" + *oldName + "\""},
			{Label: "both", Help: "one git push reaches both servers"},
			{Label: "gitea", Help: "Gitea only; just add a github remote"},
		}, 0)
		*pushTo = modes[choice]
	}

	options := relink.Options{
		GitHub:        ghClient,
		Root:          root,
		GiteaHost:     parsed.Host,
		GitHubUser:    ghLogin,
		GitHubTok:     ghCred.Token,
		OldRemoteName: *oldName,
		Mode:          *pushTo,
		Verify:        *verify,
		Only:          only,
		ModeFor:       modeFor,
		GitEnv:        migrate.CredentialEnv("x-access-token", ghCred.Token),
		Targets:       targets,
	}

	// The plan is the scan narrowed to what was chosen. Running the sweep a
	// second time would ask GitHub about every clone again for an answer that
	// cannot have changed while somebody was reading the screen.
	plan := relinkPlanFromProbe(probe, only, modeFor, *pushTo, *oldName)
	printRelinkResults(plan)

	if *dryRun {
		return nil
	}

	pending := 0
	for _, r := range plan {
		if r.Action == "planned" {
			pending++
		}
	}
	if pending == 0 {
		fmt.Println("Nothing to do.")
		return nil
	}

	id := relink.Identity{Name: me.Login, Email: me.NoReply}
	if concerned, redacted := commitAsConcerned(plan, modeFor, *pushTo, id); len(concerned) > 0 {
		switch {
		case *commitAs == "github":
			options.CommitAs = &id
		case *commitAs == "keep":
		case prompt.Interactive() && !*assumeYes:
			question, yes := commitAsQuestion(concerned, redacted, id)
			if prompt.Confirm("\n"+question, yes) {
				options.CommitAs = &id
			}
		case redacted:
			fmt.Printf("New commits in %s will still carry %s, and the next push would publish it.\n"+
				"Pass --commit-as=github to make them as %s in those clones.\n",
				count(len(concerned), "clone", "clones"), strings.Join(commitEmails(concerned, id), ", "), id)
		}
	}

	if !*assumeYes {
		if !prompt.Interactive() {
			return fmt.Errorf("refusing to change anything without a terminal to confirm on; "+
				"pass --yes to proceed or --dry-run to preview (%d clone(s) would be repointed)", pending)
		}
		question := fmt.Sprintf("\nRepoint %d clone%s (push mode: %s)?",
			pending, plural(pending, "", "s"), *pushTo)
		if !prompt.Confirm(question, false) {
			fmt.Println("Cancelled; nothing was changed.")
			return nil
		}
	}

	options.Log = serialLog()
	fmt.Println()
	results, err := relink.Run(ctx, options)
	if err != nil {
		return err
	}
	printRelinkResults(results)
	return nil
}

// giteaTargets works out the name each Gitea repository took on GitHub the
// way the migration did, so that a relink run on its own finds renamed
// repositories. Names typed on the selection screen are not recorded
// anywhere and cannot be recovered here.
func giteaTargets(ctx context.Context, giteaURL string) (map[string]string, error) {
	client, _, err := resolveGitea(giteaURL)
	if err != nil {
		return nil, err
	}
	login, err := giteaLogin(ctx, client)
	if err != nil {
		return nil, err
	}
	repos, err := client.ListRepos(ctx)
	if err != nil {
		return nil, err
	}
	return relinkTargets(migrate.Targets(repos, login), nil), nil
}

// offerRelink asks whether to repoint the local clones, and opens the
// repointing screen if the answer is yes.
//
// Deliberately quiet about its own failures: the migration has already
// succeeded by this point, and a directory that cannot be scanned is a reason
// to say so and stop, not to report the whole run as failed.
func offerRelink(ctx context.Context, prompt *ui.Prompter, clonesRoot, giteaURL, ghLogin, ghToken string,
	assumeYes bool, redacted int, targets map[string]string) {
	// Never without being asked. Repointing rewrites remotes in directories
	// the migration never touched, so --yes, which is consent to the migration
	// that was described, is not consent to this.
	if !prompt.Interactive() || assumeYes {
		if redacted > 0 {
			fmt.Printf("\n%s rewritten, so the clones on this machine can no longer push to "+
				"GitHub: what is there now is different commits.\n"+
				"Run `gitea2github relink .` to have them take the rewritten history on.\n",
				count(redacted, "history was", "histories were"))
			return
		}
		fmt.Println("\nYour local clones still push to Gitea. Run `gitea2github relink .` to repoint them.")
		return
	}

	// The directory the migration looked in for work not yet on Gitea: the
	// copies found there are the ones to repoint.
	cwd := clonesRoot

	// A migration that copied histories verbatim leaves clones that still
	// work, so repointing them is tidying and the default is no. One that
	// rewrote them leaves clones that cannot push to what was just created,
	// which is not tidying and should not be stumbled past.
	// Not "to GitHub": the screen behind this question offers Gitea and both
	// as well, and naming one of the three here prejudges a choice that is
	// better made with the list in front of you.
	question := fmt.Sprintf("\nRepoint the clones under %s?", shortenPath(cwd))
	def := false
	if redacted > 0 {
		fmt.Printf("\n%s rewritten. The clones on this machine still hold the original,\n"+
			"so they can no longer push to GitHub: what is there now is different commits.\n",
			count(redacted, "history was", "histories were"))
		question = fmt.Sprintf("Have the clones under %s take on the rewritten history?",
			shortenPath(cwd))
		def = true
	}
	if !prompt.Confirm(question, def) {
		if redacted > 0 {
			fmt.Println("Left alone. Those clones cannot push to GitHub until they take the " +
				"rewritten history on -- `gitea2github relink` when you are ready. " +
				"Pushing to Gitea still works.")
			return
		}
		fmt.Println("Left alone. Run `gitea2github relink <directory>` whenever you want to.")
		return
	}

	parsed, err := url.Parse(giteaURL)
	if err != nil {
		return
	}
	gh := newGitHub(ghToken)
	probe, err := relink.Run(ctx, relink.Options{
		GitHub: gh,
		Root:   cwd, GiteaHost: parsed.Host, GitHubUser: ghLogin, GitHubTok: ghToken,
		OldRemoteName: "gitea", Mode: relink.ModeGitHub, Verify: true, DryRun: true,
		Targets: targets,
	})
	if err != nil {
		fmt.Printf("  could not scan %s: %v\n", shortenPath(cwd), err)
		return
	}
	if len(probe) == 0 {
		fmt.Printf("  no git working copies under %s.\n", shortenPath(cwd))
		fmt.Println("  Run `gitea2github relink <directory>` against the folder that holds your clones.")
		return
	}

	base := relink.Options{
		GitHub:    gh,
		GiteaHost: parsed.Host, GitHubUser: ghLogin, GitHubTok: ghToken,
		OldRemoteName: "gitea", Mode: relink.ModeGitHub, Verify: true,
		GitEnv:  migrate.CredentialEnv("x-access-token", ghToken),
		Targets: targets,
	}
	choice, screenErr := chooseRelink(ctx, probe, relink.ModeGitHub, "gitea", cwd, ghLogin, base)
	switch {
	case screenErr != nil:
		fmt.Printf("  %v\n", screenErr)
		return
	case choice.cancelled, len(choice.only) == 0:
		fmt.Println("  Left alone; no remote was changed.")
		return
	}
	only, modes := choice.only, choice.modes
	base.Targets = withNames(base.Targets, choice.renames)

	// The screen may have been pointed elsewhere, or given names, in which
	// case the chosen paths came from a different scan than this one.
	root := cwd
	if newRoot := expandHome(choice.root); (choice.root != "" && newRoot != cwd) || len(choice.renames) > 0 {
		if choice.root != "" {
			root = newRoot
		}
		base.Root, base.DryRun = root, true
		if rescanned, err := relink.Run(ctx, base); err == nil {
			probe = rescanned
		}
		base.DryRun = false
	}

	plan := relinkPlanFromProbe(probe, only, modes, relink.ModeGitHub, "gitea")
	printRelinkResults(plan)
	if me, err := base.GitHub.Identity(ctx); err == nil {
		id := relink.Identity{Name: me.Login, Email: me.NoReply}
		if concerned, redacted := commitAsConcerned(plan, modes, relink.ModeGitHub, id); len(concerned) > 0 {
			if question, yes := commitAsQuestion(concerned, redacted, id); prompt.Confirm(question, yes) {
				base.CommitAs = &id
			}
		}
	}
	if !prompt.Confirm(fmt.Sprintf("Repoint %d clone%s?", len(only), plural(len(only), "", "s")), false) {
		fmt.Println("Cancelled; no remote was changed.")
		return
	}

	base.Root, base.Only, base.ModeFor = root, only, modes
	base.Log = serialLog()
	results, err := relink.Run(ctx, base)
	if err != nil {
		fmt.Printf("  %v\n", err)
		return
	}
	printRelinkResults(results)
}

// commitAsConcerned is the clones in a plan whose pushes will go to GitHub
// and whose commits are not made as the GitHub identity -- the ones where
// who a commit is made as is about to become public. redacted reports that
// one of them takes on a rewritten history, where the address in its config
// is one the migration went out of its way to hide.
func commitAsConcerned(plan []relink.Result, modes map[string]string, fallback string,
	id relink.Identity) (concerned []relink.Result, redacted bool) {

	for _, r := range plan {
		if r.Action != "planned" {
			continue
		}
		mode := fallback
		if m, ok := modes[r.Path]; ok && m != "" {
			mode = m
		}
		if !r.Redacted && mode != relink.ModeGitHub {
			continue // still pushes to Gitea too
		}
		if r.CommitName == id.Name && r.CommitEmail == id.Email {
			continue
		}
		concerned = append(concerned, r)
		redacted = redacted || r.Redacted
	}
	return concerned, redacted
}

// commitEmails is the addresses those clones commit with, other than the
// no-reply one, each once.
func commitEmails(clones []relink.Result, id relink.Identity) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range clones {
		e := r.CommitEmail
		if e == "" {
			e = "no address at all"
		}
		if e != id.Email && !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	return out
}

// commitAsQuestion asks whether those clones should commit as the GitHub
// identity, in plain words, and says which answer is the default.
//
// After a redacting migration the answer defaults to yes: the address in
// their config is the one the rewrite hid, and the next push would publish
// it. Otherwise it defaults to no, since that history was published as it
// is, and changing who someone commits as is theirs to choose.
func commitAsQuestion(clones []relink.Result, redacted bool, id relink.Identity) (string, bool) {
	where := "this clone"
	if len(clones) > 1 {
		where = fmt.Sprintf("these %d clones", len(clones))
	}
	emails := commitEmails(clones, id)
	if redacted && len(emails) > 0 {
		return fmt.Sprintf("New commits in %s would still carry %s,\n"+
			"and the next push would publish it on GitHub.\n"+
			"Make them as %s, in %s only?", where, strings.Join(emails, ", "), id, where), true
	}
	seen := map[string]bool{}
	var current []string
	for _, r := range clones {
		c := relink.Identity{Name: r.CommitName, Email: r.CommitEmail}.String()
		if !seen[c] {
			seen[c] = true
			current = append(current, c)
		}
	}
	return fmt.Sprintf("New commits in %s are made as %s.\n"+
		"Make them as %s instead, in %s only?", where, strings.Join(current, ", "), id, where), redacted
}

// relinkChoice is what the repointing screen was left with.
type relinkChoice struct {
	only      map[string]bool
	modes     map[string]string
	root      string            // the directory it ended up looking at
	renames   map[string]string // names given with r, by Gitea repository
	cancelled bool
}

// chooseRelink opens the repointing screen and returns what was chosen.
func chooseRelink(ctx context.Context, probe []relink.Result, mode, oldName, root, ghLogin string,
	base relink.Options) (relinkChoice, error) {

	clones := clonesFromProbe(ctx, probe, mode)

	model := tui.NewRelinkModel(clones, mode, oldName).
		WithRoot(shortenPath(root), relinkScanner(ctx, base)).
		WithRenamer(relinkRenamer(ctx, base))
	header := fmt.Sprintf("github.com/%s", ghLogin)
	switch screenErr := tui.Run(header, model); {
	case errors.Is(screenErr, tui.ErrCancelled):
		return relinkChoice{cancelled: true}, nil
	case screenErr != nil:
		return relinkChoice{}, screenErr
	}
	only, modes := model.Chosen()
	// The screen may have been pointed somewhere else, in which case the paths
	// it chose belong to a different directory than the one it opened on.
	return relinkChoice{only: only, modes: modes, root: model.Root(), renames: model.Renames()}, nil
}

// relinkRenamer returns the callback the repointing screen uses to look for
// one clone's repository under a name typed for it.
func relinkRenamer(ctx context.Context, base relink.Options) tui.Renamer {
	return func(c tui.Clone, name string) (tui.Clone, error) {
		opts := base
		// The clone's own directory: the scan finds it and nothing else.
		opts.Root, opts.DryRun = c.Path, true
		opts.Only, opts.ModeFor = nil, nil
		opts.Targets = withNames(base.Targets, map[string]string{strings.ToLower(c.Source): name})

		found, err := relink.Run(ctx, opts)
		if err != nil {
			return c, err
		}
		for _, row := range clonesFromProbe(ctx, found, c.Mode) {
			if row.Path == c.Path {
				return row, nil
			}
		}
		return c, fmt.Errorf("%s is no longer a git working copy", c.Display)
	}
}

// parseNames reads --name values, owner/repo=name.
func parseNames(values []string) (map[string]string, error) {
	out := map[string]string{}
	for _, v := range values {
		full, name, ok := strings.Cut(v, "=")
		full, name = strings.TrimSpace(full), strings.TrimSpace(name)
		if !ok || name == "" || strings.Count(full, "/") != 1 {
			return nil, fmt.Errorf("--name %q: expected owner/repo=name, e.g. --name teammate/quadchecker=quadchecker-team", v)
		}
		out[strings.ToLower(full)] = name
	}
	return out, nil
}

// withNames lays names over a target map, keyed by Gitea repository in lower
// case, making each one a name GitHub accepts. Neither map is changed.
func withNames(targets, names map[string]string) map[string]string {
	out := make(map[string]string, len(targets)+len(names))
	for k, v := range targets {
		out[k] = v
	}
	for k, v := range names {
		out[strings.ToLower(k)] = github.SanitizeName(v)
	}
	return out
}

// relinkScanner returns the callback the repointing screen uses to look at
// another directory.
//
// The screen is given a function rather than the options themselves so that it
// stays free of the migrate and relink packages' configuration, and so a test
// can drive it with a fake that touches no disk.
func relinkScanner(ctx context.Context, base relink.Options) tui.Rescan {
	return func(root string) ([]tui.Clone, error) {
		opts := base
		opts.Root = expandHome(root)
		opts.DryRun = true
		// A directory chosen on the screen replaces the earlier selection
		// wholesale, so neither filter from the previous scan applies.
		opts.Only, opts.ModeFor = nil, nil

		found, err := relink.Run(ctx, opts)
		if err != nil {
			return nil, err
		}
		return clonesFromProbe(ctx, found, base.Mode), nil
	}
}

// clonesFromProbe turns a scan into rows for the screen.
func clonesFromProbe(ctx context.Context, probe []relink.Result, mode string) []tui.Clone {
	clones := make([]tui.Clone, 0, len(probe))
	for _, r := range probe {
		clone := tui.Clone{
			Path: r.Path, Display: shortenPath(r.Path), Mode: mode,
			Redacted: r.Redacted, Public: r.Public,
			Source: r.Source, Target: r.Target,
		}
		// Anything the scan did not mark as planned cannot be repointed by
		// this run, so the reason it gave is shown instead of a destination.
		// A clone that cannot take on a rewritten history was already marked
		// skipped by the scan, with the reason, so it arrives here blocked.
		if r.Action != "planned" {
			clone.Blocked = r.Reason
			if wrongName(r) {
				clone.Blocked += " -- if it has another name there, press r"
			}
		}
		clones = append(clones, clone)
	}
	return clones
}

// relinkPlanFromProbe narrows the scan to the chosen clones and relabels each
// with the destination picked for it.
func relinkPlanFromProbe(probe []relink.Result, only map[string]bool,
	modes map[string]string, fallback, oldName string) []relink.Result {

	var plan []relink.Result
	for _, r := range probe {
		if only != nil && !only[r.Path] {
			continue
		}
		// A rewritten history is adopted whatever destination was picked, and
		// its row already says so; relabelling it would promise an ordinary
		// repoint that is not what runs.
		if r.Action == "planned" && !r.Redacted {
			mode := fallback
			if m, ok := modes[r.Path]; ok && m != "" {
				mode = m
			}
			r.Reason = relink.Describe(mode, oldName)
		}
		plan = append(plan, r)
	}
	return plan
}

// printRelinkResults renders the relink table, used for both the plan and the
// outcome.
func printRelinkResults(results []relink.Result) {
	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ACTION\tPATH\tGITHUB\tDETAIL")
	var problems []relink.AdoptProblem
	for _, r := range results {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Action, shortenPath(r.Path), r.NewURL, r.Reason)
		problems = append(problems, r.AdoptProblems...)
	}
	_ = w.Flush()
	fmt.Println()
	if len(problems) > 0 {
		fmt.Println(relink.AdoptAdvice(problems))
	}
	for _, r := range results {
		if wrongName(r) {
			fmt.Println("If a repository has another name on GitHub, give it:\n" +
				"  gitea2github relink --name owner/repo=name-on-github")
			fmt.Println()
			break
		}
	}
}

// wrongName reports a clone that may simply be looking under the wrong name:
// nothing by its name on GitHub, or something by its name that is another
// project. A name typed during the migration is the usual reason.
func wrongName(r relink.Result) bool {
	return r.Action == "skipped" && r.Source != "" &&
		(strings.HasPrefix(r.Reason, "no repository named") || strings.Contains(r.Reason, "does not match this clone"))
}
