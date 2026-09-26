package migrate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JohnStarlight/gitea2github/internal/gitea"
	"github.com/JohnStarlight/gitea2github/internal/redact"
)

// localScene is a finished project on Gitea with a copy on this computer that
// has had a little more work done in it since.
func localScene(t *testing.T) (resumeScene, string) {
	s := newResumeScene(t) // Gitea: main, feature, tag v1; GitHub stand-in empty
	work := filepath.Join(filepath.Dir(s.gitea), "clone")
	s.git(filepath.Dir(s.gitea), "clone", "-q", s.gitea, work)
	s.git(work, "checkout", "-q", "main")
	s.git(work, "checkout", "-q", "feature")
	s.git(work, "checkout", "-q", "main")
	s.git(work, "config", "user.email", "you@example.com")

	commit := func(file, msg string) {
		if err := os.WriteFile(filepath.Join(work, file), []byte(msg+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		s.git(work, "add", ".")
		s.git(work, "commit", "-qm", msg)
	}
	commit("extra.txt", "last touches") // main: 1 commit only here
	s.git(work, "checkout", "-q", "-b", "experiment")
	commit("idea.txt", "an idea") // new branch
	commit("idea.txt", "the idea, better")
	s.git(work, "checkout", "-q", "main")
	s.git(work, "tag", "v2") // a tag only here
	return s, work
}

func TestFindLocalWorkSeesWhatGiteaLacks(t *testing.T) {
	s, work := localScene(t)
	got, err := FindLocalWork(context.Background(), work, s.gitea, "", "")
	if err != nil {
		t.Fatal(err)
	}
	// experiment was started from main after main's local commit, so that
	// commit is on both: counted per branch, as git itself would.
	if want := "experiment: new branch, 3 commits; main: 1 commit; tag v2"; got.Describe() != want {
		t.Errorf("Describe() = %q, want %q", got.Describe(), want)
	}
}

// A teammate pushed to feature while this copy was being worked on: the two
// feature branches each have what the other lacks. Neither can be taken over
// the other, so the copy's is left out, and says so.
func TestDivergedBranchIsNotTaken(t *testing.T) {
	s, work := localScene(t)

	other := filepath.Join(filepath.Dir(s.gitea), "teammate")
	s.git(filepath.Dir(s.gitea), "clone", "-q", "-b", "feature", s.gitea, other)
	if err := os.WriteFile(filepath.Join(other, "theirs.txt"), []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.git(other, "add", ".")
	s.git(other, "commit", "-qm", "teammate's work")
	s.git(other, "push", "-q", "origin", "feature")

	s.git(work, "checkout", "-q", "feature")
	if err := os.WriteFile(filepath.Join(work, "mine.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.git(work, "add", ".")
	s.git(work, "commit", "-qm", "my work")

	got, err := FindLocalWork(context.Background(), work, s.gitea, "", "")
	if err != nil {
		t.Fatal(err)
	}
	branches, _ := got.Taken()
	for _, b := range branches {
		if b == "feature" {
			t.Fatalf("a diverged branch was taken: %+v", got)
		}
	}
	if !strings.Contains(got.Describe(), "feature: 1 commit, NOT included") {
		t.Errorf("Describe() = %q, want feature reported as not included", got.Describe())
	}
}

// TestMigrationTakesTheWorkFromThisComputer is the whole point: the project
// is finished locally, migrated with redaction, and GitHub ends up with the
// work that never went near Gitea -- redacted like everything else -- while
// the copy on this computer is left exactly as it was.
func TestMigrationTakesTheWorkFromThisComputer(t *testing.T) {
	s, work := localScene(t)
	found, err := FindLocalWork(context.Background(), work, s.gitea, "", "")
	if err != nil {
		t.Fatal(err)
	}
	before := s.refs(work)

	f := &fakeGitHub{exists: false}
	f.scene = s
	client := newTestClient(f)
	results := Run(context.Background(), []gitea.Repo{s.repo(true)}, Options{
		GiteaUser: "me", GitHubUser: "me", GitHubTok: "t0ken-for-tests", Concurrency: 1,
		Mapper:    redact.NewMapper(nil, ""),
		LocalWork: map[string]LocalWork{"me/demo": found},
		GitHub:    client,
	})
	got := results[0]
	if got.Status != StatusMigrated || !got.WithLocalWork {
		t.Fatalf("want migrated with local work, got %s (%s) local=%v", got.Status, got.Reason, got.WithLocalWork)
	}

	for _, ref := range []string{"refs/heads/main", "refs/heads/experiment", "refs/heads/feature", "refs/tags/v1", "refs/tags/v2"} {
		if _, err := runGit(context.Background(), s.github, "", "rev-parse", "--verify", ref); err != nil {
			t.Errorf("GitHub is missing %s", ref)
		}
	}
	if files := s.git(s.github, "ls-tree", "--name-only", "main"); !strings.Contains(files, "extra.txt") {
		t.Errorf("GitHub's main lacks the local commit; it has: %s", files)
	}
	if authors := s.git(s.github, "log", "--all", "--format=%ae"); strings.Contains(authors, "you@example.com") {
		t.Errorf("the local work reached GitHub un-redacted:\n%s", authors)
	}
	if s.refs(work) != before {
		t.Error("the copy on this computer was changed")
	}
}

// TestPushLocalWorkSendsItToGitea covers the optional second answer.
func TestPushLocalWorkSendsItToGitea(t *testing.T) {
	s, work := localScene(t)
	found, err := FindLocalWork(context.Background(), work, s.gitea, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := PushLocalWork(context.Background(), found, s.gitea, "", ""); err != nil {
		t.Fatal(err)
	}
	again, err := FindLocalWork(context.Background(), work, s.gitea, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !again.Empty() {
		t.Errorf("after pushing, Gitea still lacks: %s", again.Describe())
	}
}
