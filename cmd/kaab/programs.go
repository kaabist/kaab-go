package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/kaabist/kaab-go/client"
	"github.com/kaabist/kaab-go/kaab"
	"github.com/spf13/cobra"
)

func (a *app) programsCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "programs", Short: "Push, pull, lint and compare programs"}
	cmd.AddCommand(a.programsPush(), a.programsPull(), a.programsLint(), a.programsDiff(), a.programsList(), a.programsAlias())
	return cmd
}

func readDefinition(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var definition map[string]any
	if err := json.Unmarshal(data, &definition); err != nil {
		return nil, fmt.Errorf("%s is not a JSON object: %w", path, err)
	}
	return definition, nil
}

func (a *app) programsPush() *cobra.Command {
	var publish bool
	var name, notes, description string
	cmd := &cobra.Command{
		Use:   "push FILE.json",
		Short: "Create the program a definition file names, or add a version to it",
		Long:  "Pushes a program definition: a new name creates the program, a known one gets a new\nversion. The version is a draft unless --publish, which also moves alias latest.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			definition, err := readDefinition(args[0])
			if err != nil {
				return err
			}
			c, _, err := a.client()
			if err != nil {
				return err
			}
			pushed, err := c.Programs.Push(cmd.Context(), definition, kaab.PushOptions{
				Name: name, Publish: &publish, Notes: notes, Description: description,
			})
			if err != nil {
				return err
			}
			if a.g.json {
				return printJSON(a.env.stdout, pushed)
			}
			version := pushed.Program.LatestVersion
			if pushed.Version != nil {
				version = pushed.Version.Version
			}
			verb := "added version"
			if pushed.Created {
				verb = "created with version"
			}
			state := "draft"
			if publish {
				state = "published"
			}
			_, _ = fmt.Fprintf(a.env.stdout, "%s (%s): %s %d, %s\n", pushed.Program.Name, pushed.Program.ID, verb, version, state)
			return nil
		},
	}
	cmd.Flags().BoolVar(&publish, "publish", false, "publish the version (moves alias latest)")
	cmd.Flags().StringVar(&name, "name", "", "the program's name (the definition's by default)")
	cmd.Flags().StringVar(&notes, "notes", "", "notes on the new version")
	cmd.Flags().StringVar(&description, "description", "", "a new program's description")
	return cmd
}

// resolveVersion turns "name", "name@3" or "name@production" into the program and a version
// number (latest when none is given).
func resolveVersion(cmd *cobra.Command, c *kaab.Client, ref string) (*client.Program, int, error) {
	name, selector, _ := strings.Cut(ref, "@")
	program, err := c.Programs.Get(cmd.Context(), name)
	if err != nil {
		return nil, 0, err
	}
	if selector == "" {
		selector = "latest"
	}
	if n, err := strconv.Atoi(selector); err == nil {
		return program, n, nil
	}
	n, ok := program.Aliases[selector]
	if !ok {
		return nil, 0, fmt.Errorf("%s has no alias %q", program.Name, selector)
	}
	return program, n, nil
}

func (a *app) programsPull() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "pull NAME[@VERSION|@ALIAS]",
		Short: "Print (or save) a program version's definition",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.client()
			if err != nil {
				return err
			}
			program, n, err := resolveVersion(cmd, c, args[0])
			if err != nil {
				return err
			}
			version, err := c.Programs.Versions.Get(cmd.Context(), program.ID, n)
			if err != nil {
				return err
			}
			var value any = version.Definition
			if a.g.json {
				value = version
			}
			if output == "" || output == "-" {
				return printJSON(a.env.stdout, value)
			}
			data, err := json.MarshalIndent(value, "", "  ")
			if err != nil {
				return err
			}
			if err := os.WriteFile(output, append(data, '\n'), 0o644); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(a.env.stderr, "wrote %s@%d to %s\n", program.Name, n, output)
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "write to this file instead of stdout")
	return cmd
}

