package relink

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/JohnStarlight/gitea2github/internal/github"
)

// GitHubSide answers what the GitHub repository has, for comparing a working
// copy with it: the tree -- the files, apart from who committed them -- that a
// branch or a tag points at, and whether it exists at all.
//
// Two answer it. For the plan, GitHub's API, so that looking changes nothing
// here. For the adoption itself, GitHub's refs fetched into a private corner
// of the clone, so that the answer acted on is the one just checked.
type GitHubSide interface {
	BranchTree(ctx context.Context, branch string) (tree string, found bool, err error)
	TagTree(ctx context.Context, tag string) (tree string, found bool, err error)
}

// APISide is GitHubSide answered by GitHub's API.
type APISide struct {
	GH          *github.Client
	Owner, Name string
}

func (s APISide) BranchTree(ctx context.Context, branch string) (string, bool, error) {
	return s.GH.BranchTree(ctx, s.Owner, s.Name, branch)
}

func (s APISide) TagTree(ctx context.Context, tag string) (string, bool, error) {
	return s.GH.TagTree(ctx, s.Owner, s.Name, tag)
}

// repoSide is GitHubSide answered by refs in a local repository, under
// prefix: GitHub's own refs once fetched, or a stand-in for GitHub in tests.
type repoSide struct {
	path, prefix string
}

func (s repoSide) BranchTree(ctx context.Context, branch string) (string, bool, error) {
	return treeOf(ctx, s.path, s.prefix+"heads/"+branch)
}

func (s repoSide) TagTree(ctx context.Context, tag string) (string, bool, error) {
	return treeOf(ctx, s.path, s.prefix+"tags/"+tag)
}

func treeOf(ctx context.Context, path, ref string) (string, bool, error) {
	if _, err := gitOutput(ctx, path, "rev-parse", "--verify", "--quiet", ref); err != nil {
		return "", false, nil
	}
	tree, err := gitOutput(ctx, path, "rev-parse", ref+"^{tree}")
	return tree, err == nil, err
}

// ProblemKind is why one branch or tag stops an adoption.
type ProblemKind int

const (
	// NotOnGitHub: GitHub has no branch or tag of this name.
	NotOnGitHub ProblemKind = iota
	// NotLikeGitHub: GitHub has it, with different files.
	NotLikeGitHub
)

// AdoptProblem is one branch or tag that GitHub's copy does not match.
type AdoptProblem struct {
	Ref  string // "main", or "tag v2"
	Kind ProblemKind

	// Local is how many commits on it exist only on this computer -- never
	// on Gitea, so certainly not on GitHub. Zero with NotLikeGitHub usually
	// means the copy is behind: Gitea moved on after it last pulled.
	Local int
}

func (p AdoptProblem) String() string {
	switch {
	case p.Kind == NotOnGitHub && p.Local > 0:
		return fmt.Sprintf("%s is not on GitHub (%s only on this computer)", p.Ref, count(p.Local, "commit", "commits"))
	case p.Kind == NotOnGitHub:
		return p.Ref + " is not on GitHub"
	case p.Local > 0:
		return fmt.Sprintf("%s has %s that GitHub does not have", p.Ref, count(p.Local, "commit", "commits"))
	default:
		return p.Ref + " is different on GitHub (this copy may be behind Gitea)"
	}
}

// AdoptRisk is why a working copy cannot take on GitHub's rewritten history.
// An empty Reason means it can.
type AdoptRisk struct {
	Reason   string
	Problems []AdoptProblem
}

