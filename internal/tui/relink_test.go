package tui

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/JohnStarlight/gitea2github/internal/relink"
)

func sampleClones() []Clone {
	return []Clone{
		{Path: "/home/me/Git/ascii-art", Display: "~/Git/ascii-art"},
		{Path: "/home/me/Git/lem-in", Display: "~/Git/lem-in"},
		{Path: "/home/me/Git/notes", Display: "~/Git/notes",
			Blocked: "origin is not on platform.zone01.gr"},
	}
}

func newRelink() *RelinkModel {
	m := NewRelinkModel(sampleClones(), relink.ModeGitHub, "gitea")
	m.SetSize(96, 20)
	return m
}

// TestDestinationIsChosenPerClone is the difference from the flag it replaces:
// --push-to forces one answer onto a whole directory tree, and a folder of
// coursework rarely wants one.
func TestDestinationIsChosenPerClone(t *testing.T) {
	m := newRelink()

	m.Update(Key{Kind: KeyRune, Rune: '2'}) // this one to both
	m.Update(Key{Kind: KeyDown})
	m.Update(Key{Kind: KeyRune, Rune: '3'}) // the next to gitea

	only, modes := m.Chosen()
	if len(only) != 2 {
		t.Fatalf("chose %d clones, want 2", len(only))
	}
	if got := modes["/home/me/Git/ascii-art"]; got != relink.ModeBoth {
		t.Errorf("first clone goes to %q, want %q", got, relink.ModeBoth)
	}
	if got := modes["/home/me/Git/lem-in"]; got != relink.ModeGitea {
		t.Errorf("second clone goes to %q, want %q", got, relink.ModeGitea)
	}
}

// TestBlockedClonesAreNeverChosen keeps working copies that cannot be
// repointed out of the run whatever is pressed on them.
func TestBlockedClonesAreNeverChosen(t *testing.T) {
	m := newRelink()
	m.cursor = 2 // the one that is not a Gitea clone

	for _, k := range []Key{{Kind: KeySpace}, {Kind: KeyRune, Rune: '2'}, {Kind: KeyRune, Rune: '3'}} {
		m.Update(k)
		if only, _ := m.Chosen(); only["/home/me/Git/notes"] {
			t.Fatalf("a clone that cannot be repointed was chosen after %+v", k)
		}
	}
	if m.note == "" {
		t.Error("pressing a key on a blocked clone said nothing about why")
	}
}

// TestApplyToAllUsesTheRowUnderTheCursor covers the bulk key, which is how a
// directory of thirty clones is set without walking it.
func TestApplyToAllUsesTheRowUnderTheCursor(t *testing.T) {
	m := newRelink()
	m.Update(Key{Kind: KeyRune, Rune: '2'}) // cursor row to both
	m.Update(Key{Kind: KeyRune, Rune: 'A'})

	_, modes := m.Chosen()
	for path, mode := range modes {
		if mode != relink.ModeBoth {
			t.Errorf("%s goes to %q after A, want %q", path, mode, relink.ModeBoth)
		}
	}
}

// TestChoosingADestinationPutsACloneBack treats picking a destination as a
// statement of intent clear enough to undo an earlier exclusion.
func TestChoosingADestinationPutsACloneBack(t *testing.T) {
	m := newRelink()
	m.Update(Key{Kind: KeySpace}) // take it out
	if only, _ := m.Chosen(); only["/home/me/Git/ascii-art"] {
		t.Fatal("setup: the clone should have been excluded")
	}
	m.Update(Key{Kind: KeyRune, Rune: '3'})
	if only, _ := m.Chosen(); !only["/home/me/Git/ascii-art"] {
		t.Error("choosing a destination did not put the clone back in the run")
	}
}

// TestChosenIsNilRatherThanEmpty guards the same distinction relink.Run draws:
// a nil Only there means every clone found.
func TestChosenIsNilRatherThanEmpty(t *testing.T) {
	m := newRelink()
	m.Update(Key{Kind: KeyRune, Rune: 'n'})

	only, modes := m.Chosen()
	if only != nil || modes != nil {
		t.Errorf("Chosen = (%v, %v) with nothing selected, want nil", only, modes)
	}
}

// TestEnterRefusesAnEmptySelection stops a screen that would do nothing from
// being mistaken for one that will.
func TestRelinkEnterRefusesAnEmptySelection(t *testing.T) {
	m := newRelink()
	m.Update(Key{Kind: KeyRune, Rune: 'n'})
	m.Update(Key{Kind: KeyEnter})
	if m.Done() {
		t.Fatal("enter confirmed a run with nothing selected")
	}
}

