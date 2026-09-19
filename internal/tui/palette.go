package tui

import (
	"os"
	"strings"
)

// The palette. Declared as variables rather than constants because the screen
// picks between a 256-colour set and a sixteen-colour one at start-up: the
// distinction between "visibility changed" and "emails redacted" needs three
// clearly separate hues, and the sixteen-colour set has to borrow magenta and
// red to get them.
var (
	ansiReset   = "\x1b[0m"
	ansiBold    = "\x1b[1m"
	ansiDim     = "\x1b[2m"
	ansiReverse = "\x1b[7m"

	ansiGreen  = "\x1b[32m" // copied exactly as it is
	ansiCyan   = "\x1b[36m" // held back only by a gate
	ansiYellow = "\x1b[33m" // the transient note in the footer

	// The three kinds of change, and the toggle colours that match them.
	ansiVisibility = "\x1b[38;5;214m" // visibility flipped away from the source
	ansiRedacted   = "\x1b[38;5;141m" // history rewritten to hide addresses
	ansiBothWays   = "\x1b[38;5;204m" // both of the above

	ansiBrightGreen = "\x1b[92m"
	ansiBrightCyan  = "\x1b[96m"

	// Redaction's own control is red rather than the colour of the rows it
	// makes. It is the only choice on either screen that cannot be taken back
	// once the run finishes: the commits it rewrites are new objects, and the
	// originals are not on GitHub to return to. The rows stay purple, because
	// a whole list in red reads as an error rather than as a decision.
	ansiAlarm = "\x1b[1;31m"
)

func init() { usePalette(os.Getenv("TERM"), os.Getenv("COLORTERM")) }

// usePalette narrows the palette to the sixteen colours every terminal has
// when the environment does not claim more.
//
// Split out and taking its inputs as arguments so the choice can be tested
// without touching the process environment.
func usePalette(term, colorterm string) {
	if supports256(term, colorterm) {
		return
	}
	// Magenta and red stand in for the two shades that need 256 colours. They
	// are further apart than the originals rather than closer, so the
	// distinction survives the downgrade even though the hues do not.
	ansiVisibility = "\x1b[33m"
	ansiRedacted = "\x1b[35m"
	ansiBothWays = "\x1b[31m"
}

// supports256 reports whether the terminal advertises more than sixteen
// colours.
//
// Read from the environment rather than probed, because probing means writing
// a query to the terminal and waiting for an answer that may never come -- and
// guessing wrong here costs a slightly duller screen, not a broken one.
func supports256(term, colorterm string) bool {
	if strings.Contains(colorterm, "truecolor") || strings.Contains(colorterm, "24bit") {
		return true
	}
	return strings.Contains(term, "256")
}
