package main

import (
	"strings"
	"testing"

	"github.com/JohnStarlight/gitea2github/internal/migrate"
	"github.com/JohnStarlight/gitea2github/internal/ui"
)

// samplePlan mixes rows that will be created with rows that will not, which is
// the situation the numbering has to get right.
func samplePlan() []migrate.Result {
	return []migrate.Result{
		{Source: "me/alpha", Status: migrate.StatusPlanned, SourcePrivate: true, Private: true},
		{Source: "me/beta", Status: migrate.StatusExists},
		{Source: "me/gamma", Status: migrate.StatusPlanned, SourcePrivate: false, Private: false},
		{Source: "other/delta", Status: migrate.StatusSkipped},
		{Source: "me/epsilon", Status: migrate.StatusPlanned, SourcePrivate: true, Private: true},
	}
}

// TestPlannedIndicesSkipsUnaffectedRows checks that only rows being created are
// numbered. If skipped rows were counted, every number the user typed would
// point at the wrong repository.
func TestPlannedIndicesSkipsUnaffectedRows(t *testing.T) {
	got := plannedIndices(samplePlan())
	want := []int{0, 2, 4}
	if len(got) != len(want) {
		t.Fatalf("plannedIndices = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("plannedIndices = %v, want %v", got, want)
		}
	}
}

// TestAskVisibilityFlips covers the mapping from what the user types to what
// actually gets overridden.
func TestAskVisibilityFlips(t *testing.T) {
	plan := samplePlan()
	pending := plannedIndices(plan)

	// "1 3" means the first and third *creatable* repositories, which are
	// me/alpha and me/epsilon -- not the first and third rows of the table.
	prompt := ui.NewWith(strings.NewReader("1 3\n"), &strings.Builder{}, true)
	overrides := askVisibilityFlips(prompt, plan, pending)

	if len(overrides) != 2 {
		t.Fatalf("overrides = %v, want two entries", overrides)
	}
	// Both were private on Gitea, so flipping means public.
	if private, ok := overrides["me/alpha"]; !ok || private {
		t.Errorf("me/alpha = %v (present %v), want false, i.e. flipped to public", private, ok)
	}
	if private, ok := overrides["me/epsilon"]; !ok || private {
		t.Errorf("me/epsilon = %v (present %v), want false", private, ok)
	}
	// The rows in between must be untouched.
	if _, ok := overrides["me/beta"]; ok {
		t.Error("a row that is not being created was included in the selection")
	}
	if _, ok := overrides["me/gamma"]; ok {
		t.Error("an unselected repository was overridden")
	}
}

// TestAskVisibilityFlipsPublicToPrivate covers the other direction, since the
// question is a flip rather than a choice of one fixed value.
func TestAskVisibilityFlipsPublicToPrivate(t *testing.T) {
	plan := samplePlan()
	pending := plannedIndices(plan)

	// Number 2 is me/gamma, the public one.
	prompt := ui.NewWith(strings.NewReader("2\n"), &strings.Builder{}, true)
	overrides := askVisibilityFlips(prompt, plan, pending)

	if private, ok := overrides["me/gamma"]; !ok || !private {
		t.Errorf("me/gamma = %v (present %v), want true, i.e. flipped to private", private, ok)
	}
}

// TestAskVisibilityFlipsDefaultsToNoChange is the important one: pressing enter
// must leave every repository exactly as it is on Gitea.
func TestAskVisibilityFlipsDefaultsToNoChange(t *testing.T) {
	plan := samplePlan()
	prompt := ui.NewWith(strings.NewReader("\n"), &strings.Builder{}, true)
	if overrides := askVisibilityFlips(prompt, plan, plannedIndices(plan)); len(overrides) != 0 {
		t.Errorf("overrides = %v, want none for a bare enter", overrides)
	}
}

// TestForDisplayStripsCredentials guards the one place a user-supplied secret
// could reach the screen. Somebody used to authenticating through the URL will
// eventually pass one, and a token echoed into the scrollback outlives the run.
func TestForDisplayStripsCredentials(t *testing.T) {
	cases := map[string]string{
		// The ordinary case: nothing to hide, nothing changed.
		"https://platform.zone01.gr/git": "https://platform.zone01.gr/git",
		"http://localhost:3000":          "http://localhost:3000",

		// Both halves of a credential, and a username on its own.
		"https://me:ghp_secret@gitea.example.com/git": "https://gitea.example.com/git",
		"https://me@gitea.example.com/git":            "https://gitea.example.com/git",

		// A token containing characters that survive URL parsing.
		"https://me:ghp_aB3-_x.y@gitea.example.com": "https://gitea.example.com",
	}
	for input, want := range cases {
		if got := forDisplay(input); got != want {
			t.Errorf("forDisplay(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestForDisplayNeverLeaksOnMalformedInput is the property that matters more
// than exact formatting: whatever comes in, no secret comes out.
func TestForDisplayNeverLeaksOnMalformedInput(t *testing.T) {
	const secret = "ghp_SUPERSECRET"
	inputs := []string{
		"https://me:" + secret + "@gitea.example.com/git",
		"https://me:" + secret + "@gitea.example.com/git with a space",
		"://me:" + secret + "@broken",
		"https://me:" + secret + "@gitea.example.com/git\x7f",
	}
	for _, input := range inputs {
		if got := forDisplay(input); strings.Contains(got, secret) {
			t.Errorf("forDisplay(%q) leaked the secret: %q", input, got)
		}
	}
}
