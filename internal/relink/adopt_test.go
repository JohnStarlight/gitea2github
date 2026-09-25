package relink

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scenario builds a Gitea repository, a redacted copy of it standing in for
// GitHub, and a working copy cloned from Gitea.
func scenario(t *testing.T) (work, gitea, github string) {
	t.Helper()
	root := t.TempDir()
	gitea = filepath.Join(root, "gitea.git")
	github = filepath.Join(root, "github.git")
	work = filepath.Join(root, "work")

	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s in %s: %v: %s", strings.Join(args, " "), dir, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	run(root, "init", "-q", "--bare", gitea)
	run(root, "init", "-q", work)
	run(work, "config", "user.email", "student@zone01.gr")
	run(work, "config", "user.name", "Student")
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", ".")
	run(work, "commit", "-qm", "first")
	run(work, "branch", "-M", "main")
	// A real clone has an origin and a tracking branch, which is what the
	// unpushed-commit check reads.
	run(work, "remote", "add", "origin", gitea)
	run(work, "push", "-q", "-u", "origin", "main")

	// The redacted copy, built the way the migrator builds it.
	run(root, "init", "-q", "--bare", github)
	export := exec.Command("git", "-C", gitea, "fast-export", "--all",
		"--signed-tags=strip", "--tag-of-filtered-object=rewrite", "--use-done-feature")
	imp := exec.Command("git", "-C", github, "fast-import", "--quiet", "--done")
	stream, err := export.Output()
	if err != nil {
		t.Fatalf("fast-export: %v", err)
	}
	imp.Stdin = strings.NewReader(
		strings.ReplaceAll(string(stream), "student@zone01.gr", "e9e3c54b5e@redacted.invalid"))
	if out, err := imp.CombinedOutput(); err != nil {
		t.Fatalf("fast-import: %v: %s", err, out)
	}
	return work, gitea, github
}

func head(t *testing.T, dir string, ref ...string) string {
	t.Helper()
	args := append([]string{"rev-parse"}, ref...)
	if len(ref) == 0 {
		args = append(args, "HEAD")
	}
	out, err := gitOutput(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("rev-parse in %s: %v", dir, err)
	}
	return out
}

// TestAdoptTakesGitHubsOwnObjects is the property that makes this safe to do
// at all: the result is not a reproduction of the rewritten history but the
// history itself, so it cannot differ from it by a subtle mistake.
func TestAdoptTakesGitHubsOwnObjects(t *testing.T) {
	work, _, github := scenario(t)
	ctx := context.Background()

	before := head(t, work)
	if before == head(t, github, "main") {
		t.Fatal("setup: the clone should not already match the rewritten copy")
	}

	if err := Adopt(ctx, work, github, nil, nil); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if got, want := head(t, work), head(t, github, "main"); got != want {
		t.Errorf("after adopting, HEAD is %s, want %s", got, want)
	}
}

