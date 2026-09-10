package migrate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JohnStarlight/gitea2github/internal/redact"
)

// trapFile contains a line that is indistinguishable from a fast-export author
// header. If the rewrite ever becomes line-based, this file is what silently
// gets corrupted, so the integration test carries it through on purpose.
const trapFile = "author Trap <trap@example.com> 1717000000 +0000\n"

// TestRewriteHistoryAgainstRealGit drives the export/filter/import pipeline
// against an actual repository.
//
// The unit tests in the redact package prove the filter handles a hand-built
// stream; only this proves that what git actually emits round-trips back into a
// valid repository with the history intact.
func TestRewriteHistoryAgainstRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")

	git := func(dir string, env []string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if env != nil {
			cmd.Env = append(cmd.Environ(), env...)
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	alice := []string{
		"GIT_AUTHOR_NAME=Alice", "GIT_AUTHOR_EMAIL=alice@example.com",
		"GIT_COMMITTER_NAME=Alice", "GIT_COMMITTER_EMAIL=alice@example.com",
	}
	carol := []string{
		"GIT_AUTHOR_NAME=Carol", "GIT_AUTHOR_EMAIL=carol@example.com",
		"GIT_COMMITTER_NAME=Carol", "GIT_COMMITTER_EMAIL=carol@example.com",
	}

	git(tmp, nil, "init", "-q", "-b", "main", "src")
	writeFile(t, filepath.Join(src, "trap.txt"), trapFile)
	git(src, nil, "add", "-A")
	// The Co-authored-by trailer is the realistic case: a teammate's address
	// living in the message body rather than in the identity headers.
	git(src, alice, "commit", "-q", "-m", "First commit\n\nCo-authored-by: Bob <bob@example.com>")

	writeFile(t, filepath.Join(src, "second.txt"), "hello\n")
	git(src, nil, "add", "-A")
	git(src, carol, "commit", "-q", "-m", "Second commit")
	git(src, alice, "tag", "-a", "v1.0", "-m", "release one")

	dst := filepath.Join(tmp, "dst.git")
	mapper := redact.NewMapper("", []string{"carol@example.com"})
	if err := rewriteHistory(ctx, src, dst, mapper); err != nil {
		t.Fatalf("rewriteHistory: %v", err)
	}

	// Every identity and message in the rewritten history, in one blob of text.
	history := git(dst, nil, "log", "--all", "--format=%an <%ae>|%cn <%ce>|%B")

	for _, gone := range []string{"alice@example.com", "bob@example.com"} {
		if strings.Contains(history, gone) {
			t.Errorf("%s survived the rewrite:\n%s", gone, history)
		}
	}
	// The keep list is what lets you stay linked to your own GitHub profile.
	if !strings.Contains(history, "carol@example.com") {
		t.Errorf("kept address was redacted anyway:\n%s", history)
	}
	// Names must survive even though addresses do not, or the history stops
	// being readable as a record of who did what.
	if !strings.Contains(history, "Alice") {
		t.Errorf("author name was lost:\n%s", history)
	}

	// File content must be untouched, trap line and all.
	if got := git(dst, nil, "show", "main:trap.txt"); got != strings.TrimSpace(trapFile) {
		t.Errorf("file content was altered by the rewrite:\ngot:  %q\nwant: %q", got, strings.TrimSpace(trapFile))
	}

	// Structure must survive: both commits, the tag, and a valid HEAD.
	if count := git(dst, nil, "rev-list", "--count", "main"); count != "2" {
		t.Errorf("expected 2 commits on main, got %s", count)
	}
	if tags := git(dst, nil, "tag", "--list"); !strings.Contains(tags, "v1.0") {
		t.Errorf("annotated tag did not survive, tags = %q", tags)
	}
	if head := git(dst, nil, "symbolic-ref", "HEAD"); head != "refs/heads/main" {
		t.Errorf("HEAD = %q, want refs/heads/main", head)
	}
	if err := exec.Command("git", "-C", dst, "fsck", "--strict").Run(); err != nil {
		t.Errorf("rewritten repository fails fsck: %v", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
