// Package relink repoints local clones from Gitea to GitHub.
//
// Migrating the server-side repositories is only half the job. The working
// copies on the developer's laptop still have `origin` pointing at Gitea, so
// every subsequent `git push` goes to the repository they just moved away from.
// This package fixes that, and it is useful on its own to anyone who migrated
// by hand and now has a folder full of clones pointing at the wrong server.
//
// The old remote is renamed rather than deleted. Keeping Gitea reachable under
// its own name means a mistake is one `git push gitea` away from being
// recoverable, and costs nothing.
package relink

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/JohnStarlight/gitea2github/internal/github"
	"github.com/JohnStarlight/gitea2github/internal/redact"
)

// Result records what happened to one local clone.
type Result struct {
	Path   string // directory of the working copy
	OldURL string // the Gitea remote we found
	NewURL string // the GitHub remote we set, if any
	Action string // "relinked", "skipped", "planned" or "failed"
	Reason string

	// Redacted reports that the GitHub side holds a rewritten history with the
	// addresses hidden, so this clone's commits and its commits are different
	// objects with no ancestor in common.
	//
	// It changes what repointing can mean. A clone that keeps the original
	// history cannot push to the redacted copy -- the push is rejected -- and
	// forcing it past that would republish exactly the addresses the rewrite
	// removed. The only coherent outcome is for the clone to take on the
	// rewritten history and stop being a clone of the Gitea repository.
	Redacted bool
}

// Modes for where a relinked clone should push.
const (
	// ModeGitHub moves origin to GitHub and keeps the Gitea remote under
	// another name. For someone who is done with the old server.
	ModeGitHub = "github"

	// ModeBoth leaves origin fetching from Gitea but makes one `git push`
	// reach both servers. This is what you want while the Gitea instance is
	// still the one that matters -- a Zone01 student still has to push there
	// for audits -- and GitHub is a portfolio mirror alongside it.
	ModeBoth = "both"

	// ModeGitea changes nothing about origin and simply adds a github remote,
	// so pushing to GitHub is always an explicit act.
	ModeGitea = "gitea"
)

// Options configures a relink sweep.
type Options struct {
	Root       string // directory to scan
	GiteaHost  string // only remotes on this host are considered
	GitHubUser string
	GitHubTok  string

	// OldRemoteName is what the existing Gitea remote gets renamed to under
	// ModeGitHub.
	OldRemoteName string

	// Mode selects where a relinked clone pushes: ModeGitHub, ModeBoth or
	// ModeGitea. Empty means ModeGitHub.
	Mode string

	// ModeFor overrides Mode for individual working copies, keyed by the path
	// Run reports in a Result. It is how the selection screen records "this
	// one to GitHub, that one to both" without forcing a single answer onto a
	// whole directory tree.
	ModeFor map[string]string

	// Only, when non-nil, limits the run to these working copies. An empty map
	// is not the same as a nil one: it means nothing was chosen, and nothing
	// is touched.
	Only map[string]bool

	// GitEnv carries a credential to the one git command here that talks to a
	// remote: fetching a rewritten history during an adoption. Supplied by the
	// caller so this package holds no second copy of that mechanism.
	GitEnv []string

	// DryRun reports what would change without touching any repository.
	DryRun bool

	// Verify checks that the GitHub repository actually exists before
	// repointing at it. Without this a typo or an incomplete migration would
	// leave the clone pointing at a URL that 404s on the next push.
	Verify bool

	Log func(format string, args ...any)
}

// Run scans Root for git working copies whose origin lives on GiteaHost and
// repoints them at the corresponding GitHub repository.
func Run(ctx context.Context, opts Options) ([]Result, error) {
	if opts.OldRemoteName == "" {
		opts.OldRemoteName = "gitea"
	}
	if opts.Log == nil {
		opts.Log = func(string, ...any) {}
	}

	repos, err := findRepos(opts.Root)
	if err != nil {
		return nil, err
	}

	var gh *github.Client
	if opts.Verify {
		gh = github.New(opts.GitHubTok)
	}

	results := make([]Result, 0, len(repos))
	for _, path := range repos {
		results = append(results, relinkOne(ctx, path, gh, opts))
	}
	return results, nil
}

