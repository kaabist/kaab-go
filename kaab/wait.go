package kaab

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/kaabist/kaab-go/client"
)

// Jobs.Wait follows one job to the end, answering it on the way. It long-polls GET /jobs/{id}
// with Prefer: wait, so a job that is not moving costs one request a minute. When
// the job parks it asks the caller:
//
//   - awaiting_input → OnInput; the value is posted to POST /jobs/{id}/input and the wait goes on.
//   - needs_attention → OnAttention; the action is posted to POST /jobs/{id}/resume, or a nil
//     action leaves the job to somebody else (the panel, a hosted page) until it moves.
//
// A parked job with no handler for its state is returned as it is. succeeded, failed and
// canceled are returned; running out of Timeout is a *Error with code wait_timeout and the last
// state in Job.

const (
	// DefaultWaitTimeout bounds Jobs.Wait.
	DefaultWaitTimeout = 5 * time.Minute
	// DefaultPollInterval is the least time between two reads of the job.
	DefaultPollInterval = time.Second
)

// ResumeAction is a POST /jobs/{id}/resume body: {"skip_step": true}, {"apply": "prp_…"}, ….
type ResumeAction map[string]any

// Apply applies a proposal; promote also publishes it as a new version.
func Apply(proposalID string, promote bool) ResumeAction {
	return ResumeAction{"apply": proposalID, "promote": promote}
}

// SkipStep, RetryFromCheckpoint, Continue and Abort are the other resume verbs.
func SkipStep() ResumeAction            { return ResumeAction{"skip_step": true} }
func RetryFromCheckpoint() ResumeAction { return ResumeAction{"retry_from": "checkpoint"} }
func Continue() ResumeAction            { return ResumeAction{"continue": true} }
func Abort() ResumeAction               { return ResumeAction{"abort": true} }

// WaitOptions configures Jobs.Wait.
type WaitOptions struct {
	// Timeout bounds the whole wait (DefaultWaitTimeout when zero).
	Timeout time.Duration
	// PollInterval is the least time between two reads of the job.
	PollInterval time.Duration
	// OnInput answers job.InputRequest; its value is posted as the input.
	OnInput func(ctx context.Context, job *client.Job) (any, error)
	// OnAttention decides on job.Attention; nil leaves the job to somebody else.
	OnAttention func(ctx context.Context, job *client.Job) (ResumeAction, error)
}

// Body converts v — a map, a struct, raw JSON — to a generated request body type by way of its
// JSON, which is how a union body (SubmitJobInputJSONBody, ResumeJobJSONBody) is
// built without its From… helpers.
func Body[T any](v any) (T, error) {
	var out T
	var data []byte
	switch raw := v.(type) {
	case []byte:
		data = raw
	case json.RawMessage:
		data = raw
	default:
		var err error
		data, err = json.Marshal(v)
		if err != nil {
			return out, err
		}
	}
	err := json.Unmarshal(data, &out)
	return out, err
}

// jobCalls are the three calls the wait makes, so it can be tested against a fake. since is the
// status the long-poll compares against ("" for none); input and resume return the job's status.
type jobCalls interface {
	get(ctx context.Context, id string, wait int, since client.JobStatus) (*client.Job, error)
	input(ctx context.Context, id string, values map[string]any) (client.JobStatus, error)
	resume(ctx context.Context, id string, action ResumeAction) (client.JobStatus, error)
}

