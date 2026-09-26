package main

// End-to-end tests of the commands themselves: migrate, relink and doctor run
// as a user runs them, flags and answers included, against a Gitea and a
// GitHub that are real git repositories on disk behind fake APIs.
//
// Nothing leaves the machine. The fake Gitea serves its repositories over
// HTTP with git's own http-backend, so clones of it have Gitea's address as
// their origin, as real ones do. GitHub's repositories are reached through
// url.<dir>.insteadOf, which tells git that https://github.com/me/ is a
// directory under the test's temporary directory. Every clone, fetch and push
// the tool makes is a real one.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/JohnStarlight/gitea2github/internal/github"
	"github.com/JohnStarlight/gitea2github/internal/ui"
)

const (
	testNoReply = "1+me@users.noreply.github.com"
	personal    = "you@example.com"
)

// world is one Gitea, one GitHub and a directory of clones.
type world struct {
	t                                 *testing.T
	root, giteaDir, githubDir, clones string
	gitea                             *httptest.Server
	backend                           *cgi.Handler // git over HTTP, for the Gitea
	giteaURL                          string       // what --gitea-url is given
	repos                             []map[string]any
	gh                                *fakeGitHubAPI
}

func newWorld(t *testing.T) *world {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	w := &world{t: t, root: root,
		giteaDir:  filepath.Join(root, "gitea"),
		githubDir: filepath.Join(root, "github"),
		clones:    filepath.Join(root, "clones"),
	}
	for _, d := range []string{w.giteaDir, w.githubDir, w.clones} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skip("git --exec-path: ", err)
	}
	w.backend = &cgi.Handler{
		Path: filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend"),
		Env: []string{"GIT_PROJECT_ROOT=" + w.giteaDir, "GIT_HTTP_EXPORT_ALL=1",
			"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.receivepack", "GIT_CONFIG_VALUE_0=true"},
	}
	w.gitea = httptest.NewServer(http.HandlerFunc(w.serveGitea))
	t.Cleanup(w.gitea.Close)
	w.giteaURL = w.gitea.URL
	w.gh = &fakeGitHubAPI{t: t, dir: w.githubDir, scopes: "repo, workflow"}

	// Git sees the two servers as directories, and nothing of the user's own
	// configuration -- no credential helper, no identity.
	global := filepath.Join(root, "gitconfig")
	if err := os.WriteFile(global, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL":   global,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_COUNT":    "2",
		"GIT_CONFIG_KEY_0":    "url." + filepath.ToSlash(w.githubDir) + "/.insteadOf",
		"GIT_CONFIG_VALUE_0":  "https://github.com/me/",
		"GIT_CONFIG_KEY_1":    "init.defaultBranch",
		"GIT_CONFIG_VALUE_1":  "main",
		"GITEA_TOKEN":         "gitea-t0ken",
		"GITEA_USER":          "",
		"GITHUB_TOKEN":        "github-t0ken",
	} {
		t.Setenv(k, v)
	}

	saved := newGitHub
	newGitHub = func(token string) *github.Client {
		c := github.New(token)
		c.HTTP = &http.Client{Transport: w.gh}
		return c
	}
	t.Cleanup(func() { newGitHub = saved })
	return w
}

// git runs git, committing as whoever is given.
func (w *world) git(dir, email string, args ...string) string {
	w.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Someone", "GIT_AUTHOR_EMAIL="+email,
		"GIT_COMMITTER_NAME=Someone", "GIT_COMMITTER_EMAIL="+email)
	out, err := cmd.CombinedOutput()
	if err != nil {
		w.t.Fatalf("git %s in %s: %v: %s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes files in a working copy and commits them as email.
func (w *world) commit(work, email string, files map[string]string) {
	w.t.Helper()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0o644); err != nil {
			w.t.Fatal(err)
		}
	}
	w.git(work, email, "add", ".")
	w.git(work, email, "commit", "-qm", "change")
}