// TestQuitChangesNothing is the property every screen in this package shares.
func TestRelinkQuitChangesNothing(t *testing.T) {
	for name, k := range map[string]Key{
		"q":      {Kind: KeyRune, Rune: 'q'},
		"esc":    {Kind: KeyEscape},
		"ctrl-c": {Kind: KeyCtrlC},
	} {
		m := newRelink()
		m.Update(k)
		if !m.Done() || !m.Cancelled() {
			t.Errorf("%s did not cancel (done=%v cancelled=%v)", name, m.Done(), m.Cancelled())
		}
	}
}

// TestEachDestinationHasItsOwnColour keeps the three apart at a glance.
func TestEachDestinationHasItsOwnColour(t *testing.T) {
	seen := map[string]string{}
	for _, mode := range []string{relink.ModeGitHub, relink.ModeBoth, relink.ModeGitea} {
		c := modeColour(mode)
		if other, clash := seen[c]; clash {
			t.Errorf("%q and %q share a colour", other, mode)
		}
		seen[c] = mode
	}
}

// TestRowsCarryTheWordingOfTheDryRun stops the screen and the plan describing
// the same three modes differently.
func TestRowsCarryTheWordingOfTheDryRun(t *testing.T) {
	m := newRelink()
	m.Update(Key{Kind: KeyRune, Rune: '2'})

	line := stripANSI(strings.Join(m.renderClone(0, true), " "))
	if want := relink.Describe(relink.ModeBoth, "gitea"); !strings.Contains(line, want) {
		t.Errorf("row %q does not carry %q", line, want)
	}
}

// TestRelinkViewFitsCommonWidths sweeps the screen the way the migration one
// is swept.
func TestRelinkViewFitsCommonWidths(t *testing.T) {
	for _, w := range []int{120, 100, 80, 64, 44} {
		m := NewRelinkModel(sampleClones(), relink.ModeGitHub, "gitea")
		m.SetSize(w, 20)
		for _, line := range strings.Split(m.View("~/Git -> github.com/me"), "\r\n") {
			if got := visibleWidth(line); got > w {
				t.Errorf("at %d columns a line is %d wide: %q", w, got, stripANSI(line))
			}
		}
	}
}

// TestRelinkModelDoesNotWriteThroughToItsArgument matches the guarantee the
// migration screen makes.
func TestRelinkModelDoesNotWriteThroughToItsArgument(t *testing.T) {
	clones := sampleClones()
	m := NewRelinkModel(clones, relink.ModeGitHub, "gitea")
	m.Update(Key{Kind: KeyRune, Rune: '2'})

	for i, c := range clones {
		if c.Include || c.Mode != "" {
			t.Errorf("clone %d was written through: %+v", i, c)
		}
	}
}

// TestPadCountsRunesNotBytes guards the measurement every width check in this
// package rests on. The ellipsis pad appends is three bytes and one column, so
// counting bytes both overstates the result and cuts a multi-byte name in half.
func TestPadCountsRunesNotBytes(t *testing.T) {
	cases := []struct {
		in string
		n  int
	}{
		{"~/Git/ascii-art", 10},
		{"~/Κώδικας/ασκήσεις", 12},
		{"short", 10},
		{"日本語のリポジトリ", 6},
	}
	for _, c := range cases {
		got := pad(c.in, c.n)
		if w := len([]rune(got)); w != c.n {
			t.Errorf("pad(%q, %d) is %d columns wide, want %d (%q)", c.in, c.n, w, c.n, got)
		}
		if !utf8.ValidString(got) {
			t.Errorf("pad(%q, %d) cut a character in half: %q", c.in, c.n, got)
		}
	}
}

// TestRowsFitWhenNamesAreNotASCII is the case the byte count got wrong.
func TestRowsFitWhenNamesAreNotASCII(t *testing.T) {
	clones := []Clone{
		{Path: "/home/me/Κώδικας/ασκήσεις", Display: "~/Κώδικας/ασκήσεις"},
		{Path: "/home/me/Git/日本語", Display: "~/Git/日本語"},
	}
	for _, w := range []int{100, 80, 64, 44} {
		m := NewRelinkModel(clones, relink.ModeGitHub, "gitea")
		m.SetSize(w, 20)
		for _, line := range strings.Split(m.View("~/ -> github.com/me"), "\r\n") {
			if got := visibleWidth(line); got > w {
				t.Errorf("at %d columns a line is %d wide: %q", w, got, stripANSI(line))
			}
		}
	}
}

