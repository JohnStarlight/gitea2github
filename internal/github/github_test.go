package github

import (
	"fmt"
	"strings"
	"testing"
)

// TestSanitizeName pins down the name mapping, because a silent rename is the
// kind of bug that only shows up as a 404 much later, when someone tries to
// clone the repository the migrator claimed to have created.
func TestSanitizeName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"linear-stats", "linear-stats"}, // already legal, must pass through
		{"ascii_art.v2", "ascii_art.v2"}, // dot and underscore are allowed
		{"my repo", "my-repo"},           // spaces become dashes
		{"go/reloaded", "go-reloaded"},   // a slash would change the URL path
		{"prôject", "pr-ject"},           // non-ASCII is not accepted by GitHub
		{"", ""},                         // empty stays empty; the caller rejects it
	}
	for _, c := range cases {
		if got := SanitizeName(c.in); got != c.want {
			t.Errorf("SanitizeName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSanitizeTopics: Gitea allows topics GitHub refuses, and one refused
// topic fails the whole request.
func TestSanitizeTopics(t *testing.T) {
	in := []string{"Go", "zone01.gr", "--web--", "go", "", strings.Repeat("a", 60)}
	got := SanitizeTopics(in)
	want := []string{"go", "zone01-gr", "web", strings.Repeat("a", 50)}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("SanitizeTopics = %q, want %q", got, want)
	}
	many := make([]string, 30)
	for i := range many {
		many[i] = fmt.Sprintf("t%d", i)
	}
	if n := len(SanitizeTopics(many)); n != 20 {
		t.Errorf("kept %d topics, GitHub allows 20", n)
	}
}
