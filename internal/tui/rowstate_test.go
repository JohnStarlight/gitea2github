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

	m.rows[0].Redact = true
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

// TestRedactionRepaintsTheWholeList checks the feedback that makes pressing E
// impossible to miss: every repository being migrated changes colour at once.
func TestRedactionRepaintsTheWholeList(t *testing.T) {
	m := NewModel(stateRows(), true, true, true, false, "")
	before := m.tally()
	if before.Changed() != 0 || before.Verbatim == 0 {
		t.Fatalf("setup: expected everything verbatim, got %+v", before)
	}

	m.Update(Key{Kind: KeyRune, Rune: 'E'})
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
		if w := visibleWidth(l); w > 44 {
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

	m.Update(Key{Kind: KeyRune, Rune: 'E'})
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
		if w := visibleWidth(line); w > 80 {
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

	if bar := m.redactBar(); strings.Contains(bar, ansiAlarm) {
		t.Errorf("redaction is off but its toggle is already alarming: %q", bar)
	}
	m.Update(Key{Kind: KeyRune, Rune: 'E'})
	if bar := m.redactBar(); !strings.Contains(bar, ansiAlarm) {
		t.Errorf("redaction is on but its toggle is not drawn in alarm red: %q", bar)
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

// TestRedactionDoesNotSpreadToRowsAddedLater is the reason redaction moved
// onto the row. With one switch over the whole run, opening a gate or checking
// one more box silently rewrote the history of whatever came with it -- a
// side effect nobody asked for, on the one operation that cannot be undone by
// unchecking a box afterwards.
func TestRedactionDoesNotSpreadToRowsAddedLater(t *testing.T) {
	rows := []Row{
		{Name: "me/plain", SourcePrivate: true, Private: true},
		{Name: "me/fork", Fork: true, SourcePrivate: true, Private: true},
	}
	m := NewModel(rows, false, false, false, false, "")

	// Redact the one repository currently in the run.
	m.Update(Key{Kind: KeyRune, Rune: 'e'})
	if got := m.RedactedRepos(); len(got) != 1 || !got["me/plain"] {
		t.Fatalf("e redacted %v, want just me/plain", got)
	}

	// Now bring the fork in. It must arrive unredacted.
	m.Update(Key{Kind: KeyRune, Rune: '2'})
	got := m.RedactedRepos()
	if got["me/fork"] {
		t.Error("opening a gate redacted the repository it brought in")
	}
	if !got["me/plain"] {
		t.Error("opening a gate dropped the redaction that had been asked for")
	}
}

// TestRedactAllCoversTheSelectionAndUndoesItself keeps the bulk key usable in
// both directions: the one setting that rewrites commits must be as easy to
// take back as it is to apply.
func TestRedactAllCoversTheSelectionAndUndoesItself(t *testing.T) {
	m := NewModel(stateRows(), true, true, true, false, "")

	m.Update(Key{Kind: KeyRune, Rune: 'E'})
	first := len(m.RedactedRepos())
	if first == 0 {
		t.Fatal("E redacted nothing")
	}
	if first != m.tally().Migrating() {
		t.Errorf("E redacted %d of %d repositories in the run", first, m.tally().Migrating())
	}

	m.Update(Key{Kind: KeyRune, Rune: 'E'})
	if got := m.RedactedRepos(); got != nil {
		t.Errorf("pressing E twice left %v redacted", got)
	}
}

// TestRedactedReposIsNilRatherThanEmpty guards the distinction the migrator
// draws: a nil map there means every repository, so an empty one would redact
// the whole run when nothing was asked for.
func TestRedactedReposIsNilRatherThanEmpty(t *testing.T) {
	m := NewModel(stateRows(), false, false, false, false, "")
	if got := m.RedactedRepos(); got != nil {
		t.Errorf("RedactedRepos = %v with nothing redacted, want nil", got)
	}
	if m.Redact() {
		t.Error("Redact reports true with nothing redacted")
	}
}

// TestRedactionIgnoresRowsThatAreNotComing stops a repository that is not in
// the run from being counted, which would build a Mapper for nothing.
func TestRedactionIgnoresRowsThatAreNotComing(t *testing.T) {
	m := NewModel(stateRows(), true, true, true, true, "")
	if got := m.RedactedRepos(); got["me/attic"] {
		t.Error("a repository already on GitHub was listed for redaction")
	}

	// Unchecking a row takes it out of the redaction list too.
	m.cursor = 0
	name := m.rows[0].Name
	m.Update(Key{Kind: KeySpace})
	if got := m.RedactedRepos(); got[name] {
		t.Errorf("%s is still listed for redaction after being unchecked", name)
	}
}

// TestFooterNamesBothChangesRatherThanSayingBoth is the wording fix: "both"
// made the reader work out what the two were, and only by toggling one off.
func TestFooterNamesBothChangesRatherThanSayingBoth(t *testing.T) {
	m := NewModel(stateRows(), true, true, true, false, "")
	m.SetSize(120, 24)
	m.Update(Key{Kind: KeyRune, Rune: 'E'}) // redact everything
	m.cursor = 0
	m.Update(Key{Kind: KeyRune, Rune: 'v'}) // and flip one

	got := footerText(m)
	if strings.Contains(got, " both") && !strings.Contains(got, "visibility & redacted") {
		t.Errorf("footer %q still says \"both\" without naming the changes", got)
	}
	if !strings.Contains(got, "visibility & redacted") {
		t.Errorf("footer %q does not name the two changes", got)
	}
}

// threeWayModel has one repository of each kind of change, which is the case
// the count line has the least room for.
func threeWayModel(width int) *Model {
	rows := []Row{
		{Name: "me/redacted-one", SourcePrivate: true, Private: true},
		{Name: "me/flipped-one", SourcePrivate: true, Private: true},
		{Name: "me/both-of-them", SourcePrivate: true, Private: true},
		{Name: "me/untouched-one", SourcePrivate: true, Private: true},
		{Name: "me/behind-a-gate", Fork: true, SourcePrivate: true, Private: true},
		{Name: "me/already-there", Blocked: "already on GitHub, left untouched"},
	}
	m := NewModel(rows, false, false, false, false, "")
	m.SetSize(width, 24)
	m.cursor = 0
	m.Update(Key{Kind: KeyRune, Rune: 'e'})
	m.cursor = 1
	m.Update(Key{Kind: KeyRune, Rune: 'v'})
	m.cursor = 2
	m.Update(Key{Kind: KeyRune, Rune: 'v'})
	m.Update(Key{Kind: KeyRune, Rune: 'e'})
	return m
}

// TestBreakdownSurvivesAnEightyColumnTerminal is the width that matters. With
// all three kinds of change present the fully spelled-out line does not fit,
// and giving up the breakdown there was the wrong thing to give up: "3 with
// changes" answers less than the three counts it replaced.
func TestBreakdownSurvivesAnEightyColumnTerminal(t *testing.T) {
	got := footerText(threeWayModel(80))
	if strings.Contains(got, "with changes") {
		t.Errorf("at 80 columns the breakdown was dropped: %q", got)
	}
	for _, want := range []string{"unchanged", "visibility", "redacted", "both changes"} {
		if !strings.Contains(got, want) {
			t.Errorf("footer %q is missing %q", got, want)
		}
	}
}

// TestNarrowingGivesUpTheTailBeforeTheBreakdown pins the order. "could add"
// and "not moving" restate what the cyan and grey rows already say; the
// breakdown says something only this line can.
func TestNarrowingGivesUpTheTailBeforeTheBreakdown(t *testing.T) {
	wide := footerText(threeWayModel(120))
	if !strings.Contains(wide, "could add") {
		t.Fatalf("setup: the wide line should carry the tail: %q", wide)
	}

	got := footerText(threeWayModel(92))
	if strings.Contains(got, "could add") {
		t.Errorf("at 92 columns the tail was kept: %q", got)
	}
	if !strings.Contains(got, "visibility & redacted") {
		t.Errorf("at 92 columns the breakdown was given up before the tail: %q", got)
	}
}

// TestTheCountsDoNotDependOnTheTerminalWidth guards against the arithmetic
// changing as the window is resized, which would make the line untrustworthy.
func TestTheCountsDoNotDependOnTheTerminalWidth(t *testing.T) {
	var first tally
	for i, w := range []int{120, 100, 92, 80, 64, 40} {
		got := threeWayModel(w).tally()
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Errorf("at %d columns the tally is %+v, want %+v", w, got, first)
		}
	}
}

// TestEveryLineFitsAtCommonWidths sweeps the whole screen rather than the
// count line alone, since the breakdown is only useful if nothing else spills.
func TestEveryLineFitsAtCommonWidths(t *testing.T) {
	for _, w := range []int{120, 100, 92, 80, 72, 64, 48, 40} {
		m := threeWayModel(w)
		for _, line := range strings.Split(m.View("gitea.example.com -> github.com/me"), "\r\n") {
			if got := visibleWidth(line); got > w {
				t.Errorf("at %d columns a line is %d wide: %q", w, got, stripANSI(line))
			}
		}
	}
}

// TestWideTerminalsAreUsedRatherThanWasted is the complaint this layout
// answers: the columns were fixed, so a wide window showed names truncated
// with an ellipsis and descriptions wrapped onto a second line while most of
// the screen sat empty to the right.
func TestWideTerminalsAreUsedRatherThanWasted(t *testing.T) {
	rows := []Row{
		{Name: "JohnStarlight/ascii-art-web-stylize", SourcePrivate: true, Private: false},
		{Name: "zone01/quadchecker-team-project", SourcePrivate: true, Private: false},
	}
	for _, w := range []int{100, 120, 160, 200} {
		m := NewModel(rows, true, true, true, true, "")
		m.SetSize(w, 30)
		for i, r := range m.rows {
			lines := m.renderRow(i, false)
			if len(lines) != 1 {
				t.Errorf("at %d columns row %d wrapped onto %d lines", w, i, len(lines))
			}
			if got := stripANSI(strings.Join(lines, " ")); !strings.Contains(got, r.Name) {
				t.Errorf("at %d columns the name was truncated with room to spare: %q", w, got)
			}
		}
	}
}

// TestNarrowTerminalsStillWrap is the other half: the columns shrink rather
// than spilling off the edge.
func TestNarrowTerminalsStillWrap(t *testing.T) {
	rows := []Row{{Name: "me/a-repository-with-a-long-name", Blocked: "already on GitHub, left untouched"}}
	m := NewModel(rows, false, false, false, false, "")
	m.SetSize(46, 20)

	lines := m.renderRow(0, false)
	if len(lines) < 2 {
		t.Errorf("at 46 columns the row did not wrap: %q", lines)
	}
	for _, l := range lines {
		if got := visibleWidth(l); got > 46 {
			t.Errorf("a line is %d columns wide at 46: %q", got, stripANSI(l))
		}
	}
}

// TestOneLongNameDoesNotEatTheScreen keeps a single unusual repository from
// pushing every description out of view.
func TestOneLongNameDoesNotEatTheScreen(t *testing.T) {
	rows := []Row{
		{Name: strings.Repeat("very-long-", 12) + "name", SourcePrivate: true, Private: true},
		{Name: "me/short", SourcePrivate: true, Private: true},
	}
	m := NewModel(rows, false, false, false, false, "")
	m.SetSize(200, 20)

	nameW, _ := m.columns()
	if nameW > 60 {
		t.Errorf("the name column grew to %d columns for one long name", nameW)
	}
	if got := stripANSI(strings.Join(m.renderRow(1, false), " ")); !strings.Contains(got, "create") {
		t.Errorf("the second row lost its description: %q", got)
	}
}

// TestTheRedactionWarningAppearsOnlyWhenItApplies keeps the loudest line on
// the screen from becoming wallpaper. A warning that is always up is read
// once and then stops being read at all.
func TestTheRedactionWarningAppearsOnlyWhenItApplies(t *testing.T) {
	m := NewModel(stateRows(), true, true, true, false, "")
	m.SetSize(96, 24)

	if got := stripANSI(m.redactWarning()); got != "" {
		t.Errorf("the warning is up with nothing being redacted: %q", got)
	}
	m.Update(Key{Kind: KeyRune, Rune: 'E'})
	if m.redactWarning() == "" {
		t.Fatal("nothing warns about a run that rewrites history")
	}
}

// TestTheWarningSaysBothThingsThatMatter pins the two facts somebody needs
// before pressing e: that there is no way back, and what kind of work this is
// suitable for.
func TestTheWarningSaysBothThingsThatMatter(t *testing.T) {
	m := NewModel(stateRows(), true, true, true, true, "")
	m.SetSize(96, 24)

	got := stripANSI(m.redactWarning())
	if !strings.Contains(got, "CANNOT BE UNDONE") {
		t.Errorf("the warning does not say it cannot be undone: %q", got)
	}
	if !strings.Contains(got, "FINISHED") {
		t.Errorf("the warning does not say what it is for: %q", got)
	}
	if got != strings.ToUpper(got) {
		t.Errorf("the warning is not in capitals: %q", got)
	}
	if !strings.Contains(m.redactWarning(), ansiAlarm) {
		t.Error("the warning is not drawn in alarm red")
	}
}

// TestTheWarningShortensRatherThanBeingCut covers the narrow terminal. Half a
// sentence in the middle of a warning reads as a glitch.
func TestTheWarningShortensRatherThanBeingCut(t *testing.T) {
	for _, w := range []int{100, 80, 60, 44, 30} {
		m := NewModel(stateRows(), true, true, true, true, "")
		m.SetSize(w, 24)

		got := m.redactWarning()
		if visibleWidth(got) > w {
			t.Errorf("at %d columns the warning is %d wide: %q", w, visibleWidth(got), stripANSI(got))
		}
		if !strings.Contains(stripANSI(got), "CANNOT BE UNDONE") {
			t.Errorf("at %d columns the warning lost its point: %q", w, stripANSI(got))
		}
	}
}

// TestTheWarningTakesItsOwnLineFromTheList checks the chrome grows with it,
// rather than the warning covering a repository.
func TestTheWarningTakesItsOwnLineFromTheList(t *testing.T) {
	m := NewModel(stateRows(), true, true, true, false, "")
	m.SetSize(96, 24)
	quiet := m.chrome()

	m.Update(Key{Kind: KeyRune, Rune: 'E'})
	if loud := m.chrome(); loud != quiet+1 {
		t.Errorf("the chrome is %d lines with the warning up and %d without", loud, quiet)
	}

	for _, line := range strings.Split(m.View("gitea -> github"), "\r\n") {
		if visibleWidth(line) > 96 {
			t.Errorf("a line spills at 96 columns: %q", stripANSI(line))
		}
	}
}

// TestTheKeptAddressShortensRatherThanBeingCut covers the same rule on the
// line above it, which was losing its last word.
func TestTheKeptAddressShortensRatherThanBeingCut(t *testing.T) {
	for _, w := range []int{120, 96, 84, 70, 50} {
		m := NewModel(stateRows(), true, true, true, true, "")
		m.SetSize(w, 24)

		bar := m.redactBar()
		if visibleWidth(bar) > w {
			t.Errorf("at %d columns the bar is %d wide: %q", w, visibleWidth(bar), stripANSI(bar))
		}
		got := stripANSI(bar)
		if strings.HasSuffix(got, "addres") || strings.HasSuffix(got, "link") {
			t.Errorf("at %d columns the hint was cut mid-word: %q", w, got)
		}
	}
}