// typePath feeds a path into the directory box a character at a time.
func typePath(m *RelinkModel, path string) {
	for _, r := range path {
		m.Update(Key{Kind: KeyRune, Rune: r})
	}
}

// fakeScanner stands in for the real sweep, so the screen can be driven
// without a disk or a GitHub account behind it.
func fakeScanner(byRoot map[string][]Clone, fail map[string]error) (Rescan, *[]string) {
	var asked []string
	return func(root string) ([]Clone, error) {
		asked = append(asked, root)
		if err, bad := fail[root]; bad {
			return nil, err
		}
		return byRoot[root], nil
	}, &asked
}

// TestDirectoryCanBeChangedWithoutLeavingTheScreen is the point of the key:
// being in the wrong directory should cost a keystroke, not a restart.
func TestDirectoryCanBeChangedWithoutLeavingTheScreen(t *testing.T) {
	elsewhere := []Clone{
		{Path: "/home/me/Work/api", Display: "~/Work/api"},
		{Path: "/home/me/Work/web", Display: "~/Work/web"},
	}
	scan, asked := fakeScanner(map[string][]Clone{"~/Work": elsewhere}, nil)

	m := NewRelinkModel(sampleClones(), relink.ModeGitHub, "gitea").WithRoot("~/Git", scan)
	m.SetSize(96, 20)

	m.Update(Key{Kind: KeyRune, Rune: 'd'})
	// The box opens on the current directory, so a nearby path is a small edit.
	for range "Git" {
		m.Update(Key{Kind: KeyBackspace})
	}
	typePath(m, "Work")
	m.Update(Key{Kind: KeyEnter})

	if !m.Working() {
		t.Fatal("enter on a new directory did not ask for a scan")
	}
	m.Work()

	if len(*asked) != 1 || (*asked)[0] != "~/Work" {
		t.Errorf("scanned %v, want [~/Work]", *asked)
	}
	if m.Root() != "~/Work" {
		t.Errorf("Root = %q after the scan, want ~/Work", m.Root())
	}
	only, _ := m.Chosen()
	if len(only) != 2 || !only["/home/me/Work/api"] {
		t.Errorf("chose %v, want the two clones from the new directory", only)
	}
	for path := range only {
		if strings.Contains(path, "/Git/") {
			t.Errorf("a clone from the old directory survived the change: %s", path)
		}
	}
}

// TestWorkIsSeparateFromUpdate is what lets the screen say what it is doing.
// The scan walks the disk and asks GitHub about every clone it finds, so doing
// it inside Update would freeze the previous frame and look like a hang.
func TestWorkIsSeparateFromUpdate(t *testing.T) {
	scan, asked := fakeScanner(map[string][]Clone{"~/Work": nil}, nil)
	m := NewRelinkModel(sampleClones(), relink.ModeGitHub, "gitea").WithRoot("~/Git", scan)

	m.Update(Key{Kind: KeyRune, Rune: 'd'})
	for range "Git" {
		m.Update(Key{Kind: KeyBackspace})
	}
	typePath(m, "Work")
	m.Update(Key{Kind: KeyEnter})

	if len(*asked) != 0 {
		t.Error("Update performed the scan itself, leaving nothing to draw first")
	}
	if !m.Working() {
		t.Fatal("the screen does not report that it has work to do")
	}
	m.SetSize(96, 20)
	if frame := stripANSI(m.View("x")); !strings.Contains(frame, "scanning ~/Work") {
		t.Errorf("the frame drawn before the scan does not say what it is doing:\n%s", frame)
	}
	m.Work()
	if m.Working() {
		t.Error("the screen still reports work after doing it")
	}
}

// TestAFailedScanKeepsWhatWasOnScreen stops a mistyped path costing the
// selection built up so far.
func TestAFailedScanKeepsWhatWasOnScreen(t *testing.T) {
	scan, _ := fakeScanner(nil, map[string]error{"~/nope": errNoSuchDir})
	m := NewRelinkModel(sampleClones(), relink.ModeGitHub, "gitea").WithRoot("~/Git", scan)
	m.SetSize(96, 20)

	before, _ := m.Chosen()
	m.Update(Key{Kind: KeyRune, Rune: 'd'})
	for range "~/Git" {
		m.Update(Key{Kind: KeyBackspace})
	}
	typePath(m, "~/nope")
	m.Update(Key{Kind: KeyEnter})
	m.Work()

	after, _ := m.Chosen()
	if len(after) != len(before) {
		t.Errorf("a failed scan changed the selection: %d -> %d", len(before), len(after))
	}
	if m.Root() != "~/Git" {
		t.Errorf("a failed scan moved the directory to %q", m.Root())
	}
	if m.note == "" {
		t.Error("a failed scan said nothing about why")
	}
}

