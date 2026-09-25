package github

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// scripted answers each request with the next response in line, so a test can
// say "throttled, then fine" without a server. The API address stays the
// constant it is in production; only the transport underneath is swapped.
type scripted struct {
	responses []*http.Response
	bodies    []string // what each request carried, to check a retry resends it
}

func (s *scripted) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	s.bodies = append(s.bodies, string(body))
	resp := s.responses[0]
	s.responses = s.responses[1:]
	return resp, nil
}

func response(status int, headers map[string]string, body string) *http.Response {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: h,
		Body: io.NopCloser(strings.NewReader(body))}
}

func testClient(rt http.RoundTripper, waited *[]time.Duration) *Client {
	c := New("tok")
	c.HTTP = &http.Client{Transport: rt}
	c.wait = func(_ context.Context, d time.Duration) error {
		*waited = append(*waited, d)
		return nil
	}
	return c
}

// A throttled creation is waited out and sent again, with the same payload,
// rather than failing the repository.
func TestCreateRepoRetriesAfterRateLimit(t *testing.T) {
	rt := &scripted{responses: []*http.Response{
		response(http.StatusForbidden, map[string]string{"Retry-After": "7"}, `{}`),
		response(http.StatusCreated, nil, `{"name":"x","clone_url":"https://github.com/me/x.git"}`),
	}}
	var waited []time.Duration
	repo, err := testClient(rt, &waited).CreateRepo(context.Background(), "x", "d", "", true)
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	if repo.CloneURL != "https://github.com/me/x.git" {
		t.Errorf("clone URL = %q", repo.CloneURL)
	}
	if len(waited) != 1 || waited[0] != 7*time.Second {
		t.Errorf("waited %v, want [7s]", waited)
	}
	if len(rt.bodies) != 2 || rt.bodies[0] == "" || rt.bodies[0] != rt.bodies[1] {
		t.Errorf("retry did not resend the same payload: %q", rt.bodies)
	}
}

// The secondary limit sometimes names no time; its message is then the only
// thing separating it from a token without the scope.
func TestSecondaryLimitWithoutHeaderWaitsAMinute(t *testing.T) {
	rt := &scripted{responses: []*http.Response{
		response(http.StatusForbidden, nil, `{"message":"You have exceeded a secondary rate limit."}`),
		response(http.StatusOK, nil, `{"login":"me","id":1}`),
	}}
	var waited []time.Duration
	if _, err := testClient(rt, &waited).Login(context.Background()); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if len(waited) != 1 || waited[0] != defaultRateLimitWait {
		t.Errorf("waited %v, want [%s]", waited, defaultRateLimitWait)
	}
}

// A 403 that is not a rate limit is still reported as a scope problem, at once.
func TestForbiddenWithoutRateLimitIsNotRetried(t *testing.T) {
	rt := &scripted{responses: []*http.Response{
		response(http.StatusForbidden, nil, `{"message":"Resource not accessible by personal access token"}`),
	}}
	var waited []time.Duration
	_, err := testClient(rt, &waited).Login(context.Background())
	if err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("err = %v, want a scope error", err)
	}
	if len(waited) != 0 {
		t.Errorf("waited %v on a non-rate-limit 403", waited)
	}
}

// Retrying stops: after a few attempts, and at once when the wait is the
// hourly limit rather than a pause.
func TestRateLimitRetriesAreBounded(t *testing.T) {
	throttled := func() *http.Response {
		return response(http.StatusTooManyRequests, map[string]string{"Retry-After": "1"}, `{}`)
	}
	rt := &scripted{}
	for i := 0; i <= maxRateLimitRetries; i++ {
		rt.responses = append(rt.responses, throttled())
	}
	var waited []time.Duration
	_, err := testClient(rt, &waited).Login(context.Background())
	if _, ok := err.(*RateLimitError); !ok {
		t.Fatalf("err = %v, want RateLimitError", err)
	}
	if len(waited) != maxRateLimitRetries {
		t.Errorf("waited %d times, want %d", len(waited), maxRateLimitRetries)
	}

	long := &scripted{responses: []*http.Response{
		response(http.StatusForbidden, map[string]string{"Retry-After": "3600"}, `{}`),
	}}
	waited = nil
	if _, err := testClient(long, &waited).Login(context.Background()); err == nil {
		t.Fatal("expected the hour-long limit to be reported, not waited out")
	}
	if len(waited) != 0 {
		t.Errorf("waited %v for an hour-long limit", waited)
	}
}
