//go:build e2e

// The kaab binary against a running Kaabist API.
//
//	KAAB_E2E_API_URL=http://127.0.0.1:4000 KAAB_E2E_API_KEY=sk_… go test -tags e2e ./cmd/kaab/
//
// It builds the binary and runs it as a person would, with KAAB_API_KEY and KAAB_BASE_URL.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kaabist/kaab-go/kaab"
)

var binary string

func TestMain(m *testing.M) {
	if os.Getenv("KAAB_E2E_API_URL") == "" {
		fmt.Println("KAAB_E2E_API_URL is not set; skipping the CLI end-to-end suite")
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "kaab-e2e-")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "kaab")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

type result struct {
	stdout, stderr string
	code           int
}

func kaabCLI(t *testing.T, args ...string) result {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = append(os.Environ(),
		"KAAB_API_KEY="+os.Getenv("KAAB_E2E_API_KEY"),
		"KAAB_BASE_URL="+strings.TrimRight(os.Getenv("KAAB_E2E_API_URL"), "/")+"/v1",
		"KAAB_CONFIG_DIR="+t.TempDir(),
	)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return result{out.String(), errOut.String(), code}
}

func zooInternal() string {
	if v := os.Getenv("KAAB_E2E_ZOO_INTERNAL_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://zoo:5000"
}

// A shorthand job on the zoo, waited on, prints the golden data.
func TestE2EJobsRunPrintsTheGoldenData(t *testing.T) {
	r := kaabCLI(t, "jobs", "run",
		"--url", zooInternal()+"/sites/static/p/1",
		"--extract", "@testdata/static-p1.schema.json",
		"--wait", "30")
	if r.code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	golden, err := os.ReadFile(filepath.Join("testdata", "static-p1.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if r.stdout != strings.ReplaceAll(string(golden), "\r\n", "\n") {
		t.Fatalf("stdout:\n%s\nwant:\n%s", r.stdout, golden)
	}
	if !strings.Contains(r.stderr, " succeeded in ") {
		t.Fatalf("stderr = %q", r.stderr)
	}

	id := strings.Fields(r.stderr)[0]
	got := kaabCLI(t, "jobs", "get", id, "--json")
	var job struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if got.code != 0 || json.Unmarshal([]byte(got.stdout), &job) != nil || job.ID != id || job.Status != "succeeded" {
		t.Fatalf("jobs get: %d %s", got.code, got.stdout)
	}
	if steps := kaabCLI(t, "jobs", "steps", id); steps.code != 0 || !strings.Contains(steps.stdout, "STEP") {
		t.Fatalf("jobs steps: %d %s %s", steps.code, steps.stdout, steps.stderr)
	}
}

func TestE2EProgramsLintPushPull(t *testing.T) {
	name := fmt.Sprintf("cli-static-%x", time.Now().UnixMilli())
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "zoo", "fixtures", "programs", "static-product.json"))
	if err != nil {
		t.Fatal(err)
	}
	var definition map[string]any
	if err := json.Unmarshal(data, &definition); err != nil {
		t.Fatal(err)
	}
	definition["name"] = name
	file := filepath.Join(t.TempDir(), name+".json")
	encoded, _ := json.Marshal(definition)
	if err := os.WriteFile(file, encoded, 0o644); err != nil {
		t.Fatal(err)
	}

	if r := kaabCLI(t, "programs", "lint", file); r.code != 0 || !strings.Contains(r.stdout, ": 0 error(s), ") {
		t.Fatalf("lint: %d %s %s", r.code, r.stdout, r.stderr)
	}
	broken := filepath.Join(t.TempDir(), "broken.json")
	_ = os.WriteFile(broken, []byte(`{"kaab":"1","name":"broken","steps":[{"teleport":"x"}]}`), 0o644)
	if r := kaabCLI(t, "programs", "lint", broken); r.code != 1 {
		t.Fatalf("lint of a broken definition exited %d: %s %s", r.code, r.stdout, r.stderr)
	}

	if r := kaabCLI(t, "programs", "push", file, "--publish"); r.code != 0 || !strings.Contains(r.stdout, "created with version 1, published") {
		t.Fatalf("push: %d %s %s", r.code, r.stdout, r.stderr)
	}
	t.Cleanup(func() {
		// The CLI has no `programs delete`; the SDK removes it.
		c := kaab.New(os.Getenv("KAAB_E2E_API_KEY"), kaab.WithBaseURL(strings.TrimRight(os.Getenv("KAAB_E2E_API_URL"), "/")+"/v1"))
		_ = c.Programs.Delete(context.Background(), name)
	})
	if r := kaabCLI(t, "programs", "push", file, "--notes", "second"); r.code != 0 || !strings.Contains(r.stdout, "added version 2, draft") {
		t.Fatalf("second push: %d %s %s", r.code, r.stdout, r.stderr)
	}
	pulled := kaabCLI(t, "programs", "pull", name+"@1")
	var back map[string]any
	if pulled.code != 0 || json.Unmarshal([]byte(pulled.stdout), &back) != nil || back["name"] != name {
		t.Fatalf("pull: %d %s %s", pulled.code, pulled.stdout, pulled.stderr)
	}
	if r := kaabCLI(t, "programs", "list"); r.code != 0 || !strings.Contains(r.stdout, name) {
		t.Fatalf("list: %d %s %s", r.code, r.stdout, r.stderr)
	}
	if r := kaabCLI(t, "programs", "diff", name, "1", "2"); r.code != 0 {
		t.Fatalf("diff: %d %s %s", r.code, r.stdout, r.stderr)
	}
}

func TestE2EProposalsListAndAccount(t *testing.T) {
	if r := kaabCLI(t, "proposals", "list"); r.code != 0 || !strings.Contains(r.stdout, "KIND") {
		t.Fatalf("proposals list: %d %s %s", r.code, r.stdout, r.stderr)
	}
	if r := kaabCLI(t, "credits"); r.code != 0 || !strings.Contains(r.stdout, "available") {
		t.Fatalf("credits: %d %s %s", r.code, r.stdout, r.stderr)
	}
	if r := kaabCLI(t, "auth", "status"); r.code != 0 || !strings.Contains(r.stdout, "Authenticated to") {
		t.Fatalf("auth status: %d %s %s", r.code, r.stdout, r.stderr)
	}
}
