package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
)

// cliDocsBase is where the docs site serves the CLI reference.
const cliDocsBase = "/reference/cli/"

// docsCommand is the hidden `kaab docs generate --format md`: one Markdown page per visible
// command for the docs site. It needs no key, network or config.
func (a *app) docsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "docs",
		Short:  "Generate the CLI reference",
		Hidden: true,
		Args:   cobra.NoArgs,
	}
	var format, dir string
	generate := &cobra.Command{
		Use:   "generate",
		Short: "Write one page per command into --dir",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if format != "md" {
				return fmt.Errorf("--format %q is not supported (only md)", format)
			}
			if dir == "" {
				return errors.New("--dir is required")
			}
			files, err := generateMarkdownDocs(cmd.Root(), dir)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "wrote %d pages to %s\n", len(files), dir)
			return nil
		},
	}
	generate.Flags().StringVar(&format, "format", "md", "output format (md)")
	generate.Flags().StringVar(&dir, "dir", "", "directory to write the pages into")
	cmd.AddCommand(generate)
	return cmd
}

// docsFileName is the kebab-case page name of a command path: "kaab jobs run" -> kaab-jobs-run.
func docsFileName(commandPath string) string {
	return strings.Join(strings.Fields(commandPath), "-")
}

// docsLink rewrites cobra's link ("kaab_jobs_run.md") to the docs site route.
func docsLink(cobraFile string) string {
	return cliDocsBase + strings.ReplaceAll(strings.TrimSuffix(cobraFile, ".md"), "_", "-")
}

// generateMarkdownDocs writes one page per available command under root into dir, removes
// kaab*.md pages the tree no longer produces, and answers the written file names (sorted).
// Output is byte-identical across runs and operating systems.
func generateMarkdownDocs(root *cobra.Command, dir string) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var cmds []*cobra.Command
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		c.DisableAutoGenTag = true
		cmds = append(cmds, c)
		for _, child := range c.Commands() {
			if child.IsAvailableCommand() && !child.IsAdditionalHelpTopicCommand() {
				walk(child)
			}
		}
	}
	walk(root)

	written := map[string]bool{}
	var names []string
	for _, c := range cmds {
		var body bytes.Buffer
		if err := doc.GenMarkdownCustom(c, &body, docsLink); err != nil {
			return nil, err
		}
		page, err := markdownPage(c, body.String())
		if err != nil {
			return nil, err
		}
		name := docsFileName(c.CommandPath()) + ".md"
		if written[name] {
			return nil, fmt.Errorf("two commands map to %s", name)
		}
		written[name] = true
		names = append(names, name)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(page), 0o644); err != nil {
			return nil, err
		}
	}

	stale, err := filepath.Glob(filepath.Join(dir, "kaab*.md"))
	if err != nil {
		return nil, err
	}
	for _, path := range stale {
		if !written[filepath.Base(path)] {
			if err := os.Remove(path); err != nil {
				return nil, err
			}
		}
	}
	sort.Strings(names)
	return names, nil
}

// yamlString quotes s for YAML: a JSON string is a valid YAML double-quoted scalar.
func yamlString(s string) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// markdownPage turns cobra's Markdown into a docs page: frontmatter (title, description), cobra's
// "## <path>" heading and Short paragraph dropped (the page header renders them), "###" sections
// promoted to "##", "\n" line endings and no trailing whitespace.
func markdownPage(c *cobra.Command, body string) (string, error) {
	title, err := yamlString(c.CommandPath())
	if err != nil {
		return "", err
	}
	description, err := yamlString(c.Short)
	if err != nil {
		return "", err
	}
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.TrimPrefix(body, "## "+c.CommandPath()+"\n\n")
	body = strings.TrimPrefix(body, c.Short+"\n\n")

	var out strings.Builder
	out.WriteString("---\ntitle: " + title + "\ndescription: " + description + "\n---\n\n")
	fenced := false
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, " \t")
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
		}
		if !fenced && strings.HasPrefix(line, "### ") {
			line = line[1:]
		}
		if !fenced && strings.HasPrefix(line, "* [") {
			line = strings.Replace(line, ")\t - ", ") - ", 1)
		}
		out.WriteString(line + "\n")
	}
	return strings.TrimRight(out.String(), "\n") + "\n", nil
}
