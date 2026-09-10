package github

import "testing"

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