// relinkOne handles a single working copy.
func relinkOne(ctx context.Context, path string, gh *github.Client, opts Options) Result {
	res := Result{Path: path}

	origin, err := gitOutput(ctx, path, "remote", "get-url", "origin")
	if err != nil {
		res.Action, res.Reason = "skipped", "no origin remote"
		return res
	}
	res.OldURL = origin

	if !strings.Contains(origin, opts.GiteaHost) {
		res.Action, res.Reason = "skipped", "origin is not on "+opts.GiteaHost
		return res
	}

	// Asked after the host check so that a working copy left out on purpose is
	// still reported as a Gitea clone rather than as something unrecognised.
	if !opts.wants(path) {
		res.Action, res.Reason = "skipped", "left alone"
		return res
	}

	// Derive the repository name from the last path segment of the remote URL,
	// dropping the conventional .git suffix. This is the same name the migrator
	// used when creating the GitHub side, so the two halves line up.
	name := strings.TrimSuffix(filepath.Base(strings.TrimSuffix(origin, "/")), ".git")
	target := github.SanitizeName(name)
	res.NewURL = fmt.Sprintf("https://github.com/%s/%s.git", opts.GitHubUser, target)

	if opts.Verify && gh != nil {
		exists, err := gh.Exists(ctx, opts.GitHubUser, target)
		if err != nil {
			res.Action, res.Reason = "failed", fmt.Sprintf("checking GitHub: %v", err)
			return res
		}
		if !exists {
			res.Action, res.Reason = "skipped", "no matching repository on GitHub yet"
			return res
		}
		// Asked once the repository is known to exist. A failure here is not
		// fatal: the worst case is offering the ordinary modes for a redacted
		// repository, which git will then refuse, rather than refusing to
		// repoint anything at all.
		if addrs, err := gh.TipAuthors(ctx, opts.GitHubUser, target); err == nil {
			res.Redacted = anyRedacted(addrs)
		}
	}

	mode := opts.modeFor(path)

	// A rewritten history admits one outcome. Pushing this clone to the
	// redacted copy is refused by git, and forcing past that would republish
	// the addresses the rewrite removed, so the other two modes are not
	// offered here whatever was asked for.
	if res.Redacted && mode != ModeGitHub {
		res.Action = "skipped"
		res.Reason = "GitHub holds a rewritten history; only --push-to=github is possible"
		return res
	}

	if opts.DryRun {
		res.Action = "planned"
		if res.Redacted {
			res.Reason = "take on GitHub's rewritten history; Gitea remote removed"
			if risk := CheckAdoptable(ctx, path); risk.Reason != "" {
				res.Action, res.Reason = "skipped", risk.Reason
			}
		} else {
			res.Reason = plannedDescription(mode, opts.OldRemoteName)
		}
		return res
	}

	if res.Redacted {
		// Asked again rather than trusted from the plan: the working copy may
		// have been touched since, and what is checked here is whether work
		// would be lost.
		if risk := CheckAdoptable(ctx, path); risk.Reason != "" {
			res.Action, res.Reason = "skipped", risk.Reason
			return res
		}
		if err := Adopt(ctx, path, res.NewURL, opts.GitEnv, opts.Log); err != nil {
			res.Action, res.Reason = "failed", err.Error()
			return res
		}
		res.Action, res.Reason = "adopted", "took on the rewritten history; Gitea remote removed"
		return res
	}

	switch mode {
	case ModeGitea:
		// origin is left exactly as it is; GitHub becomes an extra remote that
		// has to be named explicitly to be pushed to.
		if err := setRemote(ctx, path, "github", res.NewURL); err != nil {
			res.Action, res.Reason = "failed", err.Error()
			return res
		}
		opts.Log("added github remote to %s", path)
		res.Action, res.Reason = "added", "origin unchanged, github remote added"

	case ModeBoth:
		if err := configureDualPush(ctx, path, origin, res.NewURL); err != nil {
			res.Action, res.Reason = "failed", err.Error()
			return res
		}
		opts.Log("dual push configured for %s", path)
		res.Action, res.Reason = "dual-push", "one push reaches both servers"

	default: // ModeGitHub
		// Rename first, then add. In this order a failed rename has destroyed
		// nothing, and a failed add still leaves a working remote under the
		// new name.
		if _, err := gitOutput(ctx, path, "remote", "rename", "origin", opts.OldRemoteName); err != nil {
			res.Action, res.Reason = "failed", fmt.Sprintf("renaming origin to %s: %v", opts.OldRemoteName, err)
			return res
		}
		if _, err := gitOutput(ctx, path, "remote", "add", "origin", res.NewURL); err != nil {
			res.Action, res.Reason = "failed", fmt.Sprintf("adding new origin: %v", err)
			return res
		}
		opts.Log("relinked %s -> %s", path, res.NewURL)
		res.Action = "relinked"
	}

	return res
}

// anyRedacted reports whether these addresses came from a redacting rewrite.
//
// One is enough. Redaction applies to a whole history, so a repository either
// went through it or did not; a single address in the shape the redact package
// produces settles which.
func anyRedacted(addrs []string) bool {
	for _, addr := range addrs {
		if redact.IsRedacted(addr) {
			return true
		}
	}
	return false
}

