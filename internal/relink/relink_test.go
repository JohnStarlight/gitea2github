package relink

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testGiteaHost = "gitea.example.com"
	testGiteaURL  = "https://gitea.example.com/someone/demo.git"
	testGitHubURL = "https://github.com/octocat/demo.git"
)

// newClone builds a working copy whose origin points at a Gitea instance,
// which is the state every relink mode starts from.
func newClone(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "demo")
	run(t, root, "git", "init", "-q", "demo")
	run(t, dir, "git", "remote", "add", "origin", testGiteaURL)
	return root
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// remoteURL returns a remote's URL, or "" when the remote does not exist.
func remoteURL(t *testing.T, dir, name string) string {
	t.Helper()
	cmd := exec.Command("git", "remote", "get-url", name)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func pushURLs(t *testing.T, dir string) []string {
	t.Helper()
	cmd := exec.Command("git", "config", "--get-all", "remote.origin.pushurl")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return strings.Fields(strings.TrimSpace(string(out)))
}

func options(root, mode string) Options {
	return Options{
		Root:          root,
		GiteaHost:     testGiteaHost,
		GitHubUser:    "octocat",
		OldRemoteName: "gitea",
		Mode:          mode,
		// Verify would need a real GitHub account behind it.
		Verify: false,
	}
}

// TestModeGitHubMovesOrigin covers the default: GitHub takes over origin and
// Gitea survives under another name, so a mistake is recoverable.
func TestModeGitHubMovesOrigin(t *testing.T) {
	root := newClone(t)
	dir := filepath.Join(root, "demo")

	if _, err := Run(context.Background(), options(root, ModeGitHub)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := remoteURL(t, dir, "origin"); got != testGitHubURL {
		t.Errorf("origin = %q, want %q", got, testGitHubURL)
	}
	if got := remoteURL(t, dir, "gitea"); got != testGiteaURL {
		t.Errorf("gitea remote = %q, want the original Gitea URL", got)
	}
}

// TestModeGiteaLeavesOriginAlone covers the conservative mode, where pushing to
// GitHub must always be deliberate.
func TestModeGiteaLeavesOriginAlone(t *testing.T) {
	root := newClone(t)
	dir := filepath.Join(root, "demo")

	if _, err := Run(context.Background(), options(root, ModeGitea)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := remoteURL(t, dir, "origin"); got != testGiteaURL {
		t.Errorf("origin = %q, want it untouched at %q", got, testGiteaURL)
	}
	if got := remoteURL(t, dir, "github"); got != testGitHubURL {
		t.Errorf("github remote = %q, want %q", got, testGitHubURL)
	}
	if urls := pushURLs(t, dir); len(urls) != 0 {
		t.Errorf("origin gained push URLs it should not have: %v", urls)
	}
}

// TestModeBothPushesToBothServers is the mode for someone still submitting to
// Gitea while mirroring to GitHub.
//
// The assertion that matters is that Gitea is still in the push list. Git stops
// using a remote's ordinary URL for pushing the moment any pushurl is set, so
// adding only GitHub would have silently replaced Gitea rather than joined it.
func TestModeBothPushesToBothServers(t *testing.T) {
	root := newClone(t)
	dir := filepath.Join(root, "demo")

	if _, err := Run(context.Background(), options(root, ModeBoth)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := remoteURL(t, dir, "origin"); got != testGiteaURL {
		t.Errorf("origin fetch URL = %q, want it still on Gitea at %q", got, testGiteaURL)
	}
	urls := pushURLs(t, dir)
	want := []string{testGiteaURL, testGitHubURL}
	if len(urls) != len(want) {
		t.Fatalf("origin push URLs = %v, want %v", urls, want)
	}
	for i := range want {
		if urls[i] != want[i] {
			t.Errorf("push URL %d = %q, want %q", i, urls[i], want[i])
		}
	}
	// Named remotes let a push be aimed at one server on purpose.
	if got := remoteURL(t, dir, "gitea"); got != testGiteaURL {
		t.Errorf("gitea remote = %q, want %q", got, testGiteaURL)
	}
	if got := remoteURL(t, dir, "github"); got != testGitHubURL {
		t.Errorf("github remote = %q, want %q", got, testGitHubURL)
	}
}

// TestModeBothIsIdempotent guards the re-run case: people relink again after
// migrating a few more repositories, and duplicated push URLs would make git
// push to the same server twice.
func TestModeBothIsIdempotent(t *testing.T) {
	root := newClone(t)
	dir := filepath.Join(root, "demo")

	for i := 0; i < 3; i++ {
		if _, err := Run(context.Background(), options(root, ModeBoth)); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	if urls := pushURLs(t, dir); len(urls) != 2 {
		t.Errorf("after 3 runs origin has %d push URLs (%v), want 2", len(urls), urls)
	}
}

// TestSkipsNonGiteaRemotes makes sure a sweep over a directory of mixed
// projects only touches the ones that came from the Gitea being migrated.
func TestSkipsNonGiteaRemotes(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "unrelated")
	run(t, root, "git", "init", "-q", "unrelated")
	run(t, dir, "git", "remote", "add", "origin", "https://example.org/other/thing.git")

	results, err := Run(context.Background(), options(root, ModeGitHub))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 || results[0].Action != "skipped" {
		t.Fatalf("expected the unrelated repository to be skipped, got %+v", results)
	}
	if got := remoteURL(t, dir, "origin"); got != "https://example.org/other/thing.git" {
		t.Errorf("origin was modified to %q", got)
	}
}

// TestTargetsPickTheRenamedRepository covers a repository that took another
// name on GitHub -- because another owner's had the same one, or because it
// was renamed on the selection screen. The clone is matched by its whole
// Gitea name, so it is pointed at where its repository went, not at whatever
// has its last path segment.
func TestTargetsPickTheRenamedRepository(t *testing.T) {
	root := newClone(t)
	opts := options(root, ModeGitHub)
	opts.DryRun = true
	opts.Targets = map[string]string{"someone/demo": "demo-someone"}

	results, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := results[0].NewURL, "https://github.com/octocat/demo-someone.git"; got != want {
		t.Errorf("NewURL = %q, want %q", got, want)
	}

	// Without an entry, the clone's own name is all there is to go on.
	opts.Targets = nil
	results, _ = Run(context.Background(), opts)
	if got, want := results[0].NewURL, "https://github.com/octocat/demo.git"; got != want {
		t.Errorf("without targets, NewURL = %q, want %q", got, want)
	}
}

func TestGiteaFullName(t *testing.T) {
	cases := map[string]string{
		"https://platform.zone01.gr/git/teammate/quadchecker.git": "teammate/quadchecker",
		"https://platform.zone01.gr/git/teammate/quadchecker":     "teammate/quadchecker",
		"https://gitea.example.com/me/demo/":                      "me/demo",
		"git@gitea.example.com:me/demo.git":                       "me/demo",
		"ssh://git@gitea.example.com:2222/me/demo.git":            "me/demo",
		"https://user@gitea.example.com/sub/me/demo.git":          "me/demo",
		// Local paths, which git reads as paths rather than host:path.
		`C:\Users\me\repos\me\demo.git`: "me/demo",
		"C:/Users/me/repos/me/demo.git": "me/demo",
		"/srv/git/me/demo.git":          "me/demo",
		"./me/demo:odd/name.git":        "demo:odd/name",
	}
	for in, want := range cases {
		if got := GiteaFullName(in); got != want {
			t.Errorf("GiteaFullName(%q) = %q, want %q", in, got, want)
		}
	}
}
