package tui

import "strings"

// Update applies one keypress and returns whether anything changed.
//
// Split from the drawing code so that the entire interaction is testable
// without a terminal: a test feeds keys and reads the model's fields.
func (m *Model) Update(k Key) {
	m.note = ""

	// The two text-entry modes swallow most keys, so they are handled before
	// the main key map rather than inside it.
	if m.editingEmail {
		m.updateEmail(k)
		return
	}
	if m.searching {
		m.updateSearch(k)
		return
	}

	switch k.Kind {
	case KeyCtrlC, KeyEscape:
		m.cancelled, m.done = true, true
		return
	case KeyEnter:
		m.confirm()
		return
	case KeyUp:
		m.move(-1)
		return
	case KeyDown:
		m.move(1)
		return
	case KeyPageUp:
		m.move(-m.pageSize())
		return
	case KeyPageDown:
		m.move(m.pageSize())
		return
	case KeyHome:
		m.cursor = 0
		return
	case KeyEnd:
		m.cursor = len(m.visible()) - 1
		m.clampCursor()
		return
	case KeySpace:
		m.toggleCurrent()
		return
	}

	if k.Kind != KeyRune {
		return
	}

	switch k.Rune {
	case 'q':
		m.cancelled, m.done = true, true
	case 'k':
		m.move(-1)
	case 'j':
		m.move(1)
	case 'v':
		m.flipCurrent()
	case 'a':
		m.setAll(true)
	case 'n':
		m.setAll(false)
	case '1':
		m.groups = !m.groups
		m.setCategory(func(r Row) bool { return r.Foreign }, m.groups)
	case '2':
		m.forks = !m.forks
		m.setCategory(func(r Row) bool { return r.Fork }, m.forks)
	case '3':
		m.archived = !m.archived
		m.setCategory(func(r Row) bool { return r.Archived }, m.archived)
	case 'e':
		m.redactCurrent()
	case 'E':
		// Everything on screen, the way a is to space. Rewriting history is
		// worth asking for explicitly rather than having it follow whatever
		// else the selection happens to pick up.
		m.redactAll()
	case 'm':
		if m.Redact() {
			m.editingEmail = true
		} else {
			m.note = "redact a repository with e before choosing an address to keep"
		}
	case '/':
		m.searching = true
	}
}

// updateSearch handles keys while the search box has focus.
func (m *Model) updateSearch(k Key) {
	switch k.Kind {
	case KeyEnter:
		m.searching = false
	case KeyEscape, KeyCtrlC:
		// Abandoning a search restores the full list rather than leaving it
		// filtered by a query the user just rejected.
		m.searching, m.query = false, ""
		m.clampCursor()
	case KeyBackspace:
		if m.query != "" {
			_, size := lastRune(m.query)
			m.query = m.query[:len(m.query)-size]
			m.clampCursor()
		}
	case KeySpace:
		m.query += " "
		m.clampCursor()
	case KeyRune:
		m.query += string(k.Rune)
		m.clampCursor()
	}
}

// updateEmail handles keys while the keep-address box has focus.
func (m *Model) updateEmail(k Key) {
	switch k.Kind {
	case KeyEnter:
		m.editingEmail = false
	case KeyEscape, KeyCtrlC:
		m.editingEmail = false
	case KeyBackspace:
		if m.keepEmail != "" {
			_, size := lastRune(m.keepEmail)
			m.keepEmail = m.keepEmail[:len(m.keepEmail)-size]
		}
	case KeyRune:
		m.keepEmail += string(k.Rune)
	}
}

// confirm ends the screen, refusing an empty selection.
//
// Enter on nothing selected is far more likely to be a mistake than a request
// to do nothing, and the user has q for that.
func (m *Model) confirm() {
	if m.tally().Migrating() == 0 {
		m.note = "nothing selected -- press a to select all, or q to quit"
		return
	}
	m.done = true
}

// move walks the cursor by delta rows, stopping at the ends.
//
// Deliberately not wrapping: a list that jumps from bottom to top under a held
// arrow key makes it easy to toggle a repository you never saw.
func (m *Model) move(delta int) {
	m.cursor += delta
	m.clampCursor()
}

