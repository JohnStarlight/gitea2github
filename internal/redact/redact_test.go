package redact

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
)

// TestRedactedIsStableAndDistinct covers the two properties the replacement
// scheme promises: the same person always gets the same replacement, and two
// different people never collide into one identity.
func TestRedactedIsStableAndDistinct(t *testing.T) {
	a := NewMapper(nil, "")
	b := NewMapper(nil, "")

	first := a.Redacted("alice@example.com")
	if again := a.Redacted("alice@example.com"); again != first {
		t.Errorf("same address mapped twice within one Mapper: %q then %q", first, again)
	}
	// Stability across Mappers is what keeps a whole account's migrated
	// repositories consistent with each other.
	if other := b.Redacted("ALICE@example.com"); other != first {
		t.Errorf("mapping is not stable across Mappers or not case-insensitive: %q vs %q", other, first)
	}
	if bob := a.Redacted("bob@example.com"); bob == first {
		t.Error("two different addresses collided into one replacement")
	}
	if !strings.HasSuffix(first, "@"+Domain) {
		t.Errorf("replacement %q does not use the reserved domain", first)
	}
	if strings.Contains(first, "alice") {
		t.Errorf("replacement %q leaks the original local part", first)
	}
}

// TestKeepList checks that your own address survives, which is what keeps your
// commits linked to your GitHub profile.
func TestKeepList(t *testing.T) {
	m := NewMapper([]string{"Me@Example.com"}, "")
	if got := m.Redacted("me@example.com"); got != "me@example.com" {
		t.Errorf("kept address was redacted to %q", got)
	}
	if got := m.Redacted("other@example.com"); got == "other@example.com" {
		t.Error("address outside the keep list was not redacted")
	}
}

// TestFilterStreamRewritesIdentitiesAndMessages is the end-to-end check on a
// hand-built fast-export stream.
//
// The blob deliberately contains a line that looks exactly like an author
// header. A line-based filter would rewrite it and silently corrupt the file;
// this asserts the payload survives byte for byte.
func TestFilterStreamRewritesIdentitiesAndMessages(t *testing.T) {
	blob := "author Sneaky <sneaky@example.com> 1717000000 +0000\nnot a real header\n"
	message := "Fix the parser\n\nCo-authored-by: Bob <bob@example.com>\n"

	var in bytes.Buffer
	in.WriteString("blob\nmark :1\ndata " + strconv.Itoa(len(blob)) + "\n" + blob)
	in.WriteString("commit refs/heads/main\nmark :2\n")
	in.WriteString("author Alice <alice@example.com> 1717000000 +0000\n")
	in.WriteString("committer Alice <alice@example.com> 1717000000 +0000\n")
	in.WriteString("data " + strconv.Itoa(len(message)) + "\n" + message)
	in.WriteString("M 100644 :1 file.txt\n\ndone\n")

	m := NewMapper(nil, "")
	var out bytes.Buffer
	if err := FilterStream(&in, &out, m); err != nil {
		t.Fatalf("FilterStream: %v", err)
	}
	got := out.String()

	// The blob must be untouched, header line and byte count alike.
	wantBlob := "data " + strconv.Itoa(len(blob)) + "\n" + blob
	if !strings.Contains(got, wantBlob) {
		t.Error("blob payload was modified; a file containing an author-like line got corrupted")
	}

	// Identity headers must be rewritten.
	if strings.Contains(got, "alice@example.com") {
		t.Error("author/committer address survived redaction")
	}
	// So must addresses inside the commit message, which is where
	// Co-authored-by trailers put a collaborator's real address.
	if strings.Contains(got, "bob@example.com") {
		t.Error("Co-authored-by address in the commit message survived redaction")
	}

	// Rewriting the message changes its length, so the data header must have
	// been recomputed or fast-import would reject the stream.
	wantMessage := strings.ReplaceAll(message, "bob@example.com", m.Redacted("bob@example.com"))
	wantHeader := "data " + strconv.Itoa(len(wantMessage)) + "\n" + wantMessage
	if !strings.Contains(got, wantHeader) {
		t.Error("commit message payload length was not recomputed after redaction")
	}

	if m.Count() != 2 {
		t.Errorf("expected 2 distinct addresses redacted, got %d", m.Count())
	}
}

