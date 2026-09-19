package tui

import (
	"fmt"
	"strings"
)

// chromeHeight is how many lines the header, the filter bar and the footer
// take, leaving the rest of the terminal for the list.
const chromeHeight = 10

// View renders the whole screen.
//
// Every frame is drawn in full rather than diffed against the last one. The
// list is at most a screenful and repaints are driven by keystrokes, so the
// cost is invisible, and it removes an entire class of bug where the screen
// and the model disagree about what is already on the terminal.
func (m *Model) View(header string) string {
	var b strings.Builder

	b.WriteString(m.headerLine(header) + "\r\n")
	b.WriteString(m.gateBar() + "\r\n")
	b.WriteString(m.redactBar() + "\r\n\r\n")

	body := m.height - chromeHeight
	if body < 3 {
		body = 3
	}

	written := 0
	if vis := m.visible(); len(vis) == 0 {
		b.WriteString(ansiDim + "  no repository matches " + quote(m.query) + ansiReset + "\r\n")
		written = 1
	} else {
		for _, line := range m.listLines(vis, body) {
			b.WriteString(line + "\r\n")
			written++
		}
	}
	// Pad so the footer keeps its place instead of walking up the screen as
	// the list shortens.
	for ; written < body; written++ {
		b.WriteString("\r\n")
	}

	b.WriteString("\r\n" + m.footer() + "\r\n")
	return b.String()
}

// listLines renders as many rows as fit in body lines, keeping the cursor
// visible.
//
// Rows are not all one line tall any more, so the window cannot be found by
// arithmetic on the cursor index. It is found by walking back from the cursor,
// adding rows until the next one would not fit, which keeps the cursor on
// screen whichever direction it was moving and never splits a row across the
// bottom edge.
func (m *Model) listLines(vis []int, body int) []string {
	heights := make([]int, len(vis))
	rendered := make([][]string, len(vis))
	for pos, row := range vis {
		rendered[pos] = m.renderRow(row, pos == m.cursor)
		heights[pos] = len(rendered[pos])
	}

	start, used := m.cursor, heights[m.cursor]
	for start > 0 && used+heights[start-1] <= body {
		start--
		used += heights[start]
	}

	var out []string
	remaining := body
	for pos := start; pos < len(vis); pos++ {
		if heights[pos] > remaining {
			break
		}
		out = append(out, rendered[pos]...)
		remaining -= heights[pos]
	}
	return out
}

// gateBar renders the three category toggles and the redaction toggle.
func (m *Model) gateBar() string {
	var parts []string
	add := func(key, label string, on bool, pred func(Row) bool) {
		total, actionable := 0, 0
		for _, r := range m.rows {
			if !pred(r) {
				continue
			}
			total++
			if r.Blocked == "" {
				actionable++
			}
		}
		if total == 0 {
			// A gate for a category the account does not contain is noise, and
			// noise is what trains people to stop reading the screen.
			return
		}

		// The toggle takes the colour of the rows it governs: cyan while they
		// wait behind it, green once they are coming along. A gate whose
		// repositories are every one of them already on GitHub can deliver
		// nothing whichever way it is set, so it is greyed out rather than
		// left advertising a count it cannot act on.
		colour := ansiBrightCyan
		switch {
		case actionable == 0:
			colour = ansiDim
		case on:
			colour = ansiBrightGreen
		}
		parts = append(parts, fmt.Sprintf("%s %s%s %s (%d)%s",
			dim(key), colour, checkbox(on), label, total, ansiReset))
	}
	add("1", "group projects", m.groups, func(r Row) bool { return r.Foreign })
	add("2", "forks", m.forks, func(r Row) bool { return r.Fork })
	add("3", "archived", m.archived, func(r Row) bool { return r.Archived })

	if len(parts) == 0 {
		return dim("  every repository here is your own, and none is a fork or archived")
	}
	// truncateANSI, not truncate: the escape sequences in the checkboxes take
	// no columns on screen, and counting them would cut the bar short of the
	// terminal width.
	return truncateANSI("  "+strings.Join(parts, "   "), m.width)
}

