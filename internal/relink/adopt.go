package relink

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// AdoptRisk is why a working copy cannot safely take on a rewritten history.
// An empty Reason means it can.
type AdoptRisk struct {
	Reason string
}

// CheckAdoptable reports whether this working copy can take on the history
// GitHub holds without losing anything.
//
// Adopting means resetting the checked-out branch onto commits that share no
// ancestor with the ones here, which is exactly the operation that quietly
// discards work. Two things have to be true first: nothing uncommitted that
// the reset would revert, and nothing committed here that has not already
// reached Gitea -- because once the clone belongs to GitHub it can never push
// to Gitea again, and an unpushed commit would have nowhere left to go.
func CheckAdoptable(ctx context.Context, path string) AdoptRisk {
	// Modified or staged tracked files. Untracked ones are left alone by a
	// hard reset, so they are not a reason to refuse.
	status, err := gitOutput(ctx, path, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return AdoptRisk{Reason: fmt.Sprintf("cannot read the working copy: %v", err)}
	}
	if strings.TrimSpace(status) != "" {
		n := len(strings.Split(strings.TrimSpace(status), "\n"))
		return AdoptRisk{Reason: fmt.Sprintf("%s uncommitted; commit or stash %s first",
			count(n, "file", "files"), pickThem(n))}
	}

	// Commits here that the Gitea remote has never seen. The branch's upstream
	// is asked first, and the matching remote-tracking branch after it, since
	// a branch created locally often has no upstream set while origin/<branch>
	// exists all the same.
	branch, err := gitOutput(ctx, path, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return AdoptRisk{Reason: fmt.Sprintf("cannot read the current branch: %v", err)}
	}
	branch = strings.TrimSpace(branch)

	for _, base := range []string{"@{upstream}", "origin/" + branch} {
		ahead, err := gitOutput(ctx, path, "rev-list", "--count", base+"..HEAD")
		if err != nil {
			continue
		}
		n, convErr := strconv.Atoi(strings.TrimSpace(ahead))
		if convErr != nil {
			continue
		}
		if n > 0 {
			return AdoptRisk{Reason: fmt.Sprintf("%s never pushed to Gitea; push %s first",
				count(n, "commit", "commits"), pickThem(n))}
		}
		return AdoptRisk{}
	}

	// Neither comparison was possible, so whether anything would be lost is
	// unknown -- and for an operation with no way back, unknown has to mean
	// no. Saying "probably fine" about commits that cannot be recovered is the
	// one answer this function must never give.
	return AdoptRisk{Reason: fmt.Sprintf(
		"cannot tell whether %q has been pushed: no upstream and no origin/%s to compare against",
		branch, branch)}
}

// Adopt makes a working copy a clone of the rewritten GitHub repository.
//
// The history is fetched and checked out rather than reproduced locally.
// Reproducing it would mean rewriting these commits with the same replacement
// addresses the migration used, and getting that subtly wrong would leave a
// clone that still cannot push. Taking GitHub's own objects cannot be subtly
// wrong: they are the same objects, so the result matches by construction.
//
// The working tree does not change. Redaction rewrites who made a commit and
// not what it contains, so every tree in the fetched history is the tree that
// was already checked out.
func Adopt(ctx context.Context, path, githubURL string, env []string, log func(string, ...any)) error {
	if log == nil {
		log = func(string, ...any) {}
	}

	branch, err := gitOutput(ctx, path, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return fmt.Errorf("reading the current branch: %w", err)
	}
	branch = strings.TrimSpace(branch)

	log("fetching the rewritten history for %s", path)
	if out, err := gitWithEnv(ctx, path, env, "fetch", githubURL, branch); err != nil {
		return fmt.Errorf("fetching from GitHub: %v: %s", err, out)
	}
	// The last check before the one step that cannot be taken back. A
	// rewritten copy of this project has exactly the files checked out here,
	// since redaction changes who made each commit and never what it
	// contains; anything else is another project that shares the name, or a
	// clone that is behind what was migrated. Either way the reset would
	// replace files, which adopting is promised never to do.
	here, errHere := gitOutput(ctx, path, "rev-parse", "HEAD^{tree}")
	there, errThere := gitOutput(ctx, path, "rev-parse", "FETCH_HEAD^{tree}")
	if errHere != nil || errThere != nil {
		return fmt.Errorf("comparing with GitHub's %s: could not read both trees", branch)
	}
	if here != there {
		return fmt.Errorf("GitHub's %s has different files from this clone -- a different "+
			"repository, or this clone is behind Gitea (pull first); nothing was changed", branch)
	}
	if out, err := gitOutput(ctx, path, "reset", "--hard", "FETCH_HEAD"); err != nil {
		return fmt.Errorf("moving onto the rewritten history: %v: %s", err, out)
	}

	// origin becomes GitHub, and the Gitea remote goes. Leaving it in place
	// would leave a remote this clone can no longer push to, sitting there
	// looking like one it can.
	if err := setRemote(ctx, path, "origin", githubURL); err != nil {
		return fmt.Errorf("pointing origin at GitHub: %w", err)
	}
	for _, name := range []string{"gitea", "github"} {
		// Errors ignored: a remote that is not there is the state we want.
		_, _ = gitOutput(ctx, path, "remote", "remove", name)
	}
	if out, err := gitOutput(ctx, path, "branch",
		"--set-upstream-to=origin/"+branch, branch); err != nil {
		// Not fatal. The remote is right, which is what matters; the tracking
		// branch is a convenience git will offer to set on the next push.
		log("could not set upstream for %s: %v: %s", path, err, out)
	}
	return nil
}

// gitWithEnv runs git with extra environment entries, which is how a
// credential reaches it without ever appearing in an argument.
func gitWithEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// count renders "1 file" or "3 files".
func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// pickThem renders "it" or "them" to match a count.
func pickThem(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}