// TestFilterStreamLeavesInlineContentAlone guards the other payload that looks
// textual but is file content.
func TestFilterStreamLeavesInlineContentAlone(t *testing.T) {
	content := "contact: inline@example.com\n"

	var in bytes.Buffer
	in.WriteString("commit refs/heads/main\nmark :1\n")
	in.WriteString("committer Alice <alice@example.com> 1717000000 +0000\n")
	in.WriteString("data 3\nhi\n")
	in.WriteString("M 100644 inline README\n")
	in.WriteString("data " + strconv.Itoa(len(content)) + "\n" + content)
	in.WriteString("\ndone\n")

	var out bytes.Buffer
	if err := FilterStream(&in, &out, NewMapper(nil, "")); err != nil {
		t.Fatalf("FilterStream: %v", err)
	}
	if !strings.Contains(out.String(), content) {
		t.Error("inline file content was redacted; only commit messages should be")
	}
}

// TestFilterStreamRejectsDelimitedData confirms we refuse a stream shape we
// cannot safely rewrite rather than guessing.
func TestFilterStreamRejectsDelimitedData(t *testing.T) {
	in := strings.NewReader("blob\ndata <<EOF\nhello\nEOF\n")
	var out bytes.Buffer
	if err := FilterStream(in, &out, NewMapper(nil, "")); err == nil {
		t.Error("expected an error for a delimited data block, got nil")
	}
}

// TestTheAddressShapeIsAContract is the test that must be argued with before
// the shape of a redacted address is changed.
//
// A repository on GitHub is the only record of how it was redacted. Recovering
// that -- to rewrite a local clone to match, or to tell which addresses were
// deliberately left alone -- means recognising a redacted address by looking at
// it. Every repository redacted before a change to this shape becomes
// unreadable by everything after it.
func TestTheAddressShapeIsAContract(t *testing.T) {
	got := NewMapper(nil, "").Redacted("student@zone01.gr")

	if want := "e9e3c54b5e@redacted.invalid"; got != want {
		t.Errorf("the redacted form of a known address is %q, want %q.\n"+
			"If this was deliberate: every repository redacted with the old shape "+
			"can no longer be recognised, and a local rewrite of one will not "+
			"reproduce its commits.", got, want)
	}
	if !IsRedacted(got) {
		t.Errorf("%q is not recognised as redacted by this package's own pattern", got)
	}
}

// TestRealAddressesAreNotMistakenForRedactedOnes covers the other direction.
// An address wrongly read as redacted would be left off the list of addresses
// to preserve, and a rewrite would then produce commits that do not match what
// is already on GitHub.
func TestRealAddressesAreNotMistakenForRedactedOnes(t *testing.T) {
	for _, addr := range []string{
		"student@zone01.gr",
		"me@example.com",
		"0123456789@example.com",          // the right shape, the wrong domain
		"e9e3c54b5e@redacted.invalid.com", // a domain that merely starts the same
		"e9e3c54@redacted.invalid",        // too short
		"e9e3c54b5ee@redacted.invalid",    // too long
		"E9E3C54B5G@redacted.invalid",     // not hexadecimal
		"",
	} {
		if IsRedacted(addr) {
			t.Errorf("%q was taken for an address this package produced", addr)
		}
	}
}

// TestTheDomainCannotBeChosen guards the reason it is fixed. A domain supplied
// by whoever ran the migration could be one that resolves, turning a redacted
// address into a real mailbox belonging to somebody else -- and a shape that
// varies from run to run cannot be recognised later.
func TestTheDomainCannotBeChosen(t *testing.T) {
	if Domain != "redacted.invalid" {
		t.Errorf("Domain = %q, want redacted.invalid (RFC 2606 reserves .invalid, "+
			"so it can never become a real mailbox)", Domain)
	}
	for _, addr := range []string{"a@b.com", "someone@zone01.gr", "x@y.co.uk"} {
		if got := NewMapper(nil, "").Redacted(addr); !strings.HasSuffix(got, "@"+Domain) {
			t.Errorf("%s redacted to %q, which is not on the fixed domain", addr, got)
		}
	}
}

