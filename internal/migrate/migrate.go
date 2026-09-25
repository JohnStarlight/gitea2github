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
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/JohnStarlight/gitea2github/internal/gitea"
	"github.com/JohnStarlight/gitea2github/internal/github"
	"github.com/JohnStarlight/gitea2github/internal/redact"
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
	Source string // Gitea full name, e.g. "JohnStarlight/linear-stats"
	Target string // GitHub full name, e.g. "JohnStarlight/linear-stats"

	// SourcePrivate and Private are the visibility on Gitea and the visibility
	// the destination would be created with. Both are reported so a plan can
	// show what is about to change rather than only the outcome.
	SourcePrivate bool
	Private       bool
	Status        Status
	Reason        string        // why it was skipped, or what failed
	Took          time.Duration // wall-clock time, useful for spotting the slow ones

	// WithLocalWork marks a repository that took work from a copy on this
	// computer as well as from Gitea.
	WithLocalWork bool

	// Resume marks a repository already on GitHub with nothing in it: the
	// remains of a run that created it and was interrupted before its push
	// landed. It is pushed into rather than created, and rather than being
	// reported as present -- which would leave it empty however many times
	// the migration was run again.
	Resume bool
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

	// Visibility decides what the destination repositories are created as.
	// The zero value mirrors the source, which is the only default that never
	// changes anyone's exposure: whatever was private on Gitea stays private,
	// whatever was public stays public.
	Visibility VisibilityMode

	// Target is the GitHub name each repository will take, keyed by Gitea full
	// name, as worked out by Targets. Empty entries and missing ones fall back
	// to the repository's own name.
	Target map[string]string

	// RenameTo is a name typed by hand on the selection screen, which beats
	// both of the above.
	RenameTo map[string]string

	// VisibilityOverride sets the visibility of individual repositories,
	// keyed by Gitea full name, and wins over Visibility. It is how the
	// interactive flow records "this one, the other way round" without
	// forcing a single choice onto the whole run.
	VisibilityOverride map[string]bool

	// DryRun resolves and filters everything but performs no clone, no
	// creation and no push. Always the right first invocation.
	DryRun bool

	// Concurrency is how many repositories are transferred at once. Cloning is
	// network-bound, so a handful of workers is a large win, but too many
	// simultaneous repository creations trip GitHub's secondary rate limit.
	Concurrency int

	// Mapper, when non-nil, rewrites every email address in the history before
	// anything is pushed. A nil Mapper means the history is transferred
	// verbatim. Expressing the choice as the presence of the collaborator
	// rather than as a separate boolean makes an inconsistent combination
	// impossible to construct.
	//
	// One Mapper is shared by every worker so that a person who appears in
	// several repositories is redacted to the same address in all of them.
	Mapper *redact.Mapper

	// RedactOnly narrows redaction to individual repositories, keyed by Gitea
	// full name. A nil map means every repository is redacted, which is what
	// --redact-emails on the command line asks for; a non-nil one is the
	// selection screen saying "these, and not the others".
	//
	// Separate from Mapper rather than folded into it because the Mapper
	// carries the shared address book: one person has to be redacted to the
	// same replacement everywhere, whichever subset of repositories was
	// chosen.
	RedactOnly map[string]bool

	// LocalWork is work found in copies of the repositories on this computer
	// that Gitea does not have, keyed by Gitea full name. What it can take is
	// added to the mirror before anything is redacted or pushed.
	LocalWork map[string]LocalWork

	// Topics, when set, returns a repository's topics on Gitea, to be given to
	// it on GitHub too. A function rather than a Gitea client so that this
	// package asks Gitea nothing itself.
	Topics func(ctx context.Context, repo gitea.Repo) ([]string, error)

	// client replaces the GitHub client Run would build, so tests can answer
	// its questions without an account behind them.
	client *github.Client

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

	gh := opts.client
	if gh == nil {
		gh = github.New(opts.GitHubTok)
	}
	gh.Log = opts.Log

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
	target := opts.targetFor(repo)
	res := Result{
		Source: repo.FullName,
		Target: opts.GitHubUser + "/" + target,
		// Decided up front rather than at creation time so that a dry run
		// reports the same visibility the real run would use. A plan that
		// omitted this could not be the thing the user chooses from.
		SourcePrivate: repo.Private,
		Private:       destinationIsPrivate(repo, opts.Visibility, opts.VisibilityOverride),
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
	existing, exists, err := gh.Lookup(ctx, opts.GitHubUser, target)
	if err != nil {
		return finish(StatusFailed, fmt.Sprintf("checking GitHub: %v", err))
	}
	if exists {
		empty, err := gh.IsEmpty(ctx, opts.GitHubUser, target)
		if err != nil {
			return finish(StatusFailed, fmt.Sprintf("checking GitHub: %v", err))
		}
		if !empty {
			return finish(StatusExists, "already on GitHub, left untouched")
		}
		res.Resume = true
		res.Private = resumeIsPrivate(repo, existing.Private, opts.Visibility, opts.VisibilityOverride)
	}

	if opts.DryRun {
		steps := "would clone, create and push"
		switch {
		case res.Resume && opts.Redacts(repo.FullName):
			steps = "empty on GitHub; would clone, redact emails and push into it"
		case res.Resume:
			steps = "empty on GitHub; would clone and push into it"
		case opts.Redacts(repo.FullName):
			steps = "would clone, redact emails, create and push"
		}
		if work, ok := opts.LocalWork[repo.FullName]; ok && work.takesAnything() {
			steps += "; with the work from this computer"
		}
		return finish(StatusPlanned, steps)
	}

	// --- Mirror clone ------------------------------------------------------
	mirrorPath := filepath.Join(workDir, strings.ReplaceAll(repo.FullName, "/", "_")+".git")
	opts.Log("cloning %s", repo.FullName)
	// Cloned from the address as it stands, with the credential supplied out
	// of band: a token spliced into this URL would be copied into the mirror's
	// own config by git, and left there if the run were interrupted.
	if out, err := runGitAs(ctx, "", opts.GiteaUser, opts.GiteaToken,
		"clone", "--mirror", repo.CloneURL, mirrorPath); err != nil {
		return finish(StatusFailed, fmt.Sprintf("clone failed: %v: %s", err, out))
	}
	// The mirror is scratch data; remove it as soon as the push is done so a
	// thirty-repository run does not accumulate thirty working copies.
	defer os.RemoveAll(mirrorPath)

	// A mirror clone of a Gitea repository also copies the pull-request refs
	// under refs/pull/. GitHub owns that namespace and rejects any push into
	// it, which would fail the whole mirror push, so drop them here.
	if out, err := pruneUnpushableRefs(ctx, mirrorPath); err != nil {
		return finish(StatusFailed, fmt.Sprintf("pruning refs: %v: %s", err, out))
	}

	// --- Work from this computer -------------------------------------------
	// Added before redaction, so that it goes through the same rewrite as
	// everything else and nothing of it reaches GitHub un-redacted.
	if work, ok := opts.LocalWork[repo.FullName]; ok && work.takesAnything() {
		opts.Log("adding the work from %s to %s", work.Clone, repo.FullName)
		if err := addLocalWork(ctx, mirrorPath, work); err != nil {
			return finish(StatusFailed, fmt.Sprintf("adding work from %s: %v", work.Clone, err))
		}
		res.WithLocalWork = true
	}

	// --- Optional email redaction ------------------------------------------
	// Rewriting has to happen between clone and push: it needs the full history
	// locally, and the point is that the un-redacted version never reaches
	// GitHub at all.
	pushFrom := mirrorPath
	if opts.Redacts(repo.FullName) {
		opts.Log("redacting email addresses in %s", repo.FullName)
		rewritten := mirrorPath + ".redacted"
		if err := rewriteHistory(ctx, mirrorPath, rewritten, opts.Mapper); err != nil {
			return finish(StatusFailed, fmt.Sprintf("redacting emails: %v", err))
		}
		defer os.RemoveAll(rewritten)
		pushFrom = rewritten
	}

	// --- Create on GitHub, or take up the empty one ------------------------
	pushTo := existing.CloneURL
	if res.Resume {
		// Set before anything is pushed, so that what arrives is never
		// visible to anyone it was not meant for, even briefly.
		if existing.Private != res.Private {
			opts.Log("making github.com/%s/%s %s", opts.GitHubUser, target, visibilityName(res.Private))
			if err := gh.SetPrivate(ctx, opts.GitHubUser, target, res.Private); err != nil {
				return finish(StatusFailed, fmt.Sprintf("setting visibility: %v", err))
			}
		}
	} else {
		opts.Log("creating github.com/%s/%s", opts.GitHubUser, target)
		created, err := gh.CreateRepo(ctx, target, repo.Description, repo.Website, res.Private)
		if err != nil {
			return finish(StatusFailed, fmt.Sprintf("creating GitHub repo: %v", err))
		}
		pushTo = created.CloneURL
	}

	// --- Mirror push -------------------------------------------------------
	opts.Log("pushing %s", repo.FullName)
	// "x-access-token" is the username GitHub expects when the password being
	// offered is a personal access token.
	//
	// --atomic makes the push all or nothing. Without it an interrupted push
	// can land some branches and not others, leaving a repository that is
	// neither empty -- so the next run would not resume it -- nor complete.
	if out, err := runGitAs(ctx, pushFrom, "x-access-token", opts.GitHubTok,
		"push", "--mirror", "--atomic", pushTo); err != nil {
		return finish(StatusFailed, fmt.Sprintf("push failed: %v: %s", err, out))
	}

	// --- What the repository page says about it ----------------------------
	// None of this is a failure if it cannot be done: the history is all
	// there, and each of these is one click on GitHub. What went wrong is
	// reported beside the result.
	var notes []string

	// A push carries branches, not which of them is the main one, so GitHub
	// picks one itself -- not necessarily Gitea's. Set to match, if Gitea's
	// is among what was pushed.
	if branch := repo.DefaultBr; branch != "" {
		if _, err := runGit(ctx, pushFrom, "", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
			if err := gh.SetDefaultBranch(ctx, opts.GitHubUser, target, branch); err != nil {
				notes = append(notes, fmt.Sprintf("default branch not set to %s: %v", branch, err))
			}
		}
	}
	// A repository created here was created with its description and
	// website; one that already existed, empty, was not.
	if res.Resume && (repo.Description != "" || repo.Website != "") {
		if err := gh.SetAbout(ctx, opts.GitHubUser, target, repo.Description, repo.Website); err != nil {
			notes = append(notes, fmt.Sprintf("description and website not set: %v", err))
		}
	}
	if opts.Topics != nil {
		topics, err := opts.Topics(ctx, repo)
		if err == nil && len(topics) > 0 {
			err = gh.SetTopics(ctx, opts.GitHubUser, target, topics)
		}
		if err != nil {
			notes = append(notes, fmt.Sprintf("topics not set: %v", err))
		}
	}

	return finish(StatusMigrated, strings.Join(notes, "; "))
}

