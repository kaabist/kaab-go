package main

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kaabist/kaab-go/client"
	"github.com/kaabist/kaab-go/kaab"
	"github.com/spf13/cobra"
)

func sortStrings(s []string) { sort.Strings(s) }

func confidence(v *float32) string {
	if v == nil {
		return "-"
	}
	return strconv.FormatFloat(float64(*v), 'f', 2, 32)
}

func (a *app) sessionsCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "sessions", Short: "Store, check and delete browser sessions"}

	var file, name, concurrency string
	var origins []string
	create := &cobra.Command{
		Use:   "create --file STATE.json --name NAME",
		Short: "Store a Playwright storage state as a session",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file == "" || name == "" {
				return errors.New("--file and --name are required")
			}
			var state any
			if err := readJSONArg("@"+file, a.env.stdin, &state); err != nil {
				return err
			}
			body := map[string]any{"name": name, "storage_state": state}
			if len(origins) > 0 {
				body["origins"] = origins
			}
			if concurrency != "" {
				body["concurrency"] = concurrency
			}
			req, err := kaab.Body[client.CreateSessionJSONBody](body)
			if err != nil {
				return err
			}
			c, _, err := a.client()
			if err != nil {
				return err
			}
			session, err := c.Sessions.Create(cmd.Context(), req)
			if err != nil {
				return err
			}
			if a.g.json {
				return printJSON(a.env.stdout, session)
			}
			_, _ = fmt.Fprintf(a.env.stdout, "%s %s (%s)\n", session.ID, session.Name, session.Status)
			return nil
		},
	}
	create.Flags().StringVar(&file, "file", "", "a Playwright storageState JSON file")
	create.Flags().StringVar(&name, "name", "", "the session's name")
	create.Flags().StringArrayVar(&origins, "origin", nil, "an origin the session is for (repeatable)")
	create.Flags().StringVar(&concurrency, "concurrency", "", "exclusive or shared")

	list := &cobra.Command{
		Use:   "list",
		Short: "List sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := a.client()
			if err != nil {
				return err
			}
			page, err := c.Sessions.List(cmd.Context(), nil)
			if err != nil {
				return err
			}
			if a.g.json {
				return printJSON(a.env.stdout, page)
			}
			rows := [][]string{}
			for _, s := range page.Data {
				rows = append(rows, []string{s.ID, s.Name, string(s.Status), strings.Join(s.Origins, " "), when(s.LastVerifiedAt), when(s.LastUsedAt)})
			}
			return table(a.env.stdout, []string{"ID", "NAME", "STATUS", "ORIGINS", "VERIFIED", "USED"}, rows)
		},
	}

	verify := &cobra.Command{
		Use:   "verify SESSION",
		Short: "Run a session's validity probe (costs verify credits)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.client()
			if err != nil {
				return err
			}
			result, err := c.Sessions.Verify(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if a.g.json {
				return printJSON(a.env.stdout, result)
			}
			_, _ = fmt.Fprintf(a.env.stdout, "%s %s at %s\n", args[0], result.Status, when(&result.CheckedAt))
			return nil
		},
	}

	del := &cobra.Command{
		Use:   "delete SESSION",
		Short: "Delete a session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.client()
			if err != nil {
				return err
			}
			if err := c.Sessions.Delete(cmd.Context(), args[0]); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(a.env.stdout, "deleted %s\n", args[0])
			return nil
		},
	}
	cmd.AddCommand(create, list, verify, del)
	return cmd
}