// TestKeptAddressesStayRecognisablyReal is what makes the keep list
// recoverable: an address that was preserved does not look like one that was
// replaced.
func TestKeptAddressesStayRecognisablyReal(t *testing.T) {
	m := NewMapper([]string{"me@example.com"}, "")

	if got := m.Redacted("me@example.com"); IsRedacted(got) {
		t.Errorf("a kept address came back looking redacted: %q", got)
	}
	if got := m.Redacted("someone@zone01.gr"); !IsRedacted(got) {
		t.Errorf("a redacted address does not look redacted: %q", got)
	}
}

// TestYourOwnAddressesBecomeYourNoReply is the promise the keep list has
// always made and could not previously deliver. GitHub attributes a commit to
// an account only when its address is one that account has verified, or its
// no-reply; an address merely left alone shows as nobody, with no avatar and
// no link. Preserving therefore bought attribution only by publishing the real
// address it was meant to hide.
func TestYourOwnAddressesBecomeYourNoReply(t *testing.T) {
	const noreply = "259051186+JohnStarlight@users.noreply.github.com"
	m := NewMapper([]string{
		"john.vogiakelis@gmail.com",
		"ivogiake@noreply.platform.zone01.gr",
	}, noreply)

	for _, mine := range []string{"john.vogiakelis@gmail.com", "ivogiake@noreply.platform.zone01.gr"} {
		got := m.Redacted(mine)
		if got != noreply {
			t.Errorf("%s became %q, want the no-reply address", mine, got)
		}
		if got == mine {
			t.Errorf("%s was left as it was, which links to nobody on GitHub", mine)
		}
	}

	// Everyone else still becomes a hash.
	if got := m.Redacted("teammate@example.com"); !IsRedacted(got) {
		t.Errorf("somebody else's address became %q, want a hash", got)
	}
}

// TestTheReplacementKeepsItsCapitals guards what goes into every commit. The
// lookup folds case so an address matches however it was typed, but a login
// written back in lower case reads as a mistake.
func TestTheReplacementKeepsItsCapitals(t *testing.T) {
	const noreply = "259051186+JohnStarlight@users.noreply.github.com"
	m := NewMapper([]string{"John.Vogiakelis@Gmail.com"}, noreply)

	if got := m.Redacted("john.vogiakelis@gmail.com"); got != noreply {
		t.Errorf("a differently-capitalised address gave %q, want %q", got, noreply)
	}
}

// TestWithoutADestinationNothingIsInvented covers the fallback. When the
// GitHub account is not known there is no address to rewrite to, and making
// one up would attribute commits to nobody at all.
func TestWithoutADestinationNothingIsInvented(t *testing.T) {
	m := NewMapper([]string{"me@example.com"}, "")

	if got := m.Redacted("me@example.com"); got != "me@example.com" {
		t.Errorf("with no destination, the address became %q, want it left alone", got)
	}
	if got := m.Redacted("them@example.com"); !IsRedacted(got) {
		t.Errorf("somebody else's address became %q, want a hash", got)
	}
}

// TestOneReplacementForEveryAddressOfYours is what makes a history readable
// afterwards: several addresses of one person collapse into one author rather
// than into several strangers.
func TestOneReplacementForEveryAddressOfYours(t *testing.T) {
	const noreply = "1+me@users.noreply.github.com"
	m := NewMapper([]string{"a@example.com", "b@example.com", "c@example.com"}, noreply)

	seen := map[string]bool{}
	for _, addr := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		seen[m.Redacted(addr)] = true
	}
	if len(seen) != 1 {
		t.Errorf("three addresses of one person became %d authors: %v", len(seen), seen)
	}
}
