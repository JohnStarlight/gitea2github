// This file holds the second of the two screens: the one that repoints local
// clones after a migration. It shares the terminal handling, the key decoding
// and the drawing helpers with the migration screen, and nothing else -- the
// two answer different questions and trying to make one model serve both would
// have bent each out of shape.
package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/JohnStarlight/gitea2github/internal/relink"
)

// Clone is one local working copy as the screen knows it.
type Clone struct {
	// Path is the directory, and the key relink.Options is keyed by. Display
	// is the same path shortened for the screen, which cannot be derived here
	// because it depends on the user's home directory.
	Path    string
	Display string

	// Blocked, when non-empty, is why this clone cannot be repointed: its
	// origin is not on the Gitea host, or the matching GitHub repository does
	// not exist yet.
	Blocked string

	// Include is whether this clone is in the run, and Mode is where it will
	// push afterwards.
	Include bool
	Mode    string
}

// RelinkModel is the state of the repointing screen.
type RelinkModel struct {
	clones  []Clone
	oldName string // what the Gitea remote is renamed to under ModeGitHub

	query     string
	searching bool
	cursor    int

	width, height   int
	done, cancelled bool
	note            string
}

// NewRelinkModel builds the screen. Every clone that can be repointed starts
// selected, with mode as its destination.
func NewRelinkModel(clones []Clone, mode, oldName string) *RelinkModel {
	owned := make([]Clone, len(clones))
	copy(owned, clones)
	for i := range owned {
		owned[i].Include = owned[i].Blocked == ""
		if owned[i].Mode == "" {
			owned[i].Mode = mode
		}
	}
	return &RelinkModel{
		clones:  owned,
		oldName: oldName,
		width:   80,
		height:  24,
	}
}

func (m *RelinkModel) SetSize(w, h int) {
	if w > 0 {
		m.width = w
	}
	if h > 0 {
		m.height = h
	}
}

func (m *RelinkModel) Done() bool      { return m.done }
func (m *RelinkModel) Cancelled() bool { return m.cancelled }

// Chosen returns the paths to repoint, and the destination for each, in the
// shape relink.Options wants.
//
// Both maps are nil when nothing is chosen, because to relink.Run a nil Only
// means every clone it finds -- the opposite of an empty selection.
func (m *RelinkModel) Chosen() (only map[string]bool, modes map[string]string) {
	only, modes = map[string]bool{}, map[string]string{}
	for _, c := range m.clones {
		if c.Blocked != "" || !c.Include {
			continue
		}
		only[c.Path] = true
		modes[c.Path] = c.Mode
	}
	if len(only) == 0 {
		return nil, nil
	}
	return only, modes
}