// CheckAdoptable reports whether this working copy can take on the history
// GitHub holds without losing anything.
//
// Adopting moves every branch and tag here onto its rewritten twin on GitHub,
// which is harmless exactly when the twin has the same files: redaction
// changes who made each commit and never what it contains, so nothing is
// lost and nothing in the working tree changes. So that is what is checked,
// for every branch and every tag -- not only the one checked out, since a
// branch left on the original history is one push away from publishing the
// addresses the rewrite removed.
//
// Anything with different files, or missing from GitHub, stops the adoption.
// Nothing is moved part of the way.
func CheckAdoptable(ctx context.Context, path string, side GitHubSide) AdoptRisk {
	// Modified or staged tracked files. Untracked ones are left alone by a
	// hard reset, so they are not a reason to refuse.
	status, err := gitOutput(ctx, path, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return AdoptRisk{Reason: fmt.Sprintf("cannot read the working copy: %v", err)}
	}
	if strings.TrimSpace(status) != "" {
		n := len(strings.Split(strings.TrimSpace(status), "\n"))
		return AdoptRisk{Reason: fmt.Sprintf("%s changed but not committed; commit %s first (or git stash)",
			count(n, "file", "files"), pickThem(n))}
	}
	if branch, err := gitOutput(ctx, path, "symbolic-ref", "--quiet", "--short", "HEAD"); err != nil || branch == "" {
		return AdoptRisk{Reason: "not on a branch; check one out first (git switch main)"}
	}

	var problems []AdoptProblem
	branches, err := gitOutput(ctx, path, "for-each-ref", "--format=%(refname:lstrip=2) %(tree)", "refs/heads")
	if err != nil {
		return AdoptRisk{Reason: fmt.Sprintf("cannot list the branches: %v", err)}
	}
	for _, line := range strings.Split(branches, "\n") {
		name, tree, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		there, found, err := side.BranchTree(ctx, name)
		if err != nil {
			return AdoptRisk{Reason: fmt.Sprintf("cannot compare %s with GitHub: %v", name, err)}
		}
		if found && there == tree {
			continue
		}
		p := AdoptProblem{Ref: name, Kind: NotLikeGitHub, Local: localCommits(ctx, path, "refs/heads/"+name)}
		if !found {
			p.Kind = NotOnGitHub
		}
		problems = append(problems, p)
	}

	tags, err := gitOutput(ctx, path, "for-each-ref", "--format=%(refname:lstrip=2)", "refs/tags")
	if err != nil {
		return AdoptRisk{Reason: fmt.Sprintf("cannot list the tags: %v", err)}
	}
	for _, name := range strings.Fields(tags) {
		here, err := gitOutput(ctx, path, "rev-parse", "refs/tags/"+name+"^{tree}")
		if err != nil {
			// A tag of something other than a commit has no files to compare
			// and nothing to publish.
			continue
		}
		there, found, err := side.TagTree(ctx, name)
		if err != nil {
			return AdoptRisk{Reason: fmt.Sprintf("cannot compare tag %s with GitHub: %v", name, err)}
		}
		if found && there == here {
			continue
		}
		p := AdoptProblem{Ref: "tag " + name, Kind: NotLikeGitHub, Local: localCommits(ctx, path, "refs/tags/"+name)}
		if !found {
			p.Kind = NotOnGitHub
		}
		problems = append(problems, p)
	}

	if len(problems) == 0 {
		return AdoptRisk{}
	}
	parts := make([]string, len(problems))
	for i, p := range problems {
		parts[i] = p.String()
	}
	return AdoptRisk{Reason: strings.Join(parts, "; "), Problems: problems}
}

// localCommits counts the commits reachable from ref that no Gitea branch
// this copy knows of reaches: work done here and never pushed.
func localCommits(ctx context.Context, path, ref string) int {
	out, err := gitOutput(ctx, path, "rev-list", "--count", ref, "--not", "--remotes=origin")
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n
}

// AdoptAdvice says, in plain words, what can be done about the problems that
// stopped adoptions. It is written for people reading English as a second
// language: one idea per sentence, and what will NOT happen said outright.
func AdoptAdvice(problems []AdoptProblem) string {
	var work, stray, behind bool
	for _, p := range problems {
		switch {
		case p.Local > 0:
			work = true
		case p.Kind == NotOnGitHub:
			stray = true
		default:
			behind = true
		}
	}
	var b strings.Builder
	b.WriteString("Nothing was changed in these copies.\n")
	if work {
		b.WriteString("Some copies have work that GitHub does NOT have.\n" +
			"GitHub's copy was made without it, and this tool cannot add it now.\n" +
			"To put it on GitHub: delete that repository on GitHub,\n" +
			"then run migrate again from this directory. It will take the work along.\n")
	}
	if stray {
		b.WriteString("Some branches or tags are not on GitHub, and have no work of their own.\n" +
			"If you do not need them, delete them (git branch -D <name>, git tag -d <name>),\n" +
			"then run relink again.\n")
	}
	if behind {
		b.WriteString("Some branches are different on GitHub, with no work of their own here.\n" +
			"Usually this copy is behind: update it from Gitea (git pull), then run relink again.\n")
	}
	return b.String()
}