// Redacts reports whether this repository's history is to be rewritten.
//
// The two conditions are asked in this order because they answer different
// questions: the Mapper is whether redaction is configured at all, and
// RedactOnly is which repositories it reaches.
func (o Options) Redacts(fullName string) bool {
	if o.Mapper == nil {
		return false
	}
	if o.RedactOnly == nil {
		return true
	}
	return o.RedactOnly[fullName]
}

// runGit executes a git command with prompting disabled and returns its
// combined output with the secret redacted.
//
// Most git commands here need no credential at all -- they operate on a local
// mirror -- so this is the common case, and runGitAs is the one that has a
// token to offer.
func runGit(ctx context.Context, dir, secret string, args ...string) (string, error) {
	return runGitAs(ctx, dir, "", secret, args...)
}

// runGitAs is runGit with a credential offered to git through GIT_ASKPASS.
//
// The credential reaches git through the helper's environment rather than
// through the URL, which is what keeps it out of the argument list `ps`
// publishes and out of the config file a clone writes. See Askpass.
func runGitAs(ctx context.Context, dir, user, secret string, args ...string) (string, error) {
	// No argument may carry the credential. Keeping it out of the argument
	// list is the whole point of going through a helper -- `ps` publishes
	// arguments to every user on the machine, and git writes a clone URL into
	// the clone's own config -- so the invariant is checked here rather than
	// trusted to every call site. Splicing a token into a URL is the obvious
	// way to authenticate git from a program, which makes it the change most
	// likely to be made by someone simplifying this later, and it would
	// otherwise fail silently by working.
	if secret != "" {
		for _, arg := range args {
			if strings.Contains(arg, secret) {
				return "", fmt.Errorf(
					"refusing to run git with a credential among its arguments: " +
						"pass it through askpass instead")
			}
		}
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir

	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	self, err := os.Executable()
	switch {
	case secret == "", err != nil:
		// Nothing to offer, or no way to point git back at this binary. Empty
		// the helper so that git cannot go looking for a credential
		// interactively and hang a worker nobody is watching.
		env = append(env, "GIT_ASKPASS=")
	default:
		env = append(env, askpassEnv(self, user, secret)...)
	}
	cmd.Env = env

	out, runErr := cmd.CombinedOutput()
	return redactSecret(string(out), secret), runErr
}

// redactSecret removes a token from text destined for logs or error messages.
// git echoes the remote URL in several of its messages, and that URL carries
// the token we just injected.
//
// Named to keep it distinct from the redact package, which redacts email
// addresses out of history rather than secrets out of output.
func redactSecret(text, secret string) string {
	if secret == "" {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(strings.ReplaceAll(text, secret, "***"))
}

// pruneUnpushableRefs deletes refs that the destination will refuse.
//
// GitHub reserves refs/pull/ for its own pull-request machinery and rejects any
// attempt to write there. Gitea keeps its pull requests in the same namespace,
// and a mirror clone copies them, so every repository that ever had a pull
// request would otherwise fail at the final push.
func pruneUnpushableRefs(ctx context.Context, repoPath string) (string, error) {
	out, err := runGit(ctx, repoPath, "", "for-each-ref", "--format=%(refname)", "refs/pull")
	if err != nil {
		return out, err
	}
	for _, ref := range strings.Fields(out) {
		if msg, err := runGit(ctx, repoPath, "", "update-ref", "-d", ref); err != nil {
			return msg, fmt.Errorf("deleting %s: %w", ref, err)
		}
	}
	return "", nil
}

// rewriteHistory produces a new bare repository at dst holding the history of
// src with every email address replaced.
//
// It works by streaming `git fast-export` through the redaction filter into
// `git fast-import`. Going through a second repository rather than rewriting in
// place means a failure part way through leaves the original mirror intact and
// nothing half-rewritten is ever pushed.
//
// Every commit hash changes as a result, because the author and committer
// identities are part of what a commit hashes. The migrated history is
// therefore a parallel copy rather than the same history, and commit
// signatures, which cannot survive an identity change, are dropped.
func rewriteHistory(ctx context.Context, src, dst string, m *redact.Mapper) error {
	if out, err := runGit(ctx, "", "", "init", "--bare", "--quiet", dst); err != nil {
		return fmt.Errorf("creating rewrite target: %v: %s", err, out)
	}

	// --signed-tags=strip: a tag signature covers the old object, so it is
	// invalid the moment anything is rewritten; keeping it would produce tags
	// that fail verification rather than tags that are honestly unsigned.
	export := exec.CommandContext(ctx, "git", "-C", src, "fast-export", "--all",
		"--signed-tags=strip", "--tag-of-filtered-object=rewrite", "--use-done-feature")
	imp := exec.CommandContext(ctx, "git", "-C", dst, "fast-import", "--quiet", "--done")

	var exportErr, importErr bytes.Buffer
	export.Stderr = &exportErr
	imp.Stderr = &importErr

	exported, err := export.StdoutPipe()
	if err != nil {
		return err
	}
	imported, err := imp.StdinPipe()
	if err != nil {
		return err
	}

	if err := export.Start(); err != nil {
		return fmt.Errorf("starting fast-export: %w", err)
	}
	if err := imp.Start(); err != nil {
		_ = export.Process.Kill()
		_ = export.Wait()
		return fmt.Errorf("starting fast-import: %w", err)
	}

	filterErr := redact.FilterStream(exported, imported, m)

	// A filter that stopped early stopped reading, and fast-export blocks on
	// the full pipe it is still writing into. Waiting for it then waits
	// forever. Its output is of no use any more -- nothing is pushed from a
	// failed rewrite, and the mirror it reads from is untouched -- so it is
	// stopped rather than waited out.
	if filterErr != nil {
		_ = export.Process.Kill()
	}

	// Close the import side first: fast-import only finishes once its stdin is
	// closed, so waiting before closing would deadlock.
	closeErr := imported.Close()
	importWait := imp.Wait()
	exportWait := export.Wait()

	switch {
	case filterErr != nil:
		// Usually the filter failed because fast-import died and the next
		// write hit a broken pipe, which explains nothing; fast-import's
		// stderr says why. Both are kept, because the filter can also fail
		// on its own account, and then fast-import only complains that its
		// input stopped.
		if reason := strings.TrimSpace(importErr.String()); reason != "" {
			return fmt.Errorf("filtering history: %w; fast-import said: %s", filterErr, reason)
		}
		return fmt.Errorf("filtering history: %w", filterErr)
	case closeErr != nil:
		return fmt.Errorf("closing fast-import input: %w", closeErr)
	case exportWait != nil:
		return fmt.Errorf("fast-export: %v: %s", exportWait, strings.TrimSpace(exportErr.String()))
	case importWait != nil:
		return fmt.Errorf("fast-import: %v: %s", importWait, strings.TrimSpace(importErr.String()))
	}

	// fast-import recreates refs but not HEAD. A push does not carry HEAD
	// either -- the default branch on GitHub is set explicitly after the push
	// -- but the rewritten repository is kept a faithful copy all the same.
	if head, err := runGit(ctx, src, "", "symbolic-ref", "HEAD"); err == nil && head != "" {
		if out, err := runGit(ctx, dst, "", "symbolic-ref", "HEAD", head); err != nil {
			return fmt.Errorf("setting HEAD to %s: %v: %s", head, err, out)
		}
	}
	return nil
}

// VisibilityMode is the blanket rule applied to repositories with no
// individual override.
type VisibilityMode string

const (
	// VisibilityMirror creates each repository as it is on Gitea. It is the
	// zero value because it is the only setting that cannot change how exposed
	// anything is.
	VisibilityMirror VisibilityMode = ""

	// VisibilityPrivate and VisibilityPublic force every repository one way,
	// for unattended runs where nobody is there to choose per repository.
	VisibilityPrivate VisibilityMode = "private"
	VisibilityPublic  VisibilityMode = "public"
)

// resumeIsPrivate decides the visibility of an empty repository an earlier
// run left on GitHub, which is about to be filled.
//
// Two answers already exist -- how the repository is on Gitea, and how the
// empty one is on GitHub -- and when they disagree neither can be assumed to
// be the one meant: the empty one may have been made by hand, public on
// purpose or by mistake. Private is the answer that exposes nothing it should
// not, and anything the user chose explicitly beats it.
func resumeIsPrivate(source gitea.Repo, existingPrivate bool, mode VisibilityMode, override map[string]bool) bool {
	if private, ok := override[source.FullName]; ok {
		return private
	}
	switch mode {
	case VisibilityPrivate:
		return true
	case VisibilityPublic:
		return false
	}
	return source.Private || existingPrivate
}

func visibilityName(private bool) string {
	if private {
		return "private"
	}
	return "public"
}

// destinationIsPrivate decides the visibility of the repository about to be
// created on GitHub.
//
// Pulled out of the migration path so the precedence can be tested. Getting it
// backwards would publish private work, which is not a mistake worth
// discovering in production.
func destinationIsPrivate(source gitea.Repo, mode VisibilityMode, override map[string]bool) bool {
	// An explicit per-repository choice is the most specific thing the user
	// said, so it beats the blanket rule.
	if private, ok := override[source.FullName]; ok {
		return private
	}
	switch mode {
	case VisibilityPrivate:
		return true
	case VisibilityPublic:
		return false
	default:
		return source.Private
	}
}