// giteaRepo puts a repository on Gitea: one commit by author, holding files.
func (w *world) giteaRepo(owner, name string, private bool, author string, files map[string]string) {
	w.t.Helper()
	work := filepath.Join(w.root, "seed-"+owner+"-"+name)
	w.git(w.root, author, "init", "-q", work)
	w.commit(work, author, files)
	bare := filepath.Join(w.giteaDir, owner, name+".git")
	w.git(w.root, author, "clone", "-q", "--bare", work, bare)
	w.repos = append(w.repos, map[string]any{
		"name": name, "full_name": owner + "/" + name, "private": private,
		"clone_url": w.gitea.URL + "/" + owner + "/" + name + ".git", "default_branch": "main",
		"description": "the " + name + " project", "owner": map[string]string{"login": owner},
		"permissions": map[string]bool{"admin": true, "push": true, "pull": true},
	})
}

// clone makes a working copy of a Gitea repository under the clones directory.
func (w *world) clone(owner, name string) string {
	w.t.Helper()
	path := filepath.Join(w.clones, name)
	w.git(w.clones, personal, "clone", "-q", w.gitea.URL+"/"+owner+"/"+name+".git", path)
	return path
}

func (w *world) serveGitea(rw http.ResponseWriter, r *http.Request) {
	reply := func(v any) { _ = json.NewEncoder(rw).Encode(v) }
	p, api := strings.CutPrefix(r.URL.Path, "/api/v1")
	if !api {
		w.backend.ServeHTTP(rw, r) // git over HTTP
		return
	}
	switch {
	case p == "/version":
		reply(map[string]string{"version": "1.27.3"})
	case p == "/user":
		reply(map[string]string{"login": "me"})
	case p == "/user/repos":
		if r.URL.Query().Get("page") == "1" {
			reply(w.repos)
			return
		}
		reply([]any{})
	case strings.HasSuffix(p, "/topics"):
		reply(map[string][]string{"topics": {"go", "zone01"}})
	default:
		http.NotFound(rw, r)
	}
}

// fakeGitHubAPI answers GitHub's API for the user "me" from bare repositories
// in dir -- one per repository, created when the API is asked to create one --
// and records what it was asked to change.
type fakeGitHubAPI struct {
	t      *testing.T
	dir    string
	scopes string

	mu    sync.Mutex
	calls []string // "create demo", "default demo main", "topics demo go,zone01", ...
}

func (f *fakeGitHubAPI) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeGitHubAPI) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeGitHubAPI) bare(name string) string { return filepath.Join(f.dir, name+".git") }

func (f *fakeGitHubAPI) run(name string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", f.bare(name)}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

func (f *fakeGitHubAPI) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	f.serve(rec, req)
	return rec.Result(), nil
}

