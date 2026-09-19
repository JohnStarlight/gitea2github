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

// TestModifiedWinsOverVerbatim pins the rule that a row whose contents change
// is marked as such, whichever way the change was asked for.
func TestModifiedWinsOverVerbatim(t *testing.T) {
	m := NewModel(stateRows(), false, false, false, false, "")
	if got := m.rowState(m.rows[0]); got != stateVerbatim {
		t.Fatalf("an untouched row is %v, want stateVerbatim", got)
	}

	// A visibility flip is a change.
	m.rows[0].Private = false
	if got := m.rowState(m.rows[0]); got != stateModified {
		t.Errorf("a row flipped to public is %v, want stateModified", got)
	}
	m.rows[0].Private = true

	// So is rewriting the history, and it applies to every selected row.
	m.redact = true
	if got := m.rowState(m.rows[0]); got != stateModified {
		t.Errorf("with redaction on, a row is %v, want stateModified", got)
	}
}

// TestRedactionRepaintsTheWholeList checks the feedback that makes pressing e
// impossible to miss: every repository being migrated changes colour at once.
func TestRedactionRepaintsTheWholeList(t *testing.T) {
	m := NewModel(stateRows(), true, true, true, false, "")
	before := m.tally()
	if before.Modified != 0 || before.Verbatim == 0 {
		t.Fatalf("setup: expected everything verbatim, got %+v", before)
	}

	m.Update(Key{Kind: KeyRune, Rune: 'e'})
	after := m.tally()
	if after.Verbatim != 0 {
		t.Errorf("after redaction %d rows are still verbatim, want none", after.Verbatim)
	}
	if after.Modified != before.Verbatim {
		t.Errorf("modified = %d, want the %d rows that were verbatim",
			after.Modified, before.Verbatim)
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
	for _, want := range []string{"to migrate", "->", "unchanged", "+", "with changes"} {
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
	if got := footerText(m); !strings.Contains(got, "all with changes") || strings.Contains(got, "+") {
		t.Errorf("with everything modified the footer is %q, want it collapsed", got)
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
