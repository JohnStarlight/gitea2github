package gitea

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
	r.Owner.Login = "JohnStarlight"

	if !r.OwnedBy("JohnStarlight") {
		t.Error("exact match should be owned")
	}
	if !r.OwnedBy("JOHNSTARLIGHT") {
		t.Error("Gitea logins are case-insensitive, so the check must be too")
	}
	if r.OwnedBy("someone-else") {
		t.Error("a different login must not be reported as owner")
	}
}

// TestLogin checks that whose token it is comes from the server, and that a
// token without read:user is reported as a scope problem rather than as a
// broken token: the remedies are different.
func TestLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/user" {
			http.NotFound(w, r)
			return
		}
		switch r.Header.Get("Authorization") {
		case "token good":
			fmt.Fprint(w, `{"login":"JohnStarlight","email":"x@example.com"}`)
		case "token narrow":
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"token does not have at least one of required scope(s): [read:user]"}`)
		default:
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message":"invalid username, password or token"}`)
		}
	}))
	defer srv.Close()

	login, err := New(srv.URL, "good").Login(context.Background())
	if err != nil || login != "JohnStarlight" {
		t.Errorf("Login() = %q, %v; want JohnStarlight", login, err)
	}

	_, err = New(srv.URL, "narrow").Login(context.Background())
	var scopeErr *ScopeError
	if !errors.As(err, &scopeErr) {
		t.Errorf("narrow token: err = %v, want ScopeError", err)
	}

	_, err = New(srv.URL, "revoked").Login(context.Background())
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Errorf("revoked token: err = %v, want AuthError", err)
	}
}