func (f *fakeGitHubAPI) serve(rw http.ResponseWriter, r *http.Request) {
	reply := func(status int, v any) {
		rw.WriteHeader(status)
		_ = json.NewEncoder(rw).Encode(v)
	}
	p := r.URL.Path
	if p == "/user" {
		rw.Header().Set("X-OAuth-Scopes", f.scopes)
		reply(200, map[string]any{"login": "me", "id": 1})
		return
	}
	if r.Method == http.MethodPost && p == "/user/repos" {
		var body struct{ Name string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if out, err := exec.Command("git", "init", "-q", "--bare", f.bare(body.Name)).CombinedOutput(); err != nil {
			f.t.Errorf("creating %s: %v: %s", body.Name, err, out)
		}
		f.record("create " + body.Name)
		reply(201, f.repo(body.Name))
		return
	}

	rest, ok := strings.CutPrefix(p, "/repos/me/")
	if !ok {
		reply(404, map[string]string{"message": "Not Found"})
		return
	}
	name, sub, _ := strings.Cut(rest, "/")
	if _, err := os.Stat(f.bare(name)); err != nil {
		reply(404, map[string]string{"message": "Not Found"})
		return
	}
	empty := func() bool { out, _ := f.run(name, "for-each-ref"); return out == "" }

	switch {
	case sub == "" && r.Method == http.MethodGet:
		reply(200, f.repo(name))
	case sub == "" && r.Method == http.MethodPatch:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if b, ok := body["default_branch"].(string); ok {
			f.record("default " + name + " " + b)
		} else {
			f.record("patch " + name)
		}
		reply(200, f.repo(name))
	case sub == "topics" && r.Method == http.MethodPut:
		var body struct{ Names []string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.record("topics " + name + " " + strings.Join(body.Names, ","))
		reply(200, body)
	case sub == "commits" && empty():
		reply(409, map[string]string{"message": "Git Repository is empty."})
	case sub == "commits":
		reply(200, []any{})
	case strings.HasPrefix(sub, "commits/"):
		sha := strings.TrimPrefix(sub, "commits/")
		if _, err := f.run(name, "cat-file", "-e", sha+"^{commit}"); err != nil {
			reply(422, map[string]string{"message": "No commit found for SHA: " + sha})
			return
		}
		reply(200, map[string]string{"sha": sha})
	case strings.HasPrefix(sub, "branches/"):
		tree, err := f.run(name, "rev-parse", "refs/heads/"+strings.TrimPrefix(sub, "branches/")+"^{tree}")
		if err != nil {
			reply(404, map[string]string{"message": "Branch not found"})
			return
		}
		reply(200, map[string]any{"commit": map[string]any{"commit": map[string]any{"tree": map[string]string{"sha": tree}}}})
	case strings.HasPrefix(sub, "git/ref/tags/"):
		sha, err := f.run(name, "rev-parse", "refs/tags/"+strings.TrimPrefix(sub, "git/ref/tags/"))
		if err != nil {
			reply(404, map[string]string{"message": "Not Found"})
			return
		}
		kind, _ := f.run(name, "cat-file", "-t", sha)
		reply(200, map[string]any{"object": map[string]string{"type": kind, "sha": sha}})
	case strings.HasPrefix(sub, "git/tags/"):
		target, _ := f.run(name, "rev-parse", strings.TrimPrefix(sub, "git/tags/")+"^{commit}")
		reply(200, map[string]any{"object": map[string]string{"type": "commit", "sha": target}})
	case strings.HasPrefix(sub, "git/commits/"):
		tree, _ := f.run(name, "rev-parse", strings.TrimPrefix(sub, "git/commits/")+"^{tree}")
		reply(200, map[string]any{"tree": map[string]string{"sha": tree}})
	default:
		reply(404, map[string]string{"message": "unexpected " + r.Method + " " + p})
	}
}

func (f *fakeGitHubAPI) repo(name string) map[string]any {
	return map[string]any{"name": name, "full_name": "me/" + name, "private": true,
		"clone_url": "https://github.com/me/" + name + ".git"}
}

// run runs a command the way main does, with the answers given to its
// questions, and returns what it printed and the error it ended with.
func run(t *testing.T, answers string, cmd func(context.Context, []string) error, args ...string) (string, error) {
	t.Helper()
	interactive := answers != ""
	saved := newPrompter
	newPrompter = func() *ui.Prompter {
		return ui.NewWith(strings.NewReader(answers), io.Discard, interactive)
	}
	defer func() { newPrompter = saved }()

	var err error
	out := captureStdout(t, func() { err = cmd(context.Background(), args) })
	return out, err
}

// authors lists who made the commits in a repository.
func (w *world) authors(repo string) string {
	return w.git(repo, personal, "log", "--all", "--format=%ae")
}

// TestMigrateDryRunChangesNothing: the plan is printed, and nothing is
// created, pushed or asked.
func TestMigrateDryRunChangesNothing(t *testing.T) {
	w := newWorld(t)
	w.giteaRepo("me", "ascii-art", true, personal, map[string]string{"main.go": "package main\n"})
	w.giteaRepo("teammate", "lem-in", true, personal, map[string]string{"main.go": "package main\n"})

	out, err := run(t, "", cmdMigrate, "--gitea-url", w.giteaURL, "--clones", w.clones, "--dry-run")
	if err != nil {
		t.Fatalf("migrate --dry-run: %v\n%s", err, out)
	}
	for _, want := range []string{"planned me/ascii-art", "owned by teammate (use --collaborations to include)"} {
		if !strings.Contains(squeeze(out), want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
	if calls := w.gh.Calls(); len(calls) != 0 {
		t.Errorf("a dry run changed GitHub: %v", calls)
	}
}

// TestMigrateRedactsAndCarriesTheDetails is a whole unattended migration:
// created, pushed with the history redacted and the kept address turned into
// the no-reply, and the default branch and topics set.
func TestMigrateRedactsAndCarriesTheDetails(t *testing.T) {
	w := newWorld(t)
	w.giteaRepo("me", "ascii-art", true, personal, map[string]string{"main.go": "package main\n"})

	out, err := run(t, "", cmdMigrate, "--gitea-url", w.giteaURL, "--clones", w.clones,
		"--yes", "--redact-emails", "--keep-email", personal)
	if err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	if !strings.Contains(out, "1 migrated") {
		t.Errorf("summary:\n%s", out)
	}
	if got := w.authors(w.gh.bare("ascii-art")); got != "1+me@users.noreply.github.com" {
		t.Errorf("GitHub's history is by %q, want the no-reply only", got)
	}
	want := []string{"create ascii-art", "default ascii-art main", "topics ascii-art go,zone01"}
	if got := w.gh.Calls(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("GitHub was asked %v, want %v", got, want)
	}
}

// TestMigrateHoldsBackAddressesInFiles: a redacted repository that would be
// public, with an address in a file, is not created, and the run says so and
// ends in an error a script can see.
func TestMigrateHoldsBackAddressesInFiles(t *testing.T) {
	w := newWorld(t)
	w.giteaRepo("me", "ascii-art", false, personal,
		map[string]string{"package.json": `{"author": "maria@mail.example.gr"}`})

	out, err := run(t, "", cmdMigrate, "--gitea-url", w.giteaURL, "--clones", w.clones,
		"--yes", "--redact-emails")
	if err == nil || !strings.Contains(err.Error(), "held back") {
		t.Fatalf("migrate = %v, want a held-back error\n%s", err, out)
	}
	for _, want := range []string{"me/ascii-art was NOT migrated", "maria@mail.example.gr", "--allow-emails-in-files"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if calls := w.gh.Calls(); len(calls) != 0 {
		t.Errorf("a held-back repository reached GitHub: %v", calls)
	}
}

// TestMigrateByTheNumberedQuestions answers every question the way a person
// at a terminal without the full screen would, and checks each answer made it
// into the run: the group project included, the history redacted with the
// address kept as the no-reply, the work on this computer taken to GitHub and
// not to Gitea.
func TestMigrateByTheNumberedQuestions(t *testing.T) {
	w := newWorld(t)
	w.giteaRepo("me", "ascii-art", true, personal, map[string]string{"main.go": "package main\n"})
	w.giteaRepo("teammate", "lem-in", true, "teammate@example.net", map[string]string{"main.go": "package main\n"})
	copyOnDisk := w.clone("me", "ascii-art")
	w.commit(copyOnDisk, personal, map[string]string{"extra.go": "package main // last touches\n"})
	giteaBefore := w.git(filepath.Join(w.giteaDir, "me", "ascii-art.git"), personal, "rev-parse", "main")

	answers := strings.Join([]string{
		"y",      // include the repository owned by someone else
		"y",      // replace email addresses
		personal, // an address of yours, to stay linked
		"",       // visibility: keep as they are
		"y",      // put the work on this computer on GitHub too
		"n",      // and not on Gitea
		"y",      // migrate
		"n",      // do not take on the rewritten history in the clones now
	}, "\n") + "\n"
	out, err := run(t, answers, cmdMigrate, "--gitea-url", w.giteaURL, "--clones", w.clones, "--no-tui")
	if err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	if !strings.Contains(out, "2 migrated") {
		t.Fatalf("summary:\n%s", out)
	}
	if files := w.git(w.gh.bare("ascii-art"), personal, "ls-tree", "--name-only", "main"); !strings.Contains(files, "extra.go") {
		t.Errorf("the work on this computer did not reach GitHub; main has: %s", files)
	}
	if got := w.authors(w.gh.bare("ascii-art")); strings.Contains(got, personal) {
		t.Errorf("an address reached GitHub un-redacted:\n%s", got)
	}
	if got := w.authors(w.gh.bare("lem-in")); strings.Contains(got, "teammate@example.net") || !strings.Contains(got, "@redacted.invalid") {
		t.Errorf("the group project's history was not redacted:\n%s", got)
	}
	if after := w.git(filepath.Join(w.giteaDir, "me", "ascii-art.git"), personal, "rev-parse", "main"); after != giteaBefore {
		t.Error("the work was sent to Gitea although the answer was no")
	}
}

// TestRelinkAdoptsAndCommitsAsGitHub follows a redacting migration with the
// repointing: the plan says the clone takes on the rewritten history -- not
// that it is repointed, which it once said -- the clone then does, and its new
// commits are made as the GitHub identity, in that clone only.
func TestRelinkAdoptsAndCommitsAsGitHub(t *testing.T) {
	w := newWorld(t)
	w.giteaRepo("me", "ascii-art", true, personal, map[string]string{"main.go": "package main\n"})
	copyOnDisk := w.clone("me", "ascii-art")
	w.git(copyOnDisk, personal, "config", "user.name", "someone-on-gitea")
	w.git(copyOnDisk, personal, "config", "user.email", personal)
	if out, err := run(t, "", cmdMigrate, "--gitea-url", w.giteaURL, "--clones", w.clones,
		"--yes", "--redact-emails", "--keep-email", personal); err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}

	answers := "y\ny\n" // commit as the GitHub identity; repoint
	out, err := run(t, answers, cmdRelink, "--gitea-url", w.giteaURL, "--no-tui", "--push-to", "github", w.clones)
	if err != nil {
		t.Fatalf("relink: %v\n%s", err, out)
	}
	if !strings.Contains(out, "take on GitHub's rewritten history") {
		t.Errorf("the plan does not describe the adoption:\n%s", out)
	}
	if got, want := w.git(copyOnDisk, personal, "rev-parse", "main"), w.git(w.gh.bare("ascii-art"), personal, "rev-parse", "main"); got != want {
		t.Errorf("the clone's main is %s, want GitHub's %s", got, want)
	}
	if got := w.git(copyOnDisk, personal, "config", "--local", "user.email"); got != testNoReply {
		t.Errorf("the clone commits as %q, want the no-reply", got)
	}
	if remotes := w.git(copyOnDisk, personal, "remote"); remotes != "origin" {
		t.Errorf("remotes after adopting: %q, want origin only", remotes)
	}
}

// TestDoctorSaysWhatATokenLacks: a token that cannot create repositories
// fails doctor, with the remedy.
func TestDoctorSaysWhatATokenLacks(t *testing.T) {
	w := newWorld(t)
	w.giteaRepo("me", "ascii-art", true, personal, map[string]string{"main.go": "package main\n"})
	w.gh.scopes = "gist"

	out, err := run(t, "", cmdDoctor, "--gitea-url", w.giteaURL)
	if err == nil {
		t.Fatalf("doctor passed with a token that cannot create repositories:\n%s", out)
	}
	for _, want := range []string{"identity     ok    me", "scopes       FAIL", "create a token with the repo and workflow scopes"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output lacks %q:\n%s", want, out)
		}
	}
}

// squeeze collapses runs of spaces, so a table can be matched without its
// column widths.
func squeeze(s string) string { return strings.Join(strings.Fields(s), " ") }

// TestListShowsWhoseEachRepositoryIs: ownership comes from whose token it is,
// not from anything stored beside it -- so it is right with GITEA_TOKEN and
// no GITEA_USER, the case that once made every repository someone else's.
func TestListShowsWhoseEachRepositoryIs(t *testing.T) {
	w := newWorld(t)
	w.giteaRepo("me", "ascii-art", true, personal, map[string]string{"main.go": "package main\n"})
	w.giteaRepo("teammate", "lem-in", false, personal, map[string]string{"main.go": "package main\n"})

	out, err := run(t, "", cmdList, "--gitea-url", w.giteaURL)
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	for _, want := range []string{"me/ascii-art private own", "teammate/lem-in public collaborator"} {
		if !strings.Contains(squeeze(out), want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
}
