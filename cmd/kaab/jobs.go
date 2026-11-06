package main

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/kaabist/kaab-go/client"
	"github.com/kaabist/kaab-go/kaab"
	"github.com/spf13/cobra"
)

func (a *app) jobsCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "jobs", Short: "Run, inspect and answer jobs"}
	cmd.AddCommand(
		a.jobsRun(), a.jobsGet(), a.jobsList(), a.jobsSteps(), a.jobsCancel(),
		a.jobsInput(), a.jobsResume(), a.jobsTakeover(), a.jobsSnapshot(),
	)
	return cmd
}

func (a *app) jobsRun() *cobra.Command {
	var url, extract, program, session, maxTier, idempotencyKey, webhookURL string
	var inputs, tags []string
	var wait int
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "run (--url URL --extract @schema.json | --program NAME[@VERSION] [--input k=v]...)",
		Short: "Create a job and, with --wait, print its data",
		Long: "Creates a job (POST /jobs). With --wait N the request is held up to N seconds (≤ 60) for\n" +
			"the result: a succeeded job's data is printed as JSON on stdout and a summary on stderr.\n\n" +
			"Exit codes: 0 succeeded (or still running), 2 failed or canceled, 3 parked, waiting for an\n" +
			"input or for attention.",
		Example: "  kaab jobs run --url https://example.com --extract '{\"title\":\"h1\"}' --wait 30\n" +
			"  kaab jobs run --program acme-report@production --input report_id=42 --session acme --wait 60",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if (url == "") == (program == "") {
				return errors.New("give exactly one of --url (with --extract) or --program")
			}
			body := map[string]any{}
			if url != "" {
				if extract == "" {
					return errors.New("--url needs --extract (a schema: @file.json or inline JSON)")
				}
				var schema any
				if err := readJSONArg(extract, a.env.stdin, &schema); err != nil {
					return err
				}
				body["url"] = url
				body["extract"] = schema
			} else {
				body["program"] = program
			}
			if len(inputs) > 0 {
				values, err := parseInputs(inputs)
				if err != nil {
					return err
				}
				body["inputs"] = values
			}
			if session != "" {
				body["session"] = session
			}
			if maxTier != "" {
				body["options"] = map[string]any{"max_tier": maxTier}
			}
			if webhookURL != "" {
				body["webhook_url"] = webhookURL
			}
			if len(tags) > 0 {
				body["tags"] = tags
			}
			if dryRun {
				body["dry_run"] = true
			}
			req, err := kaab.Body[client.CreateJobJSONBody](body)
			if err != nil {
				return err
			}
			c, _, err := a.client()
			if err != nil {
				return err
			}
			opts := []kaab.RequestOption{}
			if wait > 0 {
				opts = append(opts, kaab.WithWait(wait))
			}
			if idempotencyKey != "" {
				opts = append(opts, kaab.WithIdempotencyKey(idempotencyKey))
			}
			job, err := c.Jobs.Create(cmd.Context(), req, opts...)
			if err != nil {
				return err
			}
			return a.printJob(job)
		},
	}
	f := cmd.Flags()
	f.StringVar(&url, "url", "", "the page of a shorthand job")
	f.StringVar(&extract, "extract", "", "the extract schema: @file.json, - (stdin) or inline JSON")
	f.StringVar(&program, "program", "", "a program reference: name, name@3, name@production, prg_…, pv_…")
	f.StringArrayVar(&inputs, "input", nil, "a program input, name=value (repeatable; JSON values are parsed)")
	f.StringVar(&session, "session", "", "a stored session to run in")
	f.StringVar(&maxTier, "max-tier", "", "the highest tier the job may use: http, browser or apex")
	f.StringVar(&webhookURL, "webhook-url", "", "where the job's events are delivered")
	f.StringArrayVar(&tags, "tag", nil, "a tag (repeatable)")
	f.BoolVar(&dryRun, "dry-run", false, "stop before the first side-effect step")
	f.IntVar(&wait, "wait", 0, "seconds to wait for the result (1–60)")
	f.StringVar(&idempotencyKey, "idempotency-key", "", "the Idempotency-Key (a ULID by default)")
	return cmd
}

