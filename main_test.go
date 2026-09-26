package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/JohnStarlight/gitea2github/internal/gitea"
	"github.com/JohnStarlight/gitea2github/internal/migrate"
	"github.com/JohnStarlight/gitea2github/internal/redact"
	"github.com/JohnStarlight/gitea2github/internal/relink"
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

// mixedAccount is the awkward case the questions exist for: one plain
// repository, one fork, one archived, one belonging to somebody else.
func mixedAccount() []gitea.Repo {
	own := func(name string) gitea.Repo {
		r := gitea.Repo{Name: name, FullName: "me/" + name}
		r.Owner.Login = "me"
		return r
	}
	plain := own("ascii-art")
	fork := own("old-mirror")
	fork.Fork = true
	attic := own("first-try")
	attic.Archived = true
	theirs := gitea.Repo{Name: "groupie", FullName: "zone01/groupie"}
	theirs.Owner.Login = "zone01"
	return []gitea.Repo{plain, fork, attic, theirs}
}

// askAll drives askExclusions with scripted keystrokes and returns the answers
// together with everything that was printed.
func askAll(t *testing.T, input string, given map[string]bool, start exclusions) (exclusions, string) {
	t.Helper()
	var out strings.Builder
	prompt := ui.NewWith(strings.NewReader(input), &out, true)
	if given == nil {
		given = map[string]bool{}
	}
	return askExclusions(prompt, mixedAccount(), "me", given, start), out.String()
}

// TestAskExclusionsAcceptsEverything walks the whole sequence saying yes, which
// is the path that widens a migration the most and so the one worth pinning.
func TestAskExclusionsAcceptsEverything(t *testing.T) {
	got, _ := askAll(t, "y\ny\ny\ny\nme@example.com\n", nil,
		exclusions{KeepEmail: "git@example.com"})

	if !got.Collaborations || !got.Forks || !got.Archived || !got.RedactEmails {
		t.Errorf("answering yes to everything gave %+v", got)
	}
	if got.KeepEmail != "me@example.com" {
		t.Errorf("KeepEmail = %q, want the typed address", got.KeepEmail)
	}
}

// TestAskExclusionsDefaultsToExcluding is the safety property: pressing Enter
// through every question must not publish anybody else's work.
func TestAskExclusionsDefaultsToExcluding(t *testing.T) {
	got, _ := askAll(t, "\n\n\n\n\n", nil, exclusions{})

	if got.Collaborations || got.Forks || got.Archived {
		t.Errorf("bare Enter opted into something: %+v", got)
	}
	if got.RedactEmails {
		t.Error("bare Enter turned redaction on, which rewrites history unasked")
	}
}

// TestAskExclusionsSkipsQuestionsAnsweredOnTheCommandLine covers the rule that
// an explicit flag is a decision, not an opening for a question.
func TestAskExclusionsSkipsQuestionsAnsweredOnTheCommandLine(t *testing.T) {
	given := map[string]bool{"collaborations": true, "forks": true, "archived": true}
	start := exclusions{Collaborations: true, Forks: true, Archived: true}

	// Only the redaction question is left, so one answer is all the input needed.
	got, printed := askAll(t, "n\n", given, start)

	if !got.Collaborations || !got.Forks || !got.Archived {
		t.Errorf("a flag set on the command line was overwritten: %+v", got)
	}
	for _, unwanted := range []string{"owned by other people", "Include 1 fork", "archived"} {
		if strings.Contains(printed, unwanted) {
			t.Errorf("asked about %q although the flag was given:\n%s", unwanted, printed)
		}
	}
}

// TestAskExclusionsStaysSilentAboutCategoriesTheAccountLacks guards the reason
// the counts are there at all.
func TestAskExclusionsStaysSilentAboutCategoriesTheAccountLacks(t *testing.T) {
	plain := gitea.Repo{Name: "solo", FullName: "me/solo"}
	plain.Owner.Login = "me"

	var out strings.Builder
	prompt := ui.NewWith(strings.NewReader("n\n"), &out, true)
	askExclusions(prompt, []gitea.Repo{plain}, "me", map[string]bool{}, exclusions{})

	for _, unwanted := range []string{"fork", "archived", "other people"} {
		if strings.Contains(strings.ToLower(out.String()), unwanted) {
			t.Errorf("asked about %q for an account that has none:\n%s", unwanted, out.String())
		}
	}
}

// TestAskExclusionsDropsTheKeptAddressWhenRedactionIsOff stops a stale address
// reaching redact.NewMapper, where it would be counted as a decision the user
// never made.
func TestAskExclusionsDropsTheKeptAddressWhenRedactionIsOff(t *testing.T) {
	got, _ := askAll(t, "n\nn\nn\nn\n", nil, exclusions{KeepEmail: "git@example.com"})

	if got.RedactEmails {
		t.Fatal("setup: redaction should be off")
	}
	if got.KeepEmail != "" {
		t.Errorf("KeepEmail = %q with redaction off, want it dropped", got.KeepEmail)
	}
}

