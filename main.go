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
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"

	"github.com/JohnStarlight/gitea2github/internal/creds"
	"github.com/JohnStarlight/gitea2github/internal/gitea"
	"github.com/JohnStarlight/gitea2github/internal/github"
	"github.com/JohnStarlight/gitea2github/internal/migrate"
	"github.com/JohnStarlight/gitea2github/internal/redact"
	"github.com/JohnStarlight/gitea2github/internal/relink"
	"github.com/JohnStarlight/gitea2github/internal/tui"
	"github.com/JohnStarlight/gitea2github/internal/ui"
)

// defaultGiteaURL points at the Zone01 Greece instance, which is the audience
// this tool was written for. Any other Gitea works via --gitea-url.
const defaultGiteaURL = "https://platform.zone01.gr/git"

func main() {
	// A cancellable context wired to Ctrl-C means an interrupted migration
	// stops handing out new repositories and still prints the summary for the
	// ones that finished, instead of dying mid-push with no report.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Answer git's credential prompt and exit. Checked before the subcommands
	// because git invokes this binary with a prompt string as its only
	// argument, which matches no subcommand and must not reach usage().
	if os.Getenv(migrate.AskpassEnv) != "" {
		migrate.Askpass(os.Args[1:], os.Stdout)
		return
	}

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "doctor":
		err = cmdDoctor(ctx, os.Args[2:])
	case "list":
		err = cmdList(ctx, os.Args[2:])
	case "migrate":
		err = cmdMigrate(ctx, os.Args[2:])
	case "relink":
		err = cmdRelink(ctx, os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `gitea2github - move repositories from Gitea to GitHub, branches and tags included

Commands:
  doctor    Check that both credentials resolve and have the scopes needed
  list      List the Gitea repositories that would be considered
  migrate   Mirror repositories to GitHub
  relink    Repoint local clones from Gitea to GitHub (takes a directory)

migrate and relink never change anything without showing you the plan first and
asking. Run them with no flags and they will ask what you want.

  --dry-run   print the plan and stop, asking nothing
  --yes       skip the questions and the confirmation (required without a terminal)

Run "gitea2github <command> -h" for the flags of each command.

Credentials are looked up in this order:
  Gitea   GITEA_TOKEN env var, then the git credential helper for the host
  GitHub  GITHUB_TOKEN env var, then the gh CLI
`)
}

// giteaFlags registers the flags every Gitea-touching command shares.
func giteaFlags(fs *flag.FlagSet) *string {
	return fs.String("gitea-url", defaultGiteaURL, "Gitea base URL")
}

// resolveGitea builds an authenticated Gitea client and reports which
// credential store answered, so failures are self-diagnosing.
func resolveGitea(giteaURL string) (*gitea.Client, creds.Credential, error) {
	parsed, err := url.Parse(giteaURL)
	if err != nil {
		return nil, creds.Credential{}, fmt.Errorf("invalid --gitea-url %q: %w", giteaURL, err)
	}
	c, err := creds.Gitea(parsed.Host)
	if err != nil {
		return nil, creds.Credential{}, err
	}
	return gitea.New(giteaURL, c.Token), c, nil
}

// cmdDoctor verifies the whole credential chain before the user commits to a
// migration. Finding out that a token is too narrow after twenty repositories
// have already moved is far worse than finding out up front.
func cmdDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	giteaURL := giteaFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Every line of this report is one check in a fixed-width column, and the
	// remedies are multi-line, so failures are printed through a helper that
	// keeps the continuation lines aligned under the first.
	failures := 0
	ok := func(check, format string, args ...any) {
		fmt.Printf("  %-12s ok    %s\n", check, fmt.Sprintf(format, args...))
	}
	fail := func(check, format string, args ...any) {
		failures++
		fmt.Printf("  %-12s FAIL  %s\n", check, alignContinuation(fmt.Sprintf(format, args...)))
	}
	hint := func(format string, args ...any) {
		fmt.Printf("               fix   %s\n", alignContinuation(fmt.Sprintf(format, args...)))
	}

	fmt.Println("Gitea")
	client, c, err := resolveGitea(*giteaURL)
	if err != nil {
		fail("credential", "%v", err)
	} else {
		ok("credential", "user=%s via %s", c.Username, c.Source)
		settingsURL := strings.TrimRight(*giteaURL, "/") + "/user/settings/applications"

		// Version needs no particular scope, so it separates "the server or the
		// token is wrong" from "the token is fine but too narrow". Getting that
		// order right is the whole point of this command.
		version, err := client.Version(ctx)
		var authErr *gitea.AuthError
		switch {
		case err == nil:
			ok("reachable", "%s (Gitea %s)", *giteaURL, version)

			// Listing needs the broadest scope of anything the migrator does,
			// so probing it here turns a later mysterious 403 into advice.
			if repos, err := client.ListRepos(ctx); err != nil {
				var scopeErr *gitea.ScopeError
				if errors.As(err, &scopeErr) {
					fail("list repos", "%s", scopeErr.Message)
					hint("create a token with BOTH read:user and write:repository\nat %s", settingsURL)
				} else {
					fail("list repos", "%v", err)
				}
			} else {
				ok("list repos", "%d repositories visible", len(repos))
			}

		case errors.As(err, &authErr):
			// The saved credential is stale far more often than it is merely
			// under-scoped, because minting a replacement token in Gitea
			// invalidates the old one while the keychain keeps serving it.
			fail("token", "the server rejected it: %s", authErr.Message)
			hint("the token is wrong, expired or revoked. Try a current one with\nGITEA_TOKEN=<token> gitea2github doctor\nMint one at %s", settingsURL)

		default:
			fail("reachable", "%v", err)
		}
	}

	fmt.Println("\nGitHub")
	ghCred, err := creds.GitHub()
	if err != nil {
		fail("credential", "%v", err)
	} else {
		ok("credential", "via %s", ghCred.Source)
		if login, err := github.New(ghCred.Token).Login(ctx); err != nil {
			fail("identity", "%v", err)
		} else {
			ok("identity", "%s", login)
		}
	}

	// A non-zero exit lets doctor be used as a precondition in a script, which
	// silently returning success would make impossible.
	if failures > 0 {
		fmt.Println()
		return fmt.Errorf("%d check(s) failed; see above", failures)
	}
	return nil
}

