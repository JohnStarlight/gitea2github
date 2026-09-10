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
	a := NewMapper("", nil)
	b := NewMapper("", nil)

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
	if !strings.HasSuffix(first, "@"+DefaultDomain) {
		t.Errorf("replacement %q does not use the reserved domain", first)
	}
	if strings.Contains(first, "alice") {
		t.Errorf("replacement %q leaks the original local part", first)
	}
}

// TestKeepList checks that your own address survives, which is what keeps your
// commits linked to your GitHub profile.
func TestKeepList(t *testing.T) {
	m := NewMapper("", []string{"Me@Example.com"})
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

	m := NewMapper("", nil)
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
	if err := FilterStream(&in, &out, NewMapper("", nil)); err != nil {
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
	if err := FilterStream(in, &out, NewMapper("", nil)); err == nil {
		t.Error("expected an error for a delimited data block, got nil")
	}
}