func (a *app) jobsGet() *cobra.Command {
	var wait int
	cmd := &cobra.Command{
		Use:   "get JOB_ID",
		Short: "Print a job (its data, or with --json its envelope)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.client()
			if err != nil {
				return err
			}
			opts := []kaab.RequestOption{}
			if wait > 0 {
				opts = append(opts, kaab.WithWait(wait))
			}
			job, err := c.Jobs.Get(cmd.Context(), args[0], nil, opts...)
			if err != nil {
				return err
			}
			return a.printJob(job)
		},
	}
	cmd.Flags().IntVar(&wait, "wait", 0, "long-poll up to N seconds for a change of state (1–60)")
	return cmd
}

func (a *app) jobsList() *cobra.Command {
	var status, program, tag string
	var limit int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List jobs, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := a.client()
			if err != nil {
				return err
			}
			params := &client.ListJobsParams{Limit: &limit}
			if status != "" {
				s := client.ListJobsParamsStatus(status)
				params.Status = &s
			}
			if program != "" {
				params.Program = &program
			}
			if tag != "" {
				params.Tag = &tag
			}
			page, err := c.Jobs.List(cmd.Context(), params)
			if err != nil {
				return err
			}
			if a.g.json {
				return printJSON(a.env.stdout, page)
			}
			rows := make([][]string, 0, len(page.Data))
			for _, job := range page.Data {
				name := "-"
				if job.Program.Name != nil {
					name = fmt.Sprintf("%s@%d", *job.Program.Name, job.Program.Version)
				}
				rows = append(rows, []string{job.ID, string(job.Status), name, when(&job.CreatedAt)})
			}
			return table(a.env.stdout, []string{"ID", "STATUS", "PROGRAM", "CREATED"}, rows)
		},
	}
	cmd.Flags().StringVar(&status, "status", "", "only jobs in this status")
	cmd.Flags().StringVar(&program, "program", "", "only jobs of this program")
	cmd.Flags().StringVar(&tag, "tag", "", "only jobs with this tag")
	cmd.Flags().IntVar(&limit, "limit", 25, "how many (1–100)")
	return cmd
}

func (a *app) jobsSteps() *cobra.Command {
	return &cobra.Command{
		Use:   "steps JOB_ID",
		Short: "List a job's steps with their status and timing",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.client()
			if err != nil {
				return err
			}
			steps, err := c.Jobs.Steps(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if a.g.json {
				return printJSON(a.env.stdout, steps)
			}
			rows := make([][]string, 0, len(steps.Data))
			for _, step := range steps.Data {
				duration := "-"
				if step.DurationMs != nil {
					duration = strconv.Itoa(*step.DurationMs) + " ms"
				}
				rows = append(rows, []string{step.StepPath, step.Action, string(step.Status), duration})
			}
			return table(a.env.stdout, []string{"STEP", "ACTION", "STATUS", "DURATION"}, rows)
		},
	}
}

func (a *app) jobsCancel() *cobra.Command {
	return &cobra.Command{
		Use:   "cancel JOB_ID",
		Short: "Cancel a job (refunds what it reserved and did not use)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.client()
			if err != nil {
				return err
			}
			job, err := c.Jobs.Cancel(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if a.g.json {
				return printJSON(a.env.stdout, job)
			}
			_, _ = fmt.Fprintf(a.env.stdout, "%s %s\n", job.ID, job.Status)
			return nil
		},
	}
}

func (a *app) jobsInput() *cobra.Command {
	return &cobra.Command{
		Use:   "input JOB_ID NAME=VALUE...",
		Short: "Answer a job waiting for an input",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			values, err := parseInputs(args[1:])
			if err != nil {
				return err
			}
			body, err := kaab.Body[client.SubmitJobInputJSONBody](map[string]any{"values": values})
			if err != nil {
				return err
			}
			c, _, err := a.client()
			if err != nil {
				return err
			}
			job, err := c.Jobs.Input(cmd.Context(), args[0], body)
			if err != nil {
				return err
			}
			if a.g.json {
				return printJSON(a.env.stdout, job)
			}
			_, _ = fmt.Fprintf(a.env.stdout, "%s %s\n", job.ID, job.Status)
			return nil
		},
	}
}

