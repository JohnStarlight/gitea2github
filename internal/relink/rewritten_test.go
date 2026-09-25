package relink

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JohnStarlight/gitea2github/internal/github"
	"github.com/JohnStarlight/gitea2github/internal/redact"
)

const noReply = "259051186+JohnStarlight@users.noreply.github.com"

// rewriteScene is a Gitea repository, a working copy cloned from it, and a
// bare repository standing in for what the migration put on GitHub.
type rewriteScene struct {
	root, gitea, work, github string
}

func (s rewriteScene) git(t *testing.T, dir string, args ...string) string {
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

// newRewriteScene builds a solo project: every commit by the one address.
func newRewriteScene(t *testing.T) rewriteScene {
	t.Helper()
	root := t.TempDir()
	s := rewriteScene{root: root, gitea: filepath.Join(root, "gitea.git"),
		work: filepath.Join(root, "work"), github: filepath.Join(root, "github.git")}

	s.git(t, root, "init", "-q", "--bare", s.gitea)
	s.git(t, root, "init", "-q", s.work)
	s.git(t, s.work, "config", "user.email", "you@example.com")
	s.git(t, s.work, "config", "user.name", "You")
	for i := 1; i <= 3; i++ {
		if err := os.WriteFile(filepath.Join(s.work, "main.go"), []byte(fmt.Sprintf("step %d\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		s.git(t, s.work, "add", ".")
		s.git(t, s.work, "commit", "-qm", fmt.Sprintf("step %d", i))
	}
	s.git(t, s.work, "branch", "-M", "main")
	s.git(t, s.work, "remote", "add", "origin", s.gitea)
	s.git(t, s.work, "push", "-q", "-u", "origin", "main")
	return s
}

// migrate fills the GitHub stand-in the way the migrator would: verbatim when
// m is nil, and through the real redaction filter otherwise.
func (s rewriteScene) migrate(t *testing.T, m *redact.Mapper) {
	t.Helper()
	if m == nil {
		s.git(t, s.root, "clone", "-q", "--mirror", s.gitea, s.github)
		return
	}
	s.git(t, s.root, "init", "-q", "--bare", s.github)
	export := exec.Command("git", "-C", s.gitea, "fast-export", "--all", "--use-done-feature")
	imp := exec.Command("git", "-C", s.github, "fast-import", "--quiet", "--done")
	out, err := export.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	in, err := imp.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := export.Start(); err != nil {
		t.Fatal(err)
	}
	if err := imp.Start(); err != nil {
		t.Fatal(err)
	}
	if err := redact.FilterStream(out, in, m); err != nil {
		t.Fatal(err)
	}
	in.Close()
	if err := imp.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := export.Wait(); err != nil {
		t.Fatal(err)
	}
}

// fakeGitHub answers the API calls relink makes from the GitHub stand-in, so
// that whether a commit "is on GitHub" is decided by git, not by the test.
type fakeGitHub struct {
	t     *testing.T
	scene rewriteScene
	empty bool // the repository exists but nothing was ever pushed to it
}

func (f fakeGitHub) RoundTrip(req *http.Request) (*http.Response, error) {
	reply := func(status int, body any) (*http.Response, error) {
		encoded, _ := json.Marshal(body)
		return &http.Response{StatusCode: status, Status: http.StatusText(status),
			Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(encoded)))}, nil
	}
	const prefix = "/repos/JohnStarlight/gitea"
	path := req.URL.Path
	switch {
	case path == prefix:
		return reply(200, map[string]any{"name": "gitea", "private": true})

	case strings.HasPrefix(path, prefix+"/commits/"):
		if f.empty {
			return reply(409, map[string]string{"message": "Git Repository is empty."})
		}
		sha := strings.TrimPrefix(path, prefix+"/commits/")
		if exec.Command("git", "-C", f.scene.github, "cat-file", "-e", sha+"^{commit}").Run() != nil {
			return reply(422, map[string]string{"message": "No commit found for SHA: " + sha})
		}
		return reply(200, map[string]string{"sha": sha})

	case path == prefix+"/commits":
		if f.empty {
			return reply(409, map[string]string{"message": "Git Repository is empty."})
		}
		type ident struct {
			Email string `json:"email"`
		}
		type commit struct {
			Commit struct {
				Author    ident `json:"author"`
				Committer ident `json:"committer"`
			} `json:"commit"`
		}
		var list []commit
		for _, line := range strings.Split(f.scene.git(f.t, f.scene.github, "log", "--all", "-10", "--format=%ae %ce"), "\n") {
			var c commit
			fmt.Sscan(line, &c.Commit.Author.Email, &c.Commit.Committer.Email)
			list = append(list, c)
		}
		return reply(200, list)
	}
	return reply(404, map[string]string{"message": "Not Found"})
}

// plan runs relink as a dry run against the fake GitHub and returns the one
// working copy's result.
func (s rewriteScene) plan(t *testing.T, empty bool) Result {
	t.Helper()
	client := github.New("tok")
	client.HTTP = &http.Client{Transport: fakeGitHub{t: t, scene: s, empty: empty}}
	results, err := Run(context.Background(), Options{
		Root:       s.root,
		GiteaHost:  "gitea.git", // origin is a local path ending in this
		GitHubUser: "JohnStarlight",
		Mode:       ModeGitHub,
		Verify:     true,
		DryRun:     true,
		client:     client,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Path == s.work {
			return r
		}
	}
	t.Fatalf("working copy not in results: %+v", results)
	return Result{}
}

// TestRewriteWithOnlyYourOwnAddressIsRecognised is the case that used to be
// missed. A solo project redacted with --keep-email comes out with every
// address turned into the GitHub no-reply, so not one of them has the
// redacted shape -- yet every hash changed, and the clone can no longer push.
func TestRewriteWithOnlyYourOwnAddressIsRecognised(t *testing.T) {
	s := newRewriteScene(t)
	s.migrate(t, redact.NewMapper([]string{"you@example.com"}, noReply))

	got := s.plan(t, false)
	if !got.Redacted {
		t.Fatalf("a rewritten history was not recognised: %+v", got)
	}
	if got.Action != "planned" || !strings.Contains(got.Reason, "rewritten history") {
		t.Errorf("want the adoption offered, got %s: %s", got.Action, got.Reason)
	}
}

// TestRewriteWithHashedAddressesIsRecognised keeps the case that always
// worked working.
func TestRewriteWithHashedAddressesIsRecognised(t *testing.T) {
	s := newRewriteScene(t)
	s.migrate(t, redact.NewMapper(nil, noReply))

	if got := s.plan(t, false); !got.Redacted {
		t.Fatalf("a redacted history was not recognised: %+v", got)
	}
}

// TestVerbatimMigrationIsNotMistakenForARewrite is the other direction: the
// same commits on both sides must get an ordinary repoint, not an adoption.
func TestVerbatimMigrationIsNotMistakenForARewrite(t *testing.T) {
	s := newRewriteScene(t)
	s.migrate(t, nil)

	got := s.plan(t, false)
	if got.Redacted {
		t.Fatalf("an unchanged history was taken for a rewrite: %+v", got)
	}
	if got.Action != "planned" {
		t.Errorf("want an ordinary repoint planned, got %s: %s", got.Action, got.Reason)
	}
}

// TestBranchDeletedFromGiteaIsNotMistakenForARewrite covers a clone that
// still remembers a branch Gitea no longer had when the migration ran. That
// branch is missing from GitHub for a reason that has nothing to do with
// redaction, and one missing commit must not outvote one that is there.
func TestBranchDeletedFromGiteaIsNotMistakenForARewrite(t *testing.T) {
	s := newRewriteScene(t)
	s.git(t, s.work, "checkout", "-q", "-b", "old-idea")
	if err := os.WriteFile(filepath.Join(s.work, "idea.txt"), []byte("maybe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.git(t, s.work, "add", ".")
	s.git(t, s.work, "commit", "-qm", "an idea")
	s.git(t, s.work, "push", "-q", "-u", "origin", "old-idea")
	s.git(t, s.work, "checkout", "-q", "main")
	// Deleted on the server; the clone's origin/old-idea is left behind.
	s.git(t, s.gitea, "branch", "-D", "old-idea")
	s.migrate(t, nil)

	if got := s.plan(t, false); got.Redacted {
		t.Fatalf("a stale branch was taken for a rewrite: %+v", got)
	}
}

// TestEmptyGitHubRepositoryIsNotAdopted covers a migration that created the
// repository and never pushed to it. Nothing on GitHub matches, but that is
// not a rewrite, and adopting nothing would fail half way through.
func TestEmptyGitHubRepositoryIsNotAdopted(t *testing.T) {
	s := newRewriteScene(t)
	s.git(t, s.root, "init", "-q", "--bare", s.github)

	got := s.plan(t, true)
	if got.Redacted {
		t.Fatalf("an empty repository was taken for a rewrite: %+v", got)
	}
	if got.Action != "skipped" || !strings.Contains(got.Reason, "empty") {
		t.Errorf("want it skipped as empty, got %s: %s", got.Action, got.Reason)
	}
}