// modeFor resolves the destination for one working copy.
//
// An explicit per-copy choice is the most specific thing the user said, so it
// beats the blanket one.
func (o Options) modeFor(path string) string {
	if mode, ok := o.ModeFor[path]; ok && mode != "" {
		return mode
	}
	return o.Mode
}

// wants reports whether this working copy is in the run at all.
//
// A nil Only means every clone found, which is what the command line asks for;
// an empty one means nothing was chosen, and is deliberately not the same
// thing.
func (o Options) wants(path string) bool {
	if o.Only == nil {
		return true
	}
	return o.Only[path]
}

// configureDualPush makes a single `git push` reach both servers.
//
// The mechanism is git's push URL list. The subtlety that makes this worth a
// helper: as soon as a remote has any pushurl at all, git stops using the
// remote's ordinary URL for pushing. Adding only the GitHub address would
// therefore silently *replace* Gitea as the push target rather than adding to
// it, which is the exact opposite of what the caller asked for. Both addresses
// have to be listed.
//
// origin keeps fetching from Gitea, so pulls and the existing workflow are
// untouched. Named remotes for each server are added as well, so a push can
// still be aimed at one of them on purpose.
func configureDualPush(ctx context.Context, path, giteaURL, githubURL string) error {
	// Clear any previous list so that re-running does not accumulate
	// duplicates. A missing key is the normal first-run state, not an error.
	_, _ = gitOutput(ctx, path, "config", "--unset-all", "remote.origin.pushurl")

	for _, url := range []string{giteaURL, githubURL} {
		if _, err := gitOutput(ctx, path, "config", "--add", "remote.origin.pushurl", url); err != nil {
			return fmt.Errorf("adding push url %s: %v", url, err)
		}
	}
	if err := setRemote(ctx, path, "gitea", giteaURL); err != nil {
		return err
	}
	return setRemote(ctx, path, "github", githubURL)
}

// setRemote points a named remote at a URL, creating it if it does not exist.
// Written to be safe to re-run, since a relink sweep is something people repeat
// after migrating a few more repositories.
func setRemote(ctx context.Context, path, name, url string) error {
	existing, err := gitOutput(ctx, path, "remote", "get-url", name)
	if err != nil {
		if _, err := gitOutput(ctx, path, "remote", "add", name, url); err != nil {
			return fmt.Errorf("adding remote %s: %v", name, err)
		}
		return nil
	}
	if existing == url {
		return nil
	}
	if _, err := gitOutput(ctx, path, "remote", "set-url", name, url); err != nil {
		return fmt.Errorf("updating remote %s: %v", name, err)
	}
	return nil
}

// plannedDescription explains what a dry run would have done, so --dry-run is
// informative about the chosen mode rather than just listing paths.
func plannedDescription(mode, oldName string) string {
	// No "would" in front: the ACTION column beside it already says whether
	// this is a plan or something that happened.
	return Describe(mode, oldName)
}

// Describe says what a mode does to a working copy, in terms of the two
// commands its owner will actually type.
//
// Naming the remotes that get moved -- "origin goes to GitHub" -- describes
// the mechanism and leaves the question unanswered: what happens to Gitea, and
// where does the next push go? Both halves are spelled out here, because the
// second is the one somebody is deciding on.
//
// The surviving remote is named, and said to be a remote. The name alone --
// "kept as gitea" -- reads as a state rather than as a thing, and the word
// alone would not tell anybody what to type: `git push gitea` needs both.
//
// Exported so the selection screen labels its rows with the same words the dry
// run prints. Two descriptions of the same three modes, kept in separate
// packages, would drift the first time one of them was reworded.
func Describe(mode, oldName string) string {
	switch mode {
	case ModeGitea:
		// origin is untouched; GitHub becomes an extra remote.
		return "push and pull stay on Gitea; GitHub added as the \"github\" remote"
	case ModeBoth:
		// origin keeps its Gitea fetch URL and gains both push URLs.
		return "push reaches both servers; pull still comes from Gitea"
	default:
		// origin is renamed, and a new origin points at GitHub.
		return "push and pull use GitHub; Gitea kept as the \"" + oldName + "\" remote"
	}
}

// findRepos walks root and returns every directory that contains a .git entry.
//
// It does not descend into a repository once found: submodules and vendored
// checkouts belong to their parent and should not be repointed independently.
func findRepos(root string) ([]string, error) {
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory should not abort the whole sweep.
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		// Skip the usual noise directories, which can contain thousands of
		// entries and never contain repositories worth relinking.
		switch d.Name() {
		case "node_modules", "vendor", ".cache":
			return filepath.SkipDir
		}
		if _, statErr := os.Stat(filepath.Join(path, ".git")); statErr == nil {
			found = append(found, path)
			return filepath.SkipDir
		}
		return nil
	})
	return found, err
}

// gitOutput runs a git command inside dir and returns its trimmed stdout.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}
