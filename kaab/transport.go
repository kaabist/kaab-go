package kaab

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The retry policy every Kaab SDK shares:
//
//   - A request is retried only when repeating it cannot do something twice: GET, HEAD, PUT and
//     DELETE, and a POST that carries an Idempotency-Key (Jobs.Create, Jobs.Batch, where the SDK
//     sets one unless the caller did).
//   - It is retried on 408, 429, 500, 502, 503 and 504, on a connection failure and on a
//     per-attempt timeout; never on any other answer.
//   - At most MaxRetries times (2 by default: three attempts).
//   - The delay is the answer's Retry-After when it gives one — over 60 s and the request is not
//     retried at all — else exponential backoff: 0.5 s, 1 s, 2 s … capped at 8 s, the upper half
//     jittered.
const (
	maxRetryAfter = 60 * time.Second
	baseDelay     = 500 * time.Millisecond
	maxDelay      = 8 * time.Second
	// MaxWaitSeconds caps Prefer: wait.
	MaxWaitSeconds = 60
	// longPollGrace is how much longer than its wait a long-poll may take.
	longPollGrace = 15 * time.Second
)

var retryStatuses = map[int]bool{408: true, 429: true, 500: true, 502: true, 503: true, 504: true}

func isRetryable(method string, header http.Header) bool {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete:
		return true
	}
	return header.Get("Idempotency-Key") != ""
}

// retryDelay is the wait before retry attempt+1; ok is false when Retry-After exceeds a minute.
func retryDelay(attempt int, retryAfter *time.Duration, random func() float64) (time.Duration, bool) {
	if retryAfter != nil {
		if *retryAfter > maxRetryAfter {
			return 0, false
		}
		return *retryAfter, true
	}
	ceiling := float64(baseDelay) * math.Pow(2, float64(attempt))
	if ceiling > float64(maxDelay) {
		ceiling = float64(maxDelay)
	}
	return time.Duration(ceiling/2 + random()*ceiling/2), true
}

func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second, true
	}
	at, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	d := at.Sub(now)
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second), true
}