// TestAskExclusionsNeverBlocksWithoutATerminal is the property the whole ui
// package exists for, checked through this sequence because this is the one a
// script actually reaches.
func TestAskExclusionsNeverBlocksWithoutATerminal(t *testing.T) {
	var out strings.Builder
	prompt := ui.NewWith(strings.NewReader(""), &out, false)
	got := askExclusions(prompt, mixedAccount(), "me", map[string]bool{}, exclusions{})

	if got.Collaborations || got.Forks || got.Archived || got.RedactEmails {
		t.Errorf("non-interactive run opted into something: %+v", got)
	}
	if out.String() != "" {
		t.Errorf("printed a question with nobody to answer it: %q", out.String())
	}
}

// TestMigratedCopiesAreTheOnesThatMoved decides what the offer after a
// migration talks about: copies found of repositories that moved in this run,
// each marked rewritten only when it was. A repository left untouched on
// GitHub has a copy that still matches it, and is not offered.
func TestMigratedCopiesAreTheOnesThatMoved(t *testing.T) {
	results := []migrate.Result{
		{Source: "me/moved-and-redacted", Status: migrate.StatusMigrated},
		{Source: "me/moved-plain", Status: migrate.StatusMigrated},
		{Source: "me/already-there", Status: migrate.StatusExists},
		{Source: "me/no-copy", Status: migrate.StatusMigrated},
	}
	clones := map[string][]string{
		"me/moved-and-redacted": {"/c/a"},
		"me/moved-plain":        {"/c/b"},
		"me/already-there":      {"/c/c"},
	}
	opts := migrate.Options{
		Mapper:     redact.NewMapper(nil, ""),
		RedactOnly: map[string]bool{"me/moved-and-redacted": true, "me/already-there": true},
	}
	got := migratedCopies(results, opts, clones)
	want := []migratedCopy{{"/c/a", "me/moved-and-redacted", true}, {"/c/b", "me/moved-plain", false}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("migratedCopies = %+v, want %+v", got, want)
	}

	// With no Mapper nothing was rewritten, whatever RedactOnly says.
	for _, c := range migratedCopies(results, migrate.Options{RedactOnly: opts.RedactOnly}, clones) {
		if c.Redacted {
			t.Errorf("%s marked rewritten without a Mapper", c.Source)
		}
	}
}