func (a *app) proposalsCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "proposals", Short: "Review healing proposals"}

	var status, program, kind string
	list := &cobra.Command{
		Use:   "list",
		Short: "List proposals (pending by default)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := a.client()
			if err != nil {
				return err
			}
			params := &client.ListProposalsParams{}
			if status != "" {
				s := client.ListProposalsParamsStatus(status)
				params.Status = &s
			}
			if program != "" {
				params.Program = &program
			}
			if kind != "" {
				k := client.ListProposalsParamsKind(kind)
				params.Kind = &k
			}
			page, err := c.Proposals.List(cmd.Context(), params)
			if err != nil {
				return err
			}
			if a.g.json {
				return printJSON(a.env.stdout, page)
			}
			rows := [][]string{}
			for _, p := range page.Data {
				rows = append(rows, []string{p.ID, string(p.Kind), string(p.Status), p.Program.Name, p.Target, confidence(p.Confidence), string(p.Source)})
			}
			return table(a.env.stdout, []string{"ID", "KIND", "STATUS", "PROGRAM", "TARGET", "CONFIDENCE", "SOURCE"}, rows)
		},
	}
	list.Flags().StringVar(&status, "status", "pending", "pending, approved, rejected or expired")
	list.Flags().StringVar(&program, "program", "", "only this program's")
	list.Flags().StringVar(&kind, "kind", "", "relocate, dismiss, insert_steps, parser_config or api_discovery")

	show := &cobra.Command{
		Use:   "show PROPOSAL_ID",
		Short: "Print a proposal",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.client()
			if err != nil {
				return err
			}
			p, err := c.Proposals.Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return printJSON(a.env.stdout, p)
		},
	}

	var promote bool
	var remember, notes string
	approve := &cobra.Command{
		Use:   "approve PROPOSAL_ID",
		Short: "Approve a proposal; --promote publishes it as a new version",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{"promote": promote}
			if remember != "" {
				body["remember"] = remember
			}
			if notes != "" {
				body["notes"] = notes
			}
			req, err := kaab.Body[client.ApproveProposalJSONBody](body)
			if err != nil {
				return err
			}
			c, _, err := a.client()
			if err != nil {
				return err
			}
			p, err := c.Proposals.Approve(cmd.Context(), args[0], req)
			if err != nil {
				return err
			}
			if a.g.json {
				return printJSON(a.env.stdout, p)
			}
			_, _ = fmt.Fprintf(a.env.stdout, "%s %s\n", p.ID, p.Status)
			return nil
		},
	}
	approve.Flags().BoolVar(&promote, "promote", false, "publish the change as a new version (alias latest moves)")
	approve.Flags().StringVar(&remember, "remember", "", "none or site (a dismissal remembered for the hostname)")
	approve.Flags().StringVar(&notes, "notes", "", "notes on the new version")

	var reason string
	reject := &cobra.Command{
		Use:   "reject PROPOSAL_ID",
		Short: "Reject a proposal",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{}
			if reason != "" {
				body["reason"] = reason
			}
			req, err := kaab.Body[client.RejectProposalJSONBody](body)
			if err != nil {
				return err
			}
			c, _, err := a.client()
			if err != nil {
				return err
			}
			p, err := c.Proposals.Reject(cmd.Context(), args[0], req)
			if err != nil {
				return err
			}
			if a.g.json {
				return printJSON(a.env.stdout, p)
			}
			_, _ = fmt.Fprintf(a.env.stdout, "%s %s\n", p.ID, p.Status)
			return nil
		},
	}
	reject.Flags().StringVar(&reason, "reason", "", "why")

	cmd.AddCommand(list, show, approve, reject)
	return cmd
}

