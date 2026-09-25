// The migrate command: everything between "what does Gitea have" and "it is on
// GitHub now", including the questions asked when there is no terminal for the
// selection screen.
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
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/JohnStarlight/gitea2github/internal/creds"
	"github.com/JohnStarlight/gitea2github/internal/gitea"
	"github.com/JohnStarlight/gitea2github/internal/github"
	"github.com/JohnStarlight/gitea2github/internal/migrate"
	"github.com/JohnStarlight/gitea2github/internal/redact"
	"github.com/JohnStarlight/gitea2github/internal/tui"
	"github.com/JohnStarlight/gitea2github/internal/ui"
)

// cmdMigrate is the main event.
func cmdMigrate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	giteaURL := giteaFlags(fs)
	dryRun := fs.Bool("dry-run", false, "print the plan and stop, without asking anything")
	assumeYes := fs.Bool("yes", false, "skip the questions and the confirmation, using flags and defaults")
	collabs := fs.Bool("collaborations", false, "also migrate repositories owned by other Gitea users")
	forks := fs.Bool("forks", false, "also migrate forks")
	archived := fs.Bool("archived", false, "also migrate archived repositories")
	visibility := fs.String("visibility", "mirror",
		"visibility of the created repositories: mirror, private, or public")
	concurrency := fs.Int("jobs", 4, "how many repositories to transfer at once")
	only := fs.String("only", "", "comma-separated repository names to migrate (default: all visible)")
	noTUI := fs.Bool("no-tui", false, "choose from numbered prompts instead of the full-screen selector")
	redactEmails := fs.Bool("redact-emails", false, "replace every email address in the history before pushing")
	var keepEmails stringList
	fs.Var(&keepEmails, "keep-email",
		"an address of yours, rewritten to your GitHub no-reply instead of a hash (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(keepEmails) > 0 && !*redactEmails {
		return fmt.Errorf("--keep-email has no effect without --redact-emails")
	}
	var mode migrate.VisibilityMode
	switch *visibility {
	case "mirror":
		mode = migrate.VisibilityMirror
	case "private":
		mode = migrate.VisibilityPrivate
	case "public":
		mode = migrate.VisibilityPublic
	default:
		return fmt.Errorf("--visibility must be mirror, private or public (got %q)", *visibility)
	}

	// Which flags the user actually typed. Anything they set explicitly is
	// their decision and must not be second-guessed by a question; anything
	// they left alone is fair game to ask about.
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	prompt := ui.New()

	client, giteaCred, err := resolveGitea(*giteaURL)
	if err != nil {
		return err
	}
	giteaUser, err := giteaLogin(ctx, client)
	if err != nil {
		return err
	}
	ghCred, err := creds.GitHub()
	if err != nil {
		return err
	}
	ghClient := github.New(ghCred.Token)
	me, err := ghClient.Identity(ctx)
	if err != nil {
		return fmt.Errorf("identifying GitHub user: %w", err)
	}
	ghLogin := me.Login

	repos, err := client.ListRepos(ctx)
	if err != nil {
		return err
	}
	if *only != "" {
		repos = filterByName(repos, strings.Split(*only, ","))
	}
	if len(repos) == 0 {
		fmt.Println("nothing to migrate")
		return nil
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].FullName < repos[j].FullName })

	fmt.Printf("%s -> github.com/%s  (%d repositories visible)\n", forDisplay(*giteaURL), ghLogin, len(repos))

	// Two routes to the same set of answers. The full-screen selector is the
	// good one -- nothing is decided until the whole picture is on screen, so
	// changing your mind about the forks after reading the plan costs a
	// keystroke rather than a restart. It needs a real terminal, though, so the
	// numbered prompts below remain the fallback, the answer to --no-tui, and
	// the only path a script ever takes.
	useTUI := !*noTUI && !*dryRun && !*assumeYes && tui.Available()

	// Destination names are worked out from the whole list before anything
	// runs, so two repositories that want the same one are told apart here
	// rather than by whichever worker happened to finish first.
	targets := migrate.Targets(repos, giteaUser)
	if clashes := migrate.Collisions(repos); len(clashes) > 0 {
		for name, sharing := range clashes {
			fmt.Printf("\n%d repositories are called %q; renaming to keep both:\n",
				len(sharing), name)
			for _, full := range sharing {
				fmt.Printf("  %s -> %s\n", full, targets[full])
			}
		}
	}

	var plan []migrate.Result
	var overrides map[string]bool
	var renames map[string]string

	// Which repositories are to have their history rewritten. Left nil by the
	// numbered prompts, where --redact-emails is all or nothing, and filled in
	// by the selection screen, where it is chosen row by row.
	var redactOnly map[string]bool

	if useTUI {
		// Probe every repository once with all three gates open, so the screen
		// already knows which are on GitHub and which have no commits.
		// Toggling a gate afterwards is then a local recomputation instead of
		// another sweep of the API, which is what lets the screen respond to a
		// keystroke instead of to a round trip.
		probeOptions := migrate.Options{
			GiteaUser:             giteaUser,
			GiteaToken:            giteaCred.Token,
			GitHubUser:            ghLogin,
			GitHubTok:             ghCred.Token,
			IncludeCollaborations: true,
			IncludeForks:          true,
			IncludeArchived:       true,
			Visibility:            mode,
			Concurrency:           *concurrency,
			Target:                targets,
			DryRun:                true,
		}
		fmt.Println("\nChecking what is already on GitHub...")
		probe := migrate.Run(ctx, repos, probeOptions)

		// The screen holds one address to keep unredacted, so it is seeded
		// with whatever the user already named -- their own --keep-email if
		// they passed one, and the address git commits with otherwise. Seeding
		// it from git while a --keep-email was given would quietly add a
		// second address to a list the user had already written by hand.
		seedKeep := gitUserEmail()
		if len(keepEmails) > 0 {
			seedKeep = keepEmails[0]
		}
		model := tui.NewModel(buildRows(repos, probe, giteaUser, targets),
			*collabs, *forks, *archived, *redactEmails, seedKeep)
		screenErr := tui.Run(
			fmt.Sprintf("%s  ->  github.com/%s", forDisplay(*giteaURL), ghLogin), model)
		if errors.Is(screenErr, tui.ErrCancelled) {
			fmt.Println("Cancelled; nothing was changed.")
			return nil
		}
		if screenErr != nil {
			return screenErr
		}

		// Fold the answers back into the flags. The screen has already applied
		// the three gates itself, so the migrator is handed exactly the chosen
		// repositories with its own gates opened: two sets of filters
		// disagreeing about one repository is a bug waiting to happen, and
		// there is no reason for the second set to exist.
		answered := model
		selected := answered.Selected()
		if len(selected) == 0 {
			fmt.Println("Nothing selected; nothing was changed.")
			return nil
		}
		repos = filterByFullName(repos, selected)
		*collabs, *forks, *archived = true, true, true
		*redactEmails = answered.Redact()
		keepEmails = addAddress(keepEmails, answered.KeepEmail())
		overrides = answered.VisibilityOverrides()
		redactOnly = answered.RedactedRepos()
		renames = answered.Renames()

		// The plan is the probe narrowed to what was chosen. Reusing it rather
		// than sweeping the API a second time keeps the wait to one.
		plan = planFromProbe(probe, selected, overrides)
	} else {
		// Ask about anything not already decided on the command line. Skipped
		// entirely for --dry-run, which is a preview of the defaults, and for
		// --yes, which means "do not ask me anything".
		if prompt.Interactive() && !*dryRun && !*assumeYes {
			fmt.Println()
			answers := askExclusions(prompt, repos, giteaUser, given, exclusions{
				Collaborations: *collabs,
				Forks:          *forks,
				Archived:       *archived,
				RedactEmails:   *redactEmails,
				KeepEmail:      gitUserEmail(),
			})
			*collabs, *forks = answers.Collaborations, answers.Forks
			*archived, *redactEmails = answers.Archived, answers.RedactEmails
			if !given["keep-email"] {
				keepEmails = addAddress(keepEmails, answers.KeepEmail)
			}
		}
	}

	// A single Mapper is shared by every worker so that one person is redacted
	// to the same replacement address across all of the migrated repositories.
	var mapper *redact.Mapper
	if *redactEmails {
		mapper = redact.NewMapper(keepEmails, me.NoReply)
	}

	options := migrate.Options{
		GiteaUser:             giteaUser,
		GiteaToken:            giteaCred.Token,
		GitHubUser:            ghLogin,
		GitHubTok:             ghCred.Token,
		IncludeCollaborations: *collabs,
		IncludeForks:          *forks,
		IncludeArchived:       *archived,
		Visibility:            mode,
		VisibilityOverride:    overrides,
		Concurrency:           *concurrency,
		Mapper:                mapper,
		RedactOnly:            redactOnly,
		Target:                targets,
		RenameTo:              renames,
	}

	if !useTUI {
		// Work out the plan by running the whole pipeline in dry-run mode.
		// Deriving it from the same code that will execute it is what makes the
		// preview trustworthy: there is no second implementation to drift out
		// of step.
		fmt.Println("\nWorking out what would change...")
		planOptions := options
		planOptions.DryRun = true
		plan = migrate.Run(ctx, repos, planOptions)
	}

	printResults(plan)

	if *dryRun {
		return nil
	}

	pending := plannedIndices(plan)
	if len(pending) == 0 {
		fmt.Println("Nothing to do.")
		return nil
	}

	// Visibility is chosen per repository, from the plan the user is looking
	// at, because no single answer is right for a whole account. Doing nothing
	// keeps each repository exactly as it is on Gitea. The selector already
	// offers this on its own rows, so it is only asked here.
	if !useTUI && prompt.Interactive() && !*assumeYes {
		if flips := askVisibilityFlips(prompt, plan, pending); len(flips) > 0 {
			options.VisibilityOverride = flips
			for name, private := range flips {
				for i := range plan {
					if plan[i].Source == name {
						plan[i].Private = private
					}
				}
			}
		}
	}

	// The point of no return. Everything above this line is read-only.
	if !*assumeYes {
		if !prompt.Interactive() {
			return fmt.Errorf("refusing to change anything without a terminal to confirm on; "+
				"pass --yes to proceed or --dry-run to preview (%d repositories would be migrated)", len(pending))
		}
		question := fmt.Sprintf("\nMigrate %d repositor%s to github.com/%s?",
			len(pending), plural(len(pending), "y", "ies"), ghLogin)
		if !prompt.Confirm(question, false) {
			fmt.Println("Cancelled; nothing was changed.")
			return nil
		}
	}

	options.Log = serialLog()

	fmt.Println()
	results := migrate.Run(ctx, repos, options)
	printResults(results)
	if mapper != nil {
		fmt.Printf("%d distinct email address(es) replaced\n", mapper.Count())
	}
	if failed := countStatus(results, migrate.StatusFailed); failed > 0 {
		return fmt.Errorf("%d repositories failed to migrate", failed)
	}

	// Moving the repositories is only half the job: the working copies on this
	// machine still push to Gitea. Offering it here rather than leaving it to
	// be remembered is the difference between a finished migration and one the
	// user discovers is unfinished at their next push.
	if countStatus(results, migrate.StatusMigrated) > 0 {
		offerRelink(ctx, prompt, *giteaURL, ghLogin, ghCred.Token, *assumeYes,
			redactedCount(results, options))
	}
	return nil
}

