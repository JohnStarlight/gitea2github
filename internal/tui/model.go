// Package tui draws the interactive pre-flight screen: one place to choose
// which repositories move, how visible each one lands, and whether the commit
// history is redacted on the way.
//
// The screen exists because the questions it replaces could only be answered
// in one direction. Asked as a sequence of prompts, deciding that the forks
// were worth taking after all -- three questions later, while reading the plan
// -- meant killing the program and starting again. Here every answer stays
// live until the run is confirmed.
//
// The model in this file is deliberately free of terminal code. It is a plain
// state machine over keys, so the whole of the interaction can be tested by
// feeding it bytes and reading its fields, with no pseudo-terminal in sight.
package tui

import (
	"sort"
	"strings"
)

// Row is one repository as the screen knows it.
//
// Everything needed to decide its fate is resolved before the screen opens, so
// that toggling a filter is a pure re-computation rather than another round
// trip to GitHub.
type Row struct {
	Name          string // Gitea full name, e.g. "ivogiake/lem-in"
	SourcePrivate bool   // visibility on Gitea
	Private       bool   // visibility the copy would be created with
	Fork          bool
	Archived      bool
	Foreign       bool // owned by somebody else: a group project

	// Target is the name this repository will take on GitHub, and Renamed
	// records that it differs from the repository's own name -- because two
	// repositories wanted the same one, or because it was typed here.
	Target  string
	Renamed bool

	// Blocked, when non-empty, is why this repository can never be migrated in
	// this run -- it is empty, or it is already on GitHub. Such rows stay on
	// screen, greyed out, because "where did my repository go?" is a worse
	// question than a line explaining it was already there.
	Blocked string

	// Include is the user's own choice for this row. It only has meaning while
	// the row is eligible; see Model.Selected.
	Include bool

	// Redact rewrites this repository's history to hide email addresses.
	//
	// Per row rather than per run, because rewriting history is not something
	// to have happen to a repository by side effect: with a single switch,
	// opening a gate or checking one more box silently redacted whatever came
	// with it.
	Redact bool
}

// eligible reports whether the category gates currently let this row through.
// The three conditions mirror migrate.Run exactly, so what the screen shows
// and what the migrator does cannot drift apart.
func (r Row) eligible(groups, forks, archived bool) bool {
	if r.Blocked != "" {
		return false
	}
	if r.Foreign && !groups {
		return false
	}
	if r.Fork && !forks {
		return false
	}
	if r.Archived && !archived {
		return false
	}
	return true
}

// Model is the full state of the screen.
type Model struct {
	rows []Row

	// Category gates. Off by default, matching the command line: a run that
	// was not asked about forks does not take them.
	groups, forks, archived bool

	// keepEmail is the one address left linked to its GitHub account. It stays
	// on the model rather than on the rows because it is a fact about the
	// person, not about any repository: whichever histories are rewritten,
	// this address survives all of them.
	keepEmail string

	// query filters the visible rows by substring. Typing it is a search, not
	// a selection: filtering the list never changes what is included, so a
	// half-typed search cannot silently drop a repository from the run.
	query     string
	searching bool

	cursor int // index into the visible rows, not into rows

	width, height int

	// done and cancelled record how the screen was left. Both false means it
	// is still running.
	done, cancelled bool

	// editingEmail puts keystrokes into keepEmail instead of the key map, and
	// editingTarget does the same for the destination name of one row.
	editingEmail  bool
	editingTarget bool
	targetInput   string

	// note is a transient one-line message shown in the footer.
	note string
}

// NewModel builds the screen state. Rows are shown in the order given, which
// the caller has already sorted.
func NewModel(rows []Row, groups, forks, archived, redact bool, keepEmail string) *Model {
	// The rows are copied rather than kept by reference. The screen writes to
	// them from the first line of this function onwards, and a caller that
	// reused its slice -- to build a second screen, or to read back what it
	// passed -- would find it altered underneath.
	owned := make([]Row, len(rows))
	copy(owned, rows)

	m := &Model{
		rows:      owned,
		groups:    groups,
		forks:     forks,
		archived:  archived,
		keepEmail: keepEmail,
		width:     80,
		height:    24,
	}
	// Everything the gates admit starts selected. The gates are the coarse
	// decision and the checkboxes refine it; starting with an empty selection
	// would make the common case -- "all of my own repositories" -- the one
	// that takes the most keystrokes.
	for i := range m.rows {
		m.rows[i].Include = true
		// --redact-emails on the command line asks for all of them, which is
		// the only way every row starts redacted.
		m.rows[i].Redact = redact
	}
	return m
}