// redactBar renders the history-rewriting toggle and the address kept linked
// to its GitHub account.
//
// On its own line rather than beside the category gates because the address is
// long, and an address silently truncated off the edge of the screen is how
// somebody ends up publishing the one they meant to keep private.
func (m *Model) redactBar() string {
	redacted, selected := 0, 0
	for _, r := range m.rows {
		if !r.eligible(m.groups, m.forks, m.archived) || !r.Include {
			continue
		}
		selected++
		if r.Redact {
			redacted++
		}
	}

	// A count rather than a checkbox, because redaction is no longer one
	// switch over the whole run: the useful fact is how much of the selection
	// it currently reaches.
	if redacted == 0 {
		return truncateANSI(fmt.Sprintf("  %s redact emails on a repository   %s all",
			dim("e"), dim("E")), m.width)
	}

	scope := fmt.Sprintf("%d of %d", redacted, selected)
	if redacted == selected {
		scope = fmt.Sprintf("all %d", selected)
	}
	line := fmt.Sprintf("  %s %sredacting %s%s   %s all",
		dim("e"), ansiBrightRedacted, scope, ansiReset, dim("E"))

	if m.editingEmail {
		return truncateANSI(fmt.Sprintf("%s   %s keep: %s%s%s",
			line, dim("m"), ansiReverse, m.keepEmail+" ", ansiReset), m.width)
	}
	keep := m.keepEmail
	if keep == "" {
		keep = dim("none -- press m to keep your own address linked")
	}
	return truncateANSI(fmt.Sprintf("%s   %s keep: %s", line, dim("m"), keep), m.width)
}

// renderRow draws one repository, as one line or two.
//
// Returns lines rather than a string because the reason a row is held back --
// "already on GitHub, left untouched" -- is longer than the space left for it
// on a narrow terminal, and a reason cut off mid-word is worse than a second
// line. The layout is computed from the terminal width rather than fixed, so
// the wrap happens only when it has to.
func (m *Model) renderRow(i int, cursor bool) []string {
	r := m.rows[i]
	st := m.rowState(r)
	colour := st.colour()

	marker := " "
	if cursor {
		marker = ">"
	}

	nameW, visW := m.columns()
	visibility := ""
	if st == stateVerbatim || st.changed() {
		visibility = m.visibilityWord(r)
	}

	head := fmt.Sprintf(" %s %s %s", marker, st.symbol(), pad(r.Name, nameW))
	if visW > 0 {
		head += " " + pad(visibility, visW)
	}

	detail := m.detailFor(r, st)
	room := m.width - visibleWidth(head) - 1
	if detail == "" {
		return []string{colour + strings.TrimRight(head, " ") + ansiReset}
	}
	if room >= visibleWidth(detail) {
		return []string{colour + head + " " + detail + ansiReset}
	}

	// Too long for the rest of the line: carry it onto a second one, indented
	// under the name so that it reads as belonging to the row above it.
	const indent = "      "
	wrapped := wrapText(detail, maxInt(8, m.width-len(indent)))
	lines := []string{colour + strings.TrimRight(head, " ") + ansiReset}
	for _, w := range wrapped {
		lines = append(lines, colour+indent+w+ansiReset)
	}
	return lines
}

// columns works out how wide the name and visibility columns may be.
//
// Measured from the content and the terminal rather than fixed. A fixed width
// wastes a wide window -- names truncated with an ellipsis and descriptions
// wrapping onto a second line while ninety columns sit empty to the right --
// and starves a narrow one.
func (m *Model) columns() (nameW, visW int) {
	// The visibility column only exists when some row would fill it.
	longestName, longestDetail := 0, 0
	for _, r := range m.rows {
		st := m.rowState(r)
		if visW == 0 && (st == stateVerbatim || st.changed()) {
			visW = len("private")
		}
		if w := visibleWidth(r.Name); w > longestName {
			longestName = w
		}
		if w := visibleWidth(m.detailFor(r, st)); w > longestDetail {
			longestDetail = w
		}
	}
	if m.width < 70 {
		visW = 0
	}

	// What is left once the markers, the gaps, the visibility column and the
	// longest description have had their share.
	const markers = 6 // " > * " and the space after the name
	budget := m.width - markers - longestDetail - 1
	if visW > 0 {
		budget -= visW + 1
	}

	nameW = longestName
	if nameW > budget {
		nameW = budget
	}
	// Never so narrow that a name is unrecognisable, and never so wide that
	// one unusually long name pushes every description off the screen.
	if nameW < 14 {
		nameW = minInt(14, maxInt(8, m.width/3))
	}
	if nameW > 60 {
		nameW = 60
	}
	return nameW, visW
}