func (a *app) jobsResume() *cobra.Command {
	var apply, remember string
	var promote, skip, retry, cont, abort bool
	cmd := &cobra.Command{
		Use:   "resume JOB_ID (--apply PRP_ID [--promote] [--remember site] | --skip-step | --retry-from-checkpoint | --continue | --abort)",
		Short: "Resume a job that needs attention",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var action kaab.ResumeAction
			count := 0
			if apply != "" {
				action = kaab.Apply(apply, promote)
				if remember != "" {
					action["remember"] = remember
				}
				count++
			}
			for _, pick := range []struct {
				on     bool
				action kaab.ResumeAction
			}{{skip, kaab.SkipStep()}, {retry, kaab.RetryFromCheckpoint()}, {cont, kaab.Continue()}, {abort, kaab.Abort()}} {
				if pick.on {
					action = pick.action
					count++
				}
			}
			if count != 1 {
				return errors.New("give exactly one of --apply, --skip-step, --retry-from-checkpoint, --continue, --abort")
			}
			body, err := kaab.Body[client.ResumeJobJSONBody](action)
			if err != nil {
				return err
			}
			c, _, err := a.client()
			if err != nil {
				return err
			}
			job, err := c.Jobs.Resume(cmd.Context(), args[0], body)
			if err != nil {
				return err
			}
			if a.g.json {
				return printJSON(a.env.stdout, job)
			}
			_, _ = fmt.Fprintf(a.env.stdout, "%s %s\n", job.ID, job.Status)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&apply, "apply", "", "apply this proposal (prp_…)")
	f.BoolVar(&promote, "promote", false, "with --apply: publish the change as a new version")
	f.StringVar(&remember, "remember", "", "with --apply: none or site")
	f.BoolVar(&skip, "skip-step", false, "skip the step that stopped")
	f.BoolVar(&retry, "retry-from-checkpoint", false, "run again from the last checkpoint")
	f.BoolVar(&cont, "continue", false, "continue after a takeover")
	f.BoolVar(&abort, "abort", false, "abort the job")
	return cmd
}

func (a *app) jobsTakeover() *cobra.Command {
	return &cobra.Command{
		Use:   "takeover JOB_ID",
		Short: "Open a live browser on a parked job and print its URL",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.client()
			if err != nil {
				return err
			}
			live, err := c.Jobs.Takeover(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if a.g.json {
				return printJSON(a.env.stdout, live)
			}
			_, _ = fmt.Fprintf(a.env.stdout, "%s\n", live.LiveURL)
			_, _ = fmt.Fprintf(a.env.stderr, "valid %d s; when done: kaab jobs resume %s --continue\n", live.ExpiresIn, args[0])
			return nil
		},
	}
}

func (a *app) jobsSnapshot() *cobra.Command {
	var step, output string
	cmd := &cobra.Command{
		Use:   "snapshot JOB_ID",
		Short: "Download a job's page snapshot (the final step's by default)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.client()
			if err != nil {
				return err
			}
			params := &client.GetJobSnapshotParams{}
			if step != "" {
				params.Step = &step
			}
			url, err := c.Jobs.Snapshot(cmd.Context(), args[0], params)
			if err != nil {
				return err
			}
			n, err := download(cmd.Context(), url, output, a.env.stdout)
			if err != nil {
				return fmt.Errorf("the snapshot: %w", err)
			}
			if output != "" && output != "-" {
				_, _ = fmt.Fprintf(a.env.stderr, "wrote %d bytes to %s\n", n, output)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&step, "step", "", "the step whose snapshot to download (steps[1])")
	cmd.Flags().StringVarP(&output, "output", "o", "", "write to this file instead of stdout")
	return cmd
}
