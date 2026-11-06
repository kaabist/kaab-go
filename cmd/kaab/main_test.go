package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

var update = flag.Bool("update", false, "rewrite the --help golden files")

// memoryKeyring is a keychain; broken makes every call fail, as on a machine without one.
type memoryKeyring struct {
	mu      sync.Mutex
	entries map[string]string
	broken  bool
}

func (m *memoryKeyring) Set(service, user, secret string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.broken {
		return errors.New("no keychain")
	}
	m.entries[service+"/"+user] = secret
	return nil
}

func (m *memoryKeyring) Get(service, user string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.entries[service+"/"+user]; ok && !m.broken {
		return v, nil
	}
	return "", errors.New("not found")
}

func (m *memoryKeyring) Delete(service, user string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, service+"/"+user)
	return nil
}

type harness struct {
	t       *testing.T
	vars    map[string]string
	keyring *memoryKeyring
	opened  []string
	api     *httptest.Server
	seen    []*http.Request
	bodies  []string
	handler func(w http.ResponseWriter, r *http.Request)
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, keyring: &memoryKeyring{entries: map[string]string{}}}
	h.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.seen = append(h.seen, r)
		h.bodies = append(h.bodies, string(body))
		if h.handler == nil {
			http.NotFound(w, r)
			return
		}
		h.handler(w, r)
	}))
	t.Cleanup(h.api.Close)
	h.vars = map[string]string{"KAAB_CONFIG_DIR": t.TempDir(), "KAAB_BASE_URL": h.api.URL + "/v1"}
	return h
}