// offerRelink asks whether to repoint the local clones, and opens the
// repointing screen if the answer is yes.
//
// Deliberately quiet about its own failures: the migration has already
// succeeded by this point, and a directory that cannot be scanned is a reason
// to say so and stop, not to report the whole run as failed.

// askExclusions runs the question sequence used when the selection screen is
// not available, returning the answers.
//
// Separated from cmdMigrate so the sequence can be driven by a test: this is
// the path every script, every CI job and every --no-tui run takes, and until
// it was extracted nothing exercised it end to end.
//
// A question is only asked when the account actually contains something it
// would exclude, and never when the flag was given explicitly: asking "include
// forks?" of somebody who has none is noise, and noise is what trains people
// to stop reading prompts. Anything the user set on the command line is their
// decision and must not be second-guessed by a question.
func askExclusions(prompt *ui.Prompter, repos []gitea.Repo, giteaUser string,
	given map[string]bool, current exclusions) exclusions {

	answers := current

	if n := countMatching(repos, func(r gitea.Repo) bool {
		return !r.OwnedBy(giteaUser)
	}); n > 0 && !given["collaborations"] {
		answers.Collaborations = prompt.Confirm(fmt.Sprintf(
			"Include %d repositor%s owned by other people (group projects)?",
			n, plural(n, "y", "ies")), false)
	}
	if n := countMatching(repos, func(r gitea.Repo) bool { return r.Fork }); n > 0 && !given["forks"] {
		answers.Forks = prompt.Confirm(fmt.Sprintf(
			"Include %d fork%s?", n, plural(n, "", "s")), false)
	}
	if n := countMatching(repos, func(r gitea.Repo) bool { return r.Archived }); n > 0 && !given["archived"] {
		answers.Archived = prompt.Confirm(fmt.Sprintf(
			"Include %d archived repositor%s?", n, plural(n, "y", "ies")), false)
	}

	if !given["redact-emails"] {
		// Defaulted to the collaborations answer: a repository with other
		// people's commits in it is the case where publishing addresses
		// matters most.
		answers.RedactEmails = prompt.Confirm(
			"Replace email addresses in the commit history?", answers.Collaborations)
	}
	if answers.RedactEmails && !given["keep-email"] {
		answers.KeepEmail = prompt.Line(
			"  An address of yours, to stay linked to your GitHub profile (blank for none):", current.KeepEmail)
	} else {
		answers.KeepEmail = ""
	}
	return answers
}

