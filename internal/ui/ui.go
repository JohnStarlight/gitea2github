// Package ui handles the interactive half of the command line: asking questions
// and confirming before anything is changed.
//
// The rule the whole package exists to enforce is that a prompt must never be
// the only way forward. A tool that unconditionally asks a question is unusable
// from a script and hangs outright in CI, where nobody is there to answer, so
// every prompt has to know whether a human is actually present and fall back to
// a default when one is not.
package ui

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Prompter asks questions on a terminal.
type Prompter struct {
	in  *bufio.Reader
	out io.Writer

	// interactive records whether a human is on the other end. When false every
	// question returns its default immediately rather than blocking on input
	// that will never arrive.
	interactive bool
}

// New builds a Prompter bound to the real terminal.
func New() *Prompter {
	return &Prompter{
		in:          bufio.NewReader(os.Stdin),
		out:         os.Stdout,
		interactive: IsTerminal(os.Stdin) && IsTerminal(os.Stdout),
	}
}

// NewWith builds a Prompter over explicit streams, for tests.
func NewWith(in io.Reader, out io.Writer, interactive bool) *Prompter {
	return &Prompter{in: bufio.NewReader(in), out: out, interactive: interactive}
}

// Interactive reports whether questions will actually be asked.
func (p *Prompter) Interactive() bool { return p.interactive }

// IsTerminal reports whether f is attached to a terminal rather than a pipe or
// a file.
//
// Done through the file mode instead of an ioctl so that it stays pure Go and
// needs no third-party terminal package: a character device is a terminal, a
// redirect or a pipe is not.
func IsTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// Confirm asks a yes/no question. Without a terminal it returns def unasked.
func (p *Prompter) Confirm(question string, def bool) bool {
	if !p.interactive {
		return def
	}
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	for {
		fmt.Fprintf(p.out, "%s [%s] ", question, hint)
		answer, err := p.readLine()
		if err != nil {
			// EOF means the input ended mid-question, which is not consent.
			// Falling back to the default is the only safe reading.
			fmt.Fprintln(p.out)
			return def
		}
		switch strings.ToLower(answer) {
		case "":
			return def
		case "y", "yes":
			return true
		case "n", "no":
			return false
		}
		fmt.Fprintln(p.out, "  please answer y or n")
	}
}

// Line asks for a free-text answer, returning def if the user just presses
// enter.
func (p *Prompter) Line(question, def string) string {
	if !p.interactive {
		return def
	}
	if def != "" {
		fmt.Fprintf(p.out, "%s [%s] ", question, def)
	} else {
		fmt.Fprintf(p.out, "%s ", question)
	}
	answer, err := p.readLine()
	if err != nil || answer == "" {
		return def
	}
	return answer
}

// Option is one entry in a numbered choice.
type Option struct {
	Label string // short name, also what the answer means
	Help  string // one line explaining the consequence
}

// Choose presents a numbered list and returns the chosen index.
//
// Numbered rather than free text because the options here decide where a push
// ends up, and a typo silently matching the wrong branch of a switch is exactly
// the class of mistake this whole change is meant to prevent.
func (p *Prompter) Choose(question string, options []Option, def int) int {
	if !p.interactive || len(options) == 0 {
		return def
	}
	fmt.Fprintln(p.out, question)
	for i, opt := range options {
		marker := " "
		if i == def {
			marker = "*"
		}
		fmt.Fprintf(p.out, " %s %d) %-14s %s\n", marker, i+1, opt.Label, opt.Help)
	}
	for {
		fmt.Fprintf(p.out, "Choice [%d] ", def+1)
		answer, err := p.readLine()
		if err != nil {
			fmt.Fprintln(p.out)
			return def
		}
		if answer == "" {
			return def
		}
		if n, convErr := strconv.Atoi(answer); convErr == nil && n >= 1 && n <= len(options) {
			return n - 1
		}
		fmt.Fprintf(p.out, "  please enter a number between 1 and %d\n", len(options))
	}
}

// readLine reads one trimmed line of input.
func (p *Prompter) readLine() (string, error) {
	line, err := p.in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// ParseSelection turns a line like "1 3 5" into zero-based indices into a list
// of count items.
//
// Commas are accepted alongside spaces because people type both, and a
// selection is rejected whole rather than partially applied: silently ignoring
// the one number that was out of range would act on a set the user never
// chose.
func ParseSelection(input string, count int) ([]int, error) {
	fields := strings.FieldsFunc(input, func(r rune) bool {
		return r == ' ' || r == ',' || r == '\t'
	})

	seen := map[int]bool{}
	var indices []int
	for _, field := range fields {
		n, err := strconv.Atoi(field)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", field)
		}
		if n < 1 || n > count {
			return nil, fmt.Errorf("%d is not between 1 and %d", n, count)
		}
		// Listing the same number twice is a typo, not a request to act twice.
		if !seen[n] {
			seen[n] = true
			indices = append(indices, n-1)
		}
	}
	return indices, nil
}

// Select asks for a list of numbers and returns the chosen zero-based indices.
// An empty answer selects nothing, which is how "leave everything as it is"
// stays the path of least resistance.
func (p *Prompter) Select(question string, count int) []int {
	if !p.interactive || count == 0 {
		return nil
	}
	for {
		fmt.Fprintf(p.out, "%s ", question)
		answer, err := p.readLine()
		if err != nil {
			fmt.Fprintln(p.out)
			return nil
		}
		if answer == "" {
			return nil
		}
		indices, parseErr := ParseSelection(answer, count)
		if parseErr != nil {
			fmt.Fprintf(p.out, "  %v; try again, or press Enter to change nothing\n", parseErr)
			continue
		}
		return indices
	}
}
