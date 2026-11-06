// Package webhooks verifies Kaab webhook deliveries.
//
// Kaab-Signature is "t=<unix seconds>,v1=<hex>": the HMAC-SHA256, under the endpoint's whole
// whsec_… secret, of "<t>.<raw body>". During a rotation's 24-hour overlap a second v1 under the
// previous secret follows, and a delivery is genuine when any v1 matches. A receiver must refuse a
// timestamp more than 300 s from now. The body must be the bytes as they arrived.
//
//	body, _ := io.ReadAll(r.Body)
//	event, err := webhooks.ConstructEvent(body, r.Header.Get(webhooks.SignatureHeader), secret)
package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SignatureHeader is the header the signature arrives in.
const SignatureHeader = "Kaab-Signature"

// DefaultTolerance is how far from now a signature's timestamp may be.
const DefaultTolerance = 300 * time.Second

// ErrInvalidSignature is wrapped by every verification failure.
var ErrInvalidSignature = errors.New("kaab webhook signature invalid")

// Options configures VerifyWithOptions.
type Options struct {
	// Tolerance is how far from Now the timestamp may be (DefaultTolerance when zero).
	Tolerance time.Duration
	// Now is the time to judge the timestamp by (time.Now when zero).
	Now time.Time
	// Secrets are more secrets to accept, for a receiver rotating its own configuration.
	Secrets []string
}

// Verify returns nil when header is a valid signature of body under secret.
func Verify(body []byte, header, secret string) error {
	return VerifyWithOptions(body, header, secret, Options{})
}

// VerifyWithOptions is Verify with a tolerance, a clock and extra secrets.
func VerifyWithOptions(body []byte, header, secret string, opts Options) error {
	fail := func(reason string) error { return fmt.Errorf("%w: %s", ErrInvalidSignature, reason) }
	if header == "" {
		return fail("no Kaab-Signature header")
	}
	var timestamp int64
	stamped := false
	var given [][]byte
	for part := range strings.SplitSeq(header, ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, "t="):
			t, err := strconv.ParseInt(part[2:], 10, 64)
			if err != nil {
				return fail("the signature's timestamp is not a number")
			}
			timestamp, stamped = t, true
		case strings.HasPrefix(part, "v1="):
			if sig, err := hex.DecodeString(part[3:]); err == nil {
				given = append(given, sig)
			}
		}
	}
	if !stamped {
		return fail("the signature has no timestamp")
	}
	tolerance := opts.Tolerance
	if tolerance == 0 {
		tolerance = DefaultTolerance
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	if d := now.Sub(time.Unix(timestamp, 0)); d > tolerance || d < -tolerance {
		return fail(fmt.Sprintf("the signature's timestamp is more than %s from now", tolerance))
	}
	for _, key := range append([]string{secret}, opts.Secrets...) {
		mac := hmac.New(sha256.New, []byte(key))
		mac.Write([]byte(strconv.FormatInt(timestamp, 10) + "."))
		mac.Write(body)
		expected := mac.Sum(nil)
		for _, sig := range given {
			if hmac.Equal(sig, expected) {
				return nil
			}
		}
	}
	return fail("no v1 signature matches the secret")
}

// Event is a webhook event: {id, type, created_at, org, mode, data}.
type Event struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	CreatedAt time.Time       `json:"created_at"`
	Org       string          `json:"org"`
	Mode      string          `json:"mode"`
	Data      json.RawMessage `json:"data"`
}

// ConstructEvent verifies a delivery and parses it.
func ConstructEvent(body []byte, header, secret string) (*Event, error) {
	return ConstructEventWithOptions(body, header, secret, Options{})
}

// ConstructEventWithOptions is ConstructEvent with VerifyWithOptions' options.
func ConstructEventWithOptions(body []byte, header, secret string, opts Options) (*Event, error) {
	if err := VerifyWithOptions(body, header, secret, opts); err != nil {
		return nil, err
	}
	var event Event
	if err := json.Unmarshal(body, &event); err != nil {
		return nil, fmt.Errorf("%w: the body is not an event: %v", ErrInvalidSignature, err)
	}
	return &event, nil
}

// Sign is the Kaab-Signature Kaab sends for body at t; for tests of a receiver.
func Sign(body []byte, secret string, t time.Time) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(t.Unix(), 10) + "."))
	mac.Write(body)
	return "t=" + strconv.FormatInt(t.Unix(), 10) + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}