func (h *harness) run(stdin string, args ...string) (string, string, int) {
	var out, errOut bytes.Buffer
	e := &env{
		stdout:  &out,
		stderr:  &errOut,
		stdin:   strings.NewReader(stdin),
		getenv:  func(k string) string { return h.vars[k] },
		secrets: h.keyring,
		open:    func(url string) error { h.opened = append(h.opened, url); return nil },
	}
	code := run(context.Background(), args, e)
	return out.String(), errOut.String(), code
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func jobBody(status string, extra map[string]any) map[string]any {
	job := map[string]any{
		"id": "job_01j9zzzzzzzzzzzzzzzzzzzzzz", "object": "job", "mode": "test", "status": status,
		"reason": nil, "program": map[string]any{"id": nil, "name": nil, "version": nil, "version_id": nil},
		"inputs_hash": nil, "data": nil, "raw": nil, "warnings": []any{},
		"meta":  map[string]any{"tier": "http", "tier_requested": "http", "escalations": []any{}, "region": "eu", "final_url": "https://example.com/", "cache": "miss", "duration_ms": 1200, "parked_ms": 0, "credits": map[string]any{"reserved": 2, "settled": 1}},
		"error": nil, "input_request": nil, "attention": nil, "progress": nil, "tags": []any{}, "webhook_url": nil,
		"created_at": "2026-10-05T10:00:00Z", "started_at": nil, "finished_at": nil, "expires_at": nil,
	}
	for k, v := range extra {
		job[k] = v
	}
	return job
}

func TestVersion(t *testing.T) {
	h := newHarness(t)
	out, _, code := h.run("", "version")
	if code != 0 || !strings.HasPrefix(out, "kaab dev (sdk ") {
		t.Fatalf("code = %d, out = %q", code, out)
	}
	out, _, _ = h.run("", "--version")
	if out != "kaab dev\n" {
		t.Fatalf("--version = %q", out)
	}
}

// TestHelpGolden pins the --help of every command: `go test ./cmd/kaab -update`
// rewrites testdata/help after a deliberate change.
func TestHelpGolden(t *testing.T) {
	root := newRoot(&env{stdout: io.Discard, stderr: io.Discard, stdin: strings.NewReader(""), getenv: func(string) string { return "" }})
	var paths [][]string
	var walk func(cmd *cobra.Command, path []string)
	walk = func(cmd *cobra.Command, path []string) {
		if cmd.Hidden || cmd.Name() == "help" {
			return
		}
		paths = append(paths, path)
		for _, child := range cmd.Commands() {
			walk(child, append(append([]string{}, path...), child.Name()))
		}
	}
	walk(root, nil)
	sort.Slice(paths, func(i, j int) bool { return strings.Join(paths[i], " ") < strings.Join(paths[j], " ") })
	seen := map[string]bool{}
	for _, path := range paths {
		name := "kaab"
		if len(path) > 0 {
			name = "kaab_" + strings.Join(path, "_")
		}
		seen[name+".txt"] = true
		h := newHarness(t)
		out, _, code := h.run("", append(append([]string{}, path...), "--help")...)
		if code != 0 {
			t.Fatalf("%s --help exited %d", name, code)
		}
		golden := filepath.Join("testdata", "help", name+".txt")
		if *update {
			if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(golden, []byte(out), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("%s: %v (run `go test ./cmd/kaab -update`)", golden, err)
		}
		if strings.ReplaceAll(string(want), "\r\n", "\n") != out {
			t.Errorf("%s --help changed; run `go test ./cmd/kaab -update` if that is intended:\n%s", strings.Join(path, " "), out)
		}
	}
	entries, _ := os.ReadDir(filepath.Join("testdata", "help"))
	for _, entry := range entries {
		if !seen[entry.Name()] && !*update {
			t.Errorf("testdata/help/%s belongs to no command", entry.Name())
		}
	}
}

func TestAuthLoginStatusLogout(t *testing.T) {
	h := newHarness(t)
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk_test_good" {
			reply(w, 401, map[string]any{"error": map[string]any{"code": "unauthenticated", "message": "bad key", "request_id": "req_1"}})
			return
		}
		reply(w, 200, map[string]any{"id": "org_1", "object": "org", "name": "Acme", "slug": "acme", "plan": "developer", "region": "eu", "created_at": "2026-10-05T10:00:00Z", "quotas": map[string]any{}})
	}
	if _, errOut, code := h.run("", "auth", "login", "--key", "sk_test_bad"); code != 1 || !strings.Contains(errOut, "refused the key") {
		t.Fatalf("bad key: code %d, %q", code, errOut)
	}
	if _, errOut, code := h.run("", "auth", "login", "--key", "nonsense"); code != 1 || !strings.Contains(errOut, "sk_live_") {
		t.Fatalf("malformed key: code %d, %q", code, errOut)
	}
	out, errOut, code := h.run("sk_test_good\n", "auth", "login", "--panel-url", "https://panel.test")
	if code != 0 || !strings.Contains(out, "Acme (org_1) with a test key") || !strings.Contains(out, "OS keychain") {
		t.Fatalf("login: code %d, out %q, err %q", code, out, errOut)
	}
	if h.keyring.entries["kaab/default:test"] != "sk_test_good" {
		t.Fatalf("keychain = %v", h.keyring.entries)
	}
	config, _ := os.ReadFile(filepath.Join(h.vars["KAAB_CONFIG_DIR"], "config.toml"))
	if strings.Contains(string(config), "sk_test_good") {
		t.Fatal("the key was written to config.toml")
	}
	out, _, code = h.run("", "auth", "status")
	if code != 0 || !strings.Contains(out, "Authenticated to Acme (org_1), plan developer") || !strings.Contains(out, "* ") {
		t.Fatalf("status: %d %q", code, out)
	}
	out, _, _ = h.run("", "open", "job", "job_1")
	if strings.TrimSpace(out) != "https://panel.test/jobs/job_1" || len(h.opened) != 1 {
		t.Fatalf("open: %q %v", out, h.opened)
	}
	if out, _, code = h.run("", "auth", "logout"); code != 0 || !strings.Contains(out, `profile "default"`) {
		t.Fatalf("logout: %d %q", code, out)
	}
	if len(h.keyring.entries) != 0 {
		t.Fatalf("keychain after logout = %v", h.keyring.entries)
	}
	if _, errOut, code := h.run("", "credits"); code != 1 || !strings.Contains(errOut, "kaab auth login") {
		t.Fatalf("without a key: %d %q", code, errOut)
	}
}