// TestEscapeKeepsTheCurrentDirectory covers backing out of the box.
func TestEscapeKeepsTheCurrentDirectory(t *testing.T) {
	scan, asked := fakeScanner(nil, nil)
	m := NewRelinkModel(sampleClones(), relink.ModeGitHub, "gitea").WithRoot("~/Git", scan)

	m.Update(Key{Kind: KeyRune, Rune: 'd'})
	typePath(m, "/somewhere/else")
	m.Update(Key{Kind: KeyEscape})

	if m.Working() || len(*asked) != 0 {
		t.Error("escaping the directory box still asked for a scan")
	}
	if m.Root() != "~/Git" {
		t.Errorf("Root = %q after escaping, want ~/Git", m.Root())
	}
	if m.Cancelled() {
		t.Error("escaping the directory box quit the whole screen")
	}
}

// TestTheSameDirectoryIsNotRescanned keeps an accidental Enter from costing a
// sweep of the disk and a round of GitHub lookups.
func TestTheSameDirectoryIsNotRescanned(t *testing.T) {
	scan, asked := fakeScanner(nil, nil)
	m := NewRelinkModel(sampleClones(), relink.ModeGitHub, "gitea").WithRoot("~/Git", scan)

	m.Update(Key{Kind: KeyRune, Rune: 'd'})
	m.Update(Key{Kind: KeyEnter})

	if m.Working() || len(*asked) != 0 {
		t.Errorf("re-entering the same directory rescanned it: %v", *asked)
	}
}

// TestScreensWithoutAScannerSaySo covers the model built without WithRoot.
func TestScreensWithoutAScannerSaySo(t *testing.T) {
	m := newRelink()
	m.Update(Key{Kind: KeyRune, Rune: 'd'})
	if m.note == "" {
		t.Error("d on a screen that cannot change directory said nothing")
	}
	if m.Working() {
		t.Error("a screen with no scanner asked for a scan")
	}
}

// errNoSuchDir stands in for whatever the filesystem would return.
var errNoSuchDir = errors.New("no such directory")

// TestDescriptionsSayWhereAPushGoes is the question somebody reading this
// screen is actually asking. Naming the remotes that move -- "origin goes to
// GitHub" -- describes the mechanism and leaves both halves of that question
// unanswered: where does the next push go, and what became of Gitea.
func TestDescriptionsSayWhereAPushGoes(t *testing.T) {
	for _, mode := range []string{relink.ModeGitHub, relink.ModeBoth, relink.ModeGitea} {
		got := relink.Describe(mode, "gitea")
		if !strings.Contains(got, "push") {
			t.Errorf("%s: %q does not say where a push goes", mode, got)
		}
		if !strings.Contains(strings.ToLower(got), "gitea") {
			t.Errorf("%s: %q does not say what became of Gitea", mode, got)
		}
	}

	// The three have to be told apart by reading, not only by colour.
	seen := map[string]bool{}
	for _, mode := range []string{relink.ModeGitHub, relink.ModeBoth, relink.ModeGitea} {
		d := relink.Describe(mode, "gitea")
		if seen[d] {
			t.Errorf("two modes share the description %q", d)
		}
		seen[d] = true
	}
}

// TestTheKeptRemoteNameAppearsInTheDescription covers --keep-as, which decides
// the name a push to Gitea will need afterwards.
func TestTheKeptRemoteNameAppearsInTheDescription(t *testing.T) {
	if got := relink.Describe(relink.ModeGitHub, "zone01"); !strings.Contains(got, "zone01") {
		t.Errorf("Describe with a custom name gave %q, want it to name zone01", got)
	}
}

// TestLongDescriptionsWrapRatherThanBeingCut matches the migration screen: a
// sentence cut off mid-word is worse than a second line.
func TestLongDescriptionsWrapRatherThanBeingCut(t *testing.T) {
	m := NewRelinkModel(sampleClones(), relink.ModeBoth, "gitea")
	m.SetSize(58, 22)

	lines := m.renderClone(0, false)
	if len(lines) < 2 {
		t.Fatalf("a description did not wrap at 58 columns: %q", lines)
	}
	// The continuation is indented, so the text is compared with runs of
	// whitespace collapsed rather than joined naively.
	joined := strings.Join(strings.Fields(stripANSI(strings.Join(lines, " "))), " ")
	if want := relink.Describe(relink.ModeBoth, "gitea"); !strings.Contains(joined, want) {
		t.Errorf("wrapping lost part of %q: %q", want, joined)
	}
	for _, l := range lines {
		if w := visibleWidth(l); w > 58 {
			t.Errorf("wrapped line is %d columns wide: %q", w, stripANSI(l))
		}
	}
}