// alignContinuation indents every line after the first far enough to sit under
// the start of the message column, so a multi-line remedy does not break the
// report's layout.
func alignContinuation(msg string) string {
	const column = "                     " // width of "  <check>    FAIL  "
	return strings.ReplaceAll(msg, "\n", "\n"+column)
}

// cmdList prints the repositories the migrator can see, with the reason any of
// them would be skipped. It is the cheapest way to sanity-check the filters.
func cmdList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	giteaURL := giteaFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	client, c, err := resolveGitea(*giteaURL)
	if err != nil {
		return err
	}
	repos, err := client.ListRepos(ctx)
	if err != nil {
		return err
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].FullName < repos[j].FullName })

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "REPOSITORY\tVISIBILITY\tOWNERSHIP\tNOTES")
	for _, r := range repos {
		visibility := "public"
		if r.Private {
			visibility = "private"
		}
		ownership := "own"
		if !r.OwnedBy(c.Username) {
			ownership = "collaborator"
		}
		var notes []string
		if r.Empty {
			notes = append(notes, "empty")
		}
		if r.Archived {
			notes = append(notes, "archived")
		}
		if r.Fork {
			notes = append(notes, "fork")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.FullName, visibility, ownership, strings.Join(notes, ","))
	}
	return w.Flush()
}

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
	redactDomain := fs.String("redact-domain", redact.DefaultDomain, "domain to point redacted addresses at")
	var keepEmails stringList
	fs.Var(&keepEmails, "keep-email", "address to leave untouched when redacting (repeatable)")
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
	ghCred, err := creds.GitHub()
	if err != nil {
		return err
	}
	ghClient := github.New(ghCred.Token)
	ghLogin, err := ghClient.Login(ctx)
	if err != nil {
		return fmt.Errorf("identifying GitHub user: %w", err)
	}

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

	fmt.Printf("%s -> github.com/%s  (%d repositories visible)\n", *giteaURL, ghLogin, len(repos))

	// Two routes to the same set of answers. The full-screen selector is the
	// good one -- nothing is decided until the whole picture is on screen, so
	// changing your mind about the forks after reading the plan costs a
	// keystroke rather than a restart. It needs a real terminal, though, so the
	// numbered prompts below remain the fallback, the answer to --no-tui, and
	// the only path a script ever takes.
	useTUI := !*noTUI && !*dryRun && !*assumeYes && tui.Available()

	var plan []migrate.Result
	var overrides map[string]bool

	if useTUI {
		// Probe every repository once with all three gates open, so the screen
		// already knows which are on GitHub and which have no commits.
		// Toggling a gate afterwards is then a local recomputation instead of
		// another sweep of the API, which is what lets the screen respond to a
		// keystroke instead of to a round trip.
		probeOptions := migrate.Options{
			GiteaUser:             giteaCred.Username,
			GiteaToken:            giteaCred.Token,
			GitHubUser:            ghLogin,
			GitHubTok:             ghCred.Token,
			IncludeCollaborations: true,
			IncludeForks:          true,
			IncludeArchived:       true,
			Visibility:            mode,
			Concurrency:           *concurrency,
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
		model := tui.NewModel(buildRows(repos, probe, giteaCred.Username),
			*collabs, *forks, *archived, *redactEmails, seedKeep)
		answered, screenErr := tui.Run(
			fmt.Sprintf("%s  ->  github.com/%s", *giteaURL, ghLogin), model)
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

		// The plan is the probe narrowed to what was chosen. Reusing it rather
		// than sweeping the API a second time keeps the wait to one.
		plan = planFromProbe(probe, selected, overrides)
	} else {
		// Ask about anything not already decided on the command line. Skipped
		// entirely for --dry-run, which is a preview of the defaults, and for
		// --yes, which means "do not ask me anything".
		if prompt.Interactive() && !*dryRun && !*assumeYes {
			fmt.Println()
			// The three exclusions are only worth asking about when the account
			// actually contains something they would exclude. Asking "include
			// forks?" of someone who has none is noise, and noise is what trains
			// people to stop reading prompts.
			if n := countMatching(repos, func(r gitea.Repo) bool {
				return !r.OwnedBy(giteaCred.Username)
			}); n > 0 && !given["collaborations"] {
				*collabs = prompt.Confirm(fmt.Sprintf(
					"Include %d repositor%s owned by other people (group projects)?",
					n, plural(n, "y", "ies")), false)
			}
			if n := countMatching(repos, func(r gitea.Repo) bool { return r.Fork }); n > 0 && !given["forks"] {
				*forks = prompt.Confirm(fmt.Sprintf(
					"Include %d fork%s?", n, plural(n, "", "s")), false)
			}
			if n := countMatching(repos, func(r gitea.Repo) bool { return r.Archived }); n > 0 && !given["archived"] {
				*archived = prompt.Confirm(fmt.Sprintf(
					"Include %d archived repositor%s?", n, plural(n, "y", "ies")), false)
			}

			if !given["redact-emails"] {
				*redactEmails = prompt.Confirm("Replace email addresses in the commit history?", *collabs)
			}
			if *redactEmails && !given["keep-email"] {
				if own := prompt.Line("  Your own address, to keep linked to GitHub (blank for none):", gitUserEmail()); own != "" {
					keepEmails = append(keepEmails, own)
				}
			}
		}
	}

	// A single Mapper is shared by every worker so that one person is redacted
	// to the same replacement address across all of the migrated repositories.
	var mapper *redact.Mapper
	if *redactEmails {
		mapper = redact.NewMapper(*redactDomain, keepEmails)
	}

	options := migrate.Options{
		GiteaUser:             giteaCred.Username,
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

	// Workers log concurrently, so serialise writes to stdout. Without this the
	// progress lines interleave mid-word.
	var logMu sync.Mutex
	options.Log = func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		fmt.Printf("  "+format+"\n", args...)
	}

	fmt.Println()
	results := migrate.Run(ctx, repos, options)
	printResults(results)
	if mapper != nil {
		fmt.Printf("%d distinct email address(es) replaced\n", mapper.Count())
	}
	if failed := countStatus(results, migrate.StatusFailed); failed > 0 {
		return fmt.Errorf("%d repositories failed to migrate", failed)
	}
	return nil
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

// buildRows turns the repository list and its dry-run probe into the rows the
// selection screen displays.
//
// The probe supplies the two facts the Gitea listing cannot: whether the
// repository is already on GitHub, and whether it would fail outright. Both
// become Blocked, which keeps such rows on screen with their reason rather
// than quietly dropping them -- "where did my repository go?" is a worse
// question to leave a user with than a greyed-out line answering it.
func buildRows(repos []gitea.Repo, probe []migrate.Result, giteaUser string) []tui.Row {
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
		}
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
func visibilityWord(private bool) string {
	if private {
		return "private"
	}
	return "public"
}

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
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// gitUserEmail returns the address git is configured to commit with, used as
// the suggested answer when asking which address to keep unredacted. It is the
// address the user's own commits almost certainly carry.
func gitUserEmail() string {
	out, err := exec.Command("git", "config", "--get", "user.email").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

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

	root := fs.Arg(0)
	if root == "" {
		if !prompt.Interactive() {
			return fmt.Errorf("usage: gitea2github relink [flags] <directory>")
		}
		root = prompt.Line("Which directory holds your clones?", ".")
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
	if prompt.Interactive() && !*dryRun && !*assumeYes && !given["push-to"] {
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
	}

	planOptions := options
	planOptions.DryRun = true
	fmt.Println("\nWorking out what would change...")
	plan, err := relink.Run(ctx, planOptions)
	if err != nil {
		return err
	}
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

	options.Log = func(format string, args ...any) { fmt.Printf("  "+format+"\n", args...) }
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
func printRelinkResults(results []relink.Result) {
	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ACTION\tPATH\tGITHUB\tDETAIL")
	for _, r := range results {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Action, r.Path, r.NewURL, r.Reason)
	}
	_ = w.Flush()
	fmt.Println()
}

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
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("empty value")
	}
	*l = append(*l, value)
	return nil
}
