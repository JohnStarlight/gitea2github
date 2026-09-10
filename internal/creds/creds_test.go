package creds

import "testing"

// TestDescribeHelpers checks the label that appears in doctor output. It used
// to be hardcoded to "osxkeychain", which was simply false on Linux and
// Windows and would have sent anyone there looking in the wrong place.
func TestDescribeHelpers(t *testing.T) {
	cases := []struct {
		name    string
		helpers []string
		want    string
	}{
		{"none", nil, "git credential helper"},
		{"macos", []string{"osxkeychain"}, "git credential helper (osxkeychain)"},
		{"windows", []string{"manager"}, "git credential helper (manager)"},
		{"linux", []string{"libsecret"}, "git credential helper (libsecret)"},
		{"chain", []string{"cache", "store"}, "git credential helper (cache, store)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := describeHelpers(c.helpers); got != c.want {
				t.Errorf("describeHelpers(%v) = %q, want %q", c.helpers, got, c.want)
			}
		})
	}
}

// TestConfiguredHelpersDeduplicates is an integration check against whatever
// git is installed. The same helper is very commonly set in both the system
// and the global gitconfig, and listing it twice would be noise in the one
// place a confused user goes looking.
func TestConfiguredHelpersDeduplicates(t *testing.T) {
	seen := map[string]bool{}
	for _, h := range configuredHelpers() {
		if seen[h] {
			t.Errorf("helper %q reported more than once", h)
		}
		seen[h] = true
	}
}
