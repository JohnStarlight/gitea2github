package tui

import (
	"bufio"
	"reflect"
	"strings"
	"testing"
)

// sample builds a representative account: two plain repositories, one fork,
// one group project, one archived, one already on GitHub.
func sample() []Row {
	return []Row{
		{Name: "me/ascii-art", SourcePrivate: true, Private: true},
		{Name: "me/lem-in", SourcePrivate: false, Private: false},
		{Name: "me/old-fork", Fork: true, SourcePrivate: false, Private: false},
		{Name: "team/groupie", Foreign: true, SourcePrivate: true, Private: true},
		{Name: "me/attic", Archived: true, SourcePrivate: true, Private: true},
		{Name: "me/net-cat", Blocked: "already on GitHub, left untouched"},
	}
}

func keys(s string) []Key {
	var out []Key
	for _, r := range s {
		if r == ' ' {
			out = append(out, Key{Kind: KeySpace})
			continue
		}
		out = append(out, Key{Kind: KeyRune, Rune: r})
	}
	return out
}

func (m *Model) press(ks ...Key) *Model {
	for _, k := range ks {
		m.Update(k)
	}
	return m
}

func TestGatesHoldBackTheRightRows(t *testing.T) {
	m := NewModel(sample(), false, false, false, false, "")

	got := m.Selected()
	want := []string{"me/ascii-art", "me/lem-in"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("with every gate shut, selected = %v, want %v", got, want)
	}

	// Opening each gate admits exactly its own category.
	m.press(keys("2")...) // forks
	if got := m.Selected(); !contains(got, "me/old-fork") {
		t.Errorf("after opening the fork gate, selected = %v, want it to contain me/old-fork", got)
	}
	if contains(m.Selected(), "team/groupie") {
		t.Errorf("the fork gate admitted a group project")
	}

	m.press(keys("1")...) // group projects
	m.press(keys("3")...) // archived
	if got, want := len(m.Selected()), 5; got != want {
		t.Errorf("with every gate open, selected %d repositories, want %d", got, want)
	}
}

func TestBlockedRowsAreNeverSelected(t *testing.T) {
	m := NewModel(sample(), true, true, true, false, "")
	if contains(m.Selected(), "me/net-cat") {
		t.Fatal("a repository already on GitHub was selected for migration")
	}

	// Walking onto it and pressing space must not select it either.
	for i := 0; i < len(sample()); i++ {
		if m.currentRow() == 5 {
			break
		}
		m.press(Key{Kind: KeyDown})
	}
	m.press(Key{Kind: KeySpace})
	if contains(m.Selected(), "me/net-cat") {
		t.Fatal("space selected a blocked repository")
	}
	if m.note == "" {
		t.Error("space on a blocked row said nothing about why")
	}
}

func TestClosingAGateDeselectsItsRows(t *testing.T) {
	m := NewModel(sample(), false, true, false, false, "")
	if !contains(m.Selected(), "me/old-fork") {
		t.Fatal("setup: the fork should start selected with its gate open")
	}
	m.press(keys("2")...)
	if contains(m.Selected(), "me/old-fork") {
		t.Fatal("closing the fork gate left the fork selected")
	}
	// Reopening it brings the row back, even though it was bulk-deselected.
	m.press(keys("2")...)
	if !contains(m.Selected(), "me/old-fork") {
		t.Fatal("reopening the fork gate did not restore the fork")
	}
}

func TestVisibilityFlipIsRecordedOnlyWhenItDiffers(t *testing.T) {
	m := NewModel(sample(), false, false, false, false, "")
	if got := m.VisibilityOverrides(); got != nil {
		t.Fatalf("untouched plan produced overrides %v, want none", got)
	}

	// Cursor starts on me/ascii-art, which is private on Gitea.
	m.press(keys("v")...)
	got := m.VisibilityOverrides()
	want := map[string]bool{"me/ascii-art": false}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after one flip, overrides = %v, want %v", got, want)
	}

	// Flipping back is not a decision, so it must leave no override behind.
	m.press(keys("v")...)
	if got := m.VisibilityOverrides(); got != nil {
		t.Fatalf("flipping twice left overrides %v, want none", got)
	}
}

func TestSearchFiltersTheViewButNotTheSelection(t *testing.T) {
	m := NewModel(sample(), false, false, false, false, "")
	before := m.Selected()

	m.press(keys("/")...)
	m.press(keys("lem")...)
	if got, want := len(m.visible()), 1; got != want {
		t.Errorf("search for lem showed %d rows, want %d", got, want)
	}
	if got := m.Selected(); !reflect.DeepEqual(got, before) {
		t.Errorf("searching changed the selection: %v, want %v", got, before)
	}

	// Escape abandons the search and restores the full list.
	m.press(Key{Kind: KeyEscape})
	if got, want := len(m.visible()), len(sample()); got != want {
		t.Errorf("after escaping the search, %d rows visible, want %d", got, want)
	}
	if m.Cancelled() {
		t.Error("escape out of the search box quit the whole screen")
	}
}

