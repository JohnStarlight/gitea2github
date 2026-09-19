package migrate

import (
	"sort"
	"strings"

	"github.com/JohnStarlight/gitea2github/internal/gitea"
	"github.com/JohnStarlight/gitea2github/internal/github"
)

// Targets assigns each repository the name it will take on GitHub, keyed by
// its Gitea full name.
//
// Gitea namespaces repositories by owner and GitHub does not, so two
// repositories called the same thing under different owners -- your own
// implementation of an exercise and the group's -- arrive at the same
// destination. Left alone that is not merely untidy but silently lossy: the
// first one across creates the repository and the second is reported as
// already present, so one of the two never moves and the summary says every
// repository was accounted for.
//
// The names are worked out from the whole list up front, so that what happens
// does not depend on which worker finished first.
func Targets(repos []gitea.Repo, giteaUser string) map[string]string {
	// Sorted so the assignment is the same whatever order the API returned.
	ordered := make([]gitea.Repo, len(repos))
	copy(ordered, repos)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].FullName < ordered[j].FullName })

	byBase := map[string][]gitea.Repo{}
	for _, r := range ordered {
		base := github.SanitizeName(r.Name)
		byBase[base] = append(byBase[base], r)
	}

	out := make(map[string]string, len(repos))
	for base, sharing := range byBase {
		if len(sharing) == 1 {
			out[sharing[0].FullName] = base
			continue
		}
		// Your own keeps the plain name; the others are told apart by whose
		// they are, which is the reason there are two of them.
		for _, r := range sharing {
			if r.OwnedBy(giteaUser) {
				out[r.FullName] = base
				continue
			}
			out[r.FullName] = base + "-" + github.SanitizeName(r.Owner.Login)
		}
	}
	return out
}

// Collisions returns the destination names that more than one repository wants,
// with the Gitea full names that want them, sorted.
//
// Reported rather than only resolved, so the screen can say that a name was
// changed and let it be changed again by hand: "quadchecker-teammate" is
// correct and impersonal, and the person who owns the repositories usually has
// a better word for which one it is.
func Collisions(repos []gitea.Repo) map[string][]string {
	byBase := map[string][]string{}
	for _, r := range repos {
		base := github.SanitizeName(r.Name)
		byBase[base] = append(byBase[base], r.FullName)
	}
	out := map[string][]string{}
	for base, sharing := range byBase {
		if len(sharing) > 1 {
			sort.Strings(sharing)
			out[base] = sharing
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// targetFor is the name one repository will take, honouring a rename typed on
// the screen over the one worked out from the list.
func (o Options) targetFor(repo gitea.Repo) string {
	if name, ok := o.RenameTo[repo.FullName]; ok && strings.TrimSpace(name) != "" {
		return github.SanitizeName(strings.TrimSpace(name))
	}
	if name, ok := o.Target[repo.FullName]; ok && name != "" {
		return name
	}
	return github.SanitizeName(repo.Name)
}
