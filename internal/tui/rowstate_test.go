package tui

import (
	"fmt"
	"strings"
	"testing"
)

func stateRows() []Row {
	return []Row{
		{Name: "me/plain", SourcePrivate: true, Private: true},
		{Name: "me/gone", Blocked: "already on GitHub, left untouched"},
		{Name: "me/fork", Fork: true},
		{Name: "team/shared", Foreign: true, SourcePrivate: true, Private: true},
	}
}

// TestRowStateSeparatesCannotFromCouldNot is the distinction the colours exist
// for. A repository already on GitHub and a fork behind a closed gate were
// drawn identically before, although one is inert and the other is a keystroke
// away.
func TestRowStateSeparatesCannotFromCouldNot(t *testing.T) {
	m := NewModel(stateRows(), false, false, false, false, "")

	if got := m.rowState(m.rows[1]); got != stateInert {
		t.Errorf("a repository already on GitHub is %v, want stateInert", got)
	}
	if got := m.rowState(m.rows[2]); got != stateAvailable {
		t.Errorf("a fork behind a closed gate is %v, want stateAvailable", got)
	}
	if m.rowState(m.rows[1]).colour() == m.rowState(m.rows[2]).colour() {
		t.Error("inert and available rows are drawn in the same colour")
	}
	if m.rowState(m.rows[1]).symbol() == m.rowState(m.rows[2]).symbol() {
		t.Error("inert and available rows are drawn with the same symbol, " +
			"so the screen does not read without colour")
	}
}

// TestEachKindOfChangeIsItsOwnState keeps the two changes apart. They are not
// alike -- redaction is global, visibility is decided row by row -- and a row
// that has had both done to it is a third thing again.
func TestEachKindOfChangeIsItsOwnState(t *testing.T) {
	m := NewModel(stateRows(), false, false, false, false, "")
	if got := m.rowState(m.rows[0]); got != stateVerbatim {
		t.Fatalf("an untouched row is %v, want stateVerbatim", got)
	}

	m.rows[0].Private = false
	if got := m.rowState(m.rows[0]); got != stateVisibility {
		t.Errorf("a row flipped to public is %v, want stateVisibility", got)
	}

	m.redact = true
	if got := m.rowState(m.rows[0]); got != stateBothWays {
		t.Errorf("a row both flipped and redacted is %v, want stateBothWays", got)
	}

	m.rows[0].Private = true
	if got := m.rowState(m.rows[0]); got != stateRedacted {
		t.Errorf("a row only redacted is %v, want stateRedacted", got)
	}
}

// TestTheThreeChangesAreToldApartByColour is the point of splitting them: the
// screen has to say which change was made, not merely that one was.
func TestTheThreeChangesAreToldApartByColour(t *testing.T) {
	seen := map[string]state{}
	for _, s := range []state{stateVerbatim, stateVisibility, stateRedacted, stateBothWays, stateAvailable, stateInert} {
		if other, clash := seen[s.colour()]; clash {
			t.Errorf("states %v and %v share a colour", other, s)
		}
		seen[s.colour()] = s
	}
}

// TestSixteenColourFallbackKeepsThemApart covers the terminal that cannot show
// the 256-colour shades: the distinction has to survive the downgrade.
func TestSixteenColourFallbackKeepsThemApart(t *testing.T) {
	vis, red, both := ansiVisibility, ansiRedacted, ansiBothWays
	t.Cleanup(func() { ansiVisibility, ansiRedacted, ansiBothWays = vis, red, both })

	usePalette("xterm", "")
	for _, c := range []string{ansiVisibility, ansiRedacted, ansiBothWays} {
		if strings.Contains(c, "38;5;") {
			t.Errorf("a 256-colour code survived the fallback: %q", c)
		}
	}
	if ansiVisibility == ansiRedacted || ansiRedacted == ansiBothWays || ansiVisibility == ansiBothWays {
		t.Error("the fallback palette collapsed two changes into one colour")
	}
	if ansiVisibility == ansiGreen || ansiRedacted == ansiCyan {
		t.Error("the fallback palette collides with the states it must stay clear of")
	}
}

// TestPaletteChoiceReadsTheEnvironment pins when the fuller palette is used.
func TestPaletteChoiceReadsTheEnvironment(t *testing.T) {
	cases := map[[2]string]bool{
		{"xterm-256color", ""}:  true,
		{"screen-256color", ""}: true,
		{"xterm", "truecolor"}:  true,
		{"xterm", "24bit"}:      true,
		{"xterm", ""}:           false,
		{"vt100", ""}:           false,
		{"", ""}:                false,
	}
	for in, want := range cases {
		if got := supports256(in[0], in[1]); got != want {
			t.Errorf("supports256(%q, %q) = %v, want %v", in[0], in[1], got, want)
		}
	}
}

