package migrate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/JohnStarlight/gitea2github/internal/gitea"
	"github.com/JohnStarlight/gitea2github/internal/github"
)

// resumeScene is a Gitea repository and a bare repository standing in for its
// copy on GitHub, which the fake API below reports on.
type resumeScene struct {
	t             *testing.T
	gitea, github string
}

func (s resumeScene) git(dir string, args ...string) string {
	s.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=A", "GIT_AUTHOR_EMAIL=a@example.com",
		"GIT_COMMITTER_NAME=A", "GIT_COMMITTER_EMAIL=a@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// refs lists every ref in a repository with what it points at.
func (s resumeScene) refs(dir string) string {
	return s.git(dir, "for-each-ref", "--format=%(refname) %(objectname)")
}

// newResumeScene builds a Gitea repository with more than one branch and a
// tag -- the things a partial push would lose -- and an empty GitHub copy.
func newResumeScene(t *testing.T) resumeScene {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	s := resumeScene{t: t, gitea: filepath.Join(root, "gitea.git"), github: filepath.Join(root, "github.git")}
	work := filepath.Join(root, "work")

	s.git(root, "init", "-q", "--bare", s.gitea)
	s.git(root, "init", "-q", work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.git(work, "add", ".")
	s.git(work, "commit", "-qm", "first")
	s.git(work, "branch", "-M", "main")
	s.git(work, "tag", "v1")
	s.git(work, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(work, "b.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.git(work, "add", ".")
	s.git(work, "commit", "-qm", "second")
	s.git(work, "push", "-q", "--mirror", s.gitea)

	s.git(root, "init", "-q", "--bare", s.github)
	return s
}

// fakeGitHub answers the calls a migration makes. Whether the repository
// exists and whether it is empty are read from the stand-in itself.
type fakeGitHub struct {
	scene   resumeScene
	exists  bool
	private bool // visibility of the existing repository

	mu     sync.Mutex
	events []string // "create", "patch private=true" ..., in order
	// refsAtPatch is what the stand-in held when its visibility was changed,
	// to check that nothing had been pushed yet.
	refsAtPatch string
}

func (f *fakeGitHub) RoundTrip(req *http.Request) (*http.Response, error) {
	reply := func(status int, body any) (*http.Response, error) {
		encoded, _ := json.Marshal(body)
		return &http.Response{StatusCode: status, Status: http.StatusText(status),
			Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(encoded)))}, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	repo := map[string]any{"name": "demo", "private": f.private, "clone_url": f.scene.github}
	switch {
	case req.Method == http.MethodGet && req.URL.Path == "/repos/me/demo":
		if !f.exists {
			return reply(404, map[string]string{"message": "Not Found"})
		}
		return reply(200, repo)

	case req.Method == http.MethodGet && req.URL.Path == "/repos/me/demo/commits":
		if f.scene.refs(f.scene.github) == "" {
			return reply(409, map[string]string{"message": "Git Repository is empty."})
		}
		return reply(200, []any{})

	case req.Method == http.MethodPatch && req.URL.Path == "/repos/me/demo":
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)
		if branch, ok := body["default_branch"].(string); ok {
			f.events = append(f.events, "default "+branch)
			return reply(200, repo)
		}
		private, _ := body["private"].(bool)
		f.private = private
		f.refsAtPatch = f.scene.refs(f.scene.github)
		f.events = append(f.events, "patch private="+map[bool]string{true: "true", false: "false"}[private])
		return reply(200, repo)

	case req.Method == http.MethodPost && req.URL.Path == "/user/repos":
		f.events = append(f.events, "create")
		f.exists = true
		return reply(201, repo)
	}
	return reply(404, map[string]string{"message": "unexpected " + req.Method + " " + req.URL.Path})
}

// repo is the Gitea repository of the scene as the API would list it.
func (s resumeScene) repo(private bool) gitea.Repo {
	repo := gitea.Repo{Name: "demo", FullName: "me/demo", CloneURL: s.gitea, Private: private}
	repo.Owner.Login = "me"
	return repo
}

// newTestClient is a GitHub client whose requests f answers.
func newTestClient(f *fakeGitHub) *github.Client {
	client := github.New("t0ken-for-tests")
	client.HTTP = &http.Client{Transport: f}
	return client
}

func (s resumeScene) run(f *fakeGitHub, sourcePrivate, dryRun bool, mode VisibilityMode) Result {
	s.t.Helper()
	f.scene = s
	client := newTestClient(f)
	results := Run(context.Background(), []gitea.Repo{s.repo(sourcePrivate)}, Options{
		GiteaUser:   "me",
		GitHubUser:  "me",
		GitHubTok:   "t0ken-for-tests",
		Visibility:  mode,
		Concurrency: 1,
		DryRun:      dryRun,
		client:      client,
	})
	return results[0]
}

// TestEmptyRepositoryIsResumed is the interrupted run: created on GitHub, the
// push never landed. The next run has to fill it -- every branch and tag --
// rather than report it as present and leave it empty for good.
func TestEmptyRepositoryIsResumed(t *testing.T) {
	s := newResumeScene(t)
	f := &fakeGitHub{exists: true, private: true}

	plan := s.run(f, true, true, "")
	if plan.Status != StatusPlanned || !plan.Resume || !strings.Contains(plan.Reason, "empty on GitHub") {
		t.Fatalf("plan: want a resume, got %s (%s) resume=%v", plan.Status, plan.Reason, plan.Resume)
	}

	got := s.run(f, true, false, "")
	if got.Status != StatusMigrated {
		t.Fatalf("want migrated, got %s: %s", got.Status, got.Reason)
	}
	if len(f.events) != 0 {
		t.Errorf("an empty repository of matching visibility needs no API change, got %v", f.events)
	}
	if want, have := s.refs(s.gitea), s.refs(s.github); have != want {
		t.Errorf("GitHub holds\n%s\nwant everything Gitea has:\n%s", have, want)
	}
}

// TestRepositoryWithCommitsIsLeftAlone keeps what "exists" always meant.
func TestRepositoryWithCommitsIsLeftAlone(t *testing.T) {
	s := newResumeScene(t)
	s.git(s.gitea, "push", "-q", "--mirror", s.github)
	before := s.refs(s.github)
	f := &fakeGitHub{exists: true, private: true}

	got := s.run(f, true, false, "")
	if got.Status != StatusExists || got.Resume {
		t.Fatalf("want exists, got %s (%s) resume=%v", got.Status, got.Reason, got.Resume)
	}
	if len(f.events) != 0 || s.refs(s.github) != before {
		t.Errorf("a repository with commits was touched: %v", f.events)
	}
}

// TestPublicEmptyRepositoryIsMadePrivateFirst covers an empty repository that
// is public -- made by hand, say -- about to receive one that is private on
// Gitea. It is made private before anything is pushed, not after.
func TestPublicEmptyRepositoryIsMadePrivateFirst(t *testing.T) {
	s := newResumeScene(t)
	f := &fakeGitHub{exists: true, private: false}

	got := s.run(f, true, false, "")
	if got.Status != StatusMigrated || !got.Private {
		t.Fatalf("want migrated private, got %s private=%v: %s", got.Status, got.Private, got.Reason)
	}
	if len(f.events) != 1 || f.events[0] != "patch private=true" {
		t.Fatalf("want one change to private, got %v", f.events)
	}
	if f.refsAtPatch != "" {
		t.Errorf("visibility was changed after the push had landed:\n%s", f.refsAtPatch)
	}
}

// TestNewRepositoryIsStillCreated keeps the ordinary path ordinary.
func TestNewRepositoryIsStillCreated(t *testing.T) {
	s := newResumeScene(t)
	f := &fakeGitHub{exists: false}

	got := s.run(f, false, false, "")
	if got.Status != StatusMigrated || got.Resume {
		t.Fatalf("want a fresh migration, got %s (%s) resume=%v", got.Status, got.Reason, got.Resume)
	}
	if len(f.events) != 1 || f.events[0] != "create" {
		t.Errorf("want one creation, got %v", f.events)
	}
	if want, have := s.refs(s.gitea), s.refs(s.github); have != want {
		t.Errorf("GitHub holds\n%s\nwant\n%s", have, want)
	}
}

// TestResumeVisibility pins down the rule for an empty repository being
// filled: where Gitea and the empty copy disagree, private; anything chosen
// explicitly wins.
func TestResumeVisibility(t *testing.T) {
	cases := []struct {
		name                    string
		sourcePrivate, existing bool
		mode                    VisibilityMode
		override                map[string]bool
		want                    bool
	}{
		{"both private", true, true, "", nil, true},
		{"both public", false, false, "", nil, false},
		{"private on Gitea, public empty copy", true, false, "", nil, true},
		{"public on Gitea, private empty copy", false, true, "", nil, true},
		{"chosen public on the screen", true, true, "", map[string]bool{"me/demo": false}, false},
		{"chosen private on the screen", false, false, "", map[string]bool{"me/demo": true}, true},
		{"--visibility=public", true, true, VisibilityPublic, nil, false},
		{"--visibility=private", false, false, VisibilityPrivate, nil, true},
	}
	for _, c := range cases {
		repo := gitea.Repo{FullName: "me/demo", Private: c.sourcePrivate}
		if got := resumeIsPrivate(repo, c.existing, c.mode, c.override); got != c.want {
			t.Errorf("%s: private = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestDefaultBranchFollowsGitea: a push carries branches but not which one is
// the main one, so it is set on GitHub to Gitea's, after the push -- and only
// when that branch was pushed at all.
func TestDefaultBranchFollowsGitea(t *testing.T) {
	for _, c := range []struct {
		giteaDefault string
		want         []string
	}{
		{"feature", []string{"create", "default feature"}},
		{"gone", []string{"create"}}, // not among what was pushed
		{"", []string{"create"}},
	} {
		s := newResumeScene(t)
		f := &fakeGitHub{exists: false}
		f.scene = s
		repo := s.repo(false)
		repo.DefaultBr = c.giteaDefault
		got := Run(context.Background(), []gitea.Repo{repo}, Options{
			GiteaUser: "me", GitHubUser: "me", GitHubTok: "t0ken-for-tests", Concurrency: 1,
			client: newTestClient(f),
		})[0]
		if got.Status != StatusMigrated {
			t.Fatalf("%q: %s: %s", c.giteaDefault, got.Status, got.Reason)
		}
		if strings.Join(f.events, ",") != strings.Join(c.want, ",") {
			t.Errorf("Gitea default %q: calls %v, want %v", c.giteaDefault, f.events, c.want)
		}
	}
}
