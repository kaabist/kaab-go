package kaab

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kaabist/kaab-go/client"
)

// recorded is one request the fake API received.
type recorded struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   string
}

// fakeAPI answers each request with the next of answers for its "METHOD /path" (the last one
// forever), and records every request.
type fakeAPI struct {
	mu       sync.Mutex
	requests []recorded
	answers  map[string][]func(w http.ResponseWriter)
	calls    map[string]int
}

func newFake(t *testing.T) (*fakeAPI, *Client, *[]time.Duration) {
	t.Helper()
	f := &fakeAPI{answers: map[string][]func(http.ResponseWriter){}, calls: map[string]int{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	delays := &[]time.Duration{}
	c := New("sk_test_abc",
		WithBaseURL(srv.URL+"/v1"),
		withRandom(func() float64 { return 0.5 }),
		withSleep(func(_ context.Context, d time.Duration) error { *delays = append(*delays, d); return nil }),
	)
	return f, c, delays
}

func (f *fakeAPI) on(method, path string, answers ...func(http.ResponseWriter)) {
	f.answers[method+" "+path] = answers
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, recorded{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), string(body)})
	key := r.Method + " " + r.URL.Path
	answers := f.answers[key]
	n := f.calls[key]
	f.calls[key]++
	f.mu.Unlock()
	if len(answers) == 0 {
		writeJSON(404, map[string]any{"error": map[string]any{"code": "not_found", "message": "no answer", "request_id": "req_fake"}})(w)
		return
	}
	answers[min(n, len(answers)-1)](w)
}

func writeJSON(status int, body any, headers ...string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Kaab-Request-Id", "req_test")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
}

func apiError(status int, code string, headers ...string) func(http.ResponseWriter) {
	return writeJSON(status, map[string]any{"error": map[string]any{"code": code, "message": code + " happened", "request_id": "req_err"}}, headers...)
}

func jobJSON(status string) map[string]any {
	return map[string]any{
		"id": "job_01j9zzzzzzzzzzzzzzzzzzzzzz", "object": "job", "mode": "test", "status": status,
		"reason": nil, "program": map[string]any{"id": "prg_x", "name": "p", "version": 1, "version_id": "pv_x"},
		"inputs_hash": nil, "data": nil, "raw": nil, "warnings": []any{}, "meta": map[string]any{},
		"error": nil, "input_request": nil, "attention": nil, "progress": nil, "tags": []any{},
		"webhook_url": nil, "created_at": "2026-10-05T10:00:00Z", "started_at": nil, "finished_at": nil, "expires_at": nil,
	}
}