// adoptPrefix is where GitHub's refs are fetched to while an adoption is
// checked: outside refs/heads and refs/tags, so that nothing the user sees
// or pushes changes until every check has passed.
const adoptPrefix = "refs/gitea2github/adopt/"

// Adopt makes a working copy a clone of the rewritten GitHub repository.
//
// The history is fetched rather than reproduced locally. Reproducing it would
// mean rewriting these commits with the same replacement addresses the
// migration used, and getting that subtly wrong would leave a clone that
// still cannot push. Taking GitHub's own objects cannot be subtly wrong: they
// are the same objects, so the result matches by construction.
//
// Everything GitHub has is fetched first, into a private corner of the clone,
// and the whole of CheckAdoptable is asked again against exactly that. Only
// when every branch and tag has its twin is anything moved; otherwise the
// private refs are removed and the clone is as it was.
//
// The working tree does not change: the branch checked out is moved onto a
// commit with the same files.
func Adopt(ctx context.Context, path, githubURL string, env []string, log func(string, ...any)) error {
	if log == nil {
		log = func(string, ...any) {}
	}
	clean := func() {
		refs, _ := gitOutput(ctx, path, "for-each-ref", "--format=%(refname)", adoptPrefix)
		for _, ref := range strings.Fields(refs) {
			_, _ = gitOutput(ctx, path, "update-ref", "-d", ref)
		}
	}
	defer clean()

	log("fetching the rewritten history for %s", path)
	if out, err := gitWithEnv(ctx, path, env, "fetch", "--quiet", "--no-tags", githubURL,
		"+refs/heads/*:"+adoptPrefix+"heads/*", "+refs/tags/*:"+adoptPrefix+"tags/*"); err != nil {
		return fmt.Errorf("fetching from GitHub: %v: %s", err, out)
	}

	// The last check before the steps that cannot be taken back, against
	// exactly what was fetched.
	if risk := CheckAdoptable(ctx, path, repoSide{path: path, prefix: adoptPrefix}); risk.Reason != "" {
		return fmt.Errorf("%s; nothing was changed", risk.Reason)
	}

	current, err := gitOutput(ctx, path, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return fmt.Errorf("reading the current branch: %w", err)
	}
	branches, _ := gitOutput(ctx, path, "for-each-ref", "--format=%(refname:lstrip=2)", "refs/heads")
	tags, _ := gitOutput(ctx, path, "for-each-ref", "--format=%(refname:lstrip=2)", "refs/tags")

	// The branch checked out goes by reset, so that the index follows; its
	// files are the same, so the working tree does not move. Every other
	// branch and tag is simply repointed.
	if out, err := gitOutput(ctx, path, "reset", "--hard", adoptPrefix+"heads/"+current); err != nil {
		return fmt.Errorf("moving %s onto the rewritten history: %v: %s", current, err, out)
	}
	for _, b := range strings.Fields(branches) {
		if b == current {
			continue
		}
		if out, err := gitOutput(ctx, path, "update-ref", "refs/heads/"+b, adoptPrefix+"heads/"+b); err != nil {
			return fmt.Errorf("moving %s onto the rewritten history: %v: %s", b, err, out)
		}
	}
	for _, t := range strings.Fields(tags) {
		if _, err := gitOutput(ctx, path, "rev-parse", "--verify", "--quiet", adoptPrefix+"tags/"+t); err != nil {
			continue // not a tag of a commit; left as it was, see CheckAdoptable
		}
		if out, err := gitOutput(ctx, path, "update-ref", "refs/tags/"+t, adoptPrefix+"tags/"+t); err != nil {
			return fmt.Errorf("moving tag %s onto the rewritten history: %v: %s", t, err, out)
		}
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

	// origin/* still point at Gitea's original commits, under GitHub's name.
	// Left like that, git status reports every branch as diverged -- which is
	// the one message that invites a pull that fails or a push --force that
	// republishes everything. Fetching replaces them with GitHub's.
	if out, err := gitWithEnv(ctx, path, env, "fetch", "--quiet", "--prune", "--no-tags", "origin"); err != nil {
		log("could not refresh origin/* for %s: %v: %s", path, err, out)
	}
	for _, b := range strings.Fields(branches) {
		if out, err := gitOutput(ctx, path, "branch", "--set-upstream-to=origin/"+b, b); err != nil {
			// Not fatal. The remote is right, which is what matters; the
			// tracking branch is a convenience git will offer on the next push.
			log("could not set upstream for %s in %s: %v: %s", b, path, err, out)
		}
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