type clock interface {
	now() time.Time
	sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{}

func (realClock) now() time.Time                                   { return time.Now() }
func (realClock) sleep(ctx context.Context, d time.Duration) error { return sleepContext(ctx, d) }

type serviceCalls struct{ jobs *JobsService }

func (s serviceCalls) get(ctx context.Context, id string, wait int, since client.JobStatus) (*client.Job, error) {
	if wait == 0 {
		return s.jobs.Get(ctx, id, nil)
	}
	var params *client.GetJobParams
	if since != "" {
		status := client.GetJobParamsSinceStatus(since)
		params = &client.GetJobParams{SinceStatus: &status}
	}
	return s.jobs.Get(ctx, id, params, WithWait(wait))
}

func (s serviceCalls) input(ctx context.Context, id string, values map[string]any) (client.JobStatus, error) {
	body, err := Body[client.SubmitJobInputJSONBody](map[string]any{"values": values})
	if err != nil {
		return "", err
	}
	job, err := s.jobs.Input(ctx, id, body)
	if err != nil {
		return "", err
	}
	return job.Status, nil
}

func (s serviceCalls) resume(ctx context.Context, id string, action ResumeAction) (client.JobStatus, error) {
	body, err := Body[client.ResumeJobJSONBody](action)
	if err != nil {
		return "", err
	}
	job, err := s.jobs.Resume(ctx, id, body)
	if err != nil {
		return "", err
	}
	return job.Status, nil
}

// Wait follows job id to succeeded, failed or canceled, answering inputs and attention on the way.
func (r *JobsService) Wait(ctx context.Context, id string, opts WaitOptions) (*client.Job, error) {
	return waitForJob(ctx, serviceCalls{jobs: r}, id, opts, realClock{})
}

// Terminal reports whether status is one a job never leaves.
func Terminal(status client.JobStatus) bool {
	switch string(status) {
	case "succeeded", "failed", "canceled":
		return true
	}
	return false
}

func parkKey(job *client.Job) string {
	if string(job.Status) == "awaiting_input" && job.InputRequest != nil {
		return fmt.Sprintf("input:%s:%s", job.InputRequest.Name, job.InputRequest.ExpiresAt.Format(time.RFC3339Nano))
	}
	if string(job.Status) == "needs_attention" && job.Attention != nil {
		return fmt.Sprintf("attention:%s:%s", job.Attention.Step, job.Attention.ExpiresAt.Format(time.RFC3339Nano))
	}
	return ""
}

func waitForJob(ctx context.Context, calls jobCalls, id string, opts WaitOptions, clk clock) (*client.Job, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultWaitTimeout
	}
	poll := opts.PollInterval
	if poll <= 0 {
		poll = DefaultPollInterval
	}
	deadline := clk.now().Add(timeout)
	left := ""
	var job *client.Job
	// The status the next long-poll compares against: the last one this wait saw, or what input
	// or resume answered, so a job that already moved on is answered at once.
	var since client.JobStatus
	timedOut := func() error {
		return &Error{Code: "wait_timeout", Message: fmt.Sprintf("job %s was still %s after %s", id, job.Status, timeout), Job: job}
	}
	for {
		started := clk.now()
		remaining := deadline.Sub(started)
		if remaining <= 0 && job != nil {
			return job, timedOut()
		}
		wait := 0
		if job != nil {
			wait = int(math.Max(1, math.Min(MaxWaitSeconds, math.Ceil(remaining.Seconds()))))
		}
		if wait == 0 {
			since = ""
		}
		next, err := calls.get(ctx, id, wait, since)
		if err != nil {
			return job, err
		}
		job = next
		since = job.Status
		if Terminal(job.Status) {
			return job, nil
		}
		key := parkKey(job)
		if key != "" && key != left {
			if string(job.Status) == "awaiting_input" {
				if opts.OnInput == nil {
					return job, nil
				}
				value, err := opts.OnInput(ctx, job)
				if err != nil {
					return job, err
				}
				status, err := calls.input(ctx, id, map[string]any{job.InputRequest.Name: value})
				if err != nil {
					return job, err
				}
				since = status
				continue
			}
			if opts.OnAttention == nil {
				return job, nil
			}
			action, err := opts.OnAttention(ctx, job)
			if err != nil {
				return job, err
			}
			if action == nil {
				left = key
			} else {
				status, err := calls.resume(ctx, id, action)
				if err != nil {
					return job, err
				}
				since = status
				continue
			}
		}
		if key == "" {
			left = ""
		}
		if !clk.now().Before(deadline) {
			return job, timedOut()
		}
		if elapsed := clk.now().Sub(started); elapsed < poll {
			pause := min(poll-elapsed, max(0, deadline.Sub(clk.now())))
			if err := clk.sleep(ctx, pause); err != nil {
				return job, err
			}
		}
	}
}