func TestSelectNoneIsScopedToTheSearch(t *testing.T) {
	m := NewModel(sample(), false, false, false, false, "")
	m.press(keys("/")...)
	m.press(keys("lem")...)
	m.press(Key{Kind: KeyEnter}) // leave the box, keep the query
	m.press(keys("n")...)

	if contains(m.Selected(), "me/lem-in") {
		t.Error("n left the searched-for repository selected")
	}
	if !contains(m.Selected(), "me/ascii-art") {
		t.Error("n deselected a repository the search was hiding")
	}
}

func TestEnterRefusesAnEmptySelection(t *testing.T) {
	m := NewModel(sample(), false, false, false, false, "")
	m.press(keys("n")...)
	m.press(Key{Kind: KeyEnter})
	if m.Done() {
		t.Fatal("enter confirmed a run with nothing selected")
	}
	if m.note == "" {
		t.Error("refusing an empty selection explained nothing")
	}
}

func TestQuitAndCtrlCCancel(t *testing.T) {
	for name, k := range map[string]Key{
		"q":      {Kind: KeyRune, Rune: 'q'},
		"ctrl-c": {Kind: KeyCtrlC},
		"escape": {Kind: KeyEscape},
	} {
		m := NewModel(sample(), false, false, false, false, "")
		m.press(k)
		if !m.Done() || !m.Cancelled() {
			t.Errorf("%s did not cancel the screen (done=%v cancelled=%v)", name, m.Done(), m.Cancelled())
		}
	}
}

func TestRedactionTogglesAndClearsTheKeptAddress(t *testing.T) {
	m := NewModel(sample(), false, false, false, true, "me@example.com")
	m.press(keys("e")...) // off
	if m.Redact() {
		t.Fatal("e did not turn redaction off")
	}
	if m.KeepEmail() != "" {
		t.Errorf("turning redaction off kept the address %q", m.KeepEmail())
	}
}

func TestCursorDoesNotWrap(t *testing.T) {
	m := NewModel(sample(), false, false, false, false, "")
	for i := 0; i < 50; i++ {
		m.press(Key{Kind: KeyUp})
	}
	if m.cursor != 0 {
		t.Errorf("cursor ran off the top to %d", m.cursor)
	}
	for i := 0; i < 50; i++ {
		m.press(Key{Kind: KeyDown})
	}
	if want := len(sample()) - 1; m.cursor != want {
		t.Errorf("cursor = %d at the bottom, want %d", m.cursor, want)
	}
}

func TestViewFitsTheTerminalWidth(t *testing.T) {
	m := NewModel(sample(), true, true, true, true, "me@example.com")
	m.SetSize(40, 20)
	for _, line := range strings.Split(m.View("gitea -> github"), "\r\n") {
		if w := len(stripANSI(line)); w > 40 {
			t.Errorf("line is %d columns wide, want at most 40: %q", w, stripANSI(line))
		}
	}
}

func TestReadKeyDecodesSequences(t *testing.T) {
	cases := map[string]Key{
		"\x1b[A":  {Kind: KeyUp},
		"\x1b[B":  {Kind: KeyDown},
		"\x1b[5~": {Kind: KeyPageUp},
		"\x1b[6~": {Kind: KeyPageDown},
		"\x1b":    {Kind: KeyEscape},
		"\r":      {Kind: KeyEnter},
		" ":       {Kind: KeySpace},
		"\x03":    {Kind: KeyCtrlC},
		"\x7f":    {Kind: KeyBackspace},
		"a":       {Kind: KeyRune, Rune: 'a'},
		"λ":       {Kind: KeyRune, Rune: 'λ'},
	}
	for input, want := range cases {
		got, err := ReadKey(bufio.NewReader(strings.NewReader(input)))
		if err != nil {
			t.Errorf("ReadKey(%q) returned %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("ReadKey(%q) = %+v, want %+v", input, got, want)
		}
	}
}

func TestBackspaceRemovesWholeRunes(t *testing.T) {
	m := NewModel(sample(), false, false, false, true, "")
	m.press(keys("m")...) // focus the address box
	m.press(keys("λμ")...)
	m.press(Key{Kind: KeyBackspace})
	if got, want := m.KeepEmail(), "λ"; got != want {
		t.Errorf("after backspace the address is %q, want %q", got, want)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