// TestRedactionRepaintsTheWholeList checks the feedback that makes pressing e
// impossible to miss: every repository being migrated changes colour at once.
func TestRedactionRepaintsTheWholeList(t *testing.T) {
	m := NewModel(stateRows(), true, true, true, false, "")
	before := m.tally()
	if before.Changed() != 0 || before.Verbatim == 0 {
		t.Fatalf("setup: expected everything verbatim, got %+v", before)
	}

	m.Update(Key{Kind: KeyRune, Rune: 'e'})
	after := m.tally()
	if after.Verbatim != 0 {
		t.Errorf("after redaction %d rows are still verbatim, want none", after.Verbatim)
	}
	if after.Changed() != before.Verbatim {
		t.Errorf("changed = %d, want the %d rows that were verbatim",
			after.Changed(), before.Verbatim)
	}
	if after.Migrating() != before.Migrating() {
		t.Errorf("redaction changed how many repositories migrate: %d -> %d",
			before.Migrating(), after.Migrating())
	}
}

// TestWholeLineCarriesTheColour is the point of the change: a cue only at the
// left edge is read once and then lost along the line.
func TestWholeLineCarriesTheColour(t *testing.T) {
	m := NewModel(stateRows(), false, false, false, false, "")
	m.SetSize(96, 24)

	lines := m.renderRow(2, false) // the gated fork
	if len(lines) == 0 {
		t.Fatal("no lines rendered")
	}
	line := lines[0]
	if !strings.HasPrefix(line, ansiCyan) {
		t.Errorf("an available row does not open in cyan: %q", line)
	}
	// The colour must still be in force where the reason is written, not
	// closed after the marker.
	if idx := strings.Index(line, ansiReset); idx != -1 && idx < strings.Index(line, "fork") {
		t.Errorf("the colour is closed before the end of the line: %q", line)
	}
}

// TestLongReasonsWrapInsteadOfBeingCut covers the narrow terminal: a reason cut
// off mid-word is worse than a second line.
func TestLongReasonsWrapInsteadOfBeingCut(t *testing.T) {
	m := NewModel(stateRows(), false, false, false, false, "")
	m.SetSize(44, 30)

	lines := m.renderRow(1, false) // "already on GitHub, left untouched"
	if len(lines) < 2 {
		t.Fatalf("a long reason did not wrap at 44 columns: %q", lines)
	}
	joined := stripANSI(strings.Join(lines, " "))
	if !strings.Contains(joined, "already on GitHub, left untouched") {
		t.Errorf("the reason was lost in wrapping: %q", joined)
	}
	for _, l := range lines {
		if w := len(stripANSI(l)); w > 44 {
			t.Errorf("wrapped line is %d columns wide, want at most 44: %q", w, stripANSI(l))
		}
	}
	// The continuation belongs to its row, so it carries the row's colour.
	if !strings.HasPrefix(lines[1], ansiDim) {
		t.Errorf("the wrapped line does not carry the row colour: %q", lines[1])
	}
}

// TestWideTerminalsDoNotWrap is the other half: the second line appears only
// when it has to.
func TestWideTerminalsDoNotWrap(t *testing.T) {
	m := NewModel(stateRows(), false, false, false, false, "")
	m.SetSize(120, 24)
	for i := range m.rows {
		if lines := m.renderRow(i, false); len(lines) != 1 {
			t.Errorf("row %d took %d lines at 120 columns, want 1", i, len(lines))
		}
	}
}

// TestCursorStaysVisibleWhenRowsWrap guards the scrolling arithmetic, which
// can no longer assume every row is one line tall.
func TestCursorStaysVisibleWhenRowsWrap(t *testing.T) {
	var rows []Row
	for i := 0; i < 30; i++ {
		rows = append(rows, Row{
			Name:    "me/repository-with-a-long-name-" + string(rune('a'+i%26)),
			Blocked: "already on GitHub, left untouched",
		})
	}
	m := NewModel(rows, false, false, false, false, "")
	m.SetSize(48, 20)

	for _, cursor := range []int{0, 7, 15, 29} {
		m.cursor = cursor
		body := m.height - chromeHeight
		lines := m.listLines(m.visible(), body)
		if len(lines) > body {
			t.Errorf("cursor %d: rendered %d lines into a %d-line body", cursor, len(lines), body)
		}
		wanted := stripANSI(m.renderRow(cursor, true)[0])
		var found bool
		for _, l := range lines {
			if stripANSI(l) == wanted {
				found = true
			}
		}
		if !found {
			t.Errorf("cursor %d scrolled off screen", cursor)
		}
	}
}