func TestAuthFallsBackToAFile(t *testing.T) {
	h := newHarness(t)
	h.keyring.broken = true
	h.handler = func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 200, map[string]any{"id": "org_1", "object": "org", "name": "Acme", "slug": "acme", "plan": "free", "region": "eu", "created_at": "2026-10-05T10:00:00Z", "quotas": map[string]any{}})
	}
	out, _, code := h.run("", "--profile", "ci", "auth", "login", "--key", "sk_live_abc")
	if code != 0 || !strings.Contains(out, "credentials.toml") {
		t.Fatalf("login: %d %q", code, out)
	}
	path := filepath.Join(h.vars["KAAB_CONFIG_DIR"], "credentials.toml")
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "sk_live_abc") {
		t.Fatalf("credentials = %q, %v", data, err)
	}
	if _, _, code := h.run("", "--profile", "ci", "credits", "--json"); code != 0 && code != 1 {
		t.Fatalf("credits: %d", code)
	}
	if h.seen[len(h.seen)-1].Header.Get("Authorization") != "Bearer sk_live_abc" {
		t.Fatal("the file key was not used")
	}
	h.run("", "--profile", "ci", "auth", "logout")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credentials.toml after logout: %v", err)
	}
}

func TestFileLoginKeepsKeychainKeyOfOtherMode(t *testing.T) {
	h := newHarness(t)
	h.handler = func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 200, map[string]any{"id": "org_1", "object": "org", "name": "Acme", "slug": "acme", "plan": "free", "region": "eu", "created_at": "2026-10-05T10:00:00Z", "quotas": map[string]any{}})
	}
	if _, errOut, code := h.run("", "auth", "login", "--key", "sk_live_kc"); code != 0 {
		t.Fatalf("live login: %d %q", code, errOut)
	}
	if _, errOut, code := h.run("", "auth", "login", "--key", "sk_test_file", "--file-storage"); code != 0 {
		t.Fatalf("test login: %d %q", code, errOut)
	}
	// The live key is still read from the keychain, the test key from the file.
	h.run("", "credits", "--json")
	if got := h.seen[len(h.seen)-1].Header.Get("Authorization"); got != "Bearer sk_live_kc" {
		t.Fatalf("live command sent %q", got)
	}
	h.run("", "--test", "credits", "--json")
	if got := h.seen[len(h.seen)-1].Header.Get("Authorization"); got != "Bearer sk_test_file" {
		t.Fatalf("test command sent %q", got)
	}
	config, _ := os.ReadFile(filepath.Join(h.vars["KAAB_CONFIG_DIR"], "config.toml"))
	if !strings.Contains(string(config), `live = "keyring"`) || !strings.Contains(string(config), `test = "file"`) {
		t.Fatalf("config.toml:\n%s", config)
	}
	out, _, _ := h.run("", "auth", "status")
	if !strings.Contains(out, "live:keyring,test:file") {
		t.Fatalf("status: %q", out)
	}
}

func TestOldStorageStringDecodes(t *testing.T) {
	h := newHarness(t)
	dir := h.vars["KAAB_CONFIG_DIR"]
	old := "current = \"default\"\n[profiles.default]\nstorage = \"file\"\nmodes = [\"live\", \"test\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &store{dir: dir, secrets: h.keyring}
	cfg, err := s.load()
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Profiles["default"].Storage
	if len(got) != 2 || got["live"] != "file" || got["test"] != "file" {
		t.Fatalf("storage = %v", got)
	}
	// Saved again, it is the per-mode table.
	if err := s.save(cfg); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	if !strings.Contains(string(data), "[profiles.default.storage]") {
		t.Fatalf("config.toml:\n%s", data)
	}
}

