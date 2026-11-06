//go:build contract

// The Go SDK against a running Kaabist API: the scenarios of
// sdks/CONTRACT-TESTS.md, which the TypeScript and Python SDKs run too.
//
//	KAAB_E2E_API_URL=http://127.0.0.1:4000 KAAB_E2E_API_KEY=sk_… go test -tags contract ./kaab/
package kaab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kaabist/kaab-go/client"
	"github.com/kaabist/kaab-go/webhooks"
)

type contract struct {
	c           *Client
	zoo         string
	zooInternal string
	run         string
}

func setup(t *testing.T) *contract {
	t.Helper()
	api := strings.TrimRight(os.Getenv("KAAB_E2E_API_URL"), "/")
	if api == "" {
		t.Skip("KAAB_E2E_API_URL is not set")
	}
	zoo := strings.TrimRight(envOr("KAAB_E2E_ZOO_URL", "http://127.0.0.1:5000"), "/")
	resp, err := http.Post(zoo+"/_zoo/reset", "application/json", nil)
	if err == nil {
		resp.Body.Close()
	}
	return &contract{
		c:           New(os.Getenv("KAAB_E2E_API_KEY"), WithBaseURL(api+"/v1")),
		zoo:         zoo,
		zooInternal: strings.TrimRight(envOr("KAAB_E2E_ZOO_INTERNAL_URL", "http://zoo:5000"), "/"),
		run:         fmt.Sprintf("go-%x", time.Now().UnixMilli()),
	}
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func fixture(t *testing.T, name string) map[string]any {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "..", "zoo", "fixtures", "programs", name))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// check(call(...))(t) is the call's value, or the test's end.
func check[T any](v T, err error) func(*testing.T) T {
	return func(t *testing.T) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func body[T any](t *testing.T, v any) T {
	t.Helper()
	out, err := Body[T](v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestContractShorthandJob(t *testing.T) {
	k := setup(t)
	ctx := context.Background()
	golden := fixture(t, "static-product.expected.json")["data"].(map[string]any)
	job := check(k.c.Jobs.Create(ctx, body[client.CreateJobJSONBody](t, map[string]any{
		"url":     k.zooInternal + "/sites/static/p/1",
		"extract": map[string]any{"title": "h1", "sku": map[string]any{"selector": ".sku", "regex": `SKU:\s*([A-Z0-9-]+)`}},
		"options": map[string]any{"max_tier": "http"},
		"tags":    []string{k.run},
	}), WithWait(30)))(t)
	if job.Status != "succeeded" || job.Data == nil {
		t.Fatalf("job = %s", job.Status)
	}
	if got := *job.Data; got["title"] != golden["title"] || got["sku"] != golden["sku"] {
		t.Fatalf("data = %v, golden = %v", got, golden)
	}
	waited := check(k.c.Jobs.Wait(ctx, job.ID, WaitOptions{Timeout: 10 * time.Second}))(t)
	if waited.ID != job.ID {
		t.Fatalf("waited = %s", waited.ID)
	}
	limit := 1
	var ids []string
	for item, err := range k.c.Jobs.ListAll(ctx, &client.ListJobsParams{Tag: &k.run, Limit: &limit}) {
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, item.ID)
	}
	if !slices.Contains(ids, job.ID) {
		t.Fatalf("list = %v", ids)
	}
	if steps := check(k.c.Jobs.Steps(ctx, job.ID))(t); len(steps.Data) == 0 {
		t.Fatal("no steps")
	}
	if url := check(k.c.Jobs.Snapshot(ctx, job.ID, nil))(t); !strings.HasPrefix(url, "http") {
		t.Fatalf("snapshot = %q", url)
	}
}

func TestContractOnInput(t *testing.T) {
	k := setup(t)
	ctx := context.Background()
	if existing, err := k.c.Sessions.Get(ctx, "zoo-login"); err == nil {
		_ = k.c.Sessions.Delete(ctx, existing.ID)
	}
	name := "login-2fa-" + k.run
	definition := fixture(t, "login-2fa-report.json")
	publish := true
	check(k.c.Programs.Push(ctx, definition, PushOptions{Name: name, Publish: &publish}))(t)
	t.Cleanup(func() { _ = k.c.Programs.Delete(context.Background(), name) })
	created := check(k.c.Jobs.Create(ctx, body[client.CreateJobJSONBody](t, map[string]any{
		"program": name,
		"inputs":  map[string]any{"base_url": k.zooInternal + "/sites/login-2fa", "password": "secret"},
	})))(t)
	var asked []string
	done := check(k.c.Jobs.Wait(ctx, created.ID, WaitOptions{
		Timeout: 4 * time.Minute,
		OnInput: func(_ context.Context, job *client.Job) (any, error) {
			asked = append(asked, job.InputRequest.Name)
			resp, err := http.Get(k.zoo + "/_zoo/otp")
			if err != nil {
				return nil, err
			}
			defer resp.Body.Close()
			var otp struct{ OTP string }
			err = json.NewDecoder(resp.Body).Decode(&otp)
			return otp.OTP, err
		},
	}))(t)
	if done.Status != "succeeded" || len(asked) != 1 || asked[0] != "otp" {
		t.Fatalf("status = %s, asked = %v", done.Status, asked)
	}
	session := check(k.c.Sessions.Get(ctx, "zoo-login"))(t)
	t.Cleanup(func() { _ = k.c.Sessions.Delete(context.Background(), session.ID) })
}

func TestContractProgramLifecycle(t *testing.T) {
	k := setup(t)
	ctx := context.Background()
	name := "static-product-" + k.run
	definition := fixture(t, "static-product.json")
	definition["name"] = name
	lint := check(k.c.Programs.Lint(ctx, body[client.LintProgramJSONBody](t, map[string]any{"definition": definition})))(t)
	if len(lint.Errors) != 0 {
		t.Fatalf("lint = %+v", lint.Errors)
	}
	program := check(k.c.Programs.Create(ctx, body[client.CreateProgramJSONBody](t, map[string]any{"name": name, "definition": definition, "description": "contract"})))(t)
	t.Cleanup(func() { _ = k.c.Programs.Delete(context.Background(), program.ID) })
	if got := check(k.c.Programs.Get(ctx, name))(t); got.ID != program.ID {
		t.Fatalf("get = %s", got.ID)
	}
	updated := check(k.c.Programs.Update(ctx, program.ID, body[client.UpdateProgramJSONBody](t, map[string]any{"description": "updated"})))(t)
	if updated.Description == nil || *updated.Description != "updated" {
		t.Fatalf("description = %v", updated.Description)
	}
	v2def := map[string]any{}
	for key, value := range definition {
		v2def[key] = value
	}
	v2def["description"] = "v2"
	v2 := check(k.c.Programs.Versions.Create(ctx, program.ID, body[client.CreateProgramVersionJSONBody](t, map[string]any{"definition": v2def, "publish": true})))(t)
	if v2.Version != 2 {
		t.Fatalf("version = %d", v2.Version)
	}
	versions := check(k.c.Programs.Versions.List(ctx, program.ID, nil))(t)
	if len(versions.Data) != 2 {
		t.Fatalf("versions = %d", len(versions.Data))
	}
	diff := check(k.c.Programs.Versions.Diff(ctx, program.ID, 2, &client.DiffProgramVersionParams{Against: 1}))(t)
	if len(diff.Patch) != 1 {
		t.Fatalf("patch = %+v", diff.Patch)
	}
	aliased := check(k.c.Programs.Aliases.Set(ctx, program.ID, "production", body[client.SetProgramAliasJSONBody](t, map[string]any{"version": 1})))(t)
	if aliased.Aliases["production"] != 1 {
		t.Fatalf("aliases = %v", aliased.Aliases)
	}
	if err := k.c.Programs.Aliases.Delete(ctx, program.ID, "production"); err != nil {
		t.Fatal(err)
	}
	draft := false
	pushed := check(k.c.Programs.Push(ctx, definition, PushOptions{Notes: "pushed", Publish: &draft}))(t)
	if pushed.Created || pushed.Version == nil || pushed.Version.Version != 3 {
		t.Fatalf("push = %+v", pushed)
	}
}

func TestContractSessionLifecycle(t *testing.T) {
	k := setup(t)
	ctx := context.Background()
	name := "session-" + k.run
	session := check(k.c.Sessions.Create(ctx, body[client.CreateSessionJSONBody](t, map[string]any{
		"name": name, "storage_state": map[string]any{"cookies": []any{}, "origins": []any{}}, "origins": []string{"https://example.com"},
	})))(t)
	t.Cleanup(func() { _ = k.c.Sessions.Delete(context.Background(), session.ID) })
	if got := check(k.c.Sessions.Get(ctx, name))(t); got.ID != session.ID {
		t.Fatalf("get = %s", got.ID)
	}
	shared := check(k.c.Sessions.Update(ctx, session.ID, body[client.UpdateSessionJSONBody](t, map[string]any{"concurrency": "shared"})))(t)
	if string(shared.Concurrency) != "shared" {
		t.Fatalf("concurrency = %s", shared.Concurrency)
	}
	if err := k.c.Sessions.Delete(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := k.c.Sessions.Get(ctx, session.ID); !IsCode(err, "not_found") {
		t.Fatalf("after delete: %v", err)
	}
}

func TestContractProposalsAndRefusals(t *testing.T) {
	k := setup(t)
	ctx := context.Background()
	status := client.ListProposalsParamsStatus("pending")
	check(k.c.Proposals.List(ctx, &client.ListProposalsParams{Status: &status}))(t)
	_, err := k.c.Programs.Get(ctx, "missing-"+k.run)
	var kerr *Error
	if !asError(err, &kerr) || kerr.Status != 404 || kerr.Code != "not_found" || kerr.RequestID == "" {
		t.Fatalf("err = %#v", err)
	}
}

func TestContractWebhookDelivery(t *testing.T) {
	k := setup(t)
	ctx := context.Background()
	path := "contract-" + k.run
	endpoint := check(k.c.Webhooks.Create(ctx, body[client.CreateWebhookJSONBody](t, map[string]any{
		"url": k.zooInternal + "/_zoo/webhooks/" + path, "events": []string{"job.*"}, "description": "sdk contract",
	})))(t)
	t.Cleanup(func() { _ = k.c.Webhooks.Delete(context.Background(), endpoint.ID) })
	if d := check(k.c.Webhooks.Test(ctx, endpoint.ID))(t); string(d.Status) != "succeeded" {
		t.Fatalf("ping = %s", d.Status)
	}
	resp := check(http.Get(k.zoo + "/_zoo/webhooks"))(t)
	defer resp.Body.Close()
	var sink struct {
		Deliveries []struct {
			Path      string  `json:"path"`
			Signature *string `json:"signature"`
			Body      string  `json:"body"`
		} `json:"deliveries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sink); err != nil {
		t.Fatal(err)
	}
	for _, d := range sink.Deliveries {
		if d.Path != "/_zoo/webhooks/"+path || d.Signature == nil {
			continue
		}
		if err := webhooks.Verify([]byte(d.Body), *d.Signature, endpoint.Secret); err != nil {
			t.Fatalf("verify: %v", err)
		}
		if err := webhooks.Verify([]byte(d.Body), *d.Signature, "whsec_wrong"); err == nil {
			t.Fatal("a wrong secret verified")
		}
		event := check(webhooks.ConstructEvent([]byte(d.Body), *d.Signature, endpoint.Secret))(t)
		if event.Type != "ping" {
			t.Fatalf("event = %s", event.Type)
		}
		return
	}
	t.Fatalf("the sink has no delivery on %s", path)
}