// footerText returns the tally line without its colours, for reading in tests.
func footerText(m *Model) string {
	return stripANSI(strings.Split(m.footer(), "\r\n")[0])
}

// TestFooterReadsAsArithmetic pins the shape of the count line. Four
// independent figures have to be reconciled by the reader; "30 to migrate ->
// 20 unchanged + 10 with changes" can be checked at a glance.
func TestFooterReadsAsArithmetic(t *testing.T) {
	m := NewModel(stateRows(), true, true, true, false, "")
	m.SetSize(100, 24)
	m.rows[0].Private = !m.rows[0].SourcePrivate // one row modified

	got := footerText(m)
	for _, want := range []string{"to migrate", "->", "unchanged", "+", "visibility"} {
		if !strings.Contains(got, want) {
			t.Errorf("footer %q is missing %q", got, want)
		}
	}
}

// TestFooterCollapsesWhenThereIsNothingToSplit stops the line reading
// "20 unchanged + 0 with changes", which is arithmetic nobody needs.
func TestFooterCollapsesWhenThereIsNothingToSplit(t *testing.T) {
	m := NewModel(stateRows(), true, true, true, false, "")
	m.SetSize(100, 24)

	if got := footerText(m); !strings.Contains(got, "all unchanged") || strings.Contains(got, "+") {
		t.Errorf("with nothing modified the footer is %q, want it collapsed", got)
	}

	m.Update(Key{Kind: KeyRune, Rune: 'e'})
	if got := footerText(m); !strings.Contains(got, "all redacted") || strings.Contains(got, "+") {
		t.Errorf("with everything redacted the footer is %q, want it collapsed", got)
	}
}

// TestFooterOmitsEmptyCategories keeps zeroes off the screen: a count of none
// is not news.
func TestFooterOmitsEmptyCategories(t *testing.T) {
	m := NewModel(stateRows(), true, true, true, false, "")
	m.SetSize(100, 24)

	if got := footerText(m); strings.Contains(got, "could add") {
		t.Errorf("with every gate open the footer still offers %q", got)
	}
}

// TestFooterSaysNothingToMigratePlainly covers the account where everything is
// already on GitHub, which is what a second run looks like.
func TestFooterSaysNothingToMigratePlainly(t *testing.T) {
	rows := []Row{
		{Name: "me/a", Blocked: "already on GitHub, left untouched"},
		{Name: "me/b", Blocked: "already on GitHub, left untouched"},
	}
	m := NewModel(rows, false, false, false, false, "")
	m.SetSize(100, 24)

	got := footerText(m)
	if !strings.Contains(got, "nothing to migrate") {
		t.Errorf("footer = %q, want it to say nothing to migrate", got)
	}
	if strings.Contains(got, "0 to migrate") {
		t.Errorf("footer = %q, want words rather than a zero", got)
	}
}

// TestFooterFitsAnEightyColumnTerminal is the width every terminal still
// defaults to, and the reason the repository total lives in the header.
func TestFooterFitsAnEightyColumnTerminal(t *testing.T) {
	var rows []Row
	for i := 0; i < 40; i++ {
		r := Row{Name: fmt.Sprintf("me/repo-%02d", i), SourcePrivate: true, Private: true}
		switch {
		case i < 10:
			r.Private = false // modified
		case i < 16:
			r.Fork = true // behind a gate
		case i < 20:
			r.Blocked = "already on GitHub, left untouched"
		}
		rows = append(rows, r)
	}
	m := NewModel(rows, false, false, false, false, "")
	m.SetSize(80, 24)

	for _, line := range strings.Split(m.View("gitea.example.com -> github.com/me"), "\r\n") {
		if w := len(stripANSI(line)); w > 80 {
			t.Errorf("line is %d columns wide, want at most 80: %q", w, stripANSI(line))
		}
	}
	if got := footerText(m); !strings.Contains(got, "->") {
		t.Errorf("at 80 columns the breakdown was dropped: %q", got)
	}
}

// TestHeaderCarriesTheTotal checks the standing fact stayed out of the footer.
func TestHeaderCarriesTheTotal(t *testing.T) {
	m := NewModel(stateRows(), false, false, false, false, "")
	m.SetSize(100, 24)

	header := stripANSI(strings.Split(m.View("gitea -> github"), "\r\n")[0])
	if !strings.Contains(header, "4 repositories") {
		t.Errorf("header = %q, want it to carry the total", header)
	}

	one := NewModel(stateRows()[:1], false, false, false, false, "")
	one.SetSize(100, 24)
	if got := stripANSI(strings.Split(one.View("gitea -> github"), "\r\n")[0]); !strings.Contains(got, "1 repository") {
		t.Errorf("header = %q, want the singular", got)
	}
}

