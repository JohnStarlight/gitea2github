package tui

// state is what a row is, reduced to the one thing the screen has to show.
//
// Four states, four colours, four symbols. Keeping them in one place means the
// colour and the symbol cannot drift apart from the logic that decides which
// rows actually migrate: everything is derived from this function.
type state int

const (
	// stateInert is a row nothing will happen to: already on GitHub, empty, or
	// deliberately unchecked. Grey.
	stateInert state = iota

	// stateAvailable is a row held back only by a closed gate. One keystroke
	// away from migrating, which is why it is the one state that has to catch
	// the eye rather than blend into the list. Cyan.
	stateAvailable

	// stateVerbatim is a row that will be copied to GitHub exactly as it
	// stands on Gitea. Green.
	stateVerbatim

	// stateModified is a row that will be copied with something changed:
	// visibility flipped away from the source, or the commit history rewritten
	// to redact addresses. Amber.
	//
	// It wins over stateVerbatim because it answers a different question. The
	// other three say whether a repository moves; this one says that what
	// lands on GitHub is not what sits on Gitea, which is the fact somebody
	// would most regret missing.
	stateModified
)

// rowState classifies one row against the current gates and the redaction
// setting.
func (m *Model) rowState(r Row) state {
	if r.Blocked != "" {
		return stateInert
	}
	if !r.eligible(m.groups, m.forks, m.archived) {
		return stateAvailable
	}
	if !r.Include {
		return stateInert
	}
	if r.Private != r.SourcePrivate || m.redact {
		return stateModified
	}
	return stateVerbatim
}

// colour returns the escape sequence a state is drawn in.
//
// Applied to the whole line rather than to a marker: a one-character cue at
// the far left is read once and then lost, and re-finding which row it
// belonged to means tracking back along the line every time.
func (s state) colour() string {
	switch s {
	case stateAvailable:
		return ansiCyan
	case stateVerbatim:
		return ansiGreen
	case stateModified:
		return ansiAmber
	default:
		return ansiDim
	}
}

// symbol returns the marker a state is drawn with.
//
// The symbols carry the same distinction as the colours so that the screen
// still reads without them -- in a monochrome terminal, for someone who cannot
// separate the hues, or when the output is piped somewhere.
func (s state) symbol() string {
	switch s {
	case stateAvailable:
		return "+"
	case stateVerbatim, stateModified:
		return "*"
	default:
		return "-"
	}
}

// tally counts the rows in each state, for the footer.
type tally struct {
	Verbatim  int
	Modified  int
	Available int
	Inert     int
}

// Migrating is how many repositories the run would actually transfer.
func (t tally) Migrating() int { return t.Verbatim + t.Modified }

// tally classifies every row once.
func (m *Model) tally() tally {
	var t tally
	for _, r := range m.rows {
		switch m.rowState(r) {
		case stateAvailable:
			t.Available++
		case stateVerbatim:
			t.Verbatim++
		case stateModified:
			t.Modified++
		default:
			t.Inert++
		}
	}
	return t
}
