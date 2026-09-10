// Package github is a minimal client for the three GitHub REST operations the
// migrator performs: identifying the authenticated user, checking whether a
// destination repository already exists, and creating one.
//
// Pushing the actual git objects is deliberately *not* done here — that is the
// git binary's job, and reimplementing the smart HTTP transport would be a far
// bigger project than the migration itself.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const apiBase = "https://api.github.com"

// Client talks to GitHub as one authenticated user.
type Client struct {
	Token string
	HTTP  *http.Client
}

// New builds a client with a sensible timeout. Repository creation is fast, but
// GitHub occasionally takes a few seconds to provision a new repository, so the
// timeout is generous rather than tight.
func New(token string) *Client {
	return &Client{Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Repo is the subset of GitHub's repository object we care about.
type Repo struct {
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
	HTMLURL  string `json:"html_url"`
	CloneURL string `json:"clone_url"`
}

// RateLimitError reports that GitHub asked us to slow down. Repository creation
// is subject to a stricter secondary rate limit than ordinary reads, and
// migrating thirty repositories in a burst is exactly the pattern that trips
// it, so the caller needs to distinguish this from a permanent failure and back
// off instead of giving up.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("github rate limit hit; retry after %s", e.RetryAfter)
}

// Login returns the username of the authenticated user. The migrator needs this
// to build destination URLs and to check for pre-existing repositories, and
// asking GitHub is more reliable than making the user type their own username.
func (c *Client) Login(ctx context.Context) (string, error) {
	var out struct {
		Login string `json:"login"`
	}
	if err := c.do(ctx, http.MethodGet, "/user", nil, &out); err != nil {
		return "", err
	}
	return out.Login, nil
}

// Exists reports whether owner/name is already present on GitHub.
//
// This is what makes the migrator safe to re-run: a migration that dies halfway
// through thirty repositories can be restarted, and the repositories that
// already made it across are recognised rather than colliding.
func (c *Client) Exists(ctx context.Context, owner, name string) (bool, error) {
	path := fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(name))
	err := c.do(ctx, http.MethodGet, path, nil, nil)
	if err == nil {
		return true, nil
	}
	var nf *NotFoundError
	if errors.As(err, &nf) {
		return false, nil
	}
	return false, err
}

// NotFoundError reports a 404. It is a distinct type because for Exists a 404
// is the expected happy path, not a failure.
type NotFoundError struct{ Path string }

func (e *NotFoundError) Error() string { return "github: not found: " + e.Path }

// CreateRepo creates a repository under the authenticated user's account.
//
// Gitea's description is carried over so the migrated repositories do not land
// on GitHub as an undifferentiated wall of names.
func (c *Client) CreateRepo(ctx context.Context, name, description string, private bool) (Repo, error) {
	body := map[string]any{
		"name":        name,
		"description": description,
		"private":     private,
		// The source repository already has its own README, license and
		// history. Letting GitHub auto-initialise would create a commit that
		// is not in the Gitea history, and the subsequent mirror push would
		// then have to overwrite it.
		"auto_init": false,
	}
	var out Repo
	if err := c.do(ctx, http.MethodPost, "/user/repos", body, &out); err != nil {
		return Repo{}, err
	}
	return out, nil
}

// do performs one authenticated API call. A nil body sends no payload; a nil
// out discards the response.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var payload *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(encoded)
	} else {
		payload = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return &NotFoundError{Path: path}

	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		// GitHub signals throttling in two different ways depending on whether
		// it is the primary or the secondary rate limit, so check both.
		if d, ok := retryAfter(resp); ok {
			return &RateLimitError{RetryAfter: d}
		}
		return fmt.Errorf("%s %s: %s (check that the token has the `repo` scope)", method, path, resp.Status)

	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		var apiErr struct {
			Message string `json:"message"`
			Errors  []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&apiErr)
		detail := apiErr.Message
		for _, e := range apiErr.Errors {
			if e.Message != "" {
				detail += ": " + e.Message
			}
		}
		if detail == "" {
			detail = resp.Status
		}
		return fmt.Errorf("%s %s: %s", method, path, detail)
	}

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decoding response from %s: %w", path, err)
		}
	}
	return nil
}

// retryAfter extracts a back-off duration from a throttled response, handling
// both the `Retry-After` header (seconds, used by the secondary limit) and
// `X-RateLimit-Reset` (a Unix timestamp, used by the primary limit).
func retryAfter(resp *http.Response) (time.Duration, bool) {
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			return time.Duration(secs) * time.Second, true
		}
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if v := resp.Header.Get("X-RateLimit-Reset"); v != "" {
			if unix, err := strconv.ParseInt(v, 10, 64); err == nil {
				if d := time.Until(time.Unix(unix, 0)); d > 0 {
					return d, true
				}
			}
		}
	}
	return 0, false
}

// SanitizeName maps a Gitea repository name to one GitHub will accept. GitHub
// allows only letters, digits, dot, dash and underscore, whereas Gitea is more
// permissive, so a name that is legal on one side can be rejected by the other.
func SanitizeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}