// detailFor is the right-hand text: what will happen, or why nothing will.
func (m *Model) detailFor(r Row, st state) string {
	switch st {
	case stateInert:
		if r.Blocked != "" {
			return r.Blocked
		}
		return "not selected"
	case stateAvailable:
		return gateHint(r, m.groups, m.forks, m.archived)
	case stateVisibility, stateRedacted, stateBothWays:
		var changes []string
		if r.Private != r.SourcePrivate {
			changes = append(changes, "now "+visibilityName(r.Private))
		}
		if r.Redact {
			changes = append(changes, "emails redacted")
		}
		return "create, " + strings.Join(changes, ", ")
	default:
		return "create"
	}
}

// visibilityWord renders the destination visibility for the middle column.
func (m *Model) visibilityWord(r Row) string {
	return visibilityName(r.Private)
}

// visibilityName is the word GitHub uses.
func visibilityName(private bool) string {
	if private {
		return "private"
	}
	return "public"
}

// wrapText breaks s into lines of at most width columns, on word boundaries.
func wrapText(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var lines []string
	current := words[0]
	for _, w := range words[1:] {
		if len(current)+1+len(w) <= width {
			current += " " + w
			continue
		}
		lines = append(lines, current)
		current = w
	}
	return append(lines, current)
}

// footer renders the tally and the key hints.
func (m *Model) footer() string {
	t := m.tally()

	couldAdd, notMoving := "", ""
	if t.Available > 0 {
		couldAdd = fmt.Sprintf("   %s%d could add%s", ansiCyan, t.Available, ansiReset)
	}
	if t.Inert > 0 {
		notMoving = fmt.Sprintf("   %s%d not moving%s", ansiDim, t.Inert, ansiReset)
	}

	// Narrowing order, widest first. What is given up first is the tail, not
	// the breakdown: naming the changes is the line's job, while "could add"
	// and "not moving" only restate what the cyan and grey rows already say.
	// Summing the changes into one figure is the last thing tried before the
	// bare total, because "3 with changes" answers less than it looks.
	var candidates []string
	for _, sentence := range []string{
		m.countSentence(t, namesInFull),
		m.countSentence(t, namesAbbreviated),
		m.countSentence(t, namesSummed),
	} {
		candidates = append(candidates,
			"  "+sentence+couldAdd+notMoving,
			"  "+sentence+couldAdd,
			"  "+sentence)
	}
	for _, line := range candidates {
		if visibleWidth(line) <= m.width {
			return m.footerLines(line)
		}
	}
	return m.footerLines(fmt.Sprintf("  %s%d to migrate%s", ansiBold, t.Migrating(), ansiReset))
}

// naming is how much room the count line has for the three kinds of change.
type naming int

const (
	// namesInFull spells out every kind: "1 visibility & redacted".
	namesInFull naming = iota

	// namesAbbreviated shortens only the combined one to "1 both changes".
	// The other two are named immediately before it, so "both" has its
	// referent in view -- which is exactly what it lacked when it stood alone.
	// This is the rung that fits an eighty-column terminal.
	namesAbbreviated

	// namesSummed gives up the breakdown: "3 with changes". Last resort,
	// because it answers less than it looks.
	namesSummed
)

// countSentence renders the counts as arithmetic, naming the kinds of change
// in as much detail as the given level allows.
func (m *Model) countSentence(t tally, level naming) string {
	if t.Migrating() == 0 {
		return ansiBold + "nothing to migrate" + ansiReset
	}
	total := fmt.Sprintf("%s%d to migrate%s", ansiBold, t.Migrating(), ansiReset)

	type bucket struct {
		n      int
		label  string
		colour string
	}
	buckets := []bucket{{t.Verbatim, "unchanged", ansiGreen}}
	switch level {
	case namesInFull:
		buckets = append(buckets,
			bucket{t.Visibility, "visibility", ansiVisibility},
			bucket{t.Redacted, "redacted", ansiRedacted},
			bucket{t.BothWays, "visibility & redacted", ansiBothWays})
	case namesAbbreviated:
		buckets = append(buckets,
			bucket{t.Visibility, "visibility", ansiVisibility},
			bucket{t.Redacted, "redacted", ansiRedacted},
			bucket{t.BothWays, "both changes", ansiBothWays})
	default:
		buckets = append(buckets, bucket{t.Changed(), "with changes", ansiBothWays})
	}

	var filled []bucket
	for _, b := range buckets {
		if b.n > 0 {
			filled = append(filled, b)
		}
	}

	// Everything in one bucket: there is no sum to show, and repeating the
	// count beside the total reads as an error rather than as arithmetic.
	if len(filled) == 1 {
		return fmt.Sprintf("%s, %sall %s%s", total, filled[0].colour, filled[0].label, ansiReset)
	}

	var parts []string
	for _, b := range filled {
		parts = append(parts, fmt.Sprintf("%s%d %s%s", b.colour, b.n, b.label, ansiReset))
	}
	return total + " -> " + strings.Join(parts, " + ")
}

