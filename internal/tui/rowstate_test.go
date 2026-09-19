package tui

import (
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
