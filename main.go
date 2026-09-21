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
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/JohnStarlight/gitea2github/internal/creds"
	"github.com/JohnStarlight/gitea2github/internal/gitea"
	"github.com/JohnStarlight/gitea2github/internal/migrate"
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
  relink    Repoint local clones from Gitea to GitHub (a directory, or the current one)

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

// giteaFlags registers the flags every Gitea-touching command shares.
func giteaFlags(fs *flag.FlagSet) *string {
	return fs.String("gitea-url", defaultGiteaURL, "Gitea base URL")
}

// resolveGitea builds an authenticated Gitea client and reports which
// credential store answered, so failures are self-diagnosing.

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

// serialLog returns a progress printer safe to call from several workers.
//
// Both migrations and relink scans run concurrently, and without this the
// progress lines interleave mid-word.
func serialLog() func(string, ...any) {
	var mu sync.Mutex
	return func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Printf("  "+format+"\n", args...)
	}
}

// printResults renders the per-repository outcome table and the tally beneath
// it. It deliberately returns nothing: the same renderer prints the plan and
// the outcome, and a plan is not a failure even when it contains problems.
//
// Rows that will actually be created are numbered, because the plan doubles as
// the list the user picks from when choosing visibility. Numbering only those
// rows keeps the numbers meaningful: there is nothing to choose about a
// repository that is being skipped.

// forDisplay strips any credentials from a URL before it is printed.
//
// The Gitea address comes from a flag, and somebody who is used to
// authenticating that way will sooner or later pass
// https://me:token@gitea.example.com/git. Echoing it back verbatim would put
// their token in the terminal scrollback, in a screenshot, and in the bug
// report they paste it into. Nothing else needs the credential -- the API
// client sends it as a header, and git is handed its own through askpass -- so
// the display is the only place it could escape from.
func forDisplay(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		// Unparseable, so the structure cannot be trusted: drop everything up
		// to an "@" rather than guess which part was the secret.
		if at := strings.Index(rawURL, "@"); at >= 0 {
			if slash := strings.Index(rawURL, "//"); slash >= 0 && slash < at {
				return rawURL[:slash+2] + rawURL[at+1:]
			}
		}
		return rawURL
	}
	if parsed.User == nil {
		return rawURL
	}
	parsed.User = nil
	return parsed.String()
}

// errScreenCancelled marks a screen the user left without confirming, so the
// caller can tell it apart from a real failure.

// expandHome turns a leading ~ back into the home directory, so a path typed
// on the screen behaves the way the same path typed at a shell would.
func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
}

// relinkPlanFromProbe narrows the scan to the chosen clones and relabels each
// with the destination picked for it.

// shortenPath replaces the home directory with ~ so a column of paths stays
// readable on a narrow terminal.
func shortenPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || !strings.HasPrefix(path, home) {
		return path
	}
	return "~" + strings.TrimPrefix(path, home)
}

// buildRows turns the repository list and its dry-run probe into the rows the
// selection screen displays.
//
// The probe supplies the two facts the Gitea listing cannot: whether the
// repository is already on GitHub, and whether it would fail outright. Both
// become Blocked, which keeps such rows on screen with their reason rather
// than quietly dropping them -- "where did my repository go?" is a worse
// question to leave a user with than a greyed-out line answering it.

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

// count renders "1 thing" or "3 things".
func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