// forDisplay strips any credentials from a URL before it is printed.
//
// The Gitea address comes from a flag, and somebody who is used to
// authenticating that way will sooner or later pass
// https://me:token@gitea.example.com/git. Echoing it back verbatim would put
// their token in the terminal scrollback, in a screenshot, and in the bug
// report they paste it into. Nothing else needs the credential -- the API
// client sends it as a header, and git is handed its own through askpass -- so
// the display is the only place it could escape from.

// exclusions carries the answers the numbered prompts collect, in and out.
//
// Passed as a struct rather than as five arguments and five results so that a
// caller cannot silently swap two booleans of the same type, which is exactly
// the mistake that would widen a migration without anyone noticing.
type exclusions struct {
	Collaborations bool
	Forks          bool
	Archived       bool
	RedactEmails   bool
	KeepEmail      string
}

// askExclusions runs the question sequence used when the selection screen is
// not available, returning the answers.
//
// Separated from cmdMigrate so the sequence can be driven by a test: this is
// the path every script, every CI job and every --no-tui run takes, and until
// it was extracted nothing exercised it end to end.
//
// A question is only asked when the account actually contains something it
// would exclude, and never when the flag was given explicitly: asking "include
// forks?" of somebody who has none is noise, and noise is what trains people
// to stop reading prompts. Anything the user set on the command line is their
// decision and must not be second-guessed by a question.

