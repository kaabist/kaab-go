# Kaabist for Go, and the `kaab` CLI

The official Go client for the [Kaabist](https://kaab.ist) API, and the `kaab` command-line tool
built on it.

Kaabist runs web automation as programs: log in, fill a form, download a document, read typed
values. Programs are versioned, resume from checkpoints, never submit twice after a crash, and
pause for a person when a site asks for a one-time code. This module gives you every API
operation as a typed method, with safe retries, iterators over paged lists and a `Wait` helper
that answers paused jobs for you.

```bash
go get github.com/kaabist/kaab-go
```

Go 1.24 or newer. Three packages:

| Package | |
|---|---|
| `github.com/kaabist/kaab-go/kaab` | the client |
| `github.com/kaabist/kaab-go/client` | request and response types |
| `github.com/kaabist/kaab-go/webhooks` | webhook verification, no client needed |

## Quick start

Create an API key in the panel (**Settings → API keys**, [app.kaab.ist](https://app.kaab.ist/settings/api-keys)).
Use an `sk_test_…` key while you build: test jobs run on the same engine but spend no credits,
within a daily job quota.

```bash
export KAAB_API_KEY=sk_test_...
```

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/kaabist/kaab-go/client"
	"github.com/kaabist/kaab-go/kaab"
)

func main() {
	ctx := context.Background()
	c := kaab.New("") // "" reads KAAB_API_KEY

	body, err := kaab.Body[client.CreateJobJSONBody](map[string]any{
		"url":     "https://example.com",
		"extract": map[string]any{"title": "h1"},
	})
	if err != nil {
		log.Fatal(err)
	}

	// Wait up to 30 s for the result in the same request.
	job, err := c.Jobs.Create(ctx, body, kaab.WithWait(30))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(job.Status, job.Data)
}
```

`kaab.Body[T]` builds any request body from a map, a struct or raw JSON.

## Run a saved program

A program is a flow you defined once (in the panel, with the recorder, or as JSON). Run a version
by name and pass its inputs:

```go
body, _ := kaab.Body[client.CreateJobJSONBody](map[string]any{
	"program": "supplier-invoices@production",
	"inputs":  map[string]any{"month": "2026-10"},
})
job, err := c.Jobs.Create(ctx, body)
```

## Wait for a job, and answer it on the way

`Jobs.Wait` follows a job to the end. If the site asks for a one-time code, or the job needs a
decision, your callbacks answer it and the job continues from where it stopped:

```go
done, err := c.Jobs.Wait(ctx, job.ID, kaab.WaitOptions{
	Timeout: 10 * time.Minute,
	// awaiting_input: e.g. a 2FA code the account owner types in
	OnInput: func(ctx context.Context, job *client.Job) (any, error) {
		return askTheUser(job.InputRequest.Prompt)
	},
	// needs_attention: kaab.SkipStep(), kaab.RetryFromCheckpoint(), kaab.Continue(), kaab.Abort()
	OnAttention: func(ctx context.Context, job *client.Job) (kaab.ResumeAction, error) {
		return kaab.SkipStep(), nil
	},
})
```

With a nil callback, `Wait` returns as soon as the job pauses in that state, so you can hand it to
the hosted resolve page instead.

## Lists

`ListAll` methods are iterators that fetch pages as you go:

```go
for program, err := range c.Programs.ListAll(ctx, nil) {
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(program.Name)
}
```

## Webhooks

Verify a delivery on its **raw** body before you trust it:

```go
import "github.com/kaabist/kaab-go/webhooks"

func hook(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	event, err := webhooks.ConstructEvent(raw, r.Header.Get(webhooks.SignatureHeader), os.Getenv("KAAB_WEBHOOK_SECRET"))
	if err != nil {
		http.Error(w, "bad signature", http.StatusBadRequest)
		return
	}
	if event.Type == "job.succeeded" {
		// event.Data is the raw JSON payload
	}
	w.WriteHeader(http.StatusNoContent)
}
```

## Errors, retries and idempotency

- Every API failure is a `*kaab.Error` with `Code`, `Status`, `RequestID` and `Details`; use
  `errors.As`.
- Reads, deletes and keyed creates are retried on `408`, `429` and `5xx` with backoff (2 retries
  by default).
- `Jobs.Create` and `Jobs.Batch` always send an `Idempotency-Key`, so a retry never starts a second
  job. To retry yourself, make the key once and reuse it:

```go
key := kaab.NewIdempotencyKey()
job, err := c.Jobs.Create(ctx, body, kaab.WithIdempotencyKey(key))

var apiErr *kaab.Error
if errors.As(err, &apiErr) && apiErr.Code == "insufficient_credits" {
	// ...
}
```

A `connection_error` or `timeout` on a keyed call carries the key it sent in
`Error.IdempotencyKey`: the job may already exist, and the same key returns it instead of creating
another.

## Configuration

```go
c := kaab.New("sk_live_...",
	kaab.WithBaseURL("https://api.kaab.ist/v1"), // or KAAB_BASE_URL
	kaab.WithTimeout(60*time.Second),             // per attempt
	kaab.WithMaxRetries(2),
	kaab.WithHTTPClient(&http.Client{}),
	kaab.WithHeader("X-Trace", "abc"),
)
```

Per request: `kaab.WithWait`, `WithIdempotencyKey`, `WithRequestTimeout`, `WithRequestMaxRetries`,
`WithRequestHeader`.

## The `kaab` CLI

Download a build for Linux, macOS or Windows from the
[releases page](https://github.com/kaabist/kaab-go/releases) (verify it against `checksums.txt`),
or build it with Go:

```bash
go install github.com/kaabist/kaab-go/cmd/kaab@latest
```

```bash
kaab auth login
kaab jobs run --url https://example.com --extract '{"title":"h1"}' --wait 30
kaab programs push report.json
kaab programs list
```

Also `kaab programs pull|lint|diff|alias set`, `kaab sessions`, `kaab proposals`,
`kaab schedules`, `kaab webhooks`, `kaab credits`, `kaab usage`, `kaab open job <id>` and
`kaab completion`. Exit codes: `0` ok, `1` error, `2` job failed, `3` job paused for input or
attention. [CLI guide](https://docs.kaab.ist/guides/cli).

## Links

- [SDK guide](https://docs.kaab.ist/guides/sdks) and [API reference](https://docs.kaab.ist/reference/api)
- [Webhooks](https://docs.kaab.ist/guides/webhooks), [keys and test mode](https://docs.kaab.ist/getting-started/keys-and-modes)
- [Changelog](./CHANGELOG.md) · support@kaab.ist

This repository is a mirror of the SDK as released; versions follow the API (`v1.40.x` speaks
API 1.40). Issues and questions: support@kaab.ist. MIT licensed.