func TestJobsRunExitCodes(t *testing.T) {
	h := newHarness(t)
	h.vars["KAAB_API_KEY"] = "sk_test_env"
	var status string
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		switch status {
		case "succeeded":
			reply(w, 200, jobBody("succeeded", map[string]any{"data": map[string]any{"title": "Kulaklık"}}))
		case "failed":
			reply(w, 200, jobBody("failed", map[string]any{"error": map[string]any{"code": "blocked", "message": "challenge page", "retryable": true, "step": nil, "tier": "http"}}))
		case "awaiting_input":
			reply(w, 200, jobBody("awaiting_input", map[string]any{"input_request": map[string]any{"name": "otp", "prompt": "code", "type": "string", "pattern": nil, "keep_alive_until": nil, "expires_at": "2026-10-05T10:10:00Z", "collect": "api", "screenshot": nil, "resolve_url": nil}}))
		default:
			reply(w, 402, map[string]any{"error": map[string]any{"code": "insufficient_credits", "message": "out of credits", "request_id": "req_9"}})
		}
	}
	schema := filepath.Join(t.TempDir(), "schema.json")
	_ = os.WriteFile(schema, []byte(`{"title":"h1"}`), 0o644)

	status = "succeeded"
	out, errOut, code := h.run("", "jobs", "run", "--url", "https://example.com/p/1", "--extract", "@"+schema, "--wait", "30")
	if code != 0 || strings.TrimSpace(out) != "{\n  \"title\": \"Kulaklık\"\n}" || !strings.Contains(errOut, "succeeded in 1.2 s · http · 1 credit") {
		t.Fatalf("succeeded: %d %q %q", code, out, errOut)
	}
	req := h.seen[len(h.seen)-1]
	if req.Header.Get("Prefer") != "wait=30" || req.Header.Get("Idempotency-Key") == "" || h.bodies[len(h.bodies)-1] != `{"extract":{"title":"h1"},"url":"https://example.com/p/1"}` {
		t.Fatalf("request: %v %q", req.Header, h.bodies[len(h.bodies)-1])
	}

	status = "failed"
	if _, errOut, code := h.run("", "jobs", "run", "--program", "demo@2", "--input", "n=3", "--input", "q=hello", "--wait", "5"); code != 2 || !strings.Contains(errOut, "blocked: challenge page") {
		t.Fatalf("failed: %d %q", code, errOut)
	}
	if body := h.bodies[len(h.bodies)-1]; body != `{"inputs":{"n":3,"q":"hello"},"program":"demo@2"}` {
		t.Fatalf("program body = %s", body)
	}

	status = "awaiting_input"
	if _, errOut, code := h.run("", "jobs", "get", "job_1", "--json"); code != 3 || !strings.Contains(errOut, "kaab jobs input job_01j9zzzzzzzzzzzzzzzzzzzzzz otp=<value>") {
		t.Fatalf("parked: %d %q", code, errOut)
	}

	status = ""
	if _, errOut, code := h.run("", "jobs", "run", "--program", "demo"); code != 1 || !strings.Contains(errOut, "insufficient_credits: out of credits (request req_9)") {
		t.Fatalf("refused: %d %q", code, errOut)
	}
	if _, errOut, code := h.run("", "jobs", "run", "--url", "https://example.com"); code != 1 || !strings.Contains(errOut, "--extract") {
		t.Fatalf("no schema: %d %q", code, errOut)
	}
	if _, _, code := h.run("", "jobs", "resume", "job_1"); code != 1 {
		t.Fatalf("resume without a verb: %d", code)
	}
}

func TestProgramsLintAndPush(t *testing.T) {
	h := newHarness(t)
	h.vars["KAAB_API_KEY"] = "sk_test_env"
	program := map[string]any{"id": "prg_1", "object": "program", "name": "demo", "description": nil, "latest_version": 2, "aliases": map[string]any{"latest": 2}, "canary": nil, "created_at": "2026-10-05T10:00:00Z", "updated_at": "2026-10-05T10:00:00Z"}
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/programs/lint":
			reply(w, 200, map[string]any{"errors": []any{map[string]any{"path": "steps[0]", "code": "unknown_action", "message": "no such action"}}, "warnings": []any{}, "normalized": nil})
		case r.Method == "POST" && r.URL.Path == "/v1/programs":
			// The program exists: the push adds a version (DL-EPIC-65-11).
			reply(w, 409, map[string]any{"error": map[string]any{"code": "validation_failed", "message": "exists", "request_id": "req_1", "details": map[string]any{"program_id": "prg_1"}}})
		case r.Method == "GET" && r.URL.Path == "/v1/programs/prg_1":
			reply(w, 200, program)
		case r.Method == "POST" && r.URL.Path == "/v1/programs/prg_1/versions":
			reply(w, 201, map[string]any{"id": "pv_2", "object": "program_version", "program_id": "prg_1", "version": 2, "status": "published", "definition": map[string]any{}, "definition_hash": "sha256:x", "lint": map[string]any{"errors": []any{}, "warnings": []any{}}, "notes": nil, "source": "api", "created_at": "2026-10-05T10:00:00Z", "published_at": nil})
		default:
			http.NotFound(w, r)
		}
	}
	file := filepath.Join(t.TempDir(), "demo.json")
	_ = os.WriteFile(file, []byte(`{"kaab":"1","name":"demo","steps":[]}`), 0o644)
	out, errOut, code := h.run("", "programs", "lint", file)
	if code != 1 || !strings.Contains(out, "unknown_action") || !strings.Contains(errOut, "1 error(s)") {
		t.Fatalf("lint: %d %q %q", code, out, errOut)
	}
	out, _, code = h.run("", "programs", "push", file, "--publish")
	if code != 0 || !strings.Contains(out, "demo (prg_1): added version 2, published") {
		t.Fatalf("push: %d %q", code, out)
	}
	if !strings.Contains(h.bodies[len(h.bodies)-2], `"publish":true`) {
		t.Fatalf("push body = %s", h.bodies[len(h.bodies)-2])
	}
}