// buildRows turns the repository list and its dry-run probe into the rows the
// selection screen displays.
//
// The probe supplies the two facts the Gitea listing cannot: whether the
// repository is already on GitHub, and whether it would fail outright. Both
// become Blocked, which keeps such rows on screen with their reason rather
// than quietly dropping them -- "where did my repository go?" is a worse
// question to leave a user with than a greyed-out line answering it.
func buildRows(repos []gitea.Repo, probe []migrate.Result, giteaUser string,
	targets map[string]string) []tui.Row {
	byName := make(map[string]migrate.Result, len(probe))
	for _, r := range probe {
		byName[r.Source] = r
	}

	rows := make([]tui.Row, 0, len(repos))
	for _, repo := range repos {
		res, ok := byName[repo.FullName]
		if !ok {
			continue
		}
		row := tui.Row{
			Name:          repo.FullName,
			SourcePrivate: res.SourcePrivate,
			Private:       res.Private,
			Fork:          repo.Fork,
			Archived:      repo.Archived,
			Foreign:       !repo.OwnedBy(giteaUser),
			Target:        targets[repo.FullName],
			Resume:        res.Resume,
		}
		// Marked as renamed when the destination differs from the repository's
		// own name, so the row says where it will land rather than leaving
		// somebody to notice afterwards.
		row.Renamed = row.Target != "" && row.Target != repo.Name
		// Anything the probe did not mark as planned cannot be migrated by
		// this run whatever the user chooses, so the reason it gave is shown
		// instead of a checkbox.
		if res.Status != migrate.StatusPlanned {
			row.Blocked = res.Reason
		}
		rows = append(rows, row)
	}
	return rows
}

