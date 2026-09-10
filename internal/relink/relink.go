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
)

// Result records what happened to one local clone.
type Result struct {
	Path   string // directory of the working copy
	OldURL string // the Gitea remote we found
	NewURL string // the GitHub remote we set, if any
	Action string // "relinked", "skipped", "planned" or "failed"
	Reason string
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
	}

	if opts.DryRun {
		res.Action = "planned"
		res.Reason = plannedDescription(opts.Mode, opts.OldRemoteName)
		return res
	}

	switch opts.Mode {
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
	switch mode {
	case ModeGitea:
		return "would add a github remote, leaving origin on Gitea"
	case ModeBoth:
		return "would make one push reach both servers"
	default:
		return "would move origin to GitHub, keeping Gitea as " + oldName
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
