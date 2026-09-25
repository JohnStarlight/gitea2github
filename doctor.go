// The two commands that only look: doctor, which checks the credentials and
// their scopes before anything depends on them, and list, which shows what
// Gitea holds and how each repository would be classified.
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
)

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
		ok("credential", "via %s", c.Source)
		settingsURL := strings.TrimRight(*giteaURL, "/") + "/user/settings/applications"

		// Version needs no particular scope, so it separates "the server or the
		// token is wrong" from "the token is fine but too narrow". Getting that
		// order right is the whole point of this command.
		version, err := client.Version(ctx)
		var authErr *gitea.AuthError
		switch {
		case err == nil:
			ok("reachable", "%s (Gitea %s)", forDisplay(*giteaURL), version)

			// Whose token it is decides which repositories count as yours.
			// A stored username that disagrees is harmless now -- it is not
			// what is used -- but it is worth knowing about.
			if login, err := client.Login(ctx); err != nil {
				var scopeErr *gitea.ScopeError
				if errors.As(err, &scopeErr) {
					fail("identity", "%s", scopeErr.Message)
					hint("create a token with BOTH read:user and write:repository\nat %s", settingsURL)
				} else {
					fail("identity", "%v", err)
				}
			} else if c.Username != creds.PlaceholderUser && !strings.EqualFold(c.Username, login) {
				ok("identity", "%s (the credential is stored under %q, which Gitea ignores)", login, c.Username)
			} else {
				ok("identity", "%s", login)
			}

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

// alignContinuation indents every line after the first far enough to sit under
// the start of the message column, so a multi-line remedy does not break the
// report's layout.
func alignContinuation(msg string) string {
	const column = "                     " // width of "  <check>    FAIL  "
	return strings.ReplaceAll(msg, "\n", "\n"+column)
}

// cmdList prints the repositories the migrator can see, with the reason any of
// them would be skipped. It is the cheapest way to sanity-check the filters.

// cmdList prints the repositories the migrator can see, with the reason any of
// them would be skipped. It is the cheapest way to sanity-check the filters.
func cmdList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	giteaURL := giteaFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	client, _, err := resolveGitea(*giteaURL)
	if err != nil {
		return err
	}
	login, err := giteaLogin(ctx, client)
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
		if !r.OwnedBy(login) {
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