func (a *app) schedulesCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "schedules", Short: "Run programs on a cron"}

	list := &cobra.Command{
		Use:   "list",
		Short: "List schedules",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := a.client()
			if err != nil {
				return err
			}
			page, err := c.Schedules.List(cmd.Context(), nil)
			if err != nil {
				return err
			}
			if a.g.json {
				return printJSON(a.env.stdout, page)
			}
			rows := [][]string{}
			for _, s := range page.Data {
				rows = append(rows, []string{s.ID, s.Program, s.Cron, s.Timezone, string(s.Status), when(s.NextRunAt), when(s.LastRunAt)})
			}
			return table(a.env.stdout, []string{"ID", "PROGRAM", "CRON", "TIMEZONE", "STATUS", "NEXT", "LAST"}, rows)
		},
	}

	var program, cron, timezone, file, webhook string
	var inputs, fields []string
	var changes bool
	create := &cobra.Command{
		Use:   "create (--program REF --cron EXPR | --file BODY.json)",
		Short: "Create a schedule",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			body := map[string]any{}
			if file != "" {
				if err := readJSONArg("@"+file, a.env.stdin, &body); err != nil {
					return err
				}
			}
			if program != "" {
				body["program"] = program
			}
			if cron != "" {
				body["cron"] = cron
			}
			if timezone != "" {
				body["timezone"] = timezone
			}
			if webhook != "" {
				body["webhook_url"] = webhook
			}
			if len(inputs) > 0 {
				values, err := parseInputs(inputs)
				if err != nil {
					return err
				}
				body["inputs"] = values
			}
			if changes {
				detection := map[string]any{"enabled": true}
				if len(fields) > 0 {
					detection["fields"] = fields
				}
				body["change_detection"] = detection
			}
			if body["program"] == nil || body["cron"] == nil {
				return errors.New("a schedule needs --program and --cron (or a --file that has them)")
			}
			req, err := kaab.Body[client.CreateScheduleJSONBody](body)
			if err != nil {
				return err
			}
			c, _, err := a.client()
			if err != nil {
				return err
			}
			s, err := c.Schedules.Create(cmd.Context(), req)
			if err != nil {
				return err
			}
			if a.g.json {
				return printJSON(a.env.stdout, s)
			}
			_, _ = fmt.Fprintf(a.env.stdout, "%s %s %q next at %s\n", s.ID, s.Program, s.Cron, when(s.NextRunAt))
			return nil
		},
	}
	f := create.Flags()
	f.StringVar(&program, "program", "", "the program reference (name@alias follows the alias)")
	f.StringVar(&cron, "cron", "", "five-field cron")
	f.StringVar(&timezone, "timezone", "", "IANA time zone (UTC by default)")
	f.StringArrayVar(&inputs, "input", nil, "a program input, name=value (repeatable)")
	f.BoolVar(&changes, "change-detection", false, "emit schedule.changed when the data changes")
	f.StringArrayVar(&fields, "field", nil, "with --change-detection: a field path to compare (repeatable)")
	f.StringVar(&webhook, "webhook-url", "", "where the runs' events go")
	f.StringVar(&file, "file", "", "a JSON body (POST /schedules); flags override it")

	verb := func(use, short string, call func(*kaab.Client, *cobra.Command, string) (*client.Schedule, error)) *cobra.Command {
		return &cobra.Command{
			Use:   use + " SCHEDULE_ID",
			Short: short,
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				c, _, err := a.client()
				if err != nil {
					return err
				}
				s, err := call(c, cmd, args[0])
				if err != nil {
					return err
				}
				if a.g.json {
					return printJSON(a.env.stdout, s)
				}
				_, _ = fmt.Fprintf(a.env.stdout, "%s %s\n", s.ID, s.Status)
				return nil
			},
		}
	}
	pause := verb("pause", "Pause a schedule", func(c *kaab.Client, cmd *cobra.Command, id string) (*client.Schedule, error) {
		return c.Schedules.Pause(cmd.Context(), id)
	})
	resume := verb("resume", "Resume a paused schedule", func(c *kaab.Client, cmd *cobra.Command, id string) (*client.Schedule, error) {
		return c.Schedules.Resume(cmd.Context(), id)
	})

	runs := &cobra.Command{
		Use:   "runs SCHEDULE_ID",
		Short: "List a schedule's runs, newest first",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.client()
			if err != nil {
				return err
			}
			page, err := c.Schedules.Runs(cmd.Context(), args[0], nil)
			if err != nil {
				return err
			}
			if a.g.json {
				return printJSON(a.env.stdout, page)
			}
			rows := [][]string{}
			for _, r := range page.Data {
				job := str(r.JobID)
				state := "-"
				if r.Status != nil {
					state = string(*r.Status)
				}
				if r.Skipped {
					reason := "-"
					if r.SkipReason != nil {
						reason = string(*r.SkipReason)
					}
					state = "skipped (" + reason + ")"
				}
				changed := "-"
				if r.Changed != nil {
					changed = strconv.FormatBool(*r.Changed)
				}
				rows = append(rows, []string{when(r.ScheduledFor), job, state, changed})
			}
			return table(a.env.stdout, []string{"SCHEDULED", "JOB", "STATUS", "CHANGED"}, rows)
		},
	}

	cmd.AddCommand(list, create, pause, resume, runs)
	return cmd
}