func createBody(t *testing.T) client.CreateJobJSONBody {
	t.Helper()
	body, err := Body[client.CreateJobJSONBody](map[string]any{"url": "https://example.com", "extract": map[string]any{"title": "h1"}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

var ulidPattern = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

func TestCreateSendsKeyAndIdempotencyKeyAcrossRetries(t *testing.T) {
	f, c, delays := newFake(t)
	f.on("POST", "/v1/jobs", apiError(503, "internal"), writeJSON(202, jobJSON("queued")))
	job, err := c.Jobs.Create(context.Background(), createBody(t))
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != "queued" {
		t.Fatalf("status = %s", job.Status)
	}
	if len(f.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(f.requests))
	}
	first, second := f.requests[0], f.requests[1]
	if got := first.Header.Get("Authorization"); got != "Bearer sk_test_abc" {
		t.Fatalf("Authorization = %q", got)
	}
	if !strings.HasPrefix(first.Header.Get("User-Agent"), "kaabist-sdk-go/"+Version) {
		t.Fatalf("User-Agent = %q", first.Header.Get("User-Agent"))
	}
	key := first.Header.Get("Idempotency-Key")
	if !ulidPattern.MatchString(key) || second.Header.Get("Idempotency-Key") != key {
		t.Fatalf("idempotency keys %q, %q", key, second.Header.Get("Idempotency-Key"))
	}
	if first.Body != second.Body || !strings.Contains(first.Body, `"url":"https://example.com"`) {
		t.Fatalf("bodies %q, %q", first.Body, second.Body)
	}
	if len(*delays) != 1 || (*delays)[0] != 375*time.Millisecond {
		t.Fatalf("delays = %v", *delays)
	}
}

func TestWaitAndOwnIdempotencyKey(t *testing.T) {
	f, c, _ := newFake(t)
	f.on("POST", "/v1/jobs", writeJSON(200, jobJSON("succeeded")))
	if _, err := c.Jobs.Create(context.Background(), createBody(t), WithWait(30), WithIdempotencyKey("mine")); err != nil {
		t.Fatal(err)
	}
	if got := f.requests[0].Header.Get("Prefer"); got != "wait=30" {
		t.Fatalf("Prefer = %q", got)
	}
	if got := f.requests[0].Header.Get("Idempotency-Key"); got != "mine" {
		t.Fatalf("Idempotency-Key = %q", got)
	}
	if _, err := c.Jobs.Create(context.Background(), createBody(t), WithWait(90)); err != nil {
		t.Fatal(err)
	}
	if got := f.requests[1].Header.Get("Prefer"); got != "wait=60" {
		t.Fatalf("Prefer = %q, want the cap", got)
	}
	if _, err := c.Jobs.Create(context.Background(), createBody(t), WithWait(-1)); err == nil {
		t.Fatal("a negative wait was sent")
	}
}

func TestRetriesGETThenReportsTheAPIsError(t *testing.T) {
	f, c, delays := newFake(t)
	f.on("GET", "/v1/org", apiError(502, "internal"), apiError(500, "internal"), apiError(503, "internal"))
	_, err := c.Org.Get(context.Background())
	var kerr *Error
	if !errors.As(err, &kerr) || kerr.Code != "internal" || kerr.Status != 503 || kerr.RequestID != "req_err" {
		t.Fatalf("err = %#v", err)
	}
	if len(f.requests) != 3 || len(*delays) != 2 || (*delays)[1] != 750*time.Millisecond {
		t.Fatalf("requests = %d, delays = %v", len(f.requests), *delays)
	}
}

func TestRetryAfter(t *testing.T) {
	f, c, delays := newFake(t)
	f.on("GET", "/v1/credits", apiError(429, "rate_limited", "Retry-After", "7"), apiError(429, "test_quota_exceeded", "Retry-After", "3600"))
	_, err := c.Credits.Get(context.Background())
	var kerr *Error
	if !errors.As(err, &kerr) || kerr.Code != "test_quota_exceeded" || kerr.RetryAfter != time.Hour {
		t.Fatalf("err = %#v", err)
	}
	if len(*delays) != 1 || (*delays)[0] != 7*time.Second {
		t.Fatalf("delays = %v", *delays)
	}
}

func TestNoRetryForPOSTWithoutKeyOrOtherRefusals(t *testing.T) {
	f, c, _ := newFake(t)
	f.on("POST", "/v1/jobs/job_1/cancel", apiError(503, "internal"))
	f.on("GET", "/v1/programs/x", apiError(404, "not_found"))
	if _, err := c.Jobs.Cancel(context.Background(), "job_1"); !IsCode(err, "internal") {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.Programs.Get(context.Background(), "x"); !IsCode(err, "not_found") {
		t.Fatalf("err = %v", err)
	}
	if len(f.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(f.requests))
	}
}

func TestConnectionFailureAndTimeout(t *testing.T) {
	c := New("sk_test_x", WithBaseURL("http://127.0.0.1:1/v1"), WithMaxRetries(1),
		withSleep(func(context.Context, time.Duration) error { return nil }))
	if _, err := c.Org.Get(context.Background()); !IsCode(err, "connection_error") {
		t.Fatalf("err = %v", err)
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer slow.Close()
	c = New("sk_test_x", WithBaseURL(slow.URL), WithMaxRetries(0), WithTimeout(30*time.Millisecond))
	if _, err := c.Org.Get(context.Background()); !IsCode(err, "timeout") {
		t.Fatalf("err = %v", err)
	}
}

func TestNeedsAKey(t *testing.T) {
	t.Setenv("KAAB_API_KEY", "")
	if _, err := New("").Org.Get(context.Background()); !IsCode(err, "unauthenticated") {
		t.Fatalf("err = %v", err)
	}
	t.Setenv("KAAB_API_KEY", "sk_test_env")
	t.Setenv("KAAB_BASE_URL", "")
	if c := New(""); c.apiKey != "sk_test_env" || c.BaseURL() != DefaultBaseURL {
		t.Fatalf("client = %q %q", c.apiKey, c.BaseURL())
	}
}

func TestRedirectEmptyAndPathParams(t *testing.T) {
	f, c, _ := newFake(t)
	f.on("GET", "/v1/jobs/job_1/snapshot", func(w http.ResponseWriter) {
		w.Header().Set("Location", "https://s3.test/snap?sig=1")
		w.WriteHeader(302)
	})
	f.on("DELETE", "/v1/webhooks/whk_1", func(w http.ResponseWriter) { w.WriteHeader(204) })
	f.on("GET", "/v1/programs/prg_1/versions/3/diff", writeJSON(200, map[string]any{"patch": []any{}}))
	step := "steps[1]"
	url, err := c.Jobs.Snapshot(context.Background(), "job_1", &client.GetJobSnapshotParams{Step: &step})
	if err != nil || url != "https://s3.test/snap?sig=1" {
		t.Fatalf("url = %q, err = %v", url, err)
	}
	if err := c.Webhooks.Delete(context.Background(), "whk_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Programs.Versions.Diff(context.Background(), "prg_1", 3, &client.DiffProgramVersionParams{Against: 2}); err != nil {
		t.Fatal(err)
	}
	if q := f.requests[2].Query; q != "against=2" {
		t.Fatalf("query = %q", q)
	}
}

func TestListAllFollowsCursors(t *testing.T) {
	f, c, _ := newFake(t)
	f.on("GET", "/v1/jobs",
		writeJSON(200, map[string]any{"data": []any{jobJSON("succeeded"), jobJSON("failed")}, "next_cursor": "c2"}),
		writeJSON(200, map[string]any{"data": []any{jobJSON("canceled")}, "next_cursor": nil}),
	)
	status := client.ListJobsParamsStatus("succeeded")
	var got []string
	for job, err := range c.Jobs.ListAll(context.Background(), &client.ListJobsParams{Status: &status}) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, string(job.Status))
	}
	if strings.Join(got, ",") != "succeeded,failed,canceled" {
		t.Fatalf("items = %v", got)
	}
	if f.requests[1].Query != "cursor=c2&status=succeeded" {
		t.Fatalf("second query = %q", f.requests[1].Query)
	}
}

func TestULID(t *testing.T) {
	// The time part of the ULID specification's own example.
	got := newULID(time.UnixMilli(1_469_918_176_385), make([]byte, 10))
	if got != "01ARYZ6S41"+strings.Repeat("0", 16) {
		t.Fatalf("ulid = %s", got)
	}
	if a, b := newULID(time.Now(), nil), newULID(time.Now(), nil); a == b {
		t.Fatal("two ULIDs alike")
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	if d, ok := parseRetryAfter("12", now); !ok || d != 12*time.Second {
		t.Fatalf("delta = %v %v", d, ok)
	}
	if d, ok := parseRetryAfter(now.Add(30*time.Second).Format(http.TimeFormat), now); !ok || d != 30*time.Second {
		t.Fatalf("date = %v %v", d, ok)
	}
	if _, ok := parseRetryAfter("soon", now); ok {
		t.Fatal("garbage parsed")
	}
}

// failingBody fails its first read: the headers arrived, the body did not.
type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("connection reset mid-body") }
func (failingBody) Close() error             { return nil }

// roundTripFunc is an http.RoundTripper made of a function.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReadErrorBacksOff(t *testing.T) {
	var calls int
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: failingBody{}, Request: r}, nil
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"org_1","object":"org"}`)),
			Request:    r,
		}, nil
	})
	var delays []time.Duration
	c := New("sk_test_x", WithBaseURL("https://api.test/v1"), WithHTTPClient(&http.Client{Transport: transport}),
		withRandom(func() float64 { return 0 }),
		withSleep(func(_ context.Context, d time.Duration) error { delays = append(delays, d); return nil }))
	if _, err := c.Org.Get(context.Background()); err != nil {
		t.Fatalf("err = %v", err)
	}
	if calls != 2 || len(delays) != 1 || delays[0] != 250*time.Millisecond {
		t.Fatalf("calls = %d, delays = %v; want a second attempt after the first backoff", calls, delays)
	}

	// A cancelled context after the failed read: no second request, the context's error.
	calls = 0
	ctx, cancel := context.WithCancel(context.Background())
	transport2 := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		cancel()
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: failingBody{}, Request: r}, nil
	})
	c = New("sk_test_x", WithBaseURL("https://api.test/v1"), WithHTTPClient(&http.Client{Transport: transport2}),
		withSleep(func(context.Context, time.Duration) error { t.Fatal("slept after a cancelled context"); return nil }))
	if _, err := c.Org.Get(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestTransportFailureCarriesIdempotencyKey(t *testing.T) {
	var sent []string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent = append(sent, r.Header.Get("Idempotency-Key"))
		return nil, errors.New("connection refused")
	})
	c := New("sk_test_x", WithBaseURL("https://api.test/v1"), WithHTTPClient(&http.Client{Transport: transport}),
		WithMaxRetries(1), withSleep(func(context.Context, time.Duration) error { return nil }))
	_, err := c.Jobs.Create(context.Background(), createBody(t))
	var kerr *Error
	if !errors.As(err, &kerr) || kerr.Code != "connection_error" {
		t.Fatalf("err = %v", err)
	}
	if len(sent) != 2 || sent[0] == "" || sent[1] != sent[0] || kerr.IdempotencyKey != sent[0] {
		t.Fatalf("sent %v, error key %q", sent, kerr.IdempotencyKey)
	}
	// A key made by the caller is named the same way.
	key := NewIdempotencyKey()
	if !ulidPattern.MatchString(key) {
		t.Fatalf("key %q", key)
	}
	_, err = c.Jobs.Create(context.Background(), createBody(t), WithIdempotencyKey(key))
	if !errors.As(err, &kerr) || kerr.IdempotencyKey != key || sent[len(sent)-1] != key {
		t.Fatalf("err %v, sent %v", err, sent)
	}
	// A call without a key names none.
	_, err = c.Org.Get(context.Background())
	if !errors.As(err, &kerr) || kerr.IdempotencyKey != "" {
		t.Fatalf("err %v", err)
	}
}
