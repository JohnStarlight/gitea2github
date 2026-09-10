// Package creds resolves the authentication tokens the migrator needs.
//
// There are two independent credentials in play:
//
//   - a Gitea token, used to enumerate the source repositories and to clone
//     private ones;
//   - a GitHub token, used to create the destination repositories and push to
//     them.
//
// For each one we try the cheapest, least surprising source first (an
// environment variable the user set deliberately) and only then fall back to
// credential stores that happen to be configured on the machine. This ordering
// matters: an explicit env var must always win, otherwise a stale keychain
// entry silently overrides what the user just typed.
package creds

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Credential is a resolved username/token pair plus a human-readable note about
// where it came from. The Source field exists purely so the CLI can tell the
// user which store answered, which turns "403 Forbidden" from a mystery into a
// one-line diagnosis.
type Credential struct {
	Username string
	Token    string
	Source   string
}

// Gitea resolves the Gitea credential for the given host.
//
// Resolution order:
//  1. GITEA_TOKEN environment variable (username defaults to GITEA_USER).
//  2. The git credential helper chain for that host. This is the same store
//     `git push` reads, so if the user can already push to Gitea from the
//     shell, this just works with no extra setup.
//
// The second step is not tied to any operating system. `git credential fill`
// is git's own protocol and dispatches to whatever helper is configured --
// osxkeychain on macOS, Git Credential Manager or wincred on Windows,
// libsecret, pass or store on Linux -- so the same code path serves every
// platform Go and git run on.
func Gitea(host string) (Credential, error) {
	if tok := os.Getenv("GITEA_TOKEN"); tok != "" {
		user := os.Getenv("GITEA_USER")
		if user == "" {
			// Gitea accepts any non-empty username when the password is a
			// personal access token, but git's credential protocol and basic
			// auth both want *something* in the field.
			user = "token"
		}
		return Credential{Username: user, Token: tok, Source: "GITEA_TOKEN env"}, nil
	}

	helpers := configuredHelpers()
	if len(helpers) == 0 {
		// Worth its own message: on a fresh Linux install no helper is
		// configured at all, and "credential fill failed" would send the user
		// hunting for a broken helper rather than a missing one.
		return Credential{}, fmt.Errorf(
			"no GITEA_TOKEN set and no git credential helper is configured for %s.\n"+
				"Either set GITEA_TOKEN, or configure a helper, for example:\n"+
				"  macOS    git config --global credential.helper osxkeychain\n"+
				"  Windows  git config --global credential.helper manager\n"+
				"  Linux    git config --global credential.helper libsecret   (or 'store' to keep it in a plain file)",
			host)
	}

	c, err := fromGitCredentialHelper(host, helpers)
	if err != nil {
		return Credential{}, fmt.Errorf("no GITEA_TOKEN set and the git credential helper failed for %s: %w", host, err)
	}
	return c, nil
}

// configuredHelpers reports which credential helpers git will consult, in the
// order it will try them.
//
// Reporting the real answer rather than assuming one matters for diagnosis: a
// user whose token is not being found needs to know whether git is asking a
// keychain, a plaintext file, or nothing at all.
func configuredHelpers() []string {
	out, err := exec.Command("git", "config", "--get-all", "credential.helper").Output()
	if err != nil {
		// A non-zero exit here means no helper is set, which is a legitimate
		// state rather than a failure.
		return nil
	}

	var helpers []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		// The same helper is commonly configured in both the system and the
		// global gitconfig; listing it twice would just be noise.
		if name := strings.TrimSpace(line); name != "" && !seen[name] {
			seen[name] = true
			helpers = append(helpers, name)
		}
	}
	return helpers
}

// GitHub resolves the GitHub credential.
//
// Resolution order:
//  1. GITHUB_TOKEN environment variable.
//  2. The `gh` CLI, if installed and authenticated. Most developers who work
//     with GitHub already have `gh auth login` done, so reusing its token
//     spares them from minting a second one by hand.
func GitHub() (Credential, error) {
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		return Credential{Username: "x-access-token", Token: tok, Source: "GITHUB_TOKEN env"}, nil
	}

	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return Credential{}, fmt.Errorf("no GITHUB_TOKEN set and `gh auth token` failed (is the gh CLI installed and logged in?): %w", err)
	}
	tok := strings.TrimSpace(string(out))
	if tok == "" {
		return Credential{}, fmt.Errorf("`gh auth token` returned an empty token")
	}
	return Credential{Username: "x-access-token", Token: tok, Source: "gh CLI"}, nil
}

// fromGitCredentialHelper shells out to `git credential fill`, which speaks a
// simple line-based protocol: we write the request attributes on stdin,
// terminate them with a blank line, and git writes the same attributes back
// with username and password filled in by whichever helper answered.
//
// Suppressing every interactive prompt is essential here, and each platform
// has its own way of asking. Without all three of these a cache miss makes git
// block on a prompt -- a terminal prompt, an askpass GUI, or a Git Credential
// Manager dialog on Windows -- which would hang the migrator instead of
// returning a clean error we can explain.
func fromGitCredentialHelper(host string, helpers []string) (Credential, error) {
	cmd := exec.Command("git", "credential", "fill")
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		// Set but empty, which neutralises an inherited askpass program
		// instead of letting it open a dialog nobody is watching.
		"GIT_ASKPASS=",
		"GCM_INTERACTIVE=never",
	)
	cmd.Stdin = strings.NewReader(fmt.Sprintf("protocol=https\nhost=%s\n\n", host))

	out, err := cmd.Output()
	if err != nil {
		return Credential{}, fmt.Errorf("git credential fill: %w", err)
	}

	var c Credential
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		key, value, found := strings.Cut(scanner.Text(), "=")
		if !found {
			continue
		}
		switch key {
		case "username":
			c.Username = value
		case "password":
			c.Token = value
		}
	}
	if c.Token == "" {
		return Credential{}, fmt.Errorf("helper %s returned no password for %s",
			strings.Join(helpers, ", "), host)
	}
	c.Source = describeHelpers(helpers)
	return c, nil
}

// describeHelpers renders the helper list for the Source field, so that the
// doctor output names the store that actually answered rather than guessing.
func describeHelpers(helpers []string) string {
	if len(helpers) == 0 {
		return "git credential helper"
	}
	return "git credential helper (" + strings.Join(helpers, ", ") + ")"
}
