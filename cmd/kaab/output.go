package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kaabist/kaab-go/client"
)

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// table prints rows under headers, aligned.
func table(w io.Writer, headers []string, rows [][]string) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, strings.Join(headers, "\t"))
	for _, row := range rows {
		_, _ = fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	return tw.Flush()
}

func str(p *string) string {
	if p == nil {
		return "-"
	}
	return *p
}

func when(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04:05Z")
}

// readJSONArg reads "@file", "-" (stdin) or inline JSON into v.
func readJSONArg(arg string, stdin io.Reader, v any) error {
	var data []byte
	var err error
	switch {
	case arg == "-":
		data, err = io.ReadAll(stdin)
	case strings.HasPrefix(arg, "@"):
		data, err = os.ReadFile(arg[1:])
	default:
		data = []byte(arg)
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s is not JSON: %w", arg, err)
	}
	return nil
}

// parseInputs turns ["k=v", ...] into a map; a value that parses as JSON (a number, true, a
// quoted string, an object) is that value, anything else is the string as written.
func parseInputs(pairs []string) (map[string]any, error) {
	out := map[string]any{}
	for _, pair := range pairs {
		name, value, ok := strings.Cut(pair, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("%q is not name=value", pair)
		}
		var parsed any
		if json.Unmarshal([]byte(value), &parsed) == nil {
			out[name] = parsed
		} else {
			out[name] = value
		}
	}
	return out, nil
}

// jobExit is the exit code a job's state calls for, with the hint a parked job gets.
func jobExit(job *client.Job, stderr io.Writer) error {
	switch string(job.Status) {
	case "failed", "canceled":
		reason := string(job.Status)
		if job.Error != nil {
			reason = fmt.Sprintf("%s: %s", job.Error.Code, job.Error.Message)
		}
		return &exitCodeError{code: exitFailed, err: fmt.Errorf("job %s %s", job.ID, reason)}
	case "awaiting_input":
		name := "<name>"
		if job.InputRequest != nil {
			name = job.InputRequest.Name
		}
		_, _ = fmt.Fprintf(stderr, "job %s is waiting for %q: kaab jobs input %s %s=<value>\n", job.ID, name, job.ID, name)
		return &exitCodeError{code: exitParked}
	case "needs_attention":
		reason := ""
		if job.Attention != nil {
			reason = " (" + string(job.Attention.Reason) + ")"
		}
		_, _ = fmt.Fprintf(stderr, "job %s needs attention%s: kaab jobs resume %s --skip-step | --abort | …, or kaab open job %s\n", job.ID, reason, job.ID, job.ID)
		return &exitCodeError{code: exitParked}
	}
	return nil
}

// printJob prints a job: its envelope with --json, else a summary on stderr and its data on stdout.
func (a *app) printJob(job *client.Job) error {
	if a.g.json {
		if err := printJSON(a.env.stdout, job); err != nil {
			return err
		}
		return jobExit(job, a.env.stderr)
	}
	summary := fmt.Sprintf("%s %s", job.ID, job.Status)
	if job.Meta.DurationMs > 0 {
		summary += fmt.Sprintf(" in %.1f s", float64(job.Meta.DurationMs)/1000)
	}
	if job.Meta.Tier != nil {
		summary += fmt.Sprintf(" · %s", *job.Meta.Tier)
	}
	if job.Meta.Credits != nil && job.Meta.Credits.Settled != nil {
		settled := float64(*job.Meta.Credits.Settled)
		unit := "credits"
		if settled == 1 {
			unit = "credit"
		}
		summary += fmt.Sprintf(" · %s %s", strconv.FormatFloat(settled, 'f', -1, 32), unit)
	}
	_, _ = fmt.Fprintln(a.env.stderr, summary)
	if string(job.Status) == "succeeded" && job.Data != nil {
		if err := printJSON(a.env.stdout, job.Data); err != nil {
			return err
		}
	}
	return jobExit(job, a.env.stderr)
}
