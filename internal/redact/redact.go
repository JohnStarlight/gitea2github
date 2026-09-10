// Package redact rewrites the email addresses inside a repository's history.
//
// Publishing a Gitea repository on GitHub exposes every address in it. For a
// solo project that is only your own address, but a group project carries the
// personal email of every teammate who ever committed, and none of them agreed
// to have it republished under your account.
//
// Addresses hide in two places, and redacting only the first is a trap:
//
//   - the author and committer identity headers, which is what `git log` shows;
//   - the commit message body, where "Co-authored-by: Name <addr>" trailers are
//     extremely common and just as public.
//
// The rewrite works on a `git fast-export` stream, which is the only mechanism
// that can change historical commits without external tooling such as
// git-filter-repo.
package redact

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// DefaultDomain is where redacted addresses are pointed.
//
// ".invalid" is reserved by RFC 2606 and is guaranteed never to resolve, so a
// redacted address can never accidentally become a real mailbox belonging to
// someone else. A made-up domain cannot promise that.
const DefaultDomain = "redacted.invalid"

// maxMessageBytes caps how large a commit message may be before we stop trying
// to scan it and pass it through untouched. Messages are normally a few hundred
// bytes; anything past this is pathological, and buffering it whole to run a
// regex over it would be the worse failure.
const maxMessageBytes = 4 << 20

// emailPattern matches addresses in free text. It is deliberately conservative:
// over-matching would corrupt commit messages, which is a far worse outcome
// than leaving an exotic address in place.
var emailPattern = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

// Mapper decides what each address becomes, and remembers its decisions so the
// same person maps to the same replacement everywhere.
type Mapper struct {
	domain string
	keep   map[string]bool

	// Workers migrate repositories concurrently and each one filters its own
	// stream, so the assignment table needs a lock.
	mu       sync.Mutex
	assigned map[string]string
}

// NewMapper builds a Mapper. Addresses in keep are passed through untouched --
// that is how you keep your own commits linked to your GitHub profile while
// redacting everyone else's.
func NewMapper(domain string, keep []string) *Mapper {
	if domain == "" {
		domain = DefaultDomain
	}
	k := make(map[string]bool, len(keep))
	for _, addr := range keep {
		addr = strings.ToLower(strings.TrimSpace(addr))
		if addr != "" {
			k[addr] = true
		}
	}
	return &Mapper{domain: domain, keep: k, assigned: map[string]string{}}
}

// Redacted returns the replacement for one address.
//
// The replacement is a truncated SHA-256 of the address rather than a counter
// or a scrubbed version of the original. That buys three things: the same
// person gets the same replacement in every repository migrated, so history
// stays coherent across a whole account; distinct people stay distinct, so
// `git shortlog` still separates them; and nothing of the original address
// survives, which a "first.last@..." style local part would not manage.
func (m *Mapper) Redacted(addr string) string {
	key := strings.ToLower(strings.TrimSpace(addr))
	if key == "" || m.keep[key] {
		return addr
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.assigned[key]; ok {
		return existing
	}
	sum := sha256.Sum256([]byte(key))
	replacement := hex.EncodeToString(sum[:5]) + "@" + m.domain
	m.assigned[key] = replacement
	return replacement
}

// Count reports how many distinct addresses were replaced, for the run summary.
func (m *Mapper) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.assigned)
}

// rewriteIdentity rewrites one author/committer/tagger header, which git writes
// as:
//
//	author Some Name <addr@example.com> 1717000000 +0300
//
// The address is located from the last angle brackets rather than the first,
// because a display name is allowed to contain them and the timestamp that
// follows never does.
func (m *Mapper) rewriteIdentity(line string) string {
	closing := strings.LastIndex(line, ">")
	if closing < 0 {
		return line
	}
	opening := strings.LastIndex(line[:closing], "<")
	if opening < 0 {
		return line
	}
	if addr := line[opening+1 : closing]; addr != "" {
		return line[:opening+1] + m.Redacted(addr) + line[closing:]
	}
	return line
}

