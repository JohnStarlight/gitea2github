package migrate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/JohnStarlight/gitea2github/internal/redact"
)

// TestRewriteHistoryFailsInsteadOfHanging covers fast-import dying part way
// through. The filter's next write then fails and it stops reading, which
// leaves fast-export blocked on a full pipe; waiting for fast-export at that
// point used to wait forever, freezing the whole run with nothing on screen
// until Ctrl-C.
//
// It also checks that the error says why. The filter only sees a broken pipe;
// the reason is on fast-import's stderr.
func TestRewriteHistoryFailsInsteadOfHanging(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// fast-import is made to fail by taking away its permission to write,
	// which neither root nor Windows honours.
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs file permissions that bind the current user")
	}

	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	dst := filepath.Join(tmp, "dst.git")

	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=A", "GIT_AUTHOR_EMAIL=a@example.com",
			"GIT_COMMITTER_NAME=A", "GIT_COMMITTER_EMAIL=a@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}

	// A file far larger than a pipe's buffer, so that fast-export is still
	// writing when the other end stops reading.
	git(tmp, "init", "-q", src)
	big := make([]byte, 2<<20)
	for i := range big {
		big[i] = byte(i * 7)
	}
	if err := os.WriteFile(filepath.Join(src, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	git(src, "add", ".")
	git(src, "commit", "-qm", "big")

	git(tmp, "init", "-q", "--bare", dst)
	locked := []string{filepath.Join(dst, "objects"), filepath.Join(dst, "objects", "pack")}
	for _, dir := range locked {
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, dir := range locked {
			_ = os.Chmod(dir, 0o755)
		}
	})

	// Cancelled when the test ends, so that a regression does not leave a
	// stuck fast-export behind it.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rewriteHistory(ctx, src, dst, redact.NewMapper(nil, "")) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("rewriteHistory succeeded into a repository it cannot write to")
		}
		if !strings.Contains(err.Error(), "fast-import said:") {
			t.Errorf("error does not carry fast-import's reason: %v", err)
		}
		t.Logf("reported: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("rewriteHistory hung after fast-import failed")
	}
}
