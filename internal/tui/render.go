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

	vis := m.visible()
	body := m.height - chromeHeight
	if body < 3 {
		body = 3
	}
	start := 0
	// Keep the cursor in view by scrolling the window rather than the cursor:
	// the selected row stays where the eye expects it.
	if m.cursor >= body {
		start = m.cursor - body + 1
	}
	end := start + body
	if end > len(vis) {
		end = len(vis)
	}

	if len(vis) == 0 {
		b.WriteString(ansiDim + "  no repository matches " + quote(m.query) + ansiReset + "\r\n")
	}
	for pos := start; pos < end; pos++ {
		b.WriteString(m.renderRow(vis[pos], pos == m.cursor) + "\r\n")
	}
	// Pad so the footer does not walk up the screen as the list shortens.
	for pad := end - start; pad < body; pad++ {
		b.WriteString("\r\n")
	}

	b.WriteString("\r\n" + m.footer() + "\r\n")
	return b.String()
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

// renderRow draws one repository line.
func (m *Model) renderRow(i int, cursor bool) string {
	r := m.rows[i]
	eligible := r.eligible(m.groups, m.forks, m.archived)

	mark := " "
	if cursor {
		mark = ">"
	}

	var box, name, visibility, detail string
	switch {
	case r.Blocked != "":
		box, detail = dim("-"), dim(r.Blocked)
		name = dim(pad(r.Name, 34))
	case !eligible:
		box, detail = dim("-"), dim(gateHint(r, m.groups, m.forks, m.archived))
		name = dim(pad(r.Name, 34))
	case r.Include:
		box = ansiGreen + "*" + ansiReset
		name = pad(r.Name, 34)
		visibility = m.visibilityCell(r)
		detail = dim("create")
	default:
		box = " "
		name = dim(pad(r.Name, 34))
		detail = dim("not selected")
	}

	// The visibility cell carries colour, so it is padded against its
	// uncoloured width: escape sequences take no columns on screen.
	visCell := visibility + strings.Repeat(" ", maxInt(0, 22-len(stripANSI(visibility))))
	line := fmt.Sprintf(" %s %s %s %s %s", mark, box, name, visCell, detail)

	if cursor {
		return ansiBold + truncateANSI(line, m.width) + ansiReset
	}
	return truncateANSI(line, m.width)
}

// visibilityCell renders the destination visibility, flagging any change from
// the source so a deliberate flip is visible and an accidental one is obvious.
func (m *Model) visibilityCell(r Row) string {
	word := "public"
	if r.Private {
		word = "private"
	}
	if r.Private == r.SourcePrivate {
		return word
	}
	was := "public"
	if r.SourcePrivate {
		was = "private"
	}
	return ansiYellow + word + " (was " + was + ")" + ansiReset
}

// footer renders the tally and the key hints.
func (m *Model) footer() string {
	selected, skipped, blocked := m.counts()

	tally := fmt.Sprintf("  %d to migrate   %d skipped   %d unavailable", selected, skipped, blocked)
	if len(tally) > m.width {
		// On a narrow terminal the count that matters is the one about to be
		// acted on; the other two are reassurance, not information.
		tally = fmt.Sprintf("  %d to migrate", selected)
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
	b.WriteString(ansiBold + tally + ansiReset + "\r\n")
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
