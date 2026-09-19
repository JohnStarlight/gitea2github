package migrate

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// AskpassEnv marks a run of this binary as git's credential helper rather than
// as the migrator itself.
const AskpassEnv = "GITEA2GITHUB_ASKPASS"

// The credential to hand back, carried in the helper's environment.
const (
	askpassUserEnv  = "GITEA2GITHUB_ASKPASS_USER"
	askpassTokenEnv = "GITEA2GITHUB_ASKPASS_TOKEN"
)

// Askpass answers one credential prompt from git and is the whole of the
// GIT_ASKPASS protocol: git runs the helper with the prompt as its only
// argument and reads the answer from its standard output.
//
// This exists so that a token never reaches git as part of a URL. A URL
// carrying one is exposed twice over: it appears in the argument list that
// `ps` publishes to every user on the machine for as long as the transfer
// runs, and `git clone` records the URL it cloned from, so the token also sits
// in the mirror's config until that directory is removed -- which an
// interrupted run never gets to do.
//
// An environment variable has neither problem. It is readable only by the same
// user and root, it is never written to disk, and it goes no further than the
// git process and the helper git spawns from it.
func Askpass(args []string, out io.Writer) {
	prompt := ""
	if len(args) > 0 {
		prompt = args[0]
	}
	// git asks for the username first and the password second, and the only
	// thing distinguishing the two calls is the wording of the prompt.
	if strings.HasPrefix(strings.ToLower(prompt), "username") {
		fmt.Fprintln(out, os.Getenv(askpassUserEnv))
		return
	}
	fmt.Fprintln(out, os.Getenv(askpassTokenEnv))
}

// CredentialEnv returns the environment entries that let another package run
// git against an authenticated remote the same way this one does.
//
// Exported so that repointing a clone can fetch from GitHub without a second
// copy of this mechanism growing beside the first -- and, more to the point,
// without a second place where somebody might reach for a URL with the token
// in it.
func CredentialEnv(user, token string) []string {
	self, err := os.Executable()
	if err != nil || token == "" {
		return []string{"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "GCM_INTERACTIVE=never"}
	}
	return askpassEnv(self, user, token)
}

// askpassEnv returns the environment entries that point a git subprocess at
// this binary for its credentials.
//
// GIT_TERMINAL_PROMPT stays at 0. It is doing different work than it used to:
// it no longer stops git asking at all -- the helper answers -- but it still
// stops git falling through to a terminal prompt when the helper's answer is
// rejected. Without it, a wrong token turns a clean 403 into a worker hanging
// on a password nobody is there to type.
func askpassEnv(self, user, token string) []string {
	return []string{
		"GIT_ASKPASS=" + self,
		AskpassEnv + "=1",
		askpassUserEnv + "=" + user,
		askpassTokenEnv + "=" + token,
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=never",
	}
}
