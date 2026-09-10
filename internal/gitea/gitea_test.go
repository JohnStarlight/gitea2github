package gitea

import "testing"

// TestNewBaseURL checks the API path handling. Zone01's Gitea lives under a
// /git sub-path rather than at the domain root, so appending "/api/v1" blindly
// or assuming the root are both wrong.
func TestNewBaseURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://platform.zone01.gr/git", "https://platform.zone01.gr/git/api/v1"},
		{"https://platform.zone01.gr/git/", "https://platform.zone01.gr/git/api/v1"},
		{"https://gitea.example.com", "https://gitea.example.com/api/v1"},
		// An explicit API root must be left alone rather than doubled up.
		{"https://gitea.example.com/api/v1", "https://gitea.example.com/api/v1"},
	}
	for _, c := range cases {
		if got := New(c.in, "t").BaseURL; got != c.want {
			t.Errorf("New(%q).BaseURL = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestOwnedBy covers the check that separates a user's own work from a team
// repository they were merely added to - the distinction that decides whether
// re-publishing it under their GitHub account is appropriate.
func TestOwnedBy(t *testing.T) {
	r := Repo{}
	r.Owner.Login = "ivogiake"

	if !r.OwnedBy("ivogiake") {
		t.Error("exact match should be owned")
	}
	if !r.OwnedBy("IVOGIAKE") {
		t.Error("Gitea logins are case-insensitive, so the check must be too")
	}
	if r.OwnedBy("ppetraki") {
		t.Error("a different login must not be reported as owner")
	}
}
