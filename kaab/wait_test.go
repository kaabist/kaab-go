package kaab

import (
	"context"
	"testing"
	"time"

	"github.com/kaabist/kaab-go/client"
)

type fakeClock struct{ at time.Time }

func (c *fakeClock) now() time.Time { return c.at }
func (c *fakeClock) sleep(_ context.Context, d time.Duration) error {
	c.at = c.at.Add(d)
	return nil
}

type scripted struct {
	clock   *fakeClock
	states  []*client.Job
	cost    time.Duration
	i       int
	waits   []int
	since   []client.JobStatus
	inputs  []map[string]any
	resumes []ResumeAction
}

func (s *scripted) get(_ context.Context, _ string, wait int, since client.JobStatus) (*client.Job, error) {
	s.waits = append(s.waits, wait)
	s.since = append(s.since, since)
	s.clock.at = s.clock.at.Add(s.cost)
	job := s.states[min(s.i, len(s.states)-1)]
	s.i++
	return job, nil
}

// input and resume answer as the API does: the job, queued again.
func (s *scripted) input(_ context.Context, _ string, values map[string]any) (client.JobStatus, error) {
	s.inputs = append(s.inputs, values)
	return "queued", nil
}

func (s *scripted) resume(_ context.Context, _ string, action ResumeAction) (client.JobStatus, error) {
	s.resumes = append(s.resumes, action)
	return "queued", nil
}

func state(status string) *client.Job {
	return &client.Job{ID: "job_1", Status: client.JobStatus(status)}
}

func otp(expires time.Time) *client.Job {
	job := state("awaiting_input")
	body, _ := Body[client.Job](map[string]any{
		"id": "job_1", "status": "awaiting_input",
		"input_request": map[string]any{"name": "otp", "prompt": "SMS code", "type": "string", "expires_at": expires, "collect": "api"},
	})
	job.InputRequest = body.InputRequest
	return job
}

func obstacle() *client.Job {
	job := state("needs_attention")
	body, _ := Body[client.Job](map[string]any{
		"id": "job_1", "status": "needs_attention",
		"attention": map[string]any{"reason": "obstacle", "step": "steps[6]", "detail": "covered", "expires_at": "2026-10-05T11:00:00Z", "proposals": []any{}, "actions": []any{"skip_step"}},
	})
	job.Attention = body.Attention
	return job
}

func run(t *testing.T, s *scripted, opts WaitOptions) (*client.Job, error) {
	t.Helper()
	return waitForJob(context.Background(), s, "job_1", opts, s.clock)
}

func TestWaitReturnsAFinishedJobAtOnce(t *testing.T) {
	s := &scripted{clock: &fakeClock{}, states: []*client.Job{state("succeeded")}}
	job, err := run(t, s, WaitOptions{})
	if err != nil || job.Status != "succeeded" || len(s.waits) != 1 || s.waits[0] != 0 {
		t.Fatalf("job = %v, err = %v, waits = %v", job, err, s.waits)
	}
}

func TestWaitLongPolls(t *testing.T) {
	s := &scripted{clock: &fakeClock{}, cost: 2 * time.Second, states: []*client.Job{state("queued"), state("running"), state("failed")}}
	job, err := run(t, s, WaitOptions{Timeout: 2 * time.Minute})
	if err != nil || job.Status != "failed" {
		t.Fatalf("job = %v, err = %v", job, err)
	}
	if s.waits[1] != 60 {
		t.Fatalf("waits = %v", s.waits)
	}
}

func TestWaitAnswersInputOncePerRequest(t *testing.T) {
	expires := time.Date(2026, 10, 5, 10, 10, 0, 0, time.UTC)
	s := &scripted{clock: &fakeClock{}, states: []*client.Job{state("running"), otp(expires), state("running"), state("succeeded")}}
	asked := 0
	job, err := run(t, s, WaitOptions{OnInput: func(_ context.Context, job *client.Job) (any, error) {
		asked++
		if job.InputRequest.Name != "otp" {
			t.Fatalf("input request = %+v", job.InputRequest)
		}
		return "123456", nil
	}})
	if err != nil || job.Status != "succeeded" || asked != 1 || s.inputs[0]["otp"] != "123456" {
		t.Fatalf("job = %v, err = %v, asked = %d, inputs = %v", job, err, asked, s.inputs)
	}
}

func TestWaitReturnsParkedJobsWithoutHandlers(t *testing.T) {
	s := &scripted{clock: &fakeClock{}, states: []*client.Job{otp(time.Now())}}
	if job, _ := run(t, s, WaitOptions{}); job.Status != "awaiting_input" {
		t.Fatalf("status = %s", job.Status)
	}
	s = &scripted{clock: &fakeClock{}, states: []*client.Job{obstacle()}}
	if job, _ := run(t, s, WaitOptions{}); job.Status != "needs_attention" {
		t.Fatalf("status = %s", job.Status)
	}
}

