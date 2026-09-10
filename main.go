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
  migrate   Mirror repositories to GitHub (use --dry-run first)
  relink    Repoint local clones from Gitea to GitHub

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

	fmt.Println("Gitea")
	client, c, err := resolveGitea(*giteaURL)
	if err != nil {
		fmt.Printf("  credential   FAIL  %v\n", err)
	} else {
		fmt.Printf("  credential   ok    user=%s via %s\n", c.Username, c.Source)
		settingsURL := strings.TrimRight(*giteaURL, "/") + "/user/settings/applications"

		// Version needs no particular scope, so it separates "the server or the
		// token is wrong" from "the token is fine but too narrow". Getting that
		// order right is the whole point of this command.
		version, err := client.Version(ctx)
		var authErr *gitea.AuthError
		switch {
		case err == nil:
			fmt.Printf("  reachable    ok    %s (Gitea %s)\n", *giteaURL, version)

			// Listing needs the broadest scope of anything the migrator does,
			// so probing it here turns a later mysterious 403 into advice.
			if repos, err := client.ListRepos(ctx); err != nil {
				var scopeErr *gitea.ScopeError
				if errors.As(err, &scopeErr) {
					fmt.Printf("  list repos   FAIL  %s\n", scopeErr.Message)
					fmt.Printf("               fix   create a token with BOTH read:user and write:repository\n")
					fmt.Printf("                     at %s\n", settingsURL)
				} else {
					fmt.Printf("  list repos   FAIL  %v\n", err)
				}
			} else {
				fmt.Printf("  list repos   ok    %d repositories visible\n", len(repos))
			}

		case errors.As(err, &authErr):
			// The saved credential is stale far more often than it is merely
			// under-scoped, because minting a replacement token in Gitea
			// invalidates the old one while the keychain keeps serving it.
			fmt.Printf("  token        FAIL  the server rejected it: %s\n", authErr.Message)
			fmt.Printf("               fix   the token is wrong, expired or revoked. Try a current one with\n")
			fmt.Printf("                     GITEA_TOKEN=<token> gitea2github doctor\n")
			fmt.Printf("                     Mint one at %s\n", settingsURL)

		default:
			fmt.Printf("  reachable    FAIL  %v\n", err)
		}
	}

	fmt.Println("\nGitHub")
	ghCred, err := creds.GitHub()
	if err != nil {
		fmt.Printf("  credential   FAIL  %v\n", err)
		return nil
	}
	fmt.Printf("  credential   ok    via %s\n", ghCred.Source)
	login, err := github.New(ghCred.Token).Login(ctx)
	if err != nil {
		fmt.Printf("  identity     FAIL  %v\n", err)
		return nil
	}
	fmt.Printf("  identity     ok    %s\n", login)
	return nil
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
	dryRun := fs.Bool("dry-run", false, "resolve and filter everything, but change nothing")
	collabs := fs.Bool("collaborations", false, "also migrate repositories owned by other Gitea users")
	forks := fs.Bool("forks", false, "also migrate forks")
	archived := fs.Bool("archived", false, "also migrate archived repositories")
	private := fs.Bool("private", false, "create every GitHub repository private, regardless of Gitea visibility")
	concurrency := fs.Int("jobs", 4, "how many repositories to transfer at once")
	only := fs.String("only", "", "comma-separated repository names to migrate (default: all visible)")
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

	fmt.Printf("%s -> github.com/%s  (%d repositories considered)\n\n",
		*giteaURL, ghLogin, len(repos))
	if *dryRun {
		fmt.Println("DRY RUN - nothing will be created or pushed")
	}

	// Workers log concurrently, so serialise writes to stdout. Without this the
	// progress lines interleave mid-word.
	var logMu sync.Mutex
	logf := func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		fmt.Printf("  "+format+"\n", args...)
	}

	// A single Mapper is shared by every worker so that one person is redacted
	// to the same replacement address across all of the migrated repositories.
	var mapper *redact.Mapper
	if *redactEmails {
		mapper = redact.NewMapper(*redactDomain, keepEmails)
		fmt.Printf("Redacting email addresses; %d address(es) kept as-is\n", len(keepEmails))
	}

	results := migrate.Run(ctx, repos, migrate.Options{
		GiteaUser:             giteaCred.Username,
		GiteaToken:            giteaCred.Token,
		GitHubUser:            ghLogin,
		GitHubTok:             ghCred.Token,
		IncludeCollaborations: *collabs,
		IncludeForks:          *forks,
		IncludeArchived:       *archived,
		ForcePrivate:          *private,
		DryRun:                *dryRun,
		Concurrency:           *concurrency,
		Mapper:                mapper,
		Log:                   logf,
	})

	if mapper != nil {
		fmt.Printf("\n%d distinct email address(es) replaced\n", mapper.Count())
	}
	return printSummary(results)
}

// printSummary renders the per-repository outcome table and returns a non-nil
// error if anything failed, so the process exit code reflects the run.
func printSummary(results []migrate.Result) error {
	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "STATUS\tREPOSITORY\tDETAIL")
	counts := map[migrate.Status]int{}
	for _, r := range results {
		counts[r.Status]++
		detail := r.Reason
		if r.Status == migrate.StatusMigrated {
			detail = fmt.Sprintf("-> %s (%s)", r.Target, r.Took.Round(100_000_000))
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", r.Status, r.Source, detail)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Printf("\n%d migrated, %d already present, %d skipped, %d failed, %d planned\n",
		counts[migrate.StatusMigrated], counts[migrate.StatusExists],
		counts[migrate.StatusSkipped], counts[migrate.StatusFailed],
		counts[migrate.StatusPlanned])

	if counts[migrate.StatusFailed] > 0 {
		return fmt.Errorf("%d repositories failed to migrate", counts[migrate.StatusFailed])
	}
	return nil
}

// cmdRelink repoints local working copies at GitHub.
func cmdRelink(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("relink", flag.ExitOnError)
	giteaURL := giteaFlags(fs)
	dryRun := fs.Bool("dry-run", false, "report what would change without touching anything")
	oldName := fs.String("keep-as", "gitea", "name to give the existing Gitea remote")
	verify := fs.Bool("verify", true, "confirm the GitHub repository exists before repointing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root := fs.Arg(0)
	if root == "" {
		return fmt.Errorf("usage: gitea2github relink [flags] <directory>")
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

	results, err := relink.Run(ctx, relink.Options{
		Root:          root,
		GiteaHost:     parsed.Host,
		GitHubUser:    ghLogin,
		GitHubTok:     ghCred.Token,
		OldRemoteName: *oldName,
		DryRun:        *dryRun,
		Verify:        *verify,
		Log:           func(format string, args ...any) { fmt.Printf("  "+format+"\n", args...) },
	})
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ACTION\tPATH\tDETAIL")
	for _, r := range results {
		detail := r.Reason
		if detail == "" {
			detail = r.NewURL
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", r.Action, r.Path, detail)
	}
	return w.Flush()
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