func (a *app) webhooksCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "webhooks", Short: "Manage webhook endpoints"}

	list := &cobra.Command{
		Use:   "list",
		Short: "List endpoints",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := a.client()
			if err != nil {
				return err
			}
			page, err := c.Webhooks.List(cmd.Context(), nil)
			if err != nil {
				return err
			}
			if a.g.json {
				return printJSON(a.env.stdout, page)
			}
			rows := [][]string{}
			for _, w := range page.Data {
				rows = append(rows, []string{w.ID, w.URL, strings.Join(w.Events, ","), string(w.Mode), string(w.Status)})
			}
			return table(a.env.stdout, []string{"ID", "URL", "EVENTS", "MODE", "STATUS"}, rows)
		},
	}

	var url, description, mode string
	var events []string
	create := &cobra.Command{
		Use:   "create --url URL --events job.*,credits.low",
		Short: "Create an endpoint; its secret is printed once",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if url == "" || len(events) == 0 {
				return errors.New("--url and --events are required")
			}
			body := map[string]any{"url": url, "events": events}
			if description != "" {
				body["description"] = description
			}
			if mode != "" {
				body["mode"] = mode
			}
			req, err := kaab.Body[client.CreateWebhookJSONBody](body)
			if err != nil {
				return err
			}
			c, _, err := a.client()
			if err != nil {
				return err
			}
			w, err := c.Webhooks.Create(cmd.Context(), req)
			if err != nil {
				return err
			}
			if a.g.json {
				return printJSON(a.env.stdout, w)
			}
			_, _ = fmt.Fprintf(a.env.stdout, "%s %s\nsecret (shown once): %s\n", w.ID, w.URL, w.Secret)
			return nil
		},
	}
	create.Flags().StringVar(&url, "url", "", "the https endpoint")
	create.Flags().StringSliceVar(&events, "events", nil, "event types or patterns: job.*, credits.low, * (comma-separated)")
	create.Flags().StringVar(&description, "description", "", "a note")
	create.Flags().StringVar(&mode, "mode", "", "live, test or both (both by default)")

	test := &cobra.Command{
		Use:   "test WEBHOOK_ID",
		Short: "Send a ping to an endpoint and show its answer",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.client()
			if err != nil {
				return err
			}
			d, err := c.Webhooks.Test(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if a.g.json {
				return printJSON(a.env.stdout, d)
			}
			code := "-"
			if d.ResponseCode != nil {
				code = strconv.Itoa(*d.ResponseCode)
			}
			_, _ = fmt.Fprintf(a.env.stdout, "%s %s (HTTP %s)\n", d.ID, d.Status, code)
			if string(d.Status) != "succeeded" {
				return &exitCodeError{code: exitError, err: fmt.Errorf("the endpoint did not accept the ping: %s", str(d.Error))}
			}
			return nil
		},
	}

	var status string
	deliveries := &cobra.Command{
		Use:   "deliveries WEBHOOK_ID",
		Short: "List an endpoint's deliveries, newest first",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.client()
			if err != nil {
				return err
			}
			params := &client.ListWebhookDeliveriesParams{}
			if status != "" {
				s := client.ListWebhookDeliveriesParamsStatus(status)
				params.Status = &s
			}
			page, err := c.Webhooks.Deliveries(cmd.Context(), args[0], params)
			if err != nil {
				return err
			}
			if a.g.json {
				return printJSON(a.env.stdout, page)
			}
			rows := [][]string{}
			for _, d := range page.Data {
				code := "-"
				if d.ResponseCode != nil {
					code = strconv.Itoa(*d.ResponseCode)
				}
				rows = append(rows, []string{d.ID, string(d.EventType), string(d.Status), strconv.Itoa(d.Attempt), code, when(&d.CreatedAt)})
			}
			return table(a.env.stdout, []string{"ID", "EVENT", "STATUS", "ATTEMPT", "HTTP", "CREATED"}, rows)
		},
	}
	deliveries.Flags().StringVar(&status, "status", "", "pending, succeeded, failed or exhausted")

	cmd.AddCommand(list, create, test, deliveries)
	return cmd
}

