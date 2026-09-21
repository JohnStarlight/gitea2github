package migrate

import (
	"reflect"
	"testing"

	"github.com/JohnStarlight/gitea2github/internal/gitea"
)

func repo(full string) gitea.Repo {
	slash := 0
	for i, c := range full {
		if c == '/' {
			slash = i
			break
		}
	}
	r := gitea.Repo{Name: full[slash+1:], FullName: full}
	r.Owner.Login = full[:slash]
	return r
}

// TestCollidingNamesAreToldApart is the loss this exists to prevent. Gitea
// namespaces repositories by owner and GitHub does not, so your own
// implementation of an exercise and the group's arrive at the same
// destination: the first across creates it, the second is reported as already
// present, and one of the two never moves while the summary says everything
// was accounted for.
func TestCollidingNamesAreToldApart(t *testing.T) {
	repos := []gitea.Repo{
		repo("ivogiake/quadchecker"),
		repo("teammate/quadchecker"),
		repo("ivogiake/quad"),
	}
	got := Targets(repos, "ivogiake")
	want := map[string]string{
		"ivogiake/quadchecker": "quadchecker",
		"teammate/quadchecker": "quadchecker-teammate",
		"ivogiake/quad":        "quad",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Targets = %v, want %v", got, want)
	}
}

// TestYourOwnKeepsThePlainName pins which of the two is renamed. The one you
// own is the one you will look for under its own name.
func TestYourOwnKeepsThePlainName(t *testing.T) {
	repos := []gitea.Repo{repo("someone/project"), repo("me/project"), repo("other/project")}
	got := Targets(repos, "me")

	if got["me/project"] != "project" {
		t.Errorf("your own repository became %q, want the plain name", got["me/project"])
	}
	for _, full := range []string{"someone/project", "other/project"} {
		if got[full] == "project" {
			t.Errorf("%s took the plain name as well", full)
		}
	}
	if got["someone/project"] == got["other/project"] {
		t.Errorf("two other people's repositories share the name %q", got["someone/project"])
	}
}

// TestNamesDoNotDependOnTheOrderTheyArriveIn is what stops the outcome being
// decided by whichever worker finished first.
func TestNamesDoNotDependOnTheOrderTheyArriveIn(t *testing.T) {
	forwards := []gitea.Repo{repo("a/thing"), repo("b/thing"), repo("me/thing")}
	backwards := []gitea.Repo{repo("me/thing"), repo("b/thing"), repo("a/thing")}

	if one, two := Targets(forwards, "me"), Targets(backwards, "me"); !reflect.DeepEqual(one, two) {
		t.Errorf("the order changed the names:\n  %v\n  %v", one, two)
	}
}

// TestCollisionsReportsWhatWasRenamedAndWhy lets the screen say a name was
// changed rather than changing it silently.
func TestCollisionsReportsWhatWasRenamedAndWhy(t *testing.T) {
	repos := []gitea.Repo{repo("me/solo"), repo("me/shared"), repo("them/shared")}

	got := Collisions(repos)
	if len(got) != 1 {
		t.Fatalf("Collisions = %v, want one entry", got)
	}
	if sharing := got["shared"]; !reflect.DeepEqual(sharing, []string{"me/shared", "them/shared"}) {
		t.Errorf("Collisions[shared] = %v", sharing)
	}
	if Collisions([]gitea.Repo{repo("me/solo")}) != nil {
		t.Error("a list with no collisions reported one")
	}
}

// TestATypedNameWinsOverEverything covers the rename made on the screen, which
// is the most specific thing anybody said.
func TestATypedNameWinsOverEverything(t *testing.T) {
	r := repo("teammate/quadchecker")
	opts := Options{
		Target:   map[string]string{"teammate/quadchecker": "quadchecker-teammate"},
		RenameTo: map[string]string{"teammate/quadchecker": "quadchecker-team"},
	}
	if got := opts.targetFor(r); got != "quadchecker-team" {
		t.Errorf("targetFor = %q, want the typed name", got)
	}

	// A typed name is still sanitised: it becomes a GitHub repository name.
	opts.RenameTo["teammate/quadchecker"] = "quad checker/team"
	if got := opts.targetFor(r); got != "quad-checker-team" {
		t.Errorf("targetFor = %q, want the typed name sanitised", got)
	}

	// Blank means "no opinion", not "no name".
	opts.RenameTo["teammate/quadchecker"] = "   "
	if got := opts.targetFor(r); got != "quadchecker-teammate" {
		t.Errorf("targetFor = %q, want the worked-out name", got)
	}
}

// TestNoNamesAtAllFallsBackToTheRepositoryName keeps callers that never call
// Targets working as they did.
func TestNoNamesAtAllFallsBackToTheRepositoryName(t *testing.T) {
	if got := (Options{}).targetFor(repo("me/ascii-art")); got != "ascii-art" {
		t.Errorf("targetFor = %q, want ascii-art", got)
	}
}
