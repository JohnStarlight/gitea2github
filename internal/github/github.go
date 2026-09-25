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

	// Log, when non-nil, is told when a request is held back by a rate limit.
	// A wait of a minute with nothing on screen looks exactly like a hang.
	Log func(format string, args ...any)

	// wait sleeps for d or until ctx ends. Replaced in tests so that a
	// back-off can be exercised without taking a minute.
	wait func(ctx context.Context, d time.Duration) error
}

// New builds a client with a sensible timeout. Repository creation is fast, but
// GitHub occasionally takes a few seconds to provision a new repository, so the
// timeout is generous rather than tight.
func New(token string) *Client {
	return &Client{Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}, wait: sleepCtx}
}

// Retrying on a rate limit is bounded twice over. A few attempts cover the
// secondary limit, which clears within a minute or two; a wait longer than
// maxRateLimitWait means the hourly primary limit is exhausted, and sitting
// silently until it resets would be worse than failing with the reason.
const (
	maxRateLimitRetries = 3
	maxRateLimitWait    = 5 * time.Minute

	// GitHub's guidance for a secondary limit that names no retry time is to
	// wait at least a minute.
	defaultRateLimitWait = time.Minute
)

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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

// Identity is who the token belongs to.
type Identity struct {
	Login string
	ID    int64

	// NoReply is the address GitHub attributes to this account without
	// publishing anything. It is what a redacting migration rewrites the
	// runner's own commits to: hidden, and still linked to their profile.
	NoReply string
}

// Login returns the username of the authenticated user. The migrator needs this
// to build destination URLs and to check for pre-existing repositories, and
// asking GitHub is more reliable than making the user type their own username.
func (c *Client) Login(ctx context.Context) (string, error) {
	who, err := c.Identity(ctx)
	return who.Login, err
}

// TokenScopes returns the scopes GitHub reports for the token. reported is
// false for tokens whose permissions GitHub does not list this way --
// fine-grained personal access tokens -- which have to be taken on trust.
func (c *Client) TokenScopes(ctx context.Context) (scopes []string, reported bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/user", nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("GET /user: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("GET /user: %s", resp.Status)
	}
	values, ok := resp.Header[http.CanonicalHeaderKey("X-OAuth-Scopes")]
	if !ok {
		return nil, false, nil
	}
	for _, v := range values {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				scopes = append(scopes, s)
			}
		}
	}
	return scopes, true, nil
}

// Identity returns the username, the numeric id and the no-reply address they
// combine into.
//
// The address is derived rather than asked for, because reading it from the
// API needs a scope the migrator has no other use for -- and because a token
// that can create repositories already knows enough to work it out.
func (c *Client) Identity(ctx context.Context) (Identity, error) {
	var out struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
	}
	if err := c.do(ctx, http.MethodGet, "/user", nil, &out); err != nil {
		return Identity{}, err
	}
	return Identity{
		Login:   out.Login,
		ID:      out.ID,
		NoReply: fmt.Sprintf("%d+%s@users.noreply.github.com", out.ID, out.Login),
	}, nil
}

// Lookup returns the repository if it is there.
//
// This is what makes the migrator safe to re-run: a migration that dies
// halfway through thirty repositories can be restarted, and the repositories
// that already made it across are recognised rather than colliding.
//
// Whether a repository is private decides what may safely be done with it: a
// clone that pushes to both servers sends un-redacted commits to GitHub on
// every push, which is contained while the destination is private and is a
// continuous publication of addresses while it is not.
func (c *Client) Lookup(ctx context.Context, owner, name string) (Repo, bool, error) {
	path := fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(name))
	var out Repo
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	if err == nil {
		return out, true, nil
	}
	var nf *NotFoundError
	if errors.As(err, &nf) {
		return Repo{}, false, nil
	}
	return Repo{}, false, err
}

// NotFoundError reports a 404. It is a distinct type because for Exists a 404
// is the expected happy path, not a failure.
type NotFoundError struct{ Path string }

func (e *NotFoundError) Error() string { return "github: not found: " + e.Path }

// StatusError is a refusal that is neither a 404 nor a rate limit, kept with
// its status so that a caller for whom one particular refusal is an answer
// rather than a failure can tell it apart.
type StatusError struct {
	Code int
	msg  string
}

