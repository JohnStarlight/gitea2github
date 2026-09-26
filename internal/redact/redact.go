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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Domain is where redacted addresses are pointed. It is fixed rather than
// configurable, for two reasons.
//
// ".invalid" is reserved by RFC 2606 and is guaranteed never to resolve, so a
// redacted address can never accidentally become a real mailbox belonging to
// someone else. A domain chosen by whoever ran the migration cannot promise
// that, which is reason enough on its own.
//
// The second reason is that the shape of a redacted address is read back
// later. Whether a repository was rewritten is settled by its commits, not
// its addresses -- a history redacted with only its owner's address kept has
// none in this shape -- but when the commits cannot be compared, recognising
// the addresses themselves is what is left. A shape that varies from one run
// to the next is not a shape that can be recognised.
const Domain = "redacted.invalid"

// AddressPattern matches an address this package produced.
//
// This is a contract, not an implementation detail. It is the fallback for
// telling a rewritten history from the original when the commits themselves
// cannot be compared, and changing the shape below silently strands every
// repository redacted before the change.
var AddressPattern = regexp.MustCompile(`^[0-9a-f]{10}@` + regexp.QuoteMeta(Domain) + `$`)

// IsRedacted reports whether addr is one this package produced.
func IsRedacted(addr string) bool {
	return AddressPattern.MatchString(strings.ToLower(strings.TrimSpace(addr)))
}

// maxMessageBytes caps how large a commit message may be before we stop trying
// to scan it and pass it through untouched. Messages are normally a few hundred
// bytes; anything past this is pathological, and buffering it whole to run a
// regex over it would be the worse failure.
const maxMessageBytes = 4 << 20

// emailPattern matches addresses in free text. It is deliberately conservative:
// over-matching would corrupt commit messages, which is a far worse outcome
// than leaving an exotic address in place.
var emailPattern = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

// FindAddresses returns the distinct addresses in b, in the order they first
// appear, found the way commit messages are searched.
func FindAddresses(b []byte) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range emailPattern.FindAll(b, -1) {
		addr := string(m)
		if key := strings.ToLower(addr); !seen[key] {
			seen[key] = true
			out = append(out, addr)
		}
	}
	return out
}

// Mapper decides what each address becomes within one repository, and
// remembers its decisions so the same person maps to the same replacement
// throughout that repository's history.
type Mapper struct {
	// mine are the addresses belonging to whoever is running the migration,
	// and as is the one they all become.
	mine map[string]bool
	as   string

	// key is the secret the replacements are computed with. Without one, a
	// replacement is a hash anybody can compute, and so anybody with a guess
	// at an address -- a classmate's, say, from a list of logins -- can
	// confirm it. Each repository gets its own, so the same person is not
	// recognisable as the same person across repositories either.
	key []byte

	mu       sync.Mutex
	assigned map[string]string

	// seen counts distinct addresses across every Mapper made from one with
	// ForRepository, for the run's summary.
	seen *addressSet
}

type addressSet struct {
	mu sync.Mutex
	m  map[string]bool
}

func (s *addressSet) add(addr string) {
	s.mu.Lock()
	s.m[addr] = true
	s.mu.Unlock()
}

// newKey returns a fresh random secret. It is never stored or shown: once a
// repository is redacted, nothing needs it again.
func newKey() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic("redact: no randomness available: " + err.Error())
	}
	return key
}

// NewMapper builds a Mapper. Addresses in mine become as, and every other
// address becomes a hash.
//
// Rewriting rather than preserving is what makes the promise work. GitHub
// attributes a commit to an account only when its address is one that account
// has verified, or its no-reply; an address merely left alone -- a Gitea
// no-reply, say -- is hidden but shows as nobody, with no avatar and no link.
// Preserving therefore bought attribution only by publishing the real address
// it was supposed to hide.
//
// An empty as falls back to leaving those addresses as they are, which is all
// that can be done when the destination account is not known.
func NewMapper(mine []string, as string) *Mapper {
	k := make(map[string]bool, len(mine))
	for _, addr := range mine {
		addr = strings.ToLower(strings.TrimSpace(addr))
		if addr != "" {
			k[addr] = true
		}
	}
	// Trimmed but not lowercased: the lookup keys are folded so that an
	// address matches however it was typed, but the replacement is written
	// into every commit and a mangled login reads as a mistake.
	return &Mapper{mine: k, as: strings.TrimSpace(as), key: newKey(),
		assigned: map[string]string{}, seen: &addressSet{m: map[string]bool{}}}
}

// ForRepository returns a Mapper for one repository: the same addresses kept
// as yours, a secret of its own. A migration takes one per repository.
func (m *Mapper) ForRepository() *Mapper {
	return &Mapper{mine: m.mine, as: m.as, key: newKey(),
		assigned: map[string]string{}, seen: m.seen}
}

// Redacted returns the replacement for one address.
//
// The replacement is a truncated HMAC-SHA256 of the address under this
// repository's secret, rather than a counter or a scrubbed version of the
// original. The same person gets the same replacement throughout the
// repository, so `git shortlog` still separates contributors; nothing of the
// original address survives; and, the secret being gone, nobody can test a
// guess against it.
func (m *Mapper) Redacted(addr string) string {
	key := strings.ToLower(strings.TrimSpace(addr))
	if key == "" {
		return addr
	}
	if m.mine[key] {
		if m.as == "" {
			return addr
		}
		return m.as
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.assigned[key]; ok {
		return existing
	}
	mac := hmac.New(sha256.New, m.key)
	mac.Write([]byte(key))
	replacement := hex.EncodeToString(mac.Sum(nil)[:5]) + "@" + Domain
	m.assigned[key] = replacement
	m.seen.add(key)
	return replacement
}

// Count reports how many distinct addresses were replaced across the run --
// this Mapper and every one made from it -- for the run summary.
func (m *Mapper) Count() int {
	m.seen.mu.Lock()
	defer m.seen.mu.Unlock()
	return len(m.seen.m)
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
