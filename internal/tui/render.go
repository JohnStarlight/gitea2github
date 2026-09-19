package tui

import (
	"fmt"
	"strings"
)

// chromeHeight is how many lines the header, the filter bar and the footer
// take, leaving the rest of the terminal for the list.
const chromeHeight = 10

// ANSI attributes, written out rather than pulled from a styling library.
// The screen needs six of them and nothing more.
const (
	ansiReset   = "\x1b[0m"
	ansiBold    = "\x1b[1m"
	ansiDim     = "\x1b[2m"
	ansiReverse = "\x1b[7m"
	ansiGreen   = "\x1b[32m"
	ansiYellow  = "\x1b[33m"
	ansiAmber   = "\x1b[33m"
	ansiCyan    = "\x1b[36m"
)

// View renders the whole screen.
//
// Every frame is drawn in full rather than diffed against the last one. The
// list is at most a screenful and repaints are driven by keystrokes, so the
// cost is invisible, and it removes an entire class of bug where the screen
// and the model disagree about what is already on the terminal.
func (m *Model) View(header string) string {
	var b strings.Builder

	b.WriteString(ansiBold + truncateANSI(header, m.width) + ansiReset + "\r\n")
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
	counts := map[string]int{}
	for _, r := range m.rows {
		if r.Foreign {
			counts["groups"]++
		}
		if r.Fork {
			counts["forks"]++
		}
		if r.Archived {
			counts["archived"]++
		}
	}

	var parts []string
	add := func(key, label string, on bool, n int) {
		if n == 0 {
			// A gate for a category the account does not contain is noise, and
			// noise is what trains people to stop reading the screen.
			return
		}
		parts = append(parts, fmt.Sprintf("%s %s %s (%d)", dim(key), checkbox(on), label, n))
	}
	add("1", "group projects", m.groups, counts["groups"])
	add("2", "forks", m.forks, counts["forks"])
	add("3", "archived", m.archived, counts["archived"])

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
	line := fmt.Sprintf("  %s %s redact emails", dim("e"), checkbox(m.redact))
	if !m.redact {
		return truncateANSI(line, m.width)
	}
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
	if st == stateVerbatim || st == stateModified {
		visibility = m.visibilityWord(r)
	}

	head := fmt.Sprintf(" %s %s %s", marker, st.symbol(), pad(r.Name, nameW))
	if visW > 0 {
		head += " " + pad(visibility, visW)
	}

	detail := m.detailFor(r, st)
	room := m.width - len(head) - 1
	if detail == "" {
		return []string{colour + strings.TrimRight(head, " ") + ansiReset}
	}
	if room >= len(detail) {
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
// Derived from the terminal rather than fixed: at eighty columns a
// thirty-four-character name column leaves nothing for the reason, and the
// reason is the whole point of the rows that have one.
func (m *Model) columns() (nameW, visW int) {
	nameW = 34
	if m.width < 90 {
		nameW = maxInt(14, m.width/3)
	}

	// The visibility column only exists when some row on screen would fill it.
	for _, r := range m.rows {
		if s := m.rowState(r); s == stateVerbatim || s == stateModified {
			visW = 20
			break
		}
	}
	if m.width < 70 {
		visW = 0
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
	case stateModified:
		var changes []string
		if r.Private != r.SourcePrivate {
			changes = append(changes, "now "+visibilityName(r.Private))
		}
		if m.redact {
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

	// Each count is drawn in the colour of the rows it counts, which turns the
	// footer into the legend for the list above it -- no separate key to read,
	// and nothing to fall out of step with the rows.
	parts := []string{fmt.Sprintf("%s%d verbatim%s", ansiGreen, t.Verbatim, ansiReset)}
	if t.Modified > 0 {
		parts = append(parts, fmt.Sprintf("%s%d modified%s", ansiAmber, t.Modified, ansiReset))
	}
	if t.Available > 0 {
		parts = append(parts, fmt.Sprintf("%s%d available%s", ansiCyan, t.Available, ansiReset))
	}
	parts = append(parts, fmt.Sprintf("%s%d untouched%s", ansiDim, t.Inert, ansiReset))

	tally := fmt.Sprintf("  %s%d to migrate%s   %s",
		ansiBold, t.Migrating(), ansiReset, strings.Join(parts, "   "))
	if len(stripANSI(tally)) > m.width {
		// On a narrow terminal the count that matters is the one about to be
		// acted on; the breakdown is reassurance, not information.
		tally = fmt.Sprintf("  %s%d to migrate%s", ansiBold, t.Migrating(), ansiReset)
	}

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
		full := "  space select   v visibility   a all   n none   / search   enter migrate   q quit"
		medium := "  space   v vis   a/n all/none   / search   enter go   q quit"
		short := "  enter go   q quit"
		hint = dim(pickFitting(m.width, full, medium, short))
	}

	var b strings.Builder
	b.WriteString(truncateANSI(tally, m.width) + "\r\n")
	b.WriteString(truncateANSI(hint, m.width))
	return b.String()
}

func checkbox(on bool) string {
	if on {
		return ansiGreen + "[x]" + ansiReset
	}
	return "[ ]"
}

func dim(s string) string { return ansiDim + s + ansiReset }

func quote(s string) string { return "\"" + s + "\"" }

// pad right-pads s to n columns, truncating when it does not fit.
func pad(s string, n int) string {
	if len(s) > n {
		if n <= 1 {
			return s[:maxInt(0, n)]
		}
		return s[:n-1] + "…"
	}
	return s + strings.Repeat(" ", n-len(s))
}

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