// gateRows is an account with one gate that can act and one that cannot: the
// archived repository is already on GitHub, so opening its gate changes
// nothing.
func gateRows() []Row {
	return []Row{
		{Name: "me/plain", SourcePrivate: true, Private: true},
		{Name: "me/fork", Fork: true},
		{Name: "me/attic", Archived: true, Blocked: "already on GitHub, left untouched"},
	}
}

// TestGateTakesTheColourOfTheRowsItGoverns is what ties the toggles at the top
// to the list below them: the control is drawn in the colour of what it
// produces, so what a gate touches needs no explaining.
func TestGateTakesTheColourOfTheRowsItGoverns(t *testing.T) {
	m := NewModel(gateRows(), false, false, false, false, "")
	m.SetSize(100, 24)

	// Closed, with a fork waiting behind it: the fork's row is cyan, so is the
	// gate.
	bar := m.gateBar()
	forks := gateSegment(t, bar, "forks")
	if !strings.Contains(forks, ansiBrightCyan) {
		t.Errorf("a closed gate with rows waiting is not cyan: %q", forks)
	}
	if got := m.rowState(m.rows[1]); got.colour() != ansiCyan {
		t.Errorf("setup: the fork row is %v, want cyan", got)
	}

	// Opened, the fork joins the run and both turn green.
	m.Update(Key{Kind: KeyRune, Rune: '2'})
	forks = gateSegment(t, m.gateBar(), "forks")
	if !strings.Contains(forks, ansiBrightGreen) {
		t.Errorf("an open gate is not green: %q", forks)
	}
	if got := m.rowState(m.rows[1]); got.colour() != ansiGreen {
		t.Errorf("the fork row is %v after opening its gate, want green", got)
	}
}

// TestAGateThatCanDeliverNothingIsGreyedOut stops the screen advertising a
// count it cannot act on: every repository behind this gate is already on
// GitHub, so pressing it changes nothing whichever way it is set.
func TestAGateThatCanDeliverNothingIsGreyedOut(t *testing.T) {
	m := NewModel(gateRows(), false, false, false, false, "")
	m.SetSize(100, 24)

	archived := gateSegment(t, m.gateBar(), "archived")
	if strings.Contains(archived, ansiBrightCyan) || strings.Contains(archived, ansiBrightGreen) {
		t.Errorf("a gate with nothing to give is drawn as though it had: %q", archived)
	}
	if !strings.Contains(archived, ansiDim) {
		t.Errorf("a gate with nothing to give is not greyed out: %q", archived)
	}

	// Opening it must not change the tally, which is the fact the grey is
	// promising.
	before := m.tally()
	m.Update(Key{Kind: KeyRune, Rune: '3'})
	if after := m.tally(); after != before {
		t.Errorf("opening a spent gate changed the tally: %+v -> %+v", before, after)
	}
}

// TestRedactionToggleIsDrawnInTheColourItProduces covers the other control on
// the bar.
func TestRedactionToggleIsDrawnInTheColourItProduces(t *testing.T) {
	m := NewModel(gateRows(), false, false, false, false, "")
	m.SetSize(100, 24)

	if bar := m.redactBar(); strings.Contains(bar, ansiBrightRedacted) {
		t.Errorf("redaction is off but its toggle is amber: %q", bar)
	}
	m.Update(Key{Kind: KeyRune, Rune: 'e'})
	if bar := m.redactBar(); !strings.Contains(bar, ansiBrightRedacted) {
		t.Errorf("redaction is on but its toggle is not amber: %q", bar)
	}
	if got := m.rowState(m.rows[0]); got.colour() != ansiRedacted {
		t.Errorf("with redaction on a row is %v, want the redaction colour", got)
	}
}

// gateSegment pulls one labelled toggle out of the bar, colours and all.
func gateSegment(t *testing.T, bar, label string) string {
	t.Helper()
	idx := strings.Index(bar, label)
	if idx < 0 {
		t.Fatalf("the bar does not mention %q: %q", label, stripANSI(bar))
	}
	start := strings.LastIndex(bar[:idx], "\x1b[")
	for start > 0 && !strings.HasPrefix(bar[start:], ansiBrightCyan) &&
		!strings.HasPrefix(bar[start:], ansiBrightGreen) &&
		!strings.HasPrefix(bar[start:], ansiDim) {
		start = strings.LastIndex(bar[:start], "\x1b[")
	}
	end := strings.Index(bar[idx:], ansiReset)
	if end < 0 {
		end = len(bar) - idx
	}
	return bar[start : idx+end]
}
