// Package gitea is a minimal client for the subset of the Gitea REST API that
// the migrator needs: listing the repositories a user owns or collaborates on.
//
// It deliberately depends only on the standard library. The full Gitea SDK
// pulls in a large dependency tree to model an API surface we touch three
// endpoints of, and hand-rolling those three keeps the project readable as a
// learning exercise.
package gitea

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// pageSize is how many repositories we request per API call. Gitea caps this at
// 50 by default (app.ini MAX_RESPONSE_ITEMS), so asking for more just gets
// silently truncated and would make our "is this the last page?" check wrong.
const pageSize = 50

// Client talks to one Gitea instance as one authenticated user.
type Client struct {
	// BaseURL is the API root, e.g. "https://platform.zone01.gr/git/api/v1".
	// Note that Zone01's Gitea is mounted under a /git sub-path rather than at
	// the domain root, which is exactly the kind of detail that makes
	// hardcoding "https://host/api/v1" break for real deployments.
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// Repo is the slice of Gitea's repository object we actually use. Gitea returns
// several dozen fields; decoding only these keeps the intent obvious and means
// an upstream schema addition can never break us.
type Repo struct {
	Name        string `json:"name"`
	FullName    string `json:"full_name"`
	Description string `json:"description"`
	Private     bool   `json:"private"`
	Empty       bool   `json:"empty"`
	Archived    bool   `json:"archived"`
	Fork        bool   `json:"fork"`
	CloneURL    string `json:"clone_url"`
	DefaultBr   string `json:"default_branch"`
	Owner       struct {
		Login string `json:"login"`
	} `json:"owner"`
	Permissions struct {
		Admin bool `json:"admin"`
		Push  bool `json:"push"`
		Pull  bool `json:"pull"`
	} `json:"permissions"`
}

// OwnedBy reports whether login is the repository's owner. This is the test
// that separates "my own work" from "a team project I was added to", which the
// migrator treats very differently: re-publishing someone else's repository
// under your own GitHub account is a decision the user has to make explicitly.
func (r Repo) OwnedBy(login string) bool {
	return strings.EqualFold(r.Owner.Login, login)
}

// New builds a client from a Gitea web URL such as
// "https://platform.zone01.gr/git", appending the API path if the caller has
// not already done so.
func New(baseURL, token string) *Client {
	trimmed := strings.TrimRight(baseURL, "/")
	if !strings.Contains(trimmed, "/api/v1") {
		trimmed += "/api/v1"
	}
	return &Client{
		BaseURL: trimmed,
		Token:   token,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// AuthError reports that the token was rejected outright: wrong, expired, or
// revoked. The remedy is to supply a different token.
//
// This is deliberately a separate type from ScopeError even though both arrive
// as 4xx responses, because telling a user to widen the scopes of a token that
// has been revoked sends them chasing the wrong problem entirely.
type AuthError struct {
	Endpoint string
	Message  string
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("gitea rejected the token on %s: %s", e.Endpoint, e.Message)
}

// ScopeError reports that the token authenticated correctly but is not allowed
// to perform the request. It is separated from generic HTTP failures because
// the remedy is completely different: the user must mint a new token with more
// scopes, not retry or check their network.
type ScopeError struct {
	Endpoint string
	Message  string
}

func (e *ScopeError) Error() string {
	return fmt.Sprintf("gitea token lacks the scopes required by %s: %s", e.Endpoint, e.Message)
}

// Version returns the Gitea server version. It doubles as a cheap connectivity
// check that works with any token scope, so the CLI can distinguish "the server
// is unreachable" from "the server is fine but your token is too narrow".
func (c *Client) Version(ctx context.Context) (string, error) {
	var out struct {
		Version string `json:"version"`
	}
	if err := c.get(ctx, "/version", &out); err != nil {
		return "", err
	}
	return out.Version, nil
}

// ListRepos returns every repository visible to the authenticated user,
// following pagination until a short page tells us we have reached the end.
//
// Gitea's /user/repos includes both owned repositories and ones the user was
// added to as a collaborator; callers filter afterwards using Repo.OwnedBy.
func (c *Client) ListRepos(ctx context.Context) ([]Repo, error) {
	var all []Repo
	for page := 1; ; page++ {
		var batch []Repo
		path := fmt.Sprintf("/user/repos?page=%d&limit=%d", page, pageSize)
		if err := c.get(ctx, path, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)

		// A page shorter than the limit means there is nothing after it. This
		// is more robust than trusting the X-Total-Count header, which some
		// Gitea versions omit on this endpoint.
		if len(batch) < pageSize {
			return all, nil
		}

		// Defensive stop. If a server ever ignored our page parameter we would
		// otherwise loop forever accumulating duplicates.
		if page > 200 {
			return all, fmt.Errorf("pagination did not terminate after %d pages", page)
		}
	}
}

// Repo fetches a single repository by owner and name. Unlike ListRepos this
// only needs repository scope, so it remains available to narrowly-scoped
// tokens and lets the CLI migrate an explicitly named list of repositories even
// when it cannot enumerate them.
func (c *Client) Repo(ctx context.Context, owner, name string) (Repo, error) {
	var r Repo
	path := fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(name))
	if err := c.get(ctx, path, &r); err != nil {
		return Repo{}, err
	}
	return r, nil
}

// get performs an authenticated GET and decodes the JSON body into out.
func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	// Gitea accepts the token either as basic-auth password or via this header.
	// The header form is preferred because it never puts the secret in a URL,
	// where it could end up in a proxy log.
	req.Header.Set("Authorization", "token "+c.Token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		var apiErr struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&apiErr)

		// 401 and 403 look almost identical from here but call for opposite
		// fixes: a rejected token has to be replaced, whereas a valid but
		// narrow token has to be re-minted with more scopes. Gitea muddies the
		// distinction by answering 403 for some revoked tokens, so when the
		// status alone is ambiguous fall back to what the message says.
		if resp.StatusCode == http.StatusUnauthorized ||
			strings.Contains(apiErr.Message, "invalid username, password or token") {
			return &AuthError{Endpoint: path, Message: apiErr.Message}
		}
		return &ScopeError{Endpoint: path, Message: apiErr.Message}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s: unexpected status %s", path, resp.Status)
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding response from %s: %w", path, err)
	}
	return nil
}