// SetSize records the terminal dimensions.
func (m *Model) SetSize(w, h int) {
	if w > 0 {
		m.width = w
	}
	if h > 0 {
		m.height = h
	}
}

// Done and Cancelled report how the screen ended.
func (m *Model) Done() bool      { return m.done }
func (m *Model) Cancelled() bool { return m.cancelled }

// Redact reports whether any repository in the run is to be redacted, which is
// what decides whether a Mapper is built at all.
func (m *Model) Redact() bool {
	for _, r := range m.rows {
		if r.Redact && r.eligible(m.groups, m.forks, m.archived) && r.Include {
			return true
		}
	}
	return false
}

// RedactedRepos returns the Gitea full names whose history is to be rewritten,
// in the shape migrate.Options wants.
//
// A nil result means none, and the caller must not pass an empty map instead:
// to the migrator a nil RedactOnly means "every repository", which is the
// opposite of what an empty selection asks for.
func (m *Model) RedactedRepos() map[string]bool {
	out := map[string]bool{}
	for _, r := range m.rows {
		if r.Redact && r.eligible(m.groups, m.forks, m.archived) && r.Include {
			out[r.Name] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// KeepEmail is the address left linked to its GitHub account.
func (m *Model) KeepEmail() string { return m.keepEmail }

// Gates exposes the three category answers.
func (m *Model) Gates() (groups, forks, archived bool) {
	return m.groups, m.forks, m.archived
}

// visible returns the indices of the rows the text query admits, in display
// order.
func (m *Model) visible() []int {
	var out []int
	q := strings.ToLower(strings.TrimSpace(m.query))
	for i, r := range m.rows {
		if q == "" || strings.Contains(strings.ToLower(r.Name), q) {
			out = append(out, i)
		}
	}
	return out
}

// Renames returns the destination names typed on this screen, keyed by Gitea
// full name.
//
// Only the ones that differ from what the row arrived with: handing back a
// name nobody changed would record a decision that was never made.
func (m *Model) Renames() map[string]string {
	out := map[string]string{}
	for _, r := range m.rows {
		if !r.Renamed || r.Target == "" {
			continue
		}
		if r.eligible(m.groups, m.forks, m.archived) && r.Include {
			out[r.Name] = r.Target
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Selected returns the Gitea full names that would be migrated, sorted.
//
// A row counts only when the gates admit it *and* the user left it checked,
// which is what makes the footer tally and the migration agree.
func (m *Model) Selected() []string {
	var out []string
	for _, r := range m.rows {
		if r.eligible(m.groups, m.forks, m.archived) && r.Include {
			out = append(out, r.Name)
		}
	}
	sort.Strings(out)
	return out
}

// VisibilityOverrides reports the rows whose chosen visibility differs from
// Gitea's, keyed by Gitea full name, in the shape migrate.Options wants.
//
// Only genuine differences are returned: handing the migrator an override that
// restates the source visibility would record a deliberate choice where the
// user made none.
func (m *Model) VisibilityOverrides() map[string]bool {
	out := map[string]bool{}
	for _, r := range m.rows {
		if !r.eligible(m.groups, m.forks, m.archived) || !r.Include {
			continue
		}
		if r.Private != r.SourcePrivate {
			out[r.Name] = r.Private
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// setCategory bulk-applies a gate change to the checkboxes underneath it.
//
// Opening a gate re-checks the rows behind it, so that "include forks" means
// what it says even if a fork was unchecked individually earlier. Without this
// a user could open the gate and see nothing happen, which reads as a bug.
func (m *Model) setCategory(pred func(Row) bool, on bool) {
	for i := range m.rows {
		if m.rows[i].Blocked == "" && pred(m.rows[i]) {
			m.rows[i].Include = on
		}
	}
}
