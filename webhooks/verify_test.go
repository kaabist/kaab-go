package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"
)

const secret = "whsec_test_0123456789abcdef"

var (
	body = []byte(`{"id":"evt_01j9zzzzzzzzzzzzzzzzzzzzzz","type":"ping","created_at":"2026-10-05T10:00:00Z","org":"org_01j9zzzzzzzzzzzzzzzzzzzzzz","mode":"test","data":{"url":"https://api.kaab.ist/v1/webhooks/whk_1"}}`)
	now  = time.Unix(1_790_000_000, 0)
)

func header(b []byte, key string, t time.Time) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = fmt.Fprintf(mac, "%d.%s", t.Unix(), b)
	return fmt.Sprintf("t=%d,v1=%s", t.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

func TestVerifyAcceptsWhatKaabSigns(t *testing.T) {
	sig := header(body, secret, now)
	if err := VerifyWithOptions(body, sig, secret, Options{Now: now}); err != nil {
		t.Fatalf("valid signature refused: %v", err)
	}
	if got := Sign(body, secret, now); got != sig {
		t.Fatalf("Sign = %q, want %q", got, sig)
	}
	if err := VerifyWithOptions(body, sig, "whsec_other", Options{Now: now}); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("wrong secret accepted: %v", err)
	}
	if err := VerifyWithOptions(append([]byte{}, append(body, ' ')...), sig, secret, Options{Now: now}); err == nil {
		t.Fatal("changed body accepted")
	}
}

func TestVerifyTolerance(t *testing.T) {
	sig := header(body, secret, now.Add(-301*time.Second))
	if err := VerifyWithOptions(body, sig, secret, Options{Now: now}); err == nil {
		t.Fatal("stale signature accepted")
	}
	if err := VerifyWithOptions(body, sig, secret, Options{Now: now, Tolerance: 10 * time.Minute}); err != nil {
		t.Fatalf("signature within the tolerance refused: %v", err)
	}
}

func TestVerifyRotationOverlap(t *testing.T) {
	old := header(body, "whsec_old", now)
	both := header(body, "whsec_new", now) + "," + old[len(fmt.Sprintf("t=%d,", now.Unix())):]
	for _, key := range []string{"whsec_old", "whsec_new"} {
		if err := VerifyWithOptions(body, both, key, Options{Now: now}); err != nil {
			t.Fatalf("%s refused during the overlap: %v", key, err)
		}
	}
	if err := VerifyWithOptions(body, header(body, "whsec_b", now), "whsec_a", Options{Now: now, Secrets: []string{"whsec_b"}}); err != nil {
		t.Fatalf("an extra secret was not tried: %v", err)
	}
}

func TestVerifyMalformed(t *testing.T) {
	for _, h := range []string{"", "v1=abc", "t=soon,v1=abc", "t=1"} {
		if err := Verify(body, h, secret); err == nil {
			t.Fatalf("%q accepted", h)
		}
	}
}

func TestConstructEvent(t *testing.T) {
	event, err := ConstructEventWithOptions(body, header(body, secret, now), secret, Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if event.Type != "ping" || event.ID == "" {
		t.Fatalf("event = %+v", event)
	}
	if _, err := ConstructEvent(body, "", secret); err == nil {
		t.Fatal("an unsigned body was parsed")
	}
}