func defaultRandom() float64 {
	n, err := rand.Int(rand.Reader, big.NewInt(1<<53))
	if err != nil {
		return 0.5
	}
	return float64(n.Int64()) / float64(1<<53)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func asError(err error, target **Error) bool { return errors.As(err, target) }

// RequestOption changes one call.
type RequestOption func(*requestOptions)

type requestOptions struct {
	wait           int
	idempotencyKey string
	timeout        time.Duration
	maxRetries     *int
	header         http.Header
}

// WithWait holds the request open up to seconds (1–60) for a result: Prefer: wait on
// Jobs.Create and a long-poll on Jobs.Get.
func WithWait(seconds int) RequestOption {
	return func(o *requestOptions) { o.wait = seconds }
}

// WithIdempotencyKey sets Idempotency-Key on Jobs.Create and Jobs.Batch; a ULID otherwise.
func WithIdempotencyKey(key string) RequestOption {
	return func(o *requestOptions) { o.idempotencyKey = key }
}

// WithRequestTimeout bounds each attempt of this call.
func WithRequestTimeout(d time.Duration) RequestOption {
	return func(o *requestOptions) { o.timeout = d }
}

// WithRequestMaxRetries sets this call's retries after the first attempt.
func WithRequestMaxRetries(n int) RequestOption {
	return func(o *requestOptions) { o.maxRetries = &n }
}

// WithRequestHeader adds a header to this call.
func WithRequestHeader(name, value string) RequestOption {
	return func(o *requestOptions) {
		if o.header == nil {
			o.header = http.Header{}
		}
		o.header.Add(name, value)
	}
}

// callFlags is what a generated method declares about its operation.
type callFlags struct {
	idempotent bool
	prefer     bool
	redirect   bool
}

// prepare sets the headers of req and returns the per-attempt timeout and the retry budget.
func (c *Client) prepare(req *http.Request, flags callFlags, opts []RequestOption) (time.Duration, int, error) {
	o := requestOptions{}
	for _, opt := range opts {
		opt(&o)
	}
	for name, values := range c.headers {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	for name, values := range o.header {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("User-Agent", userAgent())
	req.Header.Set("Accept", "application/json")
	timeout := c.timeout
	if o.timeout > 0 {
		timeout = o.timeout
	}
	retries := c.maxRetries
	if o.maxRetries != nil {
		retries = *o.maxRetries
	}
	if flags.idempotent {
		key := o.idempotencyKey
		if key == "" {
			key = newULID(time.Now(), nil)
		}
		req.Header.Set("Idempotency-Key", key)
	}
	if flags.prefer && o.wait != 0 {
		if o.wait < 1 {
			return 0, 0, fmt.Errorf("kaab: wait must be between 1 and %d seconds, got %d", MaxWaitSeconds, o.wait)
		}
		wait := min(o.wait, MaxWaitSeconds)
		req.Header.Set("Prefer", "wait="+strconv.Itoa(wait))
		timeout = max(timeout, time.Duration(wait)*time.Second+longPollGrace)
	}
	return timeout, retries, nil
}

// send sends req under the retry policy and returns the last response with its body read.
func (c *Client) send(ctx context.Context, req *http.Request, flags callFlags, opts []RequestOption) (*http.Response, []byte, error) {
	if c.apiKey == "" {
		return nil, nil, &Error{Code: "unauthenticated", Message: "no API key: pass one to kaab.New or set KAAB_API_KEY"}
	}
	timeout, retries, err := c.prepare(req, flags, opts)
	if err != nil {
		return nil, nil, err
	}
	var body []byte
	if req.Body != nil {
		body, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, nil, err
		}
		_ = req.Body.Close()
	}
	retryable := isRetryable(req.Method, req.Header)
	// A keyed request that got no answer may have created its job: its failure names the key, so
	// the caller can retry without a second one.
	key := req.Header.Get("Idempotency-Key")
	for attempt := 0; ; attempt++ {
		canRetry := retryable && attempt < retries
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		r := req.Clone(attemptCtx)
		if body != nil {
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}
		res, err := c.httpClient.Do(r)
		if err != nil {
			cancel()
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			code, message := "connection_error", fmt.Sprintf("%s %s failed: %v", req.Method, req.URL, err)
			if errors.Is(err, context.DeadlineExceeded) {
				code, message = "timeout", fmt.Sprintf("%s %s timed out after %s", req.Method, req.URL, timeout)
			}
			if !canRetry {
				return nil, nil, &Error{Code: code, Message: message, IdempotencyKey: key, cause: err}
			}
			delay, _ := retryDelay(attempt, nil, c.random)
			if err := c.sleep(ctx, delay); err != nil {
				return nil, nil, err
			}
			continue
		}
		data, readErr := io.ReadAll(res.Body)
		_ = res.Body.Close()
		cancel()
		if readErr != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			if !canRetry {
				return nil, nil, &Error{Code: "connection_error", Message: readErr.Error(), IdempotencyKey: key, cause: readErr}
			}
			// The same backoff as a failed connection: an immediate retry hits the same fault.
			delay, _ := retryDelay(attempt, nil, c.random)
			if err := c.sleep(ctx, delay); err != nil {
				return nil, nil, err
			}
			continue
		}
		if canRetry && retryStatuses[res.StatusCode] {
			var after *time.Duration
			if d, ok := parseRetryAfter(res.Header.Get("Retry-After"), time.Now()); ok {
				after = &d
			}
			if delay, ok := retryDelay(attempt, after, c.random); ok {
				if err := c.sleep(ctx, delay); err != nil {
					return nil, nil, err
				}
				continue
			}
		}
		return res, data, nil
	}
}

// do sends req and decodes a success into out (nil for a 204).
func (c *Client) do(ctx context.Context, req *http.Request, flags callFlags, opts []RequestOption, out any) error {
	res, body, err := c.send(ctx, req, flags, opts)
	if err != nil {
		return err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return errorFromResponse(res, body)
	}
	if out == nil {
		return nil
	}
	if len(body) == 0 {
		return &Error{Code: "unexpected_response", Message: fmt.Sprintf("%d with no body where one was expected", res.StatusCode), Status: res.StatusCode, RequestID: res.Header.Get("X-Kaab-Request-Id")}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return &Error{Code: "unexpected_response", Message: "the response is not the documented JSON: " + err.Error(), Status: res.StatusCode, RequestID: res.Header.Get("X-Kaab-Request-Id"), cause: err}
	}
	return nil
}

// redirect sends req and answers the Location of its 302, resolved against the API.
func (c *Client) redirect(ctx context.Context, req *http.Request, flags callFlags, opts []RequestOption) (string, error) {
	res, body, err := c.send(ctx, req, flags, opts)
	if err != nil {
		return "", err
	}
	location := res.Header.Get("Location")
	if res.StatusCode >= 300 && res.StatusCode < 400 && location != "" {
		base, err := url.Parse(c.serverURL())
		if err != nil {
			return location, nil
		}
		ref, err := url.Parse(location)
		if err != nil {
			return location, nil
		}
		return base.ResolveReference(ref).String(), nil
	}
	return "", errorFromResponse(res, body)
}

// jsonBody encodes a request body for a generated method.
func jsonBody(v any) (io.Reader, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(data), nil
}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewIdempotencyKey is a fresh Idempotency-Key. To retry a create yourself, make one and pass it
// on every attempt with WithIdempotencyKey.
func NewIdempotencyKey() string { return newULID(time.Now(), nil) }

// newULID is 48 bits of milliseconds and 80 random bits, Crockford base32 (26 characters).
func newULID(now time.Time, entropy []byte) string {
	if entropy == nil {
		entropy = make([]byte, 10)
		_, _ = rand.Read(entropy)
	}
	value := new(big.Int).SetUint64(uint64(now.UnixMilli()))
	value.Lsh(value, 80)
	value.Or(value, new(big.Int).SetBytes(entropy[:10]))
	out := make([]byte, 26)
	mask := big.NewInt(31)
	for i := 25; i >= 0; i-- {
		out[i] = crockford[new(big.Int).And(value, mask).Int64()]
		value.Rsh(value, 5)
	}
	return string(out)
}
