package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// JobStatus mirrors the upstream job vocabulary: running | stopping |
// completed | killed | failed.
type JobStatus string

const (
	JobRunning   JobStatus = "running"
	JobStopping  JobStatus = "stopping"
	JobCompleted JobStatus = "completed"
	JobKilled    JobStatus = "killed"
	JobFailed    JobStatus = "failed"
)

// Job is one session-scoped background task (upstream dsh-jobs registry
// record). Output accumulates in a bounded buffer; ownership is fenced by the
// session id that created the job.
type Job struct {
	ID         string
	SessionID  string
	Kind       string
	Label      string
	Status     JobStatus
	Detail     string
	StartedAt  int64
	FinishedAt int64
	cmd        *exec.Cmd
	buf        *lockedBuffer
	mu         sync.Mutex
	done       chan struct{}
}

// lockedBuffer is a mutex-guarded bytes.Buffer: the command runner
// goroutine writes while job_output reads, so the raw buffer must never be
// touched concurrently.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) Bytes() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return bytes.Clone(l.b.Bytes())
}

// jobPublic is the model-safe snapshot (upstream PublicJobSnapshot).
type jobPublic struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Label      string    `json:"label"`
	Status     JobStatus `json:"status"`
	Detail     string    `json:"detail,omitempty"`
	StartedAt  int64     `json:"startedAt"`
	FinishedAt int64     `json:"finishedAt,omitempty"`
}

var (
	jobMu   sync.Mutex
	jobSeq  = 0
	jobByID = map[string]*Job{}
)

// startJob registers and launches one background command owned by sessionID.
// The command runs detached from the tool-call context: it keeps executing
// after the call returns (upstream run_in_background semantics).
func startJob(sessionID, kind, label string, cmd *exec.Cmd) (*Job, error) {
	jobMu.Lock()
	jobSeq++
	job := &Job{
		ID:        fmt.Sprintf("job-%d", jobSeq),
		SessionID: sessionID,
		Kind:      kind,
		Label:     label,
		Status:    JobRunning,
		StartedAt: time.Now().UnixMilli(),
		buf:       &lockedBuffer{},
		done:      make(chan struct{}),
	}
	job.cmd = cmd
	cmd.Stdout = job.buf
	cmd.Stderr = job.buf
	jobByID[job.ID] = job
	jobMu.Unlock()

	go func() {
		err := cmd.Run()
		job.mu.Lock()
		job.FinishedAt = time.Now().UnixMilli()
		if err != nil {
			if job.Status != JobKilled {
				if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
					job.Status = JobCompleted
					job.Detail = fmt.Sprintf("exit code: %d", cmd.ProcessState.ExitCode())
				} else {
					job.Status = JobFailed
					job.Detail = err.Error()
				}
			}
		} else {
			job.Status = JobCompleted
			job.Detail = "exit code: 0"
		}
		job.mu.Unlock()
		close(job.done)
	}()
	return job, nil
}

// lookupJob returns the job only when owned by sessionID (authorization, not
// secrecy — ids are predictable, so the session fence is the boundary).
func lookupJob(sessionID, id string) (*Job, bool) {
	jobMu.Lock()
	defer jobMu.Unlock()
	j, ok := jobByID[id]
	if !ok || j.SessionID != sessionID {
		return nil, false
	}
	return j, true
}

// snapshot returns the model-safe public shape.
func (j *Job) snapshot() jobPublic {
	j.mu.Lock()
	defer j.mu.Unlock()
	return jobPublic{
		ID: j.ID, Kind: j.Kind, Label: j.Label, Status: j.Status,
		Detail: j.Detail, StartedAt: j.StartedAt, FinishedAt: j.FinishedAt,
	}
}

// readOutput returns a copy of the accumulated output.
func (j *Job) readOutput() []byte {
	return j.buf.Bytes()
}

// kill stops the process tree; the runner goroutine settles the record
// first-wins. The stdout/stderr pipes are closed so the pipe readers inside
// cmd.Run() unblock and the goroutine can reap the command (a bare PID kill
// leaves a shell's grandchildren running and holding the pipes open).
func (j *Job) kill() error {
	j.mu.Lock()
	if j.Status == JobCompleted || j.Status == JobFailed || j.Status == JobKilled {
		j.mu.Unlock()
		return nil
	}
	j.Status = JobKilled
	j.Detail = "killed before exit"
	proc := j.cmd.Process
	j.mu.Unlock()
	if proc != nil {
		_ = killProcessTree(proc.Pid)
	}
	closeJobPipes(j.cmd)
	return nil
}

// closeJobPipes closes the command's output pipes so the copy goroutines inside
// cmd.Run() observe EOF and let the runner reap the process. Safe to call even
// when the pipes are bytes.Buffer (no-op).
func closeJobPipes(cmd *exec.Cmd) {
	for _, p := range []io.Writer{cmd.Stdout, cmd.Stderr} {
		if c, ok := p.(io.Closer); ok {
			_ = c.Close()
		}
	}
}

// RegisterJobTools registers job_output, job_list, and job_kill (upstream
// dsh-tool-jobs contract: model-facing collection over the session-scoped
// registry).
func (r *ToolRegistry) RegisterJobTools() {
	r.Register(ToolDefinition{
		Name:        "job_output",
		Description: "Read the output of a background job started with run_in_background. Optionally wait up to timeout_ms (default 30000, capped at 600000) for the job to finish.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": { "type": "string", "description": "Job id returned by run_in_background" },
				"timeout_ms": { "type": "integer", "description": "Optional wait budget in ms (default 30000)" }
			},
			"required": ["id"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				ID        string `json:"id"`
				TimeoutMs int    `json:"timeout_ms"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			job, ok := lookupJob(ctx.SessionID, args.ID)
			if !ok {
				return nil, fmt.Errorf("unknown job %q for this session", args.ID)
			}
			wait := args.TimeoutMs
			if wait <= 0 {
				wait = 30000
			}
			if wait > 600000 {
				wait = 600000
			}
			timer := time.NewTimer(time.Duration(wait) * time.Millisecond)
			defer timer.Stop()
			select {
			case <-job.done:
			case <-timer.C:
			case <-ctx.Context.Done():
				return nil, ctx.Context.Err()
			}
			snap := job.snapshot()
			out := string(job.readOutput())
			if out == "" {
				out = "(no output)"
			}
			return fmt.Sprintf("[status: %s, %s]\n%s", snap.Status, snap.Detail, out), nil
		},
	})

	r.Register(ToolDefinition{
		Name:        "job_list",
		Description: "List background jobs started by this session with their current status.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {},
			"required": []
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			jobMu.Lock()
			defer jobMu.Unlock()
			var list []jobPublic
			for _, j := range jobByID {
				if j.SessionID != ctx.SessionID {
					continue
				}
				list = append(list, j.snapshot())
			}
			if len(list) == 0 {
				return "No background jobs.", nil
			}
			b, _ := json.MarshalIndent(list, "", "  ")
			return string(b), nil
		},
	})

	r.Register(ToolDefinition{
		Name:        "job_kill",
		Description: "Stop a background job started with run_in_background.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": { "type": "string", "description": "Job id to stop" }
			},
			"required": ["id"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			job, ok := lookupJob(ctx.SessionID, args.ID)
			if !ok {
				return nil, fmt.Errorf("unknown job %q for this session", args.ID)
			}
			if err := job.kill(); err != nil {
				return nil, err
			}
			return fmt.Sprintf("Stopped job %s.", args.ID), nil
		},
	})
}