// footerLines pairs the counts with the key hints beneath them.
func (m *Model) footerLines(tally string) string {
	var hint string
	switch {
	case m.note != "":
		hint = ansiYellow + truncate(m.note, m.width-2) + ansiReset
	case m.editingEmail:
		hint = dim(truncate("  address to keep unredacted, enter when done", m.width))
	case m.searching:
		hint = fmt.Sprintf("  search: %s%s%s   %s",
			ansiReverse, m.query+" ", ansiReset, dim("enter keeps, esc clears"))
	default:
		// Shortened rather than truncated as the terminal narrows: a hint cut
		// off mid-word is worse than a shorter list of hints.
		full := "  space select   v visibility   e redact   a/n all/none   / search   enter migrate   q quit"
		medium := "  space   v vis   e redact   a/n all   / search   enter go   q quit"
		short := "  enter go   q quit"
		hint = dim(pickFitting(m.width, full, medium, short))
	}

	var b strings.Builder
	b.WriteString(truncateANSI(tally, m.width) + "\r\n")
	b.WriteString(truncateANSI(hint, m.width))
	return b.String()
}

// headerLine draws the route and how many repositories are in play.
//
// The total belongs here rather than in the footer because it does not change
// as gates and checkboxes are toggled: the footer is for what the next
// keystroke affects, and mixing a standing fact into it made the line too wide
// for an eighty-column terminal.
func (m *Model) headerLine(header string) string {
	count := fmt.Sprintf("%d repositories", len(m.rows))
	if len(m.rows) == 1 {
		count = "1 repository"
	}
	gap := m.width - len(header) - len(count) - 2
	if gap < 2 {
		return ansiBold + truncateANSI(header, m.width) + ansiReset
	}
	return ansiBold + header + ansiReset + strings.Repeat(" ", gap) + dim(count)
}

// checkbox draws a toggle. It carries no colour of its own: the caller
// supplies the one that says what the toggle governs.
func checkbox(on bool) string {
	if on {
		return "[x]"
	}
	return "[ ]"
}

func dim(s string) string { return ansiDim + s + ansiReset }

func quote(s string) string { return "\"" + s + "\"" }

// pad right-pads s to n columns, truncating when it does not fit.
//
// Counted in runes rather than bytes. The ellipsis it appends is itself three
// bytes wide and one column, so a byte count would both mis-measure the result
// and cut a multi-byte name mid-character -- and repository names and paths
// are not always ASCII.
func pad(s string, n int) string {
	runes := []rune(s)
	if len(runes) > n {
		if n <= 1 {
			return string(runes[:maxInt(0, n)])
		}
		return string(runes[:n-1]) + "…"
	}
	return s + strings.Repeat(" ", n-len(runes))
}

// visibleWidth is how many columns s occupies once its escape sequences are
// discounted. Counted in runes: an ellipsis is three bytes and one column.
func visibleWidth(s string) int { return len([]rune(stripANSI(s))) }

func truncate(s string, w int) string {
	if w <= 0 || len(s) <= w {
		return s
	}
	return s[:w]
}

// truncateANSI cuts a line to w visible columns without counting escape
// sequences, which occupy no width on screen.
func truncateANSI(s string, w int) string {
	if w <= 0 {
		return s
	}
	visible, out, inEscape := 0, strings.Builder{}, false
	for _, r := range s {
		if r == '\x1b' {
			inEscape = true
		}
		if inEscape {
			out.WriteRune(r)
			if r == 'm' {
				inEscape = false
			}
			continue
		}
		if visible >= w {
			continue
		}
		out.WriteRune(r)
		visible++
	}
	return out.String()
}

// stripANSI removes escape sequences, for measuring how wide a cell will
// actually be.
func stripANSI(s string) string {
	var out strings.Builder
	inEscape := false
	for _, r := range s {
		if r == '\x1b' {
			inEscape = true
			continue
		}
		if inEscape {
			if r == 'm' {
				inEscape = false
			}
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// pickFitting returns the first candidate that fits in w columns, falling back
// to the last one.
func pickFitting(w int, candidates ...string) string {
	for _, c := range candidates {
		if len(c) <= w {
			return c
		}
	}
	return candidates[len(candidates)-1]
}
