// Package migrate performs the actual repository transfer: a mirror clone from
// Gitea followed by a mirror push to a freshly created GitHub repository.
//
// The mirror pair is the heart of the whole tool. `git clone --mirror` copies
// every ref — all branches, all tags, all notes — rather than just the default
// branch, and `git push --mirror` reproduces them exactly on the far side. A
// plain clone-and-push would quietly drop every branch the user was not
// standing on, which is the single most common way hand-made migrations lose
// work.
package migrate

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/JohnStarlight/gitea2github/internal/gitea"
	"github.com/JohnStarlight/gitea2github/internal/github"
)

// Status describes how one repository fared.
type Status string

const (
	StatusMigrated Status = "migrated" // created on GitHub and pushed
	StatusSkipped  Status = "skipped"  // intentionally not touched
	StatusExists   Status = "exists"   // already on GitHub, left alone
	StatusFailed   Status = "failed"   // something went wrong
	StatusPlanned  Status = "planned"  // dry run: this is what would happen
)

// Result records the outcome for one repository so the CLI can print a summary
// instead of making the user scroll back through interleaved worker output.
type Result struct {
	Source string // Gitea full name, e.g. "ivogiake/linear-stats"
	Target string // GitHub full name, e.g. "JohnStarlight/linear-stats"
	Status Status
	Reason string        // why it was skipped, or what failed
	Took   time.Duration // wall-clock time, useful for spotting the slow ones
}

// Options configures one migration run.
type Options struct {
	GiteaUser  string // login used to tell owned repos from collaborations
	GiteaToken string
	GitHubUser string
	GitHubTok  string

	// IncludeCollaborations re-publishes repositories owned by *other* Gitea
	// users under this user's GitHub account. It is off by default because
	// doing so without asking would republish a teammate's work as your own.
	IncludeCollaborations bool

	// IncludeForks and IncludeArchived are likewise off by default: forks
	// usually duplicate upstream history that is already on GitHub, and
	// archived repositories are typically dead weight in a portfolio.
	IncludeForks    bool
	IncludeArchived bool

	// Private forces every destination repository private. When false the
	// visibility of the Gitea repository is carried across unchanged.
	ForcePrivate bool

	// DryRun resolves and filters everything but performs no clone, no
	// creation and no push. Always the right first invocation.
	DryRun bool

	// Concurrency is how many repositories are transferred at once. Cloning is
	// network-bound, so a handful of workers is a large win, but too many
	// simultaneous repository creations trip GitHub's secondary rate limit.
	Concurrency int

	// WorkDir holds the temporary mirror clones. When empty a directory under
	// the system temp location is created and removed afterwards.
	WorkDir string

	// Log receives human-readable progress lines. Workers run concurrently, so
	// the CLI must supply a serialising implementation.
	Log func(format string, args ...any)
}

// Run migrates every repository in repos, honouring the filters in opts, and
// returns one Result per input repository in the original order.
func Run(ctx context.Context, repos []gitea.Repo, opts Options) []Result {
	if opts.Concurrency < 1 {
		opts.Concurrency = 4
	}
	if opts.Log == nil {
		opts.Log = func(string, ...any) {}
	}

	workDir := opts.WorkDir
	if workDir == "" {
		var err error
		workDir, err = os.MkdirTemp("", "gitea2github-")
		if err != nil {
			// Without scratch space nothing can be cloned, so fail every
			// repository with the same explanatory reason rather than
			// panicking half way through.
			results := make([]Result, len(repos))
			for i, r := range repos {
				results[i] = Result{Source: r.FullName, Status: StatusFailed,
					Reason: fmt.Sprintf("could not create work directory: %v", err)}
			}
			return results
		}
		defer os.RemoveAll(workDir)
	}

	gh := github.New(opts.GitHubTok)

	// Results are written by index, so each worker owns exactly one slot and no
	// mutex is needed to protect the slice itself.
	results := make([]Result, len(repos))

	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < opts.Concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results[i] = migrateOne(ctx, repos[i], gh, opts, workDir)
			}
		}()
	}

	for i := range repos {
		select {
		case jobs <- i:
		case <-ctx.Done():
			// Ctrl-C: stop handing out work. Jobs already in flight finish or
			// abort on their own context check.
			close(jobs)
			wg.Wait()
			return results
		}
	}
	close(jobs)
	wg.Wait()

	return results
}