// TestWideTerminalsKeepRowsOnOneLine is the other half.
func TestWideTerminalsKeepRowsOnOneLine(t *testing.T) {
	m := NewRelinkModel(sampleClones(), relink.ModeGitHub, "gitea")
	m.SetSize(120, 22)
	for i := range m.clones {
		if lines := m.renderClone(i, false); len(lines) != 1 {
			t.Errorf("clone %d took %d lines at 120 columns, want 1", i, len(lines))
		}
	}
}

// TestAnEmptyScreenSaysWhatToDo covers the directory with no clones in it,
// which is otherwise a blank rectangle.
func TestAnEmptyScreenSaysWhatToDo(t *testing.T) {
	scan := Rescan(func(string) ([]Clone, error) { return nil, nil })
	m := NewRelinkModel(nil, relink.ModeGitHub, "gitea").WithRoot("~/empty", scan)
	m.SetSize(80, 20)

	frame := stripANSI(m.View("github.com/me"))
	if !strings.Contains(frame, "press d to look somewhere else") {
		t.Errorf("an empty screen does not say what to do:\n%s", frame)
	}
}

// TestDescriptionsNameTheRemoteAndSaySoItIsOne covers what a name on its own
// left out. "kept as gitea" reads as a state rather than as a thing, and the
// word "remote" on its own would not tell anybody what to type: `git push
// gitea` needs both halves.
func TestDescriptionsNameTheRemoteAndSaySoItIsOne(t *testing.T) {
	cases := map[string]string{
		relink.ModeGitHub: "gitea",
		relink.ModeGitea:  "github",
	}
	for mode, remote := range cases {
		got := relink.Describe(mode, "gitea")
		if !strings.Contains(got, `"`+remote+`"`) {
			t.Errorf("%s: %q does not name the %q remote", mode, got, remote)
		}
		if !strings.Contains(got, "remote") {
			t.Errorf("%s: %q does not say that %q is a remote", mode, got, remote)
		}
	}
}

// TestRelinkUsesAWideTerminal matches the migration screen: on a wide window
// the paths are shown in full and the descriptions stay on one line.
func TestRelinkUsesAWideTerminal(t *testing.T) {
	clones := []Clone{
		{Path: "/home/me/Git/ascii-art-web-stylize", Display: "~/Git/ascii-art-web-stylize"},
		{Path: "/home/me/Git/atm-management-system", Display: "~/Git/atm-management-system"},
	}
	for _, w := range []int{110, 130, 160, 200} {
		m := NewRelinkModel(clones, relink.ModeGitea, "gitea")
		m.SetSize(w, 30)
		for i, c := range m.clones {
			lines := m.renderClone(i, false)
			if len(lines) != 1 {
				t.Errorf("at %d columns clone %d wrapped onto %d lines", w, i, len(lines))
			}
			got := stripANSI(strings.Join(lines, " "))
			if !strings.Contains(got, c.Display) {
				t.Errorf("at %d columns the path was truncated with room to spare: %q", w, got)
			}
			if want := relink.Describe(c.Mode, "gitea"); !strings.Contains(got, want) {
				t.Errorf("at %d columns the description was cut: %q", w, got)
			}
		}
	}
}

// TestTheColumnHoldsTheLongestDescription stops the layout shifting as
// destinations are cycled: every mode is one keystroke away, so the column has
// to fit the longest of them rather than whatever is shown right now.
func TestTheColumnHoldsTheLongestDescription(t *testing.T) {
	clones := []Clone{{Path: "/home/me/Git/a", Display: "~/Git/a"}}

	var widths []int
	for _, mode := range []string{relink.ModeGitHub, relink.ModeBoth, relink.ModeGitea} {
		m := NewRelinkModel(clones, mode, "gitea")
		m.SetSize(120, 20)
		widths = append(widths, m.nameColumn())
	}
	for _, w := range widths[1:] {
		if w != widths[0] {
			t.Errorf("the path column moves as the destination changes: %v", widths)
			break
		}
	}
}