func (e *StatusError) Error() string { return e.msg }

// ErrEmptyRepository reports a repository that exists with nothing in it.
var ErrEmptyRepository = errors.New("the GitHub repository is empty")

// HasCommit reports whether a commit is part of the repository.
//
// It is how a rewritten history is recognised. A migration that copied the
// history verbatim carried every commit across under its own hash; one that
// rewrote it carried none of them, whatever addresses the rewrite left behind.
// Asking about a commit settles which, where looking at addresses cannot: a
// project redacted with only its owner's own address kept comes out with no
// address in the redacted shape at all.
//
// A repository created but never pushed to answers ErrEmptyRepository, which
// is neither answer: nothing matches, and nothing was rewritten either.
func (c *Client) HasCommit(ctx context.Context, owner, name, sha string) (bool, error) {
	path := fmt.Sprintf("/repos/%s/%s/commits/%s",
		url.PathEscape(owner), url.PathEscape(name), url.PathEscape(sha))
	err := c.do(ctx, http.MethodGet, path, nil, nil)
	var status *StatusError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &status) && status.Code == http.StatusUnprocessableEntity:
		// "No commit found for SHA".
		return false, nil
	case errors.As(err, &status) && status.Code == http.StatusConflict:
		return false, ErrEmptyRepository
	}
	return false, err
}

// BranchTree returns the tree a branch points at: what its files are, apart
// from who committed them.
//
// It is how a rewritten copy is told from a different project. Redaction
// changes every commit's hash and none of its files, so a copy of this
// project rewritten to hide addresses has the same tree at the same branch,
// and a different project that happens to share its name does not. found is
// false when the branch does not exist.
func (c *Client) BranchTree(ctx context.Context, owner, name, branch string) (tree string, found bool, err error) {
	// Each segment escaped on its own: a branch called feature/x is two
	// segments of the path, not one containing %2F.
	segments := strings.Split(branch, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	path := fmt.Sprintf("/repos/%s/%s/branches/%s",
		url.PathEscape(owner), url.PathEscape(name), strings.Join(segments, "/"))
	var out struct {
		Commit struct {
			Commit struct {
				Tree struct {
					SHA string `json:"sha"`
				} `json:"tree"`
			} `json:"commit"`
		} `json:"commit"`
	}
	err = c.do(ctx, http.MethodGet, path, nil, &out)
	var nf *NotFoundError
	switch {
	case errors.As(err, &nf):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	return out.Commit.Commit.Tree.SHA, true, nil
}

// TagTree returns the tree a tag points at, following an annotated tag to its
// commit. found is false when the tag does not exist.
//
// It is BranchTree's counterpart for tags: a tag left on the original history
// after an adoption would publish it on the next push --tags, so a tag's twin
// has to be found and compared as a branch's is.
func (c *Client) TagTree(ctx context.Context, owner, name, tag string) (tree string, found bool, err error) {
	repo := fmt.Sprintf("/repos/%s/%s/git", url.PathEscape(owner), url.PathEscape(name))
	segments := strings.Split(tag, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}

	type object struct {
		Type string `json:"type"`
		SHA  string `json:"sha"`
	}
	var ref struct {
		Object object `json:"object"`
	}
	err = c.do(ctx, http.MethodGet, repo+"/ref/tags/"+strings.Join(segments, "/"), nil, &ref)
	var nf *NotFoundError
	switch {
	case errors.As(err, &nf):
		return "", false, nil
	case err != nil:
		return "", false, err
	}

	// An annotated tag is an object of its own, pointing at the commit.
	target := ref.Object
	for i := 0; target.Type == "tag" && i < 5; i++ {
		var annotated struct {
			Object object `json:"object"`
		}
		if err := c.do(ctx, http.MethodGet, repo+"/tags/"+target.SHA, nil, &annotated); err != nil {
			return "", false, err
		}
		target = annotated.Object
	}
	if target.Type != "commit" {
		return "", true, nil
	}
	var commit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := c.do(ctx, http.MethodGet, repo+"/commits/"+target.SHA, nil, &commit); err != nil {
		return "", false, err
	}
	return commit.Tree.SHA, true, nil
}

// IsEmpty reports whether a repository that exists has nothing in it.
//
// A migration that created a repository and was interrupted before its push
// landed leaves exactly this behind. Telling it apart from a repository that
// is really there is what lets the next run finish the job instead of
// reporting it as done. The size GitHub reports is no help: it is updated
// after the fact, and reads 0 for a while after a push has landed.
func (c *Client) IsEmpty(ctx context.Context, owner, name string) (bool, error) {
	path := fmt.Sprintf("/repos/%s/%s/commits?per_page=1", url.PathEscape(owner), url.PathEscape(name))
	err := c.do(ctx, http.MethodGet, path, nil, nil)
	var status *StatusError
	switch {
	case err == nil:
		return false, nil
	case errors.As(err, &status) && status.Code == http.StatusConflict:
		// "Git Repository is empty."
		return true, nil
	}
	return false, err
}

// SetDefaultBranch makes branch the one GitHub shows and clones by default.
func (c *Client) SetDefaultBranch(ctx context.Context, owner, name, branch string) error {
	path := fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(name))
	return c.do(ctx, http.MethodPatch, path, map[string]any{"default_branch": branch}, nil)
}

// SetPrivate changes the visibility of an existing repository.
func (c *Client) SetPrivate(ctx context.Context, owner, name string, private bool) error {
	path := fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(name))
	return c.do(ctx, http.MethodPatch, path, map[string]any{"private": private}, nil)
}