// migrateOne transfers a single repository. It never returns an error: every
// outcome, including failure, is expressed as a Result so that one broken
// repository cannot abort the other twenty-nine.
func migrateOne(ctx context.Context, repo gitea.Repo, gh *github.Client, opts Options, workDir string) Result {
	start := time.Now()
	target := github.SanitizeName(repo.Name)
	res := Result{
		Source: repo.FullName,
		Target: opts.GitHubUser + "/" + target,
	}
	finish := func(status Status, reason string) Result {
		res.Status = status
		res.Reason = reason
		res.Took = time.Since(start)
		return res
	}

	// --- Filters -----------------------------------------------------------
	// Applied here rather than before the worker pool so that the summary shows
	// every repository that was considered, with the reason it was left out.
	if repo.Empty {
		return finish(StatusSkipped, "repository has no commits")
	}
	if repo.Archived && !opts.IncludeArchived {
		return finish(StatusSkipped, "archived (use --archived to include)")
	}
	if repo.Fork && !opts.IncludeForks {
		return finish(StatusSkipped, "fork (use --forks to include)")
	}
	if !repo.OwnedBy(opts.GiteaUser) && !opts.IncludeCollaborations {
		return finish(StatusSkipped,
			fmt.Sprintf("owned by %s (use --collaborations to include)", repo.Owner.Login))
	}

	// --- Already there? ----------------------------------------------------
	exists, err := gh.Exists(ctx, opts.GitHubUser, target)
	if err != nil {
		return finish(StatusFailed, fmt.Sprintf("checking GitHub: %v", err))
	}
	if exists {
		return finish(StatusExists, "already on GitHub, left untouched")
	}

	if opts.DryRun {
		return finish(StatusPlanned, "would clone, create and push")
	}

	// --- Mirror clone ------------------------------------------------------
	mirrorPath := filepath.Join(workDir, strings.ReplaceAll(repo.FullName, "/", "_")+".git")
	cloneURL := withCredentials(repo.CloneURL, opts.GiteaUser, opts.GiteaToken)
	opts.Log("cloning %s", repo.FullName)
	if out, err := runGit(ctx, "", opts.GiteaToken, "clone", "--mirror", cloneURL, mirrorPath); err != nil {
		return finish(StatusFailed, fmt.Sprintf("clone failed: %v: %s", err, out))
	}
	// The mirror is scratch data; remove it as soon as the push is done so a
	// thirty-repository run does not accumulate thirty working copies.
	defer os.RemoveAll(mirrorPath)

	// --- Create on GitHub --------------------------------------------------
	private := repo.Private || opts.ForcePrivate
	opts.Log("creating github.com/%s/%s", opts.GitHubUser, target)
	created, err := gh.CreateRepo(ctx, target, repo.Description, private)
	if err != nil {
		return finish(StatusFailed, fmt.Sprintf("creating GitHub repo: %v", err))
	}

	// --- Mirror push -------------------------------------------------------
	pushURL := withCredentials(created.CloneURL, "x-access-token", opts.GitHubTok)
	opts.Log("pushing %s", repo.FullName)
	if out, err := runGit(ctx, mirrorPath, opts.GitHubTok, "push", "--mirror", pushURL); err != nil {
		return finish(StatusFailed, fmt.Sprintf("push failed: %v: %s", err, out))
	}

	return finish(StatusMigrated, "")
}

// withCredentials injects basic-auth credentials into an https clone URL so git
// can authenticate without an interactive prompt and without touching the
// user's credential store.
//
// The resulting string contains a secret, so it must never be logged; runGit
// redacts it from any command output before that output reaches the caller.
func withCredentials(rawURL, user, token string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	u.User = url.UserPassword(user, token)
	return u.String()
}

// runGit executes a git command with prompting disabled and returns its
// combined output with the secret redacted.
//
// GIT_TERMINAL_PROMPT=0 and the empty GIT_ASKPASS matter more than they look:
// without them a bad token makes git block forever waiting for a password that
// no one is there to type, and a background worker hanging silently is far
// harder to diagnose than a failed command.
func runGit(ctx context.Context, dir, secret string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"GCM_INTERACTIVE=never",
	)
	out, err := cmd.CombinedOutput()
	return redact(string(out), secret), err
}

// redact removes a secret from text destined for logs or error messages. git
// echoes the remote URL in several of its messages, and that URL carries the
// token we just injected.
func redact(text, secret string) string {
	if secret == "" {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(strings.ReplaceAll(text, secret, "***"))
}