// planFromProbe narrows the dry-run probe to the chosen repositories and
// applies the visibility the user picked for each.
//
// Reusing the probe rather than running a second dry run is what keeps the
// selector to a single wait: the answer to "is this already on GitHub?" does
// not change while somebody is reading the screen.

// planFromProbe narrows the dry-run probe to the chosen repositories and
// applies the visibility the user picked for each.
//
// Reusing the probe rather than running a second dry run is what keeps the
// selector to a single wait: the answer to "is this already on GitHub?" does
// not change while somebody is reading the screen.
func planFromProbe(probe []migrate.Result, selected []string, overrides map[string]bool) []migrate.Result {
	chosen := make(map[string]bool, len(selected))
	for _, name := range selected {
		chosen[name] = true
	}

	var plan []migrate.Result
	for _, res := range probe {
		if !chosen[res.Source] {
			continue
		}
		if private, ok := overrides[res.Source]; ok {
			res.Private = private
		}
		plan = append(plan, res)
	}
	return plan
}

// addAddress appends addr to list unless it is empty or already there.
//
// The screen seeds its one address from the list, so handing the same address
// straight back must not lengthen it: a duplicate would make redact.Mapper
// report a count that does not match what the user typed.

// redactedCount is how many of the repositories that moved had their history
// rewritten.
//
// It decides how hard the offer that follows presses. A migration that copied
// histories verbatim leaves clones that still work; one that rewrote them
// leaves clones that cannot push to what was just created, and saying so is
// worth more than a tidy default.
func redactedCount(results []migrate.Result, opts migrate.Options) int {
	n := 0
	for _, r := range results {
		if r.Status == migrate.StatusMigrated && opts.Redacts(r.Source) {
			n++
		}
	}
	return n
}

