// The relink command and the offer that follows a migration, which repoint local
// clones away from Gitea -- or, for a repository whose history was rewritten,
// hand the clone over to the rewritten copy.
// Command gitea2github migrates Git repositories from a Gitea instance to
// GitHub, with all branches and tags intact, and repoints local clones at the
// new home.
//
// It exists because doing this by hand — create repository, copy URL, add
// remote, push, repeat — is both tedious and lossy: the manual route usually
// carries over only the branch that happened to be checked out.
//
// Typical session:
//
//	gitea2github doctor                 # check credentials and scopes
//	gitea2github list                   # see what would be considered
//	gitea2github migrate --dry-run      # see what would happen
//	gitea2github migrate                # do it
//	gitea2github relink ~/Git           # repoint local clones
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch *pushTo {
	case relink.ModeGitHub, relink.ModeBoth, relink.ModeGitea:
	default:
		return fmt.Errorf("--push-to must be one of: %s, %s, %s (got %q)",
			relink.ModeGitHub, relink.ModeBoth, relink.ModeGitea, *pushTo)
	}

	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	prompt := ui.New()

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
	ghLogin, err := github.New(ghCred.Token).Login(ctx)
	if err != nil {
		return fmt.Errorf("identifying GitHub user: %w", err)
	}

	// Where a clone should push is the one decision here with consequences, so
	// ask it outright rather than letting the default decide silently.
	// The screen needs to know what is out there before it can offer anything,
	// so the scan comes first and is reused as the plan afterwards.
	probeOptions := relink.Options{
		Root:          root,
		GiteaHost:     parsed.Host,
		GitHubUser:    ghLogin,
		GitHubTok:     ghCred.Token,
		OldRemoteName: *oldName,
		Mode:          *pushTo,
		Verify:        *verify,
		DryRun:        true,
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
		chosen, modes, chosenRoot, screenErr := func() (map[string]bool, map[string]string, string, error) {
			c, m, r, cancelled, err := chooseRelink(ctx,
				probe, *pushTo, *oldName, root, ghLogin, relinkScanner(ctx, probeOptions))
			if cancelled {
				return nil, nil, "", errScreenCancelled
			}
			return c, m, r, err
		}()
		if errors.Is(screenErr, errScreenCancelled) {
			fmt.Println("Cancelled; nothing was changed.")
			return nil
		}
		if screenErr != nil {
			return screenErr
		}
		if len(chosen) == 0 {
			fmt.Println("Nothing selected; nothing was changed.")
			return nil
		}
		only, modeFor = chosen, modes

		// The screen may have been pointed at another directory. Its scan is
		// the one the chosen paths came from, so the plan has to be built from
		// there rather than from the sweep this command started with.
		if newRoot := expandHome(chosenRoot); newRoot != root && chosenRoot != "" {
			root = newRoot
			probeOptions.Root = root
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

// printRelinkResults renders the relink table, used for both the plan and the
// outcome.

// offerRelink asks whether to repoint the local clones, and opens the
// repointing screen if the answer is yes.
//
// Deliberately quiet about its own failures: the migration has already
// succeeded by this point, and a directory that cannot be scanned is a reason
// to say so and stop, not to report the whole run as failed.
func offerRelink(ctx context.Context, prompt *ui.Prompter, giteaURL, ghLogin, ghToken string,
	assumeYes bool, redacted int) {
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

	cwd, err := os.Getwd()
	if err != nil {
		return
	}

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
	probe, err := relink.Run(ctx, relink.Options{
		Root: cwd, GiteaHost: parsed.Host, GitHubUser: ghLogin, GitHubTok: ghToken,
		OldRemoteName: "gitea", Mode: relink.ModeGitHub, Verify: true, DryRun: true,
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
		GiteaHost: parsed.Host, GitHubUser: ghLogin, GitHubTok: ghToken,
		OldRemoteName: "gitea", Mode: relink.ModeGitHub, Verify: true,
		GitEnv: migrate.CredentialEnv("x-access-token", ghToken),
	}
	only, modes, chosenRoot, cancelled, screenErr := chooseRelink(ctx,
		probe, relink.ModeGitHub, "gitea", cwd, ghLogin, relinkScanner(ctx, base))
	switch {
	case screenErr != nil:
		fmt.Printf("  %v\n", screenErr)
		return
	case cancelled, len(only) == 0:
		fmt.Println("  Left alone; no remote was changed.")
		return
	}

	// The screen may have been pointed elsewhere, in which case the chosen
	// paths came from that scan rather than this one.
	root := cwd
	if newRoot := expandHome(chosenRoot); chosenRoot != "" && newRoot != cwd {
		root = newRoot
		base.Root, base.DryRun = root, true
		if rescanned, err := relink.Run(ctx, base); err == nil {
			probe = rescanned
		}
		base.DryRun = false
	}

	plan := relinkPlanFromProbe(probe, only, modes, relink.ModeGitHub, "gitea")
	printRelinkResults(plan)
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

// serialLog returns a progress printer safe to call from several workers.
//
// Both migrations and relink scans run concurrently, and without this the
// progress lines interleave mid-word.

// chooseRelink opens the repointing screen and returns what was chosen.
func chooseRelink(ctx context.Context, probe []relink.Result, mode, oldName, root, ghLogin string,
	rescan tui.Rescan) (
	only map[string]bool, modes map[string]string, finalRoot string, cancelled bool, err error) {

	clones := clonesFromProbe(ctx, probe, mode)

	model := tui.NewRelinkModel(clones, mode, oldName).WithRoot(shortenPath(root), rescan)
	header := fmt.Sprintf("github.com/%s", ghLogin)
	switch screenErr := tui.Run(header, model); {
	case errors.Is(screenErr, tui.ErrCancelled):
		return nil, nil, "", true, nil
	case screenErr != nil:
		return nil, nil, "", false, screenErr
	}
	only, modes = model.Chosen()
	// The screen may have been pointed somewhere else, in which case the paths
	// it chose belong to a different directory than the one it opened on.
	return only, modes, model.Root(), false, nil
}

// relinkScanner returns the callback the repointing screen uses to look at
// another directory.
//
// The screen is given a function rather than the options themselves so that it
// stays free of the migrate and relink packages' configuration, and so a test
// can drive it with a fake that touches no disk.

// errScreenCancelled marks a screen the user left without confirming, so the
// caller can tell it apart from a real failure.
var errScreenCancelled = errors.New("screen cancelled")

// chooseRelink opens the repointing screen and returns what was chosen.

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

// clonesFromProbe turns a scan into rows for the screen.
func clonesFromProbe(ctx context.Context, probe []relink.Result, mode string) []tui.Clone {
	clones := make([]tui.Clone, 0, len(probe))
	for _, r := range probe {
		clone := tui.Clone{
			Path: r.Path, Display: shortenPath(r.Path), Mode: mode,
			Redacted: r.Redacted, Public: r.Public,
		}
		// Anything the scan did not mark as planned cannot be repointed by
		// this run, so the reason it gave is shown instead of a destination.
		switch {
		case r.Action != "planned":
			clone.Blocked = r.Reason
		case r.Redacted:
			// Only asked of the clones it can matter for. Taking on a
			// rewritten history is the one operation here that can lose work,
			// and the screen has to know before it offers it.
			clone.Risk = relink.CheckAdoptable(ctx, r.Path).Reason
		}
		clones = append(clones, clone)
	}
	return clones
}

// expandHome turns a leading ~ back into the home directory, so a path typed
// on the screen behaves the way the same path typed at a shell would.

// relinkPlanFromProbe narrows the scan to the chosen clones and relabels each
// with the destination picked for it.
func relinkPlanFromProbe(probe []relink.Result, only map[string]bool,
	modes map[string]string, fallback, oldName string) []relink.Result {

	var plan []relink.Result
	for _, r := range probe {
		if only != nil && !only[r.Path] {
			continue
		}
		if r.Action == "planned" {
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

// shortenPath replaces the home directory with ~ so a column of paths stays
// readable on a narrow terminal.

// printRelinkResults renders the relink table, used for both the plan and the
// outcome.
func printRelinkResults(results []relink.Result) {
	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ACTION\tPATH\tGITHUB\tDETAIL")
	for _, r := range results {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Action, shortenPath(r.Path), r.NewURL, r.Reason)
	}
	_ = w.Flush()
	fmt.Println()
}

// filterByName keeps only the repositories whose name matches one of the given
// names, comparing case-insensitively and accepting either the bare name or the
// owner/name form.
