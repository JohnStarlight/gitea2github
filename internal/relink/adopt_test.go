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

	redactedCopy(t, gitea, github)
	return work, gitea, github
}

// redactedCopy (re)builds the GitHub stand-in from Gitea, rewritten the way
// the migrator rewrites it.
func redactedCopy(t *testing.T, gitea, github string) {
	t.Helper()
	if err := os.RemoveAll(github); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", "--bare", github).CombinedOutput(); err != nil {
		t.Fatalf("init: %v: %s", err, out)
	}
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
}

// scenarioWithBranchAndTag adds a second branch and a tag, both on Gitea,
// before the redacted copy is made.
func scenarioWithBranchAndTag(t *testing.T) (work, gitea, github string) {
	t.Helper()
	work, gitea, github = scenario(t)
	gitIn(t, work, "tag", "v1")
	gitIn(t, work, "checkout", "-q", "-b", "feature")
	commitIn(t, work, "f.txt", "feature\n")
	gitIn(t, work, "checkout", "-q", "main")
	gitIn(t, work, "push", "-q", "-u", "origin", "feature", "v1")
	redactedCopy(t, gitea, github)
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

// github is the side a check compares with, read from the GitHub stand-in.
func githubSide(path string) GitHubSide { return repoSide{path: path, prefix: "refs/"} }

// gitIn runs git in dir for a test, failing it on error.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitOutput(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return out
}

// commitIn writes a file and commits it.
func commitIn(t *testing.T, dir, file, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-qm", body)
}

// TestCheckAdoptableRefusesUncommittedWork stops a hard reset quietly
// reverting files somebody is in the middle of editing.
func TestCheckAdoptableRefusesUncommittedWork(t *testing.T) {
	work, _, github := scenario(t)

	if risk := CheckAdoptable(context.Background(), work, githubSide(github)); risk.Reason != "" {
		t.Fatalf("a clean clone was refused: %s", risk.Reason)
	}

	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	risk := CheckAdoptable(context.Background(), work, githubSide(github))
	if !strings.Contains(risk.Reason, "not committed") {
		t.Errorf("uncommitted changes were not reported: %q", risk.Reason)
	}
}

// TestCommitNotOnGitHubIsRefused covers work done here after the migration:
// adopting would move the branch onto GitHub's twin and leave the commit
// behind.
func TestCommitNotOnGitHubIsRefused(t *testing.T) {
	work, _, github := scenario(t)
	commitIn(t, work, "b.txt", "two\n")

	risk := CheckAdoptable(context.Background(), work, githubSide(github))
	if risk.Reason != "main has 1 commit that GitHub does not have" {
		t.Errorf("Reason = %q", risk.Reason)
	}
}

// TestBranchOnlyHereIsRefused is the branch that was never pushed anywhere.
// Left as it is, one push would publish the original history under it.
func TestBranchOnlyHereIsRefused(t *testing.T) {
	work, _, github := scenario(t)
	gitIn(t, work, "checkout", "-q", "-b", "experiment")
	commitIn(t, work, "c.txt", "three\n")
	gitIn(t, work, "checkout", "-q", "main")

	risk := CheckAdoptable(context.Background(), work, githubSide(github))
	if risk.Reason != "experiment is not on GitHub (1 commit only on this computer)" {
		t.Errorf("Reason = %q", risk.Reason)
	}
}

// TestTagOnlyHereIsRefused: a tag is as publishable as a branch.
func TestTagOnlyHereIsRefused(t *testing.T) {
	work, _, github := scenario(t)
	gitIn(t, work, "tag", "v9")

	risk := CheckAdoptable(context.Background(), work, githubSide(github))
	if risk.Reason != "tag v9 is not on GitHub" {
		t.Errorf("Reason = %q", risk.Reason)
	}
}