// redactText replaces every address found in free text, such as a commit
// message body.
func (m *Mapper) redactText(b []byte) []byte {
	return emailPattern.ReplaceAllFunc(b, func(match []byte) []byte {
		return []byte(m.Redacted(string(match)))
	})
}

// FilterStream rewrites a `git fast-export` stream, replacing addresses as it
// goes, and writes the result in a form `git fast-import` accepts.
//
// The stream interleaves text commands with raw binary payloads, so this cannot
// be a line-based filter. Every payload is introduced by a "data <n>" header,
// and the n bytes that follow are copied verbatim without ever being examined
// as lines. A naive line filter would corrupt any file whose contents happened
// to contain a line beginning with "author ", and would do so silently.
//
// The one payload that is rewritten is a commit or tag message, and only
// because the preceding command tells us it is text. Anything we are not
// certain about is treated as opaque.
func FilterStream(in io.Reader, out io.Writer, m *Mapper) error {
	r := bufio.NewReaderSize(in, 1<<16)
	w := bufio.NewWriterSize(out, 1<<16)

	// Defaults to false so that an unrecognised command leaves the payload
	// after it untouched. Guessing wrong in this direction leaves an address
	// in place; guessing wrong in the other corrupts a file.
	nextDataIsMessage := false

	for {
		line, err := r.ReadString('\n')

		if len(line) > 0 {
			body := strings.TrimSuffix(line, "\n")
			payloadHandled := false

			switch {
			case strings.HasPrefix(body, "data "):
				if cerr := copyData(r, w, body, nextDataIsMessage, m); cerr != nil {
					return cerr
				}
				nextDataIsMessage = false
				payloadHandled = true

			case body == "blob":
				// A blob payload is file content and must never be touched.
				nextDataIsMessage = false

			case strings.HasPrefix(body, "commit "), strings.HasPrefix(body, "tag "):
				// The next payload is this object's message.
				nextDataIsMessage = true

			case strings.HasPrefix(body, "M ") && strings.Contains(body, " inline"):
				// An inline filemodify carries file content, not a message,
				// even though it appears in the middle of a commit.
				nextDataIsMessage = false

			case strings.HasPrefix(body, "author "),
				strings.HasPrefix(body, "committer "),
				strings.HasPrefix(body, "tagger "):
				line = m.rewriteIdentity(body) + "\n"
			}

			if !payloadHandled {
				if _, werr := w.WriteString(line); werr != nil {
					return werr
				}
			}
		}

		if err == io.EOF {
			return w.Flush()
		}
		if err != nil {
			return err
		}
	}
}

// copyData handles one "data <n>" payload, rewriting it only when it is known
// to be a message.
func copyData(r *bufio.Reader, w *bufio.Writer, header string, isMessage bool, m *Mapper) error {
	spec := strings.TrimPrefix(header, "data ")
	if strings.HasPrefix(spec, "<<") {
		// git fast-export never emits the delimited form, so encountering it
		// means the input is not what we think it is. Refusing beats writing
		// a subtly wrong history.
		return fmt.Errorf("unsupported delimited data block %q in fast-export stream", header)
	}
	n, err := strconv.ParseInt(spec, 10, 64)
	if err != nil {
		return fmt.Errorf("malformed data header %q: %w", header, err)
	}

	if !isMessage || n > maxMessageBytes {
		// Pass through untouched, streaming rather than buffering so that a
		// large file does not have to fit in memory.
		if _, err := fmt.Fprintf(w, "data %d\n", n); err != nil {
			return err
		}
		if _, err := io.CopyN(w, r, n); err != nil {
			return fmt.Errorf("copying %d bytes of payload: %w", n, err)
		}
		return nil
	}

	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return fmt.Errorf("reading %d byte message: %w", n, err)
	}
	redacted := m.redactText(buf)
	if _, err := fmt.Fprintf(w, "data %d\n", len(redacted)); err != nil {
		return err
	}
	_, err = w.Write(redacted)
	return err
}
