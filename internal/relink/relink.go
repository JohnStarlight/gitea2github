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

// Options configures a relink sweep.
type Options struct {
	Root       string // directory to scan
	GiteaHost  string // only remotes on this host are considered
	GitHubUser string
	GitHubTok  string

	// OldRemoteName is what the existing Gitea remote gets renamed to.
	OldRemoteName string

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
		return res
	}

	// Rename first, then add. Doing it in this order means that if the rename
	// fails we have not yet destroyed anything, and if the add fails the user
	// still has a working remote under the new name.
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
	return res
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