// printResults renders the per-repository outcome table and the tally beneath
// it. It deliberately returns nothing: the same renderer prints the plan and
// the outcome, and a plan is not a failure even when it contains problems.
//
// Rows that will actually be created are numbered, because the plan doubles as
// the list the user picks from when choosing visibility. Numbering only those
// rows keeps the numbers meaningful: there is nothing to choose about a
// repository that is being skipped.
func printResults(results []migrate.Result) {
	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  #\tSTATUS\tREPOSITORY\tVISIBILITY\tDETAIL")

	counts := map[migrate.Status]int{}
	number := 0
	for _, r := range results {
		counts[r.Status]++

		label, visibility := "", ""
		if r.Status == migrate.StatusPlanned {
			number++
			label = fmt.Sprintf("%3d", number)
			visibility = visibilityWord(r.Private)
			// Flag the ones whose visibility would differ from Gitea's, so a
			// deliberate change is visible and an accidental one is obvious.
			if r.Private != r.SourcePrivate {
				visibility += " (was " + visibilityWord(r.SourcePrivate) + ")"
			}
		}

		detail := r.Reason
		if r.Status == migrate.StatusMigrated {
			detail = fmt.Sprintf("-> %s (%s)", r.Target, r.Took.Round(100_000_000))
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", label, r.Status, r.Source, visibility, detail)
	}
	_ = w.Flush()

	fmt.Printf("\n%d migrated, %d already present, %d skipped, %d failed, %d to do\n\n",
		counts[migrate.StatusMigrated], counts[migrate.StatusExists],
		counts[migrate.StatusSkipped], counts[migrate.StatusFailed],
		counts[migrate.StatusPlanned])
}

// exclusions carries the answers the numbered prompts collect, in and out.
//
// Passed as a struct rather than as five arguments and five results so that a
// caller cannot silently swap two booleans of the same type, which is exactly
// the mistake that would widen a migration without anyone noticing.

// plannedIndices returns the positions of the rows that will actually be
// created, in the order printResults numbers them. Sharing the order is what
// makes the numbers the user types line up with the rows they read.
func plannedIndices(results []migrate.Result) []int {
	var indices []int
	for i, r := range results {
		if r.Status == migrate.StatusPlanned {
			indices = append(indices, i)
		}
	}
	return indices
}

// askVisibilityFlips offers to invert the visibility of individual
// repositories and returns the overrides, keyed by Gitea full name.
//
// Framed as "change these" rather than "choose for each" so that the default --
// pressing Enter -- leaves every repository exactly as it is on Gitea. A
// question that has to be answered for thirty repositories would be answered
// carelessly.

// askVisibilityFlips offers to invert the visibility of individual
// repositories and returns the overrides, keyed by Gitea full name.
//
// Framed as "change these" rather than "choose for each" so that the default --
// pressing Enter -- leaves every repository exactly as it is on Gitea. A
// question that has to be answered for thirty repositories would be answered
// carelessly.
func askVisibilityFlips(prompt *ui.Prompter, plan []migrate.Result, pending []int) map[string]bool {
	fmt.Println("The repositories above will be created with the visibility shown.")
	chosen := prompt.Select(
		"To flip any, enter its number(s) separated by spaces [Enter to keep them as they are]:",
		len(pending))
	if len(chosen) == 0 {
		return nil
	}

	overrides := make(map[string]bool, len(chosen))
	for _, c := range chosen {
		row := plan[pending[c]]
		flipped := !row.Private
		overrides[row.Source] = flipped
		fmt.Printf("  %s  %s -> %s\n", row.Source, visibilityWord(row.Private), visibilityWord(flipped))
	}
	return overrides
}

// visibilityWord renders a visibility boolean the way GitHub labels it.

// visibilityWord renders a visibility boolean the way GitHub labels it.
func visibilityWord(private bool) string {
	if private {
		return "private"
	}
	return "public"
}

// countStatus tallies one outcome across a result set.

// countStatus tallies one outcome across a result set.
func countStatus(results []migrate.Result, status migrate.Status) int {
	n := 0
	for _, r := range results {
		if r.Status == status {
			n++
		}
	}
	return n
}

// countMatching counts the repositories satisfying pred, ignoring empty ones.
//
// Empty repositories can never be migrated whatever the user answers, so
// including them would inflate a count that exists to help someone decide.

// countMatching counts the repositories satisfying pred, ignoring empty ones.
//
// Empty repositories can never be migrated whatever the user answers, so
// including them would inflate a count that exists to help someone decide.
func countMatching(repos []gitea.Repo, pred func(gitea.Repo) bool) int {
	n := 0
	for _, r := range repos {
		if !r.Empty && pred(r) {
			n++
		}
	}
	return n
}

// plural picks a word form, so counts read as sentences rather than as
// "1 repositor(y/ies)".

// filterByName keeps only the repositories whose name matches one of the given
// names, comparing case-insensitively and accepting either the bare name or the
// owner/name form.
func filterByName(repos []gitea.Repo, names []string) []gitea.Repo {
	wanted := make(map[string]bool, len(names))
	for _, n := range names {
		wanted[strings.ToLower(strings.TrimSpace(n))] = true
	}
	var out []gitea.Repo
	for _, r := range repos {
		if wanted[strings.ToLower(r.Name)] || wanted[strings.ToLower(r.FullName)] {
			out = append(out, r)
		}
	}
	return out
}

// stringList collects a flag that may be repeated, so that several addresses
// can be kept with separate --keep-email arguments rather than one
// comma-separated value that would break on any address containing a comma.

// filterByFullName narrows repos to the given Gitea full names, preserving the
// original order so the plan and the results table stay in step.
func filterByFullName(repos []gitea.Repo, names []string) []gitea.Repo {
	keep := make(map[string]bool, len(names))
	for _, n := range names {
		keep[n] = true
	}
	var out []gitea.Repo
	for _, r := range repos {
		if keep[r.FullName] {
			out = append(out, r)
		}
	}
	return out
}

// plannedIndices returns the positions of the rows that will actually be
// created, in the order printResults numbers them. Sharing the order is what
// makes the numbers the user types line up with the rows they read.

// addAddress appends addr to list unless it is empty or already there.
//
// The screen seeds its one address from the list, so handing the same address
// straight back must not lengthen it: a duplicate would make redact.Mapper
// report a count that does not match what the user typed.
func addAddress(list []string, addr string) []string {
	if addr == "" {
		return list
	}
	for _, existing := range list {
		if strings.EqualFold(existing, addr) {
			return list
		}
	}
	return append(list, addr)
}

// filterByFullName narrows repos to the given Gitea full names, preserving the
// original order so the plan and the results table stay in step.