// TestCountReadsAsASentence covers the phrasing the offer is built from, which
// has to work for one and for many.
func TestCountReadsAsASentence(t *testing.T) {
	cases := map[int]string{1: "1 history was", 2: "2 histories were", 30: "30 histories were"}
	for n, want := range cases {
		if got := count(n, "history was", "histories were"); got != want {
			t.Errorf("count(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestRelinkPlanKeepsTheAdoption covers the printed plan for a clone whose
// GitHub copy is a rewritten history. What will happen to it is an adoption
// -- the Gitea remote removed, the branch reset onto GitHub's commits -- and
// relabelling it with the chosen destination would describe an ordinary
// repoint that is not what runs.
func TestRelinkPlanKeepsTheAdoption(t *testing.T) {
	probe := []relink.Result{
		{Path: "/a", Action: "planned", Redacted: true,
			Reason: "take on GitHub's rewritten history; Gitea remote removed"},
		{Path: "/b", Action: "planned", Reason: "anything"},
	}
	plan := relinkPlanFromProbe(probe, nil, nil, relink.ModeGitHub, "gitea")

	if got := plan[0].Reason; got != probe[0].Reason {
		t.Errorf("rewritten clone described as %q, want %q", got, probe[0].Reason)
	}
	if got, want := plan[1].Reason, relink.Describe(relink.ModeGitHub, "gitea"); got != want {
		t.Errorf("ordinary clone described as %q, want %q", got, want)
	}
}

// TestRelinkTargetsCarryTheRenames covers what the migration hands to the
// repointing that follows it: every name worked out for the list, with what
// the run actually used laid over it -- including a name typed on the screen,
// which is recorded nowhere else.
func TestRelinkTargetsCarryTheRenames(t *testing.T) {
	targets := map[string]string{
		"JohnStarlight/quadchecker": "quadchecker",
		"teammate/quadchecker":      "quadchecker-teammate",
		"JohnStarlight/lem-in":      "lem-in",
	}
	results := []migrate.Result{
		{Source: "teammate/quadchecker", Target: "JohnStarlight/quadchecker-team"},
		{Source: "JohnStarlight/quadchecker", Target: "JohnStarlight/quadchecker"},
	}
	got := relinkTargets(targets, results)
	want := map[string]string{
		"johnstarlight/quadchecker": "quadchecker",
		"teammate/quadchecker":      "quadchecker-team",
		"johnstarlight/lem-in":      "lem-in",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s -> %q, want %q", k, got[k], v)
		}
	}
}

func TestParseNames(t *testing.T) {
	got, err := parseNames([]string{"Teammate/QuadChecker=quadchecker-team", " me/lem-in = lem in "})
	if err != nil {
		t.Fatal(err)
	}
	if got["teammate/quadchecker"] != "quadchecker-team" || got["me/lem-in"] != "lem in" {
		t.Errorf("parseNames = %v", got)
	}
	for _, bad := range []string{"quadchecker=x", "a/b", "a/b=", "a/b/c=x"} {
		if _, err := parseNames([]string{bad}); err == nil {
			t.Errorf("parseNames(%q) accepted it", bad)
		}
	}
}

// TestNamesWinAndAreMadeValid: a name given by hand beats the one worked out,
// and is made into one GitHub accepts, as the migration made it.
func TestNamesWinAndAreMadeValid(t *testing.T) {
	targets := map[string]string{"teammate/quadchecker": "quadchecker-teammate"}
	got := withNames(targets, map[string]string{"Teammate/QuadChecker": "quad team"})
	if got["teammate/quadchecker"] != "quad-team" {
		t.Errorf("withNames = %v", got)
	}
	if targets["teammate/quadchecker"] != "quadchecker-teammate" {
		t.Error("withNames changed the map it was given")
	}
}

// TestUnmatchedNamesAreReported: a misspelt --only name must not simply vanish.
func TestUnmatchedNamesAreReported(t *testing.T) {
	var repos []gitea.Repo
	for _, full := range []string{"me/linear-stats", "me/go-reloaded"} {
		owner, name, _ := strings.Cut(full, "/")
		r := gitea.Repo{Name: name, FullName: full}
		r.Owner.Login = owner
		repos = append(repos, r)
	}
	got := unmatchedNames(repos, []string{"linear-stats", " go-reloded ", "ME/GO-RELOADED", ""})
	if len(got) != 1 || got[0] != "go-reloded" {
		t.Errorf("unmatchedNames = %q, want [go-reloded]", got)
	}
}

// TestLFSReportGivesTheCommands: a repository whose LFS files arrived on
// GitHub as pointers is named, with the commands that copy the files -- from
// Gitea's address, to the name it took on GitHub.
func TestLFSReportGivesTheCommands(t *testing.T) {
	out := captureStdout(t, func() {
		printLFS([]migrate.Result{{
			Source: "me/lem-in", Target: "JohnStarlight/lem-in-team", Status: migrate.StatusMigrated,
			SourceURL: "https://gitea.example.com/me/lem-in.git",
			LFSFiles:  []string{"maps/big.txt", "video.mp4"},
		}})
	})
	for _, want := range []string{
		"me/lem-in uses Git LFS: 2 files are on GitHub as pointers only",
		"NOT as the files themselves",
		"git clone --mirror https://gitea.example.com/me/lem-in.git lem-in-team-lfs",
		"git lfs push --all https://github.com/JohnStarlight/lem-in-team.git",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

// captureStdout returns what f prints.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// Read while f writes: a pipe holds only so much, and a command that
	// prints more than that would otherwise block on a reader that has not
	// started.
	done := make(chan []byte)
	go func() {
		out, _ := io.ReadAll(r)
		done <- out
	}()
	saved := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = saved }()
	f()
	os.Stdout = saved
	w.Close()
	return string(<-done)
}

// TestCommitAsQuestion: asked only about clones whose pushes go to GitHub and
// whose commits are not already the GitHub identity; defaulting to yes after
// a redacting migration, where the next push would publish the hidden
// address, and to no otherwise.
func TestCommitAsQuestion(t *testing.T) {
	id := relink.Identity{Name: "JohnStarlight", Email: "1+JohnStarlight@users.noreply.github.com"}
	plan := []relink.Result{
		{Path: "/a", Action: "planned", Redacted: true, CommitName: "me", CommitEmail: "you@example.com"},
		{Path: "/b", Action: "planned", CommitName: "me", CommitEmail: "you@example.com"},
		{Path: "/c", Action: "planned", CommitName: id.Name, CommitEmail: id.Email},       // already
		{Path: "/d", Action: "planned", CommitName: "me", CommitEmail: "you@example.com"}, // stays on Gitea
		{Path: "/e", Action: "skipped", CommitName: "me", CommitEmail: "you@example.com"},
	}
	concerned, redacted := commitAsConcerned(plan, map[string]string{"/d": relink.ModeGitea}, relink.ModeGitHub, id)
	if len(concerned) != 2 || !redacted {
		t.Fatalf("concerned %d clones, redacted=%v; want 2, true", len(concerned), redacted)
	}
	q, yes := commitAsQuestion(concerned, redacted, id)
	for _, want := range []string{"these 2 clones would still carry you@example.com",
		"the next push would publish it on GitHub",
		"JohnStarlight <1+JohnStarlight@users.noreply.github.com>"} {
		if !strings.Contains(q, want) {
			t.Errorf("question lacks %q:\n%s", want, q)
		}
	}
	if !yes {
		t.Error("after redaction the default should be yes")
	}

	_, yes = commitAsQuestion(concerned[1:], false, id)
	if yes {
		t.Error("without redaction the default should be no")
	}
}