// TestUntrackedFilesAreNotAReasonToRefuse keeps the check from being so strict
// that nobody can ever use it: a hard reset leaves untracked files alone.
func TestUntrackedFilesAreNotAReasonToRefuse(t *testing.T) {
	work, _, github := scenario(t)

	if err := os.WriteFile(filepath.Join(work, "notes.md"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if risk := CheckAdoptable(context.Background(), work, githubSide(github)); risk.Reason != "" {
		t.Errorf("an untracked file was treated as a danger: %s", risk.Reason)
	}
}

// TestDetachedHeadIsRefused: there is no branch to move.
func TestDetachedHeadIsRefused(t *testing.T) {
	work, _, github := scenario(t)
	gitIn(t, work, "checkout", "-q", "--detach")

	if risk := CheckAdoptable(context.Background(), work, githubSide(github)); !strings.Contains(risk.Reason, "not on a branch") {
		t.Errorf("Reason = %q", risk.Reason)
	}
}

// TestAdoptMovesEveryBranchAndTag is the whole of it: every branch and tag
// lands on its twin, origin/* are GitHub's, and git status says there is
// nothing to do -- not "diverged", which is what invites a push --force.
func TestAdoptMovesEveryBranchAndTag(t *testing.T) {
	work, gitea, github := scenarioWithBranchAndTag(t)
	ctx := context.Background()

	if err := Adopt(ctx, work, github, nil, nil); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	for _, ref := range []string{"main", "feature", "v1"} {
		if got, want := head(t, work, ref), head(t, github, ref); got != want {
			t.Errorf("%s is %s, want GitHub's %s", ref, got, want)
		}
	}
	for _, ref := range []string{"origin/main", "origin/feature"} {
		if got, want := head(t, work, ref), head(t, github, strings.TrimPrefix(ref, "origin/")); got != want {
			t.Errorf("%s is %s, want GitHub's %s", ref, got, want)
		}
	}
	if status := gitIn(t, work, "status", "-sb"); strings.Contains(status, "ahead") || strings.Contains(status, "behind") {
		t.Errorf("status after adopting: %s", status)
	}
	if refs := gitIn(t, work, "for-each-ref", adoptPrefix); refs != "" {
		t.Errorf("private refs left behind:\n%s", refs)
	}
	_ = gitea
}

// TestRefusedAdoptionChangesNothing: one branch without a twin, and not a
// single ref moves -- not the others that could have, not origin.
func TestRefusedAdoptionChangesNothing(t *testing.T) {
	work, gitea, github := scenarioWithBranchAndTag(t)
	gitIn(t, work, "checkout", "-q", "-b", "experiment")
	commitIn(t, work, "c.txt", "three\n")
	gitIn(t, work, "checkout", "-q", "main")
	before := gitIn(t, work, "for-each-ref")

	err := Adopt(context.Background(), work, github, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "experiment is not on GitHub") {
		t.Fatalf("Adopt = %v, want a refusal naming experiment", err)
	}
	if after := gitIn(t, work, "for-each-ref"); after != before {
		t.Errorf("refs changed although nothing was adopted:\nbefore\n%s\nafter\n%s", before, after)
	}
	if got := remoteURL(t, work, "origin"); got != gitea {
		t.Errorf("origin is %q, want it left at %q", got, gitea)
	}
}

// TestAdoptAdviceSaysWhatCanBeDone checks the words a person reads when an
// adoption is refused: what will NOT happen, and the one way to fix it.
func TestAdoptAdviceSaysWhatCanBeDone(t *testing.T) {
	advice := AdoptAdvice([]AdoptProblem{{Ref: "main", Kind: NotLikeGitHub, Local: 1}})
	for _, want := range []string{"Nothing was changed", "does NOT have", "delete that repository on GitHub", "run migrate again"} {
		if !strings.Contains(advice, want) {
			t.Errorf("advice lacks %q:\n%s", want, advice)
		}
	}
	if strings.Contains(advice, "git pull") {
		t.Errorf("advice for local work suggests a pull, which would not help:\n%s", advice)
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
	if err == nil || !strings.Contains(err.Error(), "main is different on GitHub") {
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

// TestAdoptAfterMigratingTheWorkOnThisComputer is the whole story from the
// user's side. A finished project, a little more work done locally and never
// pushed -- a commit on main, a new branch, a tag -- then a redacting
// migration that takes that work along, as migrate now does. The copy then
// takes on GitHub's history without a single refusal: every branch and tag it
// has has a twin with the same files.
func TestAdoptAfterMigratingTheWorkOnThisComputer(t *testing.T) {
	work, gitea, github := scenario(t)
	commitIn(t, work, "extra.txt", "last touches\n")
	gitIn(t, work, "checkout", "-q", "-b", "experiment")
	commitIn(t, work, "idea.txt", "an idea\n")
	gitIn(t, work, "checkout", "-q", "main")
	gitIn(t, work, "tag", "final")

	// The migration: Gitea's mirror, the copy's work fetched into it (as
	// migrate.addLocalWork does), then redacted.
	mirror := filepath.Join(filepath.Dir(gitea), "mirror.git")
	if out, err := exec.Command("git", "clone", "-q", "--mirror", gitea, mirror).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v: %s", err, out)
	}
	gitIn(t, mirror, "fetch", "-q", "--no-tags", work,
		"refs/heads/main:refs/heads/main", "refs/heads/experiment:refs/heads/experiment",
		"refs/tags/final:refs/tags/final")
	redactedCopy(t, mirror, github)

	if risk := CheckAdoptable(context.Background(), work, githubSide(github)); risk.Reason != "" {
		t.Fatalf("refused after a migration that took the work along: %s", risk.Reason)
	}
	if err := Adopt(context.Background(), work, github, nil, nil); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	for _, ref := range []string{"main", "experiment", "final"} {
		if got, want := head(t, work, ref), head(t, github, ref); got != want {
			t.Errorf("%s is %s, want GitHub's %s", ref, got, want)
		}
	}
	if authors := gitIn(t, work, "log", "--all", "--format=%ae", "--not", "--remotes=origin"); authors != "" {
		t.Errorf("commits outside GitHub's history remain reachable:\n%s", authors)
	}
}