func TestInputsAndJSONArgs(t *testing.T) {
	got, err := parseInputs([]string{"a=1", "b=true", "c=text", `d={"x":1}`, "e="})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"a": float64(1), "b": true, "c": "text", "d": map[string]any{"x": float64(1)}, "e": ""}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("inputs = %s", gotJSON)
	}
	if _, err := parseInputs([]string{"novalue"}); err == nil {
		t.Fatal("a pair without = was accepted")
	}
	var v map[string]any
	if err := readJSONArg("-", strings.NewReader(`{"a":1}`), &v); err != nil || v["a"] != float64(1) {
		t.Fatalf("stdin: %v %v", v, err)
	}
	if err := readJSONArg("{oops", nil, &v); err == nil {
		t.Fatal("bad JSON accepted")
	}
}

// snapshotHarness answers the snapshot route with a redirect to a file server that serve answers.
func snapshotHarness(t *testing.T, serve http.HandlerFunc) *harness {
	t.Helper()
	files := httptest.NewServer(serve)
	t.Cleanup(files.Close)
	h := newHarness(t)
	h.vars["KAAB_API_KEY"] = "sk_test_x"
	h.handler = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/jobs/job_1/snapshot" {
			http.Redirect(w, r, files.URL+"/snap.html", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	}
	return h
}

func TestSnapshotFailureKeepsExistingFile(t *testing.T) {
	h := snapshotHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		// Half the body the headers promise, then a reset connection.
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 1000\r\nContent-Type: text/html\r\n\r\n<html>half")
		_ = buf.Flush()
		_ = conn.Close()
	})
	dir := t.TempDir()
	target := filepath.Join(dir, "snap.html")
	if err := os.WriteFile(target, []byte("the old snapshot"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, errOut, code := h.run("", "jobs", "snapshot", "job_1", "-o", target)
	if code == 0 {
		t.Fatalf("a broken download succeeded: %q", errOut)
	}
	if data, _ := os.ReadFile(target); string(data) != "the old snapshot" {
		t.Fatalf("target = %q", data)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("left behind: %v", entries)
	}

	// A complete download replaces it.
	h = snapshotHarness(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>new</html>")) })
	if _, errOut, code := h.run("", "jobs", "snapshot", "job_1", "-o", target); code != 0 {
		t.Fatalf("download: %d %q", code, errOut)
	}
	if data, _ := os.ReadFile(target); string(data) != "<html>new</html>" {
		t.Fatalf("target = %q", data)
	}
}

func TestSnapshotStalledBodyTimesOut(t *testing.T) {
	saved := downloadIdleTimeout
	downloadIdleTimeout = 100 * time.Millisecond
	t.Cleanup(func() { downloadIdleTimeout = saved })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	h := snapshotHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("<html>"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	target := filepath.Join(t.TempDir(), "snap.html")
	started := time.Now()
	_, errOut, code := h.run("", "jobs", "snapshot", "job_1", "-o", target)
	if code == 0 || !strings.Contains(errOut, "stalled") {
		t.Fatalf("code %d, %q", code, errOut)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("took %s", elapsed)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target exists: %v", err)
	}
}
