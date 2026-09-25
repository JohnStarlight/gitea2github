package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/JohnStarlight/gitea2github/internal/github"
)

// scopesReply answers GET /user with the scopes header set, or absent when
// scopes is nil -- as for a fine-grained token.
type scopesReply struct{ scopes *string }

func (s scopesReply) RoundTrip(*http.Request) (*http.Response, error) {
	h := http.Header{}
	if s.scopes != nil {
		h.Set("X-OAuth-Scopes", *s.scopes)
	}
	return &http.Response{StatusCode: 200, Status: "200 OK", Header: h,
		Body: io.NopCloser(strings.NewReader(`{"login":"me","id":1}`))}, nil
}

// TestDoctorReadsTheScopes checks what doctor says for each kind of token,
// down to the remedy it offers.
func TestDoctorReadsTheScopes(t *testing.T) {
	str := func(s string) *string { return &s }
	cases := []struct {
		name   string
		scopes *string
		source string
		want   string // the report, one check per line
	}{
		{"everything", str("gist, read:org, repo, workflow"), "gh CLI",
			"ok scopes"},
		{"no workflow", str("repo"), "gh CLI",
			"ok scopes\nwarn workflow\nhint gh auth refresh -s repo,workflow"},
		{"public only", str("public_repo, workflow"), "GITHUB_TOKEN env",
			"warn scopes\nhint create a token with the repo and workflow scopes"},
		{"nothing useful", str("gist"), "gh CLI",
			"fail scopes\nhint gh auth refresh -s repo,workflow"},
		{"fine-grained", nil, "GITHUB_TOKEN env",
			"ok scopes"},
	}
	for _, c := range cases {
		var got []string
		record := func(kind string) func(string, string, ...any) {
			return func(check, _ string, _ ...any) { got = append(got, kind+" "+check) }
		}
		hint := func(format string, args ...any) { got = append(got, "hint "+fmt.Sprintf(format, args...)) }

		gh := github.New("tok")
		gh.HTTP = &http.Client{Transport: scopesReply{c.scopes}}
		checkGitHubScopes(context.Background(), gh, c.source, record("ok"), record("warn"), record("fail"), hint)

		if strings.Join(got, "\n") != c.want {
			t.Errorf("%s:\n%s\nwant\n%s", c.name, strings.Join(got, "\n"), c.want)
		}
	}
}
