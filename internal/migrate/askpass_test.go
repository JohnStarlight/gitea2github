package migrate

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestAskpassAnswersTheRightPrompt covers the one ambiguity in the GIT_ASKPASS
// protocol: git runs the same helper twice and the only thing telling the two
// calls apart is the wording of the prompt. Answering them the wrong way round
// sends the token as the username, which fails as a baffling 401.
func TestAskpassAnswersTheRightPrompt(t *testing.T) {
	t.Setenv(askpassUserEnv, "ivogiake")
	t.Setenv(askpassTokenEnv, "ghp_secret")

	cases := map[string]string{
		"Username for 'https://gitea.example.com': ":          "ivogiake",
		"username for 'https://gitea.example.com': ":          "ivogiake",
		"Password for 'https://ivogiake@gitea.example.com': ": "ghp_secret",
		"Password for 'https://x-access-token@github.com': ":  "ghp_secret",
	}
	for prompt, want := range cases {
		var out strings.Builder
		Askpass([]string{prompt}, &out)
		if got := strings.TrimSpace(out.String()); got != want {
			t.Errorf("Askpass(%q) = %q, want %q", prompt, got, want)
		}
	}
}

// TestAskpassAnswersWithANewline checks the part of the protocol that is easy
// to miss: git reads one line, so the answer has to be terminated.
func TestAskpassAnswersWithANewline(t *testing.T) {
	t.Setenv(askpassTokenEnv, "ghp_secret")
	var out strings.Builder
	Askpass([]string{"Password for 'https://x': "}, &out)
	if got := out.String(); !strings.HasSuffix(got, "\n") {
		t.Errorf("Askpass wrote %q, want it to end in a newline", got)
	}
}

// TestAskpassWithoutAPromptIsSafe covers git calling the helper with no
// argument, which must not panic and must not answer with the username.
func TestAskpassWithoutAPromptIsSafe(t *testing.T) {
	t.Setenv(askpassUserEnv, "ivogiake")
	t.Setenv(askpassTokenEnv, "ghp_secret")
	var out strings.Builder
	Askpass(nil, &out)
	if got := strings.TrimSpace(out.String()); got != "ghp_secret" {
		t.Errorf("Askpass(nil) = %q, want the token", got)
	}
}

// TestAskpassEnvCarriesTheCredentialAndNoURL is the property the whole change
// exists for: the credential travels in the environment, and nothing hands git
// a URL with a secret in it.
func TestAskpassEnvCarriesTheCredentialAndNoURL(t *testing.T) {
	env := askpassEnv("/usr/local/bin/gitea2github", "ivogiake", "ghp_secret")

	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"GIT_ASKPASS=/usr/local/bin/gitea2github",
		AskpassEnv + "=1",
		askpassUserEnv + "=ivogiake",
		askpassTokenEnv + "=ghp_secret",
		// Still required: the helper answering does not stop git falling
		// through to a terminal prompt when the answer is rejected.
		"GIT_TERMINAL_PROMPT=0",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("askpassEnv is missing %q", want)
		}
	}

	for _, entry := range env {
		if strings.Contains(entry, "://") && strings.Contains(entry, "ghp_secret") {
			t.Errorf("a credential was spliced into a URL: %q", entry)
		}
	}
}

// TestRunGitRefusesACredentialInTheArguments is the guard against the change
// this whole mechanism exists to prevent: someone splicing a token back into a
// URL, which works perfectly and leaks it to `ps` and to the clone's config.
func TestRunGitRefusesACredentialInTheArguments(t *testing.T) {
	const secret = "ghp_secret"

	_, err := runGitAs(context.Background(), "", "ivogiake", secret,
		"clone", "--mirror", "https://ivogiake:"+secret+"@gitea.example.com/me/r.git", "dest")
	if err == nil {
		t.Fatal("runGitAs ran a command with the credential in its arguments")
	}
	if !strings.Contains(err.Error(), "askpass") {
		t.Errorf("the refusal does not say what to do instead: %v", err)
	}
}

// TestRunGitAllowsACleanURL is the other half: the guard must not stand in the
// way of the ordinary call, or it would be turned off at the first opportunity.
func TestRunGitAllowsACleanURL(t *testing.T) {
	// --version touches nothing and needs no network, so this checks that the
	// guard lets a normal invocation through rather than what git then does.
	out, err := runGitAs(context.Background(), "", "ivogiake", "ghp_secret", "--version")
	if err != nil {
		t.Fatalf("runGitAs refused an ordinary command: %v (%s)", err, out)
	}
	if !strings.HasPrefix(out, "git version") {
		t.Errorf("unexpected output from git --version: %q", out)
	}
}

// TestNoCallSiteSplicesCredentialsIntoAURL reads this package's own source and
// fails if anything builds a URL with credentials in it.
//
// The runtime check in runGitAs catches the same mistake, but only once
// somebody runs a migration -- by which time the change has been merged. This
// catches it in CI instead. It is a blunt instrument and only recognises the
// obvious form, which is the form the mistake actually takes: reaching for
// url.UserPassword to authenticate git.
func TestNoCallSiteSplicesCredentialsIntoAURL(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, readErr := os.ReadFile(name)
		if readErr != nil {
			t.Fatalf("reading %s: %v", name, readErr)
		}
		for _, banned := range []string{"url.UserPassword", "url.User("} {
			if strings.Contains(string(source), banned) {
				t.Errorf("%s uses %s: credentials belong in askpass, not in a URL "+
					"-- a URL carrying a token is published by ps and written "+
					"into the clone's config", name, banned)
			}
		}
	}
}
