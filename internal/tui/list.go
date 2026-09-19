package tui

import "strings"

// list is the cursor and the search box, which both screens need and neither
// needs differently.
//
// Kept in one place because the two copies had already begun to drift: the
// same function, written twice, had lost a comment on one side. A comment is
// harmless; the next divergence would not have been, and nothing would have
// pointed it out.
//
// The count of visible rows is passed in rather than held here, because what
// counts as visible is the one part of this that genuinely differs -- one
// screen filters repositories by name, the other working copies by path.
type list struct {
	cursor    int
	query     string
	searching bool
}

// move walks the cursor by delta, stopping at the ends.
//
// Deliberately not wrapping: a list that jumps from bottom to top under a held
// arrow key makes it easy to act on a row you never saw.
func (l *list) move(delta, count int) {
	l.cursor += delta
	l.clamp(count)
}

// clamp keeps the cursor inside a list of count rows.
func (l *list) clamp(count int) {
	switch {
	case count == 0:
		l.cursor = 0
	case l.cursor < 0:
		l.cursor = 0
	case l.cursor >= count:
		l.cursor = count - 1
	}
}

// searchKey handles one keypress while the search box has focus.
//
// Typing a search is a search, not a selection: filtering the list never
// changes what is included, so a half-typed query cannot silently drop a row
// from the run.
func (l *list) searchKey(k Key, count int) {
	switch k.Kind {
	case KeyEnter:
		l.searching = false
	case KeyEscape, KeyCtrlC:
		// Abandoning a search restores the full list rather than leaving it
		// filtered by a query the user has just rejected.
		l.searching, l.query = false, ""
		l.clamp(count)
	case KeyBackspace:
		if l.query != "" {
			_, size := lastRune(l.query)
			l.query = l.query[:len(l.query)-size]
			l.clamp(count)
		}
	case KeySpace:
		l.query += " "
		l.clamp(count)
	case KeyRune:
		l.query += string(k.Rune)
		l.clamp(count)
	}
}

// matches reports whether a row's label survives the current query.
func (l *list) matches(label string) bool {
	q := strings.ToLower(strings.TrimSpace(l.query))
	return q == "" || strings.Contains(strings.ToLower(label), q)
}
