package tui

import (
	"bufio"
	"strings"
	"unicode/utf8"
)

// Key is one decoded keypress.
//
// Arrow keys arrive as multi-byte escape sequences rather than as runes, so a
// key cannot simply be a rune: the type has to be able to say "this was Up"
// without pretending some character was typed.
type Key struct {
	Kind KeyKind
	Rune rune
}

// KeyKind enumerates the keys the screen reacts to. Anything else decodes to
// KeyRune or is ignored.
type KeyKind int

const (
	KeyRune KeyKind = iota
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyHome
	KeyEnd
	KeyPageUp
	KeyPageDown
	KeyEnter
	KeyEscape
	KeyBackspace
	KeySpace
	KeyTab
	KeyCtrlC
	KeyUnknown
)

// ReadKey decodes one keypress from r.
//
// Escape is ambiguous on a terminal: pressing the Escape key and the start of
// an arrow-key sequence both begin with 0x1b. The two are told apart by
// looking at what follows, which is safe here because a terminal delivers the
// bytes of a sequence together -- the buffered reader already holds them by
// the time the first byte is read.
func ReadKey(r *bufio.Reader) (Key, error) {
	b, err := r.ReadByte()
	if err != nil {
		return Key{}, err
	}

	switch b {
	case 0x03:
		return Key{Kind: KeyCtrlC}, nil
	case '\r', '\n':
		return Key{Kind: KeyEnter}, nil
	case 0x7f, 0x08:
		return Key{Kind: KeyBackspace}, nil
	case '\t':
		return Key{Kind: KeyTab}, nil
	case ' ':
		return Key{Kind: KeySpace}, nil
	case 0x1b:
		return readEscape(r)
	}

	// A byte below 0x20 that is not handled above is a control character the
	// screen has no use for; reporting it as a rune would let Ctrl-key
	// combinations masquerade as ordinary typing in the search box.
	if b < 0x20 {
		return Key{Kind: KeyUnknown}, nil
	}

	if b < utf8.RuneSelf {
		return Key{Kind: KeyRune, Rune: rune(b)}, nil
	}

	// Multi-byte UTF-8: put the lead byte back and let the reader decode the
	// whole rune, so that a non-ASCII repository name can be searched for.
	if err := r.UnreadByte(); err != nil {
		return Key{Kind: KeyUnknown}, nil
	}
	ru, _, err := r.ReadRune()
	if err != nil {
		return Key{}, err
	}
	return Key{Kind: KeyRune, Rune: ru}, nil
}

// readEscape decodes the tail of an escape sequence, having already consumed
// the leading 0x1b.
func readEscape(r *bufio.Reader) (Key, error) {
	// Nothing buffered behind the escape means the user pressed Escape itself.
	if r.Buffered() == 0 {
		return Key{Kind: KeyEscape}, nil
	}
	b, err := r.ReadByte()
	if err != nil {
		return Key{Kind: KeyEscape}, nil
	}
	if b != '[' && b != 'O' {
		return Key{Kind: KeyEscape}, nil
	}

	var seq strings.Builder
	for {
		c, err := r.ReadByte()
		if err != nil {
			return Key{Kind: KeyEscape}, nil
		}
		// A CSI sequence ends at the first byte in the 0x40-0x7e range; the
		// digits and semicolons before it are parameters this screen ignores.
		if c >= 0x40 && c <= 0x7e {
			seq.WriteByte(c)
			break
		}
		seq.WriteByte(c)
		if seq.Len() > 8 {
			// Runaway sequence: give up rather than block forever.
			return Key{Kind: KeyUnknown}, nil
		}
	}

	s := seq.String()
	switch s {
	case "A":
		return Key{Kind: KeyUp}, nil
	case "B":
		return Key{Kind: KeyDown}, nil
	case "C":
		return Key{Kind: KeyRight}, nil
	case "D":
		return Key{Kind: KeyLeft}, nil
	case "H", "1~", "7~":
		return Key{Kind: KeyHome}, nil
	case "F", "4~", "8~":
		return Key{Kind: KeyEnd}, nil
	case "5~":
		return Key{Kind: KeyPageUp}, nil
	case "6~":
		return Key{Kind: KeyPageDown}, nil
	}
	return Key{Kind: KeyUnknown}, nil
}