// TipAuthors returns the author and committer addresses of the most recent
// commits on a repository's default branch.
//
// Used to recognise a repository whose history was rewritten to hide
// addresses, when HasCommit cannot settle it. The result of such a rewrite is the only record that it happened,
// and a local clone that still holds the original history cannot push to it --
// so the difference has to be noticed before somebody tries.
//
// A handful of commits is enough: redaction applies to a whole history, so if
// it happened at all it shows on the first commit looked at. More are fetched
// only to make a single unusual address less likely to decide the answer.
func (c *Client) TipAuthors(ctx context.Context, owner, name string) ([]string, error) {
	var commits []struct {
		Commit struct {
			Author struct {
				Email string `json:"email"`
			} `json:"author"`
			Committer struct {
				Email string `json:"email"`
			} `json:"committer"`
		} `json:"commit"`
	}
	path := fmt.Sprintf("/repos/%s/%s/commits?per_page=10", owner, name)
	if err := c.do(ctx, http.MethodGet, path, nil, &commits); err != nil {
		return nil, err
	}

	var addrs []string
	for _, c := range commits {
		addrs = append(addrs, c.Commit.Author.Email, c.Commit.Committer.Email)
	}
	return addrs, nil
}

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
//
// A rate-limited request is waited out and sent again rather than reported as
// a failure. Creating thirty repositories in a burst is exactly what trips
// GitHub's secondary limit, and a migration that fails half its repositories
// for want of a minute's pause has failed for no reason. Retrying is safe for
// every method, including the POST that creates a repository: a throttled
// request was refused before it was acted on.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var encoded []byte
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			return err
		}
	}

	wait := c.wait
	if wait == nil {
		wait = sleepCtx
	}
	for attempt := 0; ; attempt++ {
		err := c.doOnce(ctx, method, path, encoded, body != nil, out)
		var limited *RateLimitError
		if !errors.As(err, &limited) || attempt >= maxRateLimitRetries ||
			limited.RetryAfter > maxRateLimitWait {
			return err
		}
		if c.Log != nil {
			c.Log("GitHub rate limit reached; waiting %s before retrying", limited.RetryAfter.Round(time.Second))
		}
		if werr := wait(ctx, limited.RetryAfter); werr != nil {
			return err
		}
	}
}

// doOnce sends one request. The payload is passed already encoded so that a
// retry sends exactly the same bytes.
func (c *Client) doOnce(ctx context.Context, method, path string, encoded []byte, hasBody bool, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if hasBody {
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
		// The secondary limit does not always name a time, and then the only
		// thing telling it apart from a missing scope is the message.
		var apiErr struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&apiErr)
		if strings.Contains(strings.ToLower(apiErr.Message), "rate limit") {
			return &RateLimitError{RetryAfter: defaultRateLimitWait}
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
		return &StatusError{Code: resp.StatusCode,
			msg: fmt.Sprintf("%s %s: %s", method, path, detail)}
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