func (m *Model) clampCursor() {
	n := len(m.visible())
	if n == 0 {
		m.cursor = 0
		return
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= n {
		m.cursor = n - 1
	}
}

// currentRow returns the index into m.rows under the cursor, or -1.
func (m *Model) currentRow() int {
	vis := m.visible()
	if m.cursor < 0 || m.cursor >= len(vis) {
		return -1
	}
	return vis[m.cursor]
}

// toggleCurrent checks or unchecks the row under the cursor.
func (m *Model) toggleCurrent() {
	i := m.currentRow()
	if i < 0 {
		return
	}
	r := m.rows[i]
	if r.Blocked != "" {
		m.note = r.Name + ": " + r.Blocked
		return
	}
	if !r.eligible(m.groups, m.forks, m.archived) {
		m.note = r.Name + ": " + gateHint(r, m.groups, m.forks, m.archived)
		return
	}
	m.rows[i].Include = !m.rows[i].Include
}

// redactCurrent turns history rewriting on or off for the row under the
// cursor.
func (m *Model) redactCurrent() {
	i := m.currentRow()
	if i < 0 {
		return
	}
	r := m.rows[i]
	if r.Blocked != "" {
		m.note = r.Name + ": " + r.Blocked
		return
	}
	if !r.eligible(m.groups, m.forks, m.archived) {
		m.note = r.Name + ": " + gateHint(r, m.groups, m.forks, m.archived)
		return
	}
	m.rows[i].Redact = !m.rows[i].Redact
	m.forgetAddressIfUnused()
}

// redactAll turns history rewriting on for everything the search is showing,
// or off again if it is already on for all of them.
//
// Toggling on the whole set rather than only ever switching it on means the
// key can undo itself, which matters for the one setting that rewrites commits.
func (m *Model) redactAll() {
	var eligible []int
	allOn := true
	for _, i := range m.visible() {
		if !m.rows[i].eligible(m.groups, m.forks, m.archived) {
			continue
		}
		eligible = append(eligible, i)
		if !m.rows[i].Redact {
			allOn = false
		}
	}
	for _, i := range eligible {
		m.rows[i].Redact = !allOn
	}
	m.forgetAddressIfUnused()
}

// forgetAddressIfUnused drops the kept address once nothing is being redacted.
//
// It only means something while some history is being rewritten, and leaving
// it set would resurrect it the next time redaction was switched on, which is
// not something the user asked for.
func (m *Model) forgetAddressIfUnused() {
	if !m.Redact() {
		m.keepEmail = ""
	}
}

// flipCurrent inverts the destination visibility of the row under the cursor.
func (m *Model) flipCurrent() {
	i := m.currentRow()
	if i < 0 {
		return
	}
	if m.rows[i].Blocked != "" {
		m.note = m.rows[i].Name + ": " + m.rows[i].Blocked
		return
	}
	m.rows[i].Private = !m.rows[i].Private
}

// setAll checks or unchecks every row the gates admit.
//
// Scoped to the rows the search is currently showing, so that "/lem" then "n"
// deselects what the user is looking at rather than the whole account.
func (m *Model) setAll(on bool) {
	for _, i := range m.visible() {
		if m.rows[i].eligible(m.groups, m.forks, m.archived) {
			m.rows[i].Include = on
		}
	}
}

// gateHint explains which gate is holding a row back, naming the key that
// opens it.
func gateHint(r Row, groups, forks, archived bool) string {
	var reasons []string
	if r.Foreign && !groups {
		reasons = append(reasons, "a group project (press 1)")
	}
	if r.Fork && !forks {
		reasons = append(reasons, "a fork (press 2)")
	}
	if r.Archived && !archived {
		reasons = append(reasons, "archived (press 3)")
	}
	return strings.Join(reasons, ", ")
}

// lastRune returns the final rune of s and its width in bytes, so that
// backspace removes one character rather than one byte.
func lastRune(s string) (rune, int) {
	runes := []rune(s)
	if len(runes) == 0 {
		return 0, 0
	}
	last := runes[len(runes)-1]
	return last, len(string(last))
}

// pageSize is how far PageUp and PageDown jump.
func (m *Model) pageSize() int {
	n := m.height - m.chrome()
	if n < 1 {
		return 1
	}
	return n
}
