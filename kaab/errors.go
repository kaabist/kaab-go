package kaab

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/kaabist/kaab-go/client"
)

// Error is every failure the SDK returns. An API refusal carries the API's error.code and
// request_id; the SDK's own failures carry codes no API response uses —
// connection_error, timeout, wait_timeout, signature_invalid — so one switch on Code covers both.
type Error struct {
	Code    string
	Message string
	// Status is the HTTP status; 0 when no response arrived.
	Status    int
	RequestID string
	Details   map[string]any
	// RetryAfter is what the API asked the client to wait (Retry-After), when it did.
	RetryAfter time.Duration
	// Job is the last state Jobs.Wait saw, on a wait_timeout.
	Job *client.Job
	// IdempotencyKey is the Idempotency-Key a keyed call (Jobs.Create, Jobs.Batch) sent, on a
	// connection_error or timeout: the job may exist, and a retry with this key gets it instead of
	// a second one.
	IdempotencyKey string

	cause error
}

func (e *Error) Error() string {
	if e.RequestID != "" {
		return fmt.Sprintf("%s: %s (request %s)", e.Code, e.Message, e.RequestID)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap is the transport error behind a connection_error or timeout.
func (e *Error) Unwrap() error { return e.cause }

// IsCode reports whether err is a *Error with that code.
func IsCode(err error, code string) bool {
	var kerr *Error
	return asError(err, &kerr) && kerr.Code == code
}

type envelope struct {
	Error *struct {
		Code      string         `json:"code"`
		Message   string         `json:"message"`
		RequestID string         `json:"request_id"`
		Details   map[string]any `json:"details"`
	} `json:"error"`
}

// errorFromResponse is the *Error for an API response that is not a success.
func errorFromResponse(res *http.Response, body []byte) *Error {
	e := &Error{
		Code:      fmt.Sprintf("http_%d", res.StatusCode),
		Message:   res.Status,
		Status:    res.StatusCode,
		RequestID: res.Header.Get("X-Kaab-Request-Id"),
	}
	var env envelope
	if json.Unmarshal(body, &env) == nil && env.Error != nil {
		if env.Error.Code != "" {
			e.Code = env.Error.Code
		}
		if env.Error.Message != "" {
			e.Message = env.Error.Message
		}
		if env.Error.RequestID != "" {
			e.RequestID = env.Error.RequestID
		}
		e.Details = env.Error.Details
	}
	if after, ok := parseRetryAfter(res.Header.Get("Retry-After"), time.Now()); ok {
		e.RetryAfter = after
	}
	return e
}