// TestAdoptLeavesTheWorkingTreeAlone covers what redaction does not touch.
// Rewriting who made a commit does not change what it contains, so every file
// checked out stays exactly as it was.
func TestAdoptLeavesTheWorkingTreeAlone(t *testing.T) {
	work, _, github := scenario(t)

	scratch := filepath.Join(work, "scratch.txt")
	if err := os.WriteFile(scratch, []byte("not committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Adopt(context.Background(), work, github, nil, nil); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	if body, err := os.ReadFile(filepath.Join(work, "a.txt")); err != nil || string(body) != "one\n" {
		t.Errorf("a tracked file changed: %q, %v", body, err)
	}
	if body, err := os.ReadFile(scratch); err != nil || string(body) != "not committed\n" {
		t.Errorf("untracked work was lost: %q, %v", body, err)
	}
}

// TestAdoptEndsTheGiteaRelationship is the point of the whole operation. The
// clone belongs to the rewritten repository now, and a remote it can no longer
// push to must not be left sitting there looking like one it can.
func TestAdoptEndsTheGiteaRelationship(t *testing.T) {
	work, gitea, github := scenario(t)
	ctx := context.Background()

	if err := Adopt(ctx, work, github, nil, nil); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	remotes, err := gitOutput(ctx, work, "remote")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range strings.Fields(remotes) {
		if name == "gitea" {
			t.Error("a gitea remote survived the adoption")
		}
	}
	if got, _ := gitOutput(ctx, work, "remote", "get-url", "origin"); got != github {
		t.Errorf("origin is %q, want the GitHub URL", got)
	}

	// And the thing the user was protected from: pushing the original history
	// back over the rewritten one.
	if out, err := gitOutput(ctx, work, "push", gitea, "main"); err == nil {
		t.Errorf("the original history could still be pushed to Gitea: %s", out)
	}
}

// TestCheckAdoptableRefusesUncommittedWork stops a hard reset quietly
// reverting files somebody is in the middle of editing.
func TestCheckAdoptableRefusesUncommittedWork(t *testing.T) {
	work, _, _ := scenario(t)

	if risk := CheckAdoptable(context.Background(), work); risk.Reason != "" {
		t.Fatalf("a clean clone was refused: %s", risk.Reason)
	}

	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	risk := CheckAdoptable(context.Background(), work)
	if risk.Reason == "" {
		t.Fatal("a clone with uncommitted changes was allowed to adopt")
	}
	if !strings.Contains(risk.Reason, "uncommitted") {
		t.Errorf("the refusal does not say what is wrong: %q", risk.Reason)
	}
}

// TestCheckAdoptableRefusesUnpushedCommits covers the loss that cannot be
// undone: once the clone belongs to GitHub it can never push to Gitea, so a
// commit that has not reached Gitea would have nowhere left to go.
func TestCheckAdoptableRefusesUnpushedCommits(t *testing.T) {
	work, _, _ := scenario(t)
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(work, "b.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitOutput(ctx, work, "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := gitOutput(ctx, work, "commit", "-qm", "not pushed"); err != nil {
		t.Fatal(err)
	}

	risk := CheckAdoptable(ctx, work)
	if risk.Reason == "" {
		t.Fatal("a clone with unpushed commits was allowed to adopt")
	}
	if !strings.Contains(risk.Reason, "never pushed") {
		t.Errorf("the refusal does not say what is wrong: %q", risk.Reason)
	}
}

// TestUntrackedFilesAreNotAReasonToRefuse keeps the check from being so strict
// that nobody can ever use it: a hard reset leaves untracked files alone.
func TestUntrackedFilesAreNotAReasonToRefuse(t *testing.T) {
	work, _, _ := scenario(t)

	if err := os.WriteFile(filepath.Join(work, "notes.md"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if risk := CheckAdoptable(context.Background(), work); risk.Reason != "" {
		t.Errorf("an untracked file was treated as a danger: %s", risk.Reason)
	}
}

// TestBranchWithNoUpstreamIsRefused is the case that slipped through: a branch
// created locally often has no upstream set, and asking git how far ahead it
// is then fails. Reading that failure as "nothing to worry about" would let an
// operation with no way back proceed over commits nobody could get back.
func TestBranchWithNoUpstreamIsRefused(t *testing.T) {
	work, _, _ := scenario(t)
	ctx := context.Background()

	// A branch of its own, with neither an upstream nor a counterpart on the
	// remote.
	if _, err := gitOutput(ctx, work, "checkout", "-q", "-b", "experiment"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "c.txt"), []byte("three\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitOutput(ctx, work, "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := gitOutput(ctx, work, "commit", "-qm", "on a branch of its own"); err != nil {
		t.Fatal(err)
	}

	risk := CheckAdoptable(ctx, work)
	if risk.Reason == "" {
		t.Fatal("a branch whose state cannot be compared was allowed to adopt")
	}
	if !strings.Contains(risk.Reason, "cannot tell") {
		t.Errorf("the refusal does not say that the answer is unknown: %q", risk.Reason)
	}
	if !strings.Contains(risk.Reason, "experiment") {
		t.Errorf("the refusal does not name the branch: %q", risk.Reason)
	}
}

// TestRemoteTrackingBranchIsEnoughWithoutAnUpstream keeps the refusal from
// being so broad that ordinary clones trip it. A branch with no upstream set
// but a counterpart on the remote can still be compared.
func TestRemoteTrackingBranchIsEnoughWithoutAnUpstream(t *testing.T) {
	work, _, _ := scenario(t)
	ctx := context.Background()

	// Drop the upstream, leaving origin/main in place.
	if _, err := gitOutput(ctx, work, "branch", "--unset-upstream", "main"); err != nil {
		t.Fatal(err)
	}
	if risk := CheckAdoptable(ctx, work); risk.Reason != "" {
		t.Errorf("a branch with origin/main to compare against was refused: %s", risk.Reason)
	}

	// And it still notices a commit that has not been pushed.
	if err := os.WriteFile(filepath.Join(work, "d.txt"), []byte("four\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitOutput(ctx, work, "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := gitOutput(ctx, work, "commit", "-qm", "unpushed"); err != nil {
		t.Fatal(err)
	}
	risk := CheckAdoptable(ctx, work)
	if !strings.Contains(risk.Reason, "never pushed") {
		t.Errorf("an unpushed commit went unnoticed without an upstream: %q", risk.Reason)
	}
}

// TestAdoptRefusesDifferentFiles is the last line of defence. However the
// clone came to be matched with a GitHub repository, adopting resets it onto
// GitHub's commits, and that is only harmless when the files are the same.
// When they differ -- another project, or a clone behind what was migrated --
// nothing may change.
func TestAdoptRefusesDifferentFiles(t *testing.T) {
	work, gitea, github := scenario(t)
	ctx := context.Background()

	other := filepath.Join(filepath.Dir(work), "other")
	for _, args := range [][]string{
		{"init", "-q", other},
		{"-C", other, "config", "user.email", "x@example.com"},
		{"-C", other, "config", "user.name", "X"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(other, "a.txt"), []byte("somebody else's\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-C", other, "add", "."},
		{"-C", other, "commit", "-qm", "unrelated"},
		{"-C", other, "push", "-q", "--force", github, "HEAD:main"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	before := head(t, work)
	err := Adopt(ctx, work, github, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "different files") {
		t.Fatalf("Adopt = %v, want a refusal over different files", err)
	}
	if head(t, work) != before {
		t.Error("HEAD moved although the adoption was refused")
	}
	if got := remoteURL(t, work, "origin"); got != gitea {
		t.Errorf("origin is %q after a refused adoption, want it left at %q", got, gitea)
	}
	data, _ := os.ReadFile(filepath.Join(work, "a.txt"))
	if string(data) != "one\n" {
		t.Errorf("a.txt now reads %q", data)
	}
}
