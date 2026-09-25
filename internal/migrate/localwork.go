package migrate

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// LocalWork is what a copy of a repository on this computer has that Gitea
// does not: commits made here and never pushed, branches that exist only
// here, tags that exist only here.
//
// A migration copies from Gitea, so without this such work would simply be
// missing from GitHub -- silently, and for good when the history is redacted,
// since a redacted copy cannot be added to afterwards. Taking it from the
// clone as well is what lets someone finish a project locally and migrate it
// without a round trip through a server they are done with.
type LocalWork struct {
	Clone    string // the working copy it was found in
	Branches []LocalBranch
	Tags     []string // tags whose names Gitea does not have
}

// LocalBranch is one branch with commits that exist only on this computer.
type LocalBranch struct {
	Name    string
	Commits int  // commits on it that Gitea does not have
	New     bool // Gitea has no branch of this name

	// Diverged means Gitea's branch of the same name has commits this one
	// lacks, as well as the other way round -- a teammate pushed while this
	// copy was being worked on. There is no taking one without losing the
	// other, and merging is not this tool's decision, so such a branch goes
	// to GitHub as it is on Gitea.
	Diverged bool
}

// Empty reports whether there is nothing here that Gitea lacks.
func (w LocalWork) Empty() bool { return len(w.Branches) == 0 && len(w.Tags) == 0 }

// Taken is what a migration can take from this copy: every branch that only
// adds to Gitea's, and every tag.
func (w LocalWork) Taken() (branches, tags []string) {
	for _, b := range w.Branches {
		if !b.Diverged {
			branches = append(branches, b.Name)
		}
	}
	return branches, w.Tags
}

func (w LocalWork) takesAnything() bool {
	branches, tags := w.Taken()
	return len(branches)+len(tags) > 0
}

// Describe renders the work for a line of the summary, in plain words.
func (w LocalWork) Describe() string {
	var parts []string
	for _, b := range w.Branches {
		switch {
		case b.Diverged:
			parts = append(parts, fmt.Sprintf("%s: %s, NOT included (Gitea's %s also has commits this copy lacks)",
				b.Name, commits(b.Commits), b.Name))
		case b.New:
			parts = append(parts, fmt.Sprintf("%s: new branch, %s", b.Name, commits(b.Commits)))
		default:
			parts = append(parts, fmt.Sprintf("%s: %s", b.Name, commits(b.Commits)))
		}
	}
	for _, t := range w.Tags {
		parts = append(parts, "tag "+t)
	}
	return strings.Join(parts, "; ")
}

func commits(n int) string {
	if n == 1 {
		return "1 commit"
	}
	return strconv.Itoa(n) + " commits"
}

