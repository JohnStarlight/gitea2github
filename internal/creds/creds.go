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
//  2. The git credential helper chain for that host, which on macOS is
//     typically osxkeychain. This is the same store `git push` reads, so if
//     the user can already push to Gitea from the shell, this just works with
//     no extra setup.
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

	c, err := fromGitCredentialHelper(host)
	if err != nil {
		return Credential{}, fmt.Errorf("no GITEA_TOKEN set and git credential helper failed for %s: %w", host, err)
	}
	return c, nil
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
// GIT_TERMINAL_PROMPT=0 is essential here. Without it, a cache miss makes git
// block on an interactive prompt, which would hang the migrator instead of
// returning a clean error we can explain.
func fromGitCredentialHelper(host string) (Credential, error) {
	cmd := exec.Command("git", "credential", "fill")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
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
		return Credential{}, fmt.Errorf("credential helper returned no password for %s", host)
	}
	c.Source = "git credential helper (osxkeychain)"
	return c, nil
}
