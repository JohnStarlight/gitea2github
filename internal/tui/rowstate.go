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

	// The three ways a row can be copied with something changed. They are
	// separate states rather than one, because the two changes are not alike:
	// redaction is global -- one keystroke rewrites every history in the run
	// -- while visibility is decided row by row. Collapsing them hid which of
	// the two a particular row had had done to it.
	//
	// All three win over stateVerbatim, because they answer a different
	// question. The other states say whether a repository moves; these say
	// that what lands on GitHub is not what sits on Gitea, which is the fact
	// somebody would most regret missing.
	stateVisibility // visibility flipped away from the source
	stateRedacted   // history rewritten to hide addresses
	stateBothWays   // both at once
)

// changed reports whether a state is one of the three that alter what lands on
// GitHub.
func (s state) changed() bool {
	return s == stateVisibility || s == stateRedacted || s == stateBothWays
}

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
	flipped := r.Private != r.SourcePrivate
	switch {
	case flipped && m.redact:
		return stateBothWays
	case flipped:
		return stateVisibility
	case m.redact:
		return stateRedacted
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
	case stateVisibility:
		return ansiVisibility
	case stateRedacted:
		return ansiRedacted
	case stateBothWays:
		return ansiBothWays
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
	case stateVerbatim, stateVisibility, stateRedacted, stateBothWays:
		return "*"
	default:
		return "-"
	}
}

// tally counts the rows in each state, for the footer.
type tally struct {
	Verbatim   int
	Visibility int
	Redacted   int
	BothWays   int
	Available  int
	Inert      int
}

// Migrating is how many repositories the run would actually transfer.
func (t tally) Migrating() int {
	return t.Verbatim + t.Changed()
}

// Changed is how many of those arrive different from how they left.
func (t tally) Changed() int {
	return t.Visibility + t.Redacted + t.BothWays
}

// tally classifies every row once.
func (m *Model) tally() tally {
	var t tally
	for _, r := range m.rows {
		switch m.rowState(r) {
		case stateAvailable:
			t.Available++
		case stateVerbatim:
			t.Verbatim++
		case stateVisibility:
			t.Visibility++
		case stateRedacted:
			t.Redacted++
		case stateBothWays:
			t.BothWays++
		default:
			t.Inert++
		}
	}
	return t
}