func (a *app) programsLint() *cobra.Command {
	return &cobra.Command{
		Use:   "lint FILE.json",
		Short: "Lint a definition; exit 1 when it has errors",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			definition, err := readDefinition(args[0])
			if err != nil {
				return err
			}
			body, err := kaab.Body[client.LintProgramJSONBody](map[string]any{"definition": definition})
			if err != nil {
				return err
			}
			c, _, err := a.client()
			if err != nil {
				return err
			}
			result, err := c.Programs.Lint(cmd.Context(), body)
			if err != nil {
				return err
			}
			if a.g.json {
				if err := printJSON(a.env.stdout, result); err != nil {
					return err
				}
			} else {
				rows := [][]string{}
				for _, issue := range result.Errors {
					rows = append(rows, []string{"error", issue.Path, string(issue.Code), issue.Message})
				}
				for _, issue := range result.Warnings {
					rows = append(rows, []string{"warning", issue.Path, string(issue.Code), issue.Message})
				}
				if len(rows) > 0 {
					if err := table(a.env.stdout, []string{"LEVEL", "PATH", "CODE", "MESSAGE"}, rows); err != nil {
						return err
					}
				}
				_, _ = fmt.Fprintf(a.env.stdout, "%s: %d error(s), %d warning(s)\n", args[0], len(result.Errors), len(result.Warnings))
			}
			if len(result.Errors) > 0 {
				return &exitCodeError{code: exitError, err: fmt.Errorf("%s has %d error(s)", args[0], len(result.Errors))}
			}
			return nil
		},
	}
}

func (a *app) programsDiff() *cobra.Command {
	return &cobra.Command{
		Use:   "diff NAME V1 V2",
		Short: "Print the JSON Patch from version V1 to version V2",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			from, err1 := strconv.Atoi(args[1])
			to, err2 := strconv.Atoi(args[2])
			if err := errors.Join(err1, err2); err != nil {
				return fmt.Errorf("versions are numbers: %w", err)
			}
			c, _, err := a.client()
			if err != nil {
				return err
			}
			program, err := c.Programs.Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			diff, err := c.Programs.Versions.Diff(cmd.Context(), program.ID, to, &client.DiffProgramVersionParams{Against: from})
			if err != nil {
				return err
			}
			if a.g.json {
				return printJSON(a.env.stdout, diff)
			}
			if len(diff.Patch) == 0 {
				_, _ = fmt.Fprintf(a.env.stdout, "%s@%d and @%d are the same\n", program.Name, from, to)
				return nil
			}
			for _, op := range diff.Patch {
				line, err := json.Marshal(op)
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintln(a.env.stdout, string(line))
			}
			return nil
		},
	}
}

func (a *app) programsList() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List programs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := a.client()
			if err != nil {
				return err
			}
			page, err := c.Programs.List(cmd.Context(), &client.ListProgramsParams{Limit: &limit})
			if err != nil {
				return err
			}
			if a.g.json {
				return printJSON(a.env.stdout, page)
			}
			rows := [][]string{}
			for _, p := range page.Data {
				aliases := []string{}
				for alias, n := range p.Aliases {
					aliases = append(aliases, fmt.Sprintf("%s=%d", alias, n))
				}
				sortStrings(aliases)
				rows = append(rows, []string{p.ID, p.Name, strconv.Itoa(p.LatestVersion), strings.Join(aliases, " "), when(&p.UpdatedAt)})
			}
			return table(a.env.stdout, []string{"ID", "NAME", "LATEST", "ALIASES", "UPDATED"}, rows)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 25, "how many (1–100)")
	return cmd
}

func (a *app) programsAlias() *cobra.Command {
	cmd := &cobra.Command{Use: "alias", Short: "Point a program's aliases at versions"}
	cmd.AddCommand(&cobra.Command{
		Use:   "set NAME ALIAS VERSION",
		Short: "Point ALIAS at VERSION (alias latest is managed by publishing)",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			version, err := strconv.Atoi(args[2])
			if err != nil {
				return fmt.Errorf("the version is a number: %w", err)
			}
			c, _, err := a.client()
			if err != nil {
				return err
			}
			program, err := c.Programs.Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			body, err := kaab.Body[client.SetProgramAliasJSONBody](map[string]any{"version": version})
			if err != nil {
				return err
			}
			updated, err := c.Programs.Aliases.Set(cmd.Context(), program.ID, args[1], body)
			if err != nil {
				return err
			}
			if a.g.json {
				return printJSON(a.env.stdout, updated)
			}
			_, _ = fmt.Fprintf(a.env.stdout, "%s@%s → %d\n", updated.Name, args[1], version)
			return nil
		},
	})
	return cmd
}