// Paths returns the chosen clones in a stable order, for printing.
func (m *RelinkModel) Paths() []string {
	only, _ := m.Chosen()
	out := make([]string, 0, len(only))
	for p := range only {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// visible returns the indices the text query admits.
func (m *RelinkModel) visible() []int {
	var out []int
	q := strings.ToLower(strings.TrimSpace(m.query))
	for i, c := range m.clones {
		if q == "" || strings.Contains(strings.ToLower(c.Display), q) {
			out = append(out, i)
		}
	}
	return out
}

func (m *RelinkModel) currentClone() int {
	vis := m.visible()
	if m.cursor < 0 || m.cursor >= len(vis) {
		return -1
	}
	return vis[m.cursor]
}

// modeColour matches each destination to a colour, so a directory full of
// clones can be read as a shape rather than line by line.
func modeColour(mode string) string {
	switch mode {
	case relink.ModeBoth:
		return ansiVisibility
	case relink.ModeGitea:
		return ansiRedacted
	default:
		return ansiGreen
	}
}

// modeLabel is the short name shown in the middle column.
func modeLabel(mode string) string {
	switch mode {
	case relink.ModeBoth:
		return "both"
	case relink.ModeGitea:
		return "gitea"
	default:
		return "github"
	}
}

// Update applies one keypress.
func (m *RelinkModel) Update(k Key) {
	m.note = ""
	if m.searching {
		m.updateSearch(k)
		return
	}

	switch k.Kind {
	case KeyCtrlC, KeyEscape:
		m.cancelled, m.done = true, true
		return
	case KeyEnter:
		if len(m.Paths()) == 0 {
			m.note = "nothing selected -- press a to select all, or q to quit"
			return
		}
		m.done = true
		return
	case KeyUp:
		m.move(-1)
		return
	case KeyDown:
		m.move(1)
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
	case '1':
		m.setMode(relink.ModeGitHub)
	case '2':
		m.setMode(relink.ModeBoth)
	case '3':
		m.setMode(relink.ModeGitea)
	case 'A':
		m.applyModeToAll()
	case 'a':
		m.setAll(true)
	case 'n':
		m.setAll(false)
	case '/':
		m.searching = true
	}
}

func (m *RelinkModel) updateSearch(k Key) {
	switch k.Kind {
	case KeyEnter:
		m.searching = false
	case KeyEscape, KeyCtrlC:
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

func (m *RelinkModel) move(delta int) {
	m.cursor += delta
	m.clampCursor()
}

func (m *RelinkModel) clampCursor() {
	n := len(m.visible())
	switch {
	case n == 0:
		m.cursor = 0
	case m.cursor < 0:
		m.cursor = 0
	case m.cursor >= n:
		m.cursor = n - 1
	}
}

func (m *RelinkModel) toggleCurrent() {
	i := m.currentClone()
	if i < 0 {
		return
	}
	if m.clones[i].Blocked != "" {
		m.note = m.clones[i].Display + ": " + m.clones[i].Blocked
		return
	}
	m.clones[i].Include = !m.clones[i].Include
}

// setMode changes where the clone under the cursor will push.
func (m *RelinkModel) setMode(mode string) {
	i := m.currentClone()
	if i < 0 {
		return
	}
	if m.clones[i].Blocked != "" {
		m.note = m.clones[i].Display + ": " + m.clones[i].Blocked
		return
	}
	m.clones[i].Mode = mode
	// Choosing a destination for a clone that was excluded is a clear enough
	// statement of intent to put it back in the run.
	m.clones[i].Include = true
}

// applyModeToAll gives every selected clone the destination of the one under
// the cursor, which is how a whole directory is set without walking it.
func (m *RelinkModel) applyModeToAll() {
	i := m.currentClone()
	if i < 0 || m.clones[i].Blocked != "" {
		return
	}
	mode := m.clones[i].Mode
	for _, j := range m.visible() {
		if m.clones[j].Blocked == "" {
			m.clones[j].Mode = mode
		}
	}
	m.note = "every clone on screen now pushes to " + modeLabel(mode)
}

func (m *RelinkModel) setAll(on bool) {
	for _, i := range m.visible() {
		if m.clones[i].Blocked == "" {
			m.clones[i].Include = on
		}
	}
}

// relinkChrome is how many lines the header, the destination bar and the
// footer take.
const relinkChrome = 9

// View draws the screen.
func (m *RelinkModel) View(header string) string {
	var b strings.Builder

	count := fmt.Sprintf("%d clones", len(m.clones))
	if len(m.clones) == 1 {
		count = "1 clone"
	}
	gap := m.width - len(header) - len(count) - 2
	if gap < 2 {
		b.WriteString(ansiBold + truncateANSI(header, m.width) + ansiReset + "\r\n")
	} else {
		b.WriteString(ansiBold + header + ansiReset + strings.Repeat(" ", gap) + dim(count) + "\r\n")
	}
	b.WriteString(m.destinationBar() + "\r\n\r\n")

	body := m.height - relinkChrome
	if body < 3 {
		body = 3
	}

	vis := m.visible()
	written := 0
	if len(vis) == 0 {
		b.WriteString(ansiDim + "  no clone matches " + quote(m.query) + ansiReset + "\r\n")
		written = 1
	} else {
		start := m.cursor
		used := 1
		for start > 0 && used+1 <= body {
			start--
			used++
		}
		for pos := start; pos < len(vis) && written < body; pos++ {
			b.WriteString(m.renderClone(vis[pos], pos == m.cursor) + "\r\n")
			written++
		}
	}
	for ; written < body; written++ {
		b.WriteString("\r\n")
	}

	b.WriteString("\r\n" + m.relinkFooter() + "\r\n")
	return b.String()
}

// destinationBar shows the three destinations, each in its own colour, with
// the one under the cursor marked.
func (m *RelinkModel) destinationBar() string {
	current := ""
	if i := m.currentClone(); i >= 0 {
		current = m.clones[i].Mode
	}

	var parts []string
	for _, d := range []struct{ key, mode string }{
		{"1", relink.ModeGitHub}, {"2", relink.ModeBoth}, {"3", relink.ModeGitea},
	} {
		marker := "[ ]"
		if d.mode == current {
			marker = "[x]"
		}
		parts = append(parts, fmt.Sprintf("%s %s%s %s%s",
			dim(d.key), modeColour(d.mode), marker, modeLabel(d.mode), ansiReset))
	}
	return truncateANSI("  "+strings.Join(parts, "   ")+"   "+dim("A all"), m.width)
}

// renderClone draws one working copy.
func (m *RelinkModel) renderClone(i int, cursor bool) string {
	c := m.clones[i]

	marker := " "
	if cursor {
		marker = ">"
	}

	var colour, symbol, label, detail string
	switch {
	case c.Blocked != "":
		colour, symbol, detail = ansiDim, "-", c.Blocked
	case !c.Include:
		colour, symbol, detail = ansiDim, " ", "left alone"
	default:
		colour, symbol = modeColour(c.Mode), "*"
		label = modeLabel(c.Mode)
		detail = relink.Describe(c.Mode, m.oldName)
	}

	nameW := 30
	if m.width < 90 {
		nameW = maxInt(12, m.width/3)
	}
	line := fmt.Sprintf(" %s %s %s %s %s",
		marker, symbol, pad(c.Display, nameW), pad(label, 7), detail)
	if cursor {
		return colour + ansiBold + truncateANSI(line, m.width) + ansiReset
	}
	return colour + truncateANSI(line, m.width) + ansiReset
}

// relinkFooter counts the clones by destination, in the colours of the rows.
func (m *RelinkModel) relinkFooter() string {
	counts := map[string]int{}
	left := 0
	for _, c := range m.clones {
		if c.Blocked != "" || !c.Include {
			left++
			continue
		}
		counts[c.Mode]++
	}
	total := len(m.clones) - left

	var parts []string
	for _, mode := range []string{relink.ModeGitHub, relink.ModeBoth, relink.ModeGitea} {
		if counts[mode] > 0 {
			parts = append(parts, fmt.Sprintf("%s%d %s%s",
				modeColour(mode), counts[mode], modeLabel(mode), ansiReset))
		}
	}
	if left > 0 {
		parts = append(parts, fmt.Sprintf("%s%d left alone%s", ansiDim, left, ansiReset))
	}

	head := fmt.Sprintf("%s%d to repoint%s", ansiBold, total, ansiReset)
	if total == 0 {
		head = ansiBold + "nothing to repoint" + ansiReset
	}
	// One destination for everything needs no breakdown beside the total.
	tally := "  " + head
	if len(parts) > 1 || left > 0 {
		tally += "   " + strings.Join(parts, "   ")
	}
	if visibleWidth(tally) > m.width {
		tally = "  " + head
	}

	hint := dim(pickFitting(m.width,
		"  space select   1/2/3 destination   A all   a/n all/none   / search   enter repoint   q quit",
		"  space   1/2/3 dest   A all   a/n   / search   enter go   q quit",
		"  enter go   q quit"))
	if m.note != "" {
		hint = ansiYellow + truncate(m.note, m.width-2) + ansiReset
	}
	if m.searching {
		hint = fmt.Sprintf("  search: %s%s%s   %s",
			ansiReverse, m.query+" ", ansiReset, dim("enter keeps, esc clears"))
	}

	return truncateANSI(tally, m.width) + "\r\n" + truncateANSI(hint, m.width)
}