func (a *app) creditsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "credits",
		Short: "Show the credit balance and its lots",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := a.client()
			if err != nil {
				return err
			}
			credits, err := c.Credits.Get(cmd.Context())
			if err != nil {
				return err
			}
			if a.g.json {
				return printJSON(a.env.stdout, credits)
			}
			num := func(v float32) string { return strconv.FormatFloat(float64(v), 'f', -1, 32) }
			_, _ = fmt.Fprintf(a.env.stdout, "available %s (balance %s, reserved %s)\n", num(credits.Available), num(credits.Balance), num(credits.Reserved))
			rows := [][]string{}
			for _, pool := range credits.Pools {
				rows = append(rows, []string{string(pool.Type), num(pool.Amount), when(pool.ExpiresAt)})
			}
			if len(rows) == 0 {
				return nil
			}
			return table(a.env.stdout, []string{"LOT", "AMOUNT", "EXPIRES"}, rows)
		},
	}
}

func (a *app) usageCommand() *cobra.Command {
	var from, to, groupBy string
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Show usage grouped by day, program, tier or tag",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			now := time.Now().UTC()
			if to == "" {
				to = now.Format("2006-01-02")
			}
			if from == "" {
				from = now.AddDate(0, 0, -30).Format("2006-01-02")
			}
			start, err := time.Parse("2006-01-02", from)
			if err != nil {
				return fmt.Errorf("--from: %w", err)
			}
			end, err := time.Parse("2006-01-02", to)
			if err != nil {
				return fmt.Errorf("--to: %w", err)
			}
			params := &client.GetUsageParams{From: start, To: end.Add(24*time.Hour - time.Second)}
			if groupBy != "" {
				g := client.GetUsageParamsGroupBy(groupBy)
				params.GroupBy = &g
			}
			c, _, err := a.client()
			if err != nil {
				return err
			}
			usage, err := c.Usage.Get(cmd.Context(), params)
			if err != nil {
				return err
			}
			if a.g.json {
				return printJSON(a.env.stdout, usage)
			}
			num := func(v float32) string { return strconv.FormatFloat(float64(v), 'f', -1, 32) }
			rows := [][]string{}
			for _, u := range usage.Data {
				rows = append(rows, []string{u.Key, strconv.Itoa(u.Jobs), num(u.Credits), num(u.Compute), num(u.ProxyMb), num(u.LlmTokens)})
			}
			return table(a.env.stdout, []string{"KEY", "JOBS", "CREDITS", "COMPUTE", "PROXY MB", "LLM TOKENS"}, rows)
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "first day, YYYY-MM-DD (30 days ago by default)")
	cmd.Flags().StringVar(&to, "to", "", "last day, YYYY-MM-DD (today by default)")
	cmd.Flags().StringVar(&groupBy, "group-by", "", "day, program, tier or tag (day by default)")
	return cmd
}

func (a *app) openCommand() *cobra.Command {
	var printOnly bool
	cmd := &cobra.Command{Use: "open", Short: "Open a resource in the panel"}
	job := &cobra.Command{
		Use:   "job JOB_ID",
		Short: "Open a job's page in the panel",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			var profile *Profile
			if s, err := a.store(); err == nil {
				if cfg, err := s.load(); err == nil {
					profile = cfg.Profiles[a.profileName(cfg)]
				}
			}
			url := a.panelURL(profile) + "/jobs/" + args[0]
			_, _ = fmt.Fprintln(a.env.stdout, url)
			if printOnly || a.env.open == nil {
				return nil
			}
			if err := a.env.open(url); err != nil {
				_, _ = fmt.Fprintln(a.env.stderr, "could not open a browser:", err)
			}
			return nil
		},
	}
	job.Flags().BoolVar(&printOnly, "print", false, "print the URL without opening a browser")
	cmd.AddCommand(job)
	return cmd
}
