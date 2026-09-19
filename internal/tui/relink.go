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

// Rescan looks for working copies under root and returns them as rows. It is
// supplied by the caller so the model stays free of I/O and can be driven by a
// test with a fake.
type Rescan func(root string) ([]Clone, error)

// RelinkModel is the state of the repointing screen.
type RelinkModel struct {
	clones  []Clone
	oldName string // what the Gitea remote is renamed to under ModeGitHub

	// root is the directory being looked at, which can be changed without
	// leaving the screen: the first guess is the working directory, and being
	// in the wrong one should cost a keystroke rather than a restart.
	root    string
	rescan  Rescan
	newRoot string // the path typed but not yet scanned

	editingRoot bool
	scanning    string // non-empty while a scan is owed for this path

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

// WithRoot records the directory the clones came from and how to look at
// another one. Without it the screen still works; it simply cannot be pointed
// somewhere else.
func (m *RelinkModel) WithRoot(root string, rescan Rescan) *RelinkModel {
	m.root, m.rescan = root, rescan
	return m
}

// Root is the directory the chosen clones were found under.
func (m *RelinkModel) Root() string { return m.root }

// Working reports that a directory has been given and not yet scanned.
func (m *RelinkModel) Working() bool { return m.scanning != "" }

// Work performs the scan the last keystroke asked for.
//
// Separated from Update so the runner can draw the frame that says what is
// happening before the wait starts: the scan walks the disk and asks GitHub
// about every clone it finds, which is long enough for a frozen screen to look
// like a hung one.
func (m *RelinkModel) Work() {
	root := m.scanning
	m.scanning = ""
	if m.rescan == nil {
		return
	}

	found, err := m.rescan(root)
	if err != nil {
		// The list already on screen is left alone: a mistyped path should
		// cost a message, not the selection built up so far.
		m.note = "could not scan " + root + ": " + err.Error()
		return
	}

	m.clones = found
	for i := range m.clones {
		m.clones[i].Include = m.clones[i].Blocked == ""
		if m.clones[i].Mode == "" {
			m.clones[i].Mode = relink.ModeGitHub
		}
	}
	m.root = root
	m.query, m.cursor = "", 0
	if len(found) == 0 {
		m.note = "no git working copies under " + root
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
	if m.editingRoot {
		m.updateRoot(k)
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
	case 'd':
		if m.rescan == nil {
			m.note = "this screen cannot change directory"
			return
		}
		m.editingRoot = true
		m.newRoot = m.root
	case 'a':
		m.setAll(true)
	case 'n':
		m.setAll(false)
	case '/':
		m.searching = true
	}
}

// updateRoot handles keys while the directory box has focus.
func (m *RelinkModel) updateRoot(k Key) {
	switch k.Kind {
	case KeyEnter:
		m.editingRoot = false
		typed := strings.TrimSpace(m.newRoot)
		if typed == "" || typed == m.root {
			return
		}
		// Recorded rather than scanned here, so the runner can draw the frame
		// that says what is happening before the wait starts.
		m.scanning = typed
	case KeyEscape, KeyCtrlC:
		m.editingRoot = false
		m.newRoot = ""
	case KeyBackspace:
		if m.newRoot != "" {
			_, size := lastRune(m.newRoot)
			m.newRoot = m.newRoot[:len(m.newRoot)-size]
		}
	case KeySpace:
		m.newRoot += " "
	case KeyRune:
		m.newRoot += string(k.Rune)
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
const relinkChrome = 10

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
	b.WriteString(m.destinationBar() + "\r\n")
	b.WriteString(m.rootBar() + "\r\n\r\n")

	body := m.height - relinkChrome
	if body < 3 {
		body = 3
	}

	vis := m.visible()
	written := 0
	switch {
	case len(m.clones) == 0:
		b.WriteString(ansiDim + "  nothing here -- press d to look somewhere else" + ansiReset + "\r\n")
		written = 1
	case len(vis) == 0:
		b.WriteString(ansiDim + "  no clone matches " + quote(m.query) + ansiReset + "\r\n")
		written = 1
	default:
		// Rows are not all one line tall, so the window is found by walking
		// back from the cursor until the next row would not fit.
		rendered := make([][]string, len(vis))
		for pos, idx := range vis {
			rendered[pos] = m.renderClone(idx, pos == m.cursor)
		}
		start, used := m.cursor, len(rendered[m.cursor])
		for start > 0 && used+len(rendered[start-1]) <= body {
			start--
			used += len(rendered[start])
		}
		remaining := body
		for pos := start; pos < len(vis); pos++ {
			if len(rendered[pos]) > remaining {
				break
			}
			for _, line := range rendered[pos] {
				b.WriteString(line + "\r\n")
				written++
			}
			remaining -= len(rendered[pos])
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

// rootBar shows the directory being looked at, and lets it be changed.
func (m *RelinkModel) rootBar() string {
	if m.root == "" {
		// Built without a directory: there is nothing true to say here.
		return ""
	}
	if m.rescan == nil {
		return truncateANSI("  "+dim("looking at "+m.root), m.width)
	}
	if m.scanning != "" {
		return truncateANSI(fmt.Sprintf("  %s%s scanning %s...%s",
			ansiCyan, "d", m.scanning, ansiReset), m.width)
	}
	if m.editingRoot {
		return truncateANSI(fmt.Sprintf("  %s directory: %s%s%s",
			dim("d"), ansiReverse, m.newRoot+" ", ansiReset), m.width)
	}
	return truncateANSI(fmt.Sprintf("  %s directory: %s", dim("d"), m.root), m.width)
}

// nameColumn works out how wide the path column may be.
//
// Measured from the paths and the descriptions rather than fixed. A fixed
// width wastes a wide window -- paths truncated with an ellipsis and
// descriptions wrapping while half the screen sits empty -- and starves a
// narrow one.
func (m *RelinkModel) nameColumn() int {
	longestPath, longestDetail := 0, 0
	for _, c := range m.clones {
		if w := visibleWidth(c.Display); w > longestPath {
			longestPath = w
		}
		detail := c.Blocked
		if detail == "" {
			detail = relink.Describe(c.Mode, m.oldName)
		}
		if w := visibleWidth(detail); w > longestDetail {
			longestDetail = w
		}
	}
	// Every mode is one keystroke away, so the column has to stay wide enough
	// for the longest of them rather than for whatever is shown right now.
	for _, mode := range []string{relink.ModeGitHub, relink.ModeBoth, relink.ModeGitea} {
		if w := visibleWidth(relink.Describe(mode, m.oldName)); w > longestDetail {
			longestDetail = w
		}
	}

	const markers = 6 // " > * " and the space after the path
	const labelW = 7  // "github" and its gap
	budget := m.width - markers - labelW - 1 - longestDetail

	nameW := longestPath
	if nameW > budget {
		nameW = budget
	}
	if nameW < 12 {
		nameW = minInt(12, maxInt(8, m.width/3))
	}
	if nameW > 60 {
		nameW = 60
	}
	return nameW
}

// renderClone draws one working copy, as one line or two.
//
// The descriptions say what push and pull will do afterwards, which is longer
// than a marker and worth the room: a sentence cut off mid-word is the one
// thing worse than a second line.
func (m *RelinkModel) renderClone(i int, cursor bool) []string {
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
	if cursor {
		colour += ansiBold
	}

	head := fmt.Sprintf(" %s %s %s %s", marker, symbol, pad(c.Display, m.nameColumn()), pad(label, 7))

	if room := m.width - visibleWidth(head) - 1; room >= visibleWidth(detail) {
		return []string{colour + head + " " + detail + ansiReset}
	}

	const indent = "      "
	lines := []string{colour + strings.TrimRight(head, " ") + ansiReset}
	for _, w := range wrapText(detail, maxInt(8, m.width-len(indent))) {
		lines = append(lines, colour+indent+w+ansiReset)
	}
	return lines
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
		"  space select   1/2/3 destination   A all   d directory   / search   enter repoint   q quit",
		"  space   1/2/3 dest   A all   d dir   / search   enter go   q quit",
		"  space   1/2/3 dest   d dir   enter go   q quit",
		"  enter go   q quit"))
	if m.editingRoot {
		hint = dim(truncate("  type a directory, enter to scan it, esc to keep this one", m.width))
	}
	if m.note != "" {
		hint = ansiYellow + truncate(m.note, m.width-2) + ansiReset
	}
	if m.searching {
		hint = fmt.Sprintf("  search: %s%s%s   %s",
			ansiReverse, m.query+" ", ansiReset, dim("enter keeps, esc clears"))
	}

	return truncateANSI(tally, m.width) + "\r\n" + truncateANSI(hint, m.width)
}