func TestWaitAppliesOrLeavesAttention(t *testing.T) {
	s := &scripted{clock: &fakeClock{}, states: []*client.Job{obstacle(), state("queued"), state("succeeded")}}
	if _, err := run(t, s, WaitOptions{OnAttention: func(context.Context, *client.Job) (ResumeAction, error) { return SkipStep(), nil }}); err != nil {
		t.Fatal(err)
	}
	if len(s.resumes) != 1 || s.resumes[0]["skip_step"] != true {
		t.Fatalf("resumes = %v", s.resumes)
	}

	s = &scripted{clock: &fakeClock{}, states: []*client.Job{obstacle(), obstacle(), obstacle(), state("succeeded")}}
	asked := 0
	if _, err := run(t, s, WaitOptions{OnAttention: func(context.Context, *client.Job) (ResumeAction, error) { asked++; return nil, nil }}); err != nil {
		t.Fatal(err)
	}
	if asked != 1 || len(s.resumes) != 0 {
		t.Fatalf("asked = %d, resumes = %v", asked, s.resumes)
	}
}

func TestWaitTimesOutWithTheLastState(t *testing.T) {
	s := &scripted{clock: &fakeClock{}, cost: 30 * time.Second, states: []*client.Job{state("running")}}
	job, err := run(t, s, WaitOptions{Timeout: 90 * time.Second})
	if !IsCode(err, "wait_timeout") || job.Status != "running" {
		t.Fatalf("job = %v, err = %v", job, err)
	}
	var kerr *Error
	if !asError(err, &kerr) || kerr.Job == nil {
		t.Fatalf("err = %#v", err)
	}
}

func TestWaitDoesNotSpin(t *testing.T) {
	s := &scripted{clock: &fakeClock{}, states: []*client.Job{state("queued"), state("queued"), state("queued"), state("succeeded")}}
	if _, err := run(t, s, WaitOptions{PollInterval: 2 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if got := s.clock.at.Sub(time.Time{}); got != 6*time.Second {
		t.Fatalf("elapsed = %v", got)
	}
}

func TestWaitThroughTheService(t *testing.T) {
	f, c, _ := newFake(t)
	parked := jobJSON("awaiting_input")
	parked["input_request"] = map[string]any{"name": "otp", "prompt": "code", "type": "string", "pattern": nil, "keep_alive_until": nil, "expires_at": "2026-10-05T10:10:00Z", "collect": "api", "screenshot": nil, "resolve_url": nil}
	f.on("GET", "/v1/jobs/job_1", writeJSON(200, parked), writeJSON(200, jobJSON("succeeded")))
	f.on("POST", "/v1/jobs/job_1/input", writeJSON(200, jobJSON("running")))
	job, err := c.Jobs.Wait(context.Background(), "job_1", WaitOptions{
		PollInterval: time.Millisecond,
		OnInput:      func(context.Context, *client.Job) (any, error) { return "42", nil },
	})
	if err != nil || job.Status != "succeeded" {
		t.Fatalf("job = %v, err = %v", job, err)
	}
	if f.requests[1].Body != `{"values":{"otp":"42"}}` {
		t.Fatalf("input body = %s", f.requests[1].Body)
	}
	if f.requests[2].Header.Get("Prefer") == "" {
		t.Fatal("the second read did not long-poll")
	}
}

func TestWaitAfterInputSendsSinceStatus(t *testing.T) {
	first := time.Date(2026, 10, 5, 10, 10, 0, 0, time.UTC)
	// The job parks again at once on a new request: the long-poll after the answer must compare
	// against queued, not against the awaiting_input the server would see at its start.
	s := &scripted{clock: &fakeClock{}, states: []*client.Job{state("running"), otp(first), otp(first.Add(10 * time.Minute)), state("succeeded")}}
	_, err := run(t, s, WaitOptions{OnInput: func(context.Context, *client.Job) (any, error) { return "1", nil }})
	if err != nil {
		t.Fatal(err)
	}
	want := []client.JobStatus{"", "running", "queued", "queued"}
	if len(s.since) != len(want) {
		t.Fatalf("since = %v, want %v", s.since, want)
	}
	for i := range want {
		if s.since[i] != want[i] {
			t.Fatalf("since = %v, want %v", s.since, want)
		}
	}
}

func TestWaitSendsSinceStatusOverTheWire(t *testing.T) {
	f, c, _ := newFake(t)
	f.on("GET", "/v1/jobs/job_1", writeJSON(200, jobJSON("queued")), writeJSON(200, jobJSON("succeeded")))
	if _, err := c.Jobs.Wait(context.Background(), "job_1", WaitOptions{}); err != nil {
		t.Fatal(err)
	}
	if f.requests[0].Query != "" || f.requests[1].Query != "since_status=queued" {
		t.Fatalf("queries = %q, %q", f.requests[0].Query, f.requests[1].Query)
	}
	if f.requests[1].Header.Get("Prefer") == "" {
		t.Fatal("the second read is not a long-poll")
	}
}