// FindLocalWork compares a working copy with what Gitea holds now.
//
// Gitea is asked directly, with ls-remote, rather than trusted from the
// clone's origin/* branches, which are only as fresh as the last fetch. The
// clone itself is only read: nothing is fetched into it and nothing changes.
//
// A commit counts as local when no Gitea branch or tag that this copy knows
// of reaches it. Commits pushed from here are known through origin/*, which a
// push updates; commits Gitea has that this copy never fetched are simply
// not here to count.
func FindLocalWork(ctx context.Context, clone, giteaCloneURL, giteaUser, giteaToken string) (LocalWork, error) {
	work := LocalWork{Clone: clone}

	remote, err := runGitAs(ctx, clone, giteaUser, giteaToken, "ls-remote", "--heads", "--tags", giteaCloneURL)
	if err != nil {
		return work, fmt.Errorf("asking Gitea what it has: %v: %s", err, remote)
	}
	giteaHeads := map[string]string{}
	giteaTags := map[string]bool{}
	var known []string // Gitea's tips that exist in this copy
	for _, line := range strings.Split(remote, "\n") {
		sha, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(ref, "refs/heads/"):
			giteaHeads[strings.TrimPrefix(ref, "refs/heads/")] = sha
		case strings.HasPrefix(ref, "refs/tags/"):
			giteaTags[strings.TrimSuffix(strings.TrimPrefix(ref, "refs/tags/"), "^{}")] = true
		default:
			continue
		}
		if _, err := runGit(ctx, clone, "", "cat-file", "-e", sha+"^{commit}"); err == nil {
			known = append(known, sha)
		}
	}

	heads, err := runGit(ctx, clone, "", "for-each-ref", "--format=%(refname:lstrip=2)", "refs/heads")
	if err != nil {
		return work, fmt.Errorf("listing branches: %v: %s", err, heads)
	}
	for _, name := range strings.Fields(heads) {
		args := append([]string{"rev-list", "--count", "refs/heads/" + name, "--not", "--remotes=origin"}, known...)
		out, err := runGit(ctx, clone, "", args...)
		if err != nil {
			return work, fmt.Errorf("comparing %s with Gitea: %v: %s", name, err, out)
		}
		n, err := strconv.Atoi(strings.TrimSpace(out))
		if err != nil || n == 0 {
			continue
		}
		branch := LocalBranch{Name: name, Commits: n}
		tip, onGitea := giteaHeads[name]
		switch {
		case !onGitea:
			branch.New = true
		default:
			// Only adding to Gitea's branch when Gitea's tip is part of it.
			// A tip this copy has never seen means Gitea moved on.
			_, err := runGit(ctx, clone, "", "merge-base", "--is-ancestor", tip, "refs/heads/"+name)
			branch.Diverged = err != nil
		}
		work.Branches = append(work.Branches, branch)
	}

	tags, err := runGit(ctx, clone, "", "for-each-ref", "--format=%(refname:lstrip=2)", "refs/tags")
	if err != nil {
		return work, fmt.Errorf("listing tags: %v: %s", err, tags)
	}
	for _, name := range strings.Fields(tags) {
		if !giteaTags[name] {
			work.Tags = append(work.Tags, name)
		}
	}
	return work, nil
}

// addLocalWork brings what a migration takes from a working copy into the
// mirror it is about to push, before anything is redacted: the work from this
// computer goes through the same rewrite as the rest.
//
// Nothing is forced. A branch taken from the copy only adds to Gitea's, so
// the update is a fast-forward, and git refusing one here means the copy is
// not what it was when the plan was made.
func addLocalWork(ctx context.Context, mirror string, work LocalWork) error {
	branches, tags := work.Taken()
	var refspecs []string
	for _, b := range branches {
		refspecs = append(refspecs, "refs/heads/"+b+":refs/heads/"+b)
	}
	for _, t := range tags {
		refspecs = append(refspecs, "refs/tags/"+t+":refs/tags/"+t)
	}
	if len(refspecs) == 0 {
		return nil
	}
	args := append([]string{"fetch", "--quiet", "--no-tags", work.Clone}, refspecs...)
	if out, err := runGit(ctx, mirror, "", args...); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}

// PushLocalWork sends what a migration takes from a working copy to Gitea as
// well, for someone who wants the two servers to agree.
//
// Pushed to Gitea's address with the migration's own credential rather than
// to the copy's origin, which may be an SSH address the credential cannot
// answer for.
func PushLocalWork(ctx context.Context, work LocalWork, giteaCloneURL, giteaUser, giteaToken string) error {
	branches, tags := work.Taken()
	var refspecs []string
	for _, b := range branches {
		refspecs = append(refspecs, "refs/heads/"+b+":refs/heads/"+b)
	}
	for _, t := range tags {
		refspecs = append(refspecs, "refs/tags/"+t+":refs/tags/"+t)
	}
	if len(refspecs) == 0 {
		return nil
	}
	args := append([]string{"push", "--quiet", giteaCloneURL}, refspecs...)
	if out, err := runGitAs(ctx, work.Clone, giteaUser, giteaToken, args...); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}
