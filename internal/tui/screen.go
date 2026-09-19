package tui

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"

	"golang.org/x/term"
)

// ErrCancelled reports that the user left the screen without confirming.
var ErrCancelled = errors.New("cancelled at the selection screen")

// ErrNoTerminal reports that the screen cannot run here, so the caller should
// fall back to the numbered prompts.
var ErrNoTerminal = errors.New("not a terminal")

// Available reports whether the interactive screen can run on the current
// streams.
//
// Both directions have to be a terminal: reading keys needs a real stdin, and
// repainting needs a stdout that is not a file somebody is going to read
// later.
func Available() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// Run opens the screen, drives it until the user confirms or quits, and
// returns the model holding their answers.
//
// The terminal is put into raw mode so that single keystrokes arrive without
// waiting for Enter, which means this function owns a global, hostile piece of
// state: a raw terminal left behind shows no typing and no newlines, and the
// user has to type `reset` blind to recover. Every exit path therefore runs
// through the same restore, including panics and Ctrl-C.
func Run(header string, m *Model) (*Model, error) {
	if !Available() {
		return nil, ErrNoTerminal
	}

	fd := int(os.Stdin.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil, fmt.Errorf("putting the terminal into raw mode: %w", err)
	}

	out := os.Stdout
	// sync.Once rather than a plain flag: the signal handler runs on its own
	// goroutine and would otherwise race the deferred call, and restoring the
	// terminal twice is not something to leave to chance.
	var once sync.Once
	restore := func() {
		once.Do(func() {
			// Leave the alternate screen before restoring the mode, so the
			// shell prompt reappears where the user left it.
			fmt.Fprint(out, ansiShowCursor+ansiExitAltScreen)
			_ = term.Restore(fd, state)
		})
	}
	// Runs on every ordinary return, and on a panic while the stack unwinds.
	defer restore()

	// A signal arriving mid-frame would otherwise kill the process with the
	// terminal still raw. Catching it lets the same restore run first.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	defer signal.Stop(signals)
	go func() {
		if _, ok := <-signals; ok {
			restore()
			os.Exit(130) // 128 + SIGINT, the shell convention
		}
	}()

	if w, h, sizeErr := term.GetSize(int(out.Fd())); sizeErr == nil {
		m.SetSize(w, h)
	}

	fmt.Fprint(out, ansiEnterAltScreen+ansiHideCursor)

	in := bufio.NewReader(os.Stdin)
	for {
		// Re-read the size every frame rather than handling SIGWINCH, which
		// does not exist on Windows. A redraw only happens on a keystroke, so
		// this costs one cheap ioctl per key and keeps the package free of
		// per-platform code.
		if w, h, sizeErr := term.GetSize(int(out.Fd())); sizeErr == nil {
			m.SetSize(w, h)
		}

		fmt.Fprint(out, ansiClear+m.View(header))

		key, readErr := ReadKey(in)
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				// Input ended without an answer, which is not consent.
				return nil, ErrCancelled
			}
			return nil, readErr
		}

		m.Update(key)
		if m.Done() {
			if m.Cancelled() {
				return nil, ErrCancelled
			}
			return m, nil
		}
	}
}

// Terminal control sequences. Plain strings: there is no styling library here
// and these six are the whole vocabulary.
const (
	ansiEnterAltScreen = "\x1b[?1049h"
	ansiExitAltScreen  = "\x1b[?1049l"
	ansiHideCursor     = "\x1b[?25l"
	ansiShowCursor     = "\x1b[?25h"
	ansiClear          = "\x1b[H\x1b[2J"
)
