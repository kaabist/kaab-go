package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// apiKey is a full-length secret, not a prefix such as the "sk_test_" in a flag's help.
var apiKey = regexp.MustCompile(`sk_(live|test)_[A-Za-z0-9]{8,}`)

func readDocsDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, entry := range entries {
		body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name()] = string(body)
	}
	return files
}

// TestDocsGenerate pins `kaab docs generate --format md`: kebab file names, frontmatter,
// docs-site links, determinism, stale-page removal and no hidden commands.
func TestDocsGenerate(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kaab-gone.md"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.md"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := h.run("", "docs", "generate", "--format", "md", "--dir", dir)
	if code != 0 || !strings.Contains(out, "wrote ") {
		t.Fatalf("code %d, out %q, err %q", code, out, errOut)
	}
	first := readDocsDir(t, dir)

	if _, ok := first["kaab-gone.md"]; ok {
		t.Error("the stale kaab-gone.md was not removed")
	}
	if first["index.md"] != "kept" {
		t.Error("index.md (not a kaab*.md page) was touched")
	}
	for _, name := range []string{"kaab.md", "kaab-jobs.md", "kaab-jobs-run.md", "kaab-auth-login.md", "kaab-version.md", "kaab-completion.md"} {
		if _, ok := first[name]; !ok {
			t.Errorf("%s is missing", name)
		}
	}
	for name, body := range first {
		if name == "index.md" {
			continue
		}
		if strings.Contains(name, "_") || strings.HasPrefix(name, "kaab-docs") || strings.HasPrefix(name, "kaab-help") {
			t.Errorf("unexpected page %s", name)
		}
		if strings.Contains(body, "\r") || strings.Contains(body, "Auto generated") {
			t.Errorf("%s has CR line endings or a date tag", name)
		}
		for i, line := range strings.Split(body, "\n") {
			if line != strings.TrimRight(line, " \t") {
				t.Errorf("%s:%d has trailing whitespace", name, i+1)
			}
		}
		if strings.Contains(body, "<!--") {
			t.Errorf("%s contains an HTML comment", name)
		}
		if strings.Contains(body, "kaab docs") {
			t.Errorf("%s mentions the hidden docs command", name)
		}
		if apiKey.MatchString(body) {
			t.Errorf("%s contains an API key", name)
		}
		if strings.Contains(body, ".md)") {
			t.Errorf("%s links to a .md file", name)
		}
	}

	run := first["kaab-jobs-run.md"]
	header := "---\ntitle: \"kaab jobs run\"\ndescription: \"" + mustShort(t, "jobs", "run") + "\"\n---\n\n"
	if !strings.HasPrefix(run, header) {
		t.Errorf("kaab-jobs-run.md header:\n%s", run[:min(len(run), 300)])
	}
	if strings.Contains(run, "## kaab jobs run\n") || !strings.Contains(run, "\n## Options\n") {
		t.Errorf("kaab-jobs-run.md headings:\n%s", run)
	}
	if !strings.Contains(run, "* [kaab jobs](/reference/cli/kaab-jobs) - ") {
		t.Errorf("kaab-jobs-run.md parent link:\n%s", run)
	}
	root := first["kaab.md"]
	if !strings.HasPrefix(root, "---\ntitle: \"kaab\"\ndescription: \"The Kaabist command-line interface\"\n---\n") {
		t.Errorf("kaab.md header:\n%s", root[:min(len(root), 300)])
	}
	if !strings.Contains(root, "* [kaab jobs](/reference/cli/kaab-jobs) - ") || !strings.Contains(root, "* [kaab auth](/reference/cli/kaab-auth) - ") {
		t.Errorf("kaab.md child links:\n%s", root)
	}

	if _, _, code := h.run("", "docs", "generate", "--format", "md", "--dir", dir); code != 0 {
		t.Fatalf("second run exited %d", code)
	}
	second := readDocsDir(t, dir)
	if len(second) != len(first) {
		t.Fatalf("second run wrote %d files, first %d", len(second), len(first))
	}
	for name, body := range first {
		if second[name] != body {
			t.Errorf("%s differs between runs", name)
		}
	}
}

func mustShort(t *testing.T, path ...string) string {
	t.Helper()
	cmd, _, err := newRoot(&env{getenv: func(string) string { return "" }}).Find(path)
	if err != nil {
		t.Fatal(err)
	}
	return cmd.Short
}

func TestDocsGenerateRejectsFormat(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	_, errOut, code := h.run("", "docs", "generate", "--format", "html", "--dir", dir)
	if code != 1 || !strings.Contains(errOut, "only md") {
		t.Fatalf("code %d, err %q", code, errOut)
	}
	if _, _, code := h.run("", "docs", "generate"); code != 1 {
		t.Fatalf("missing --dir exited %d", code)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("wrote %d files for an unsupported format", len(entries))
	}
}

func TestDocsHidden(t *testing.T) {
	h := newHarness(t)
	out, _, _ := h.run("", "--help")
	if strings.Contains(out, "docs") {
		t.Fatalf("--help lists the hidden docs command:\n%s", out)
	}
}
