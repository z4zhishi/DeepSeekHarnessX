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

// JobPublic is the exported snapshot shape the gateway RPC (jobs.list) returns
// for the jobs panel. It is a stable JSON shape (upstream PublicJobSnapshot).
type JobPublic = jobPublic

// ListJobs returns the public snapshots of every background job owned by
// sessionID, ordered newest first. It is the gateway-facing entry for the jobs
// panel (the job_list tool remains the model-facing string form).
func ListJobs(sessionID string) []JobPublic {
	jobMu.Lock()
	defer jobMu.Unlock()
	var out []JobPublic
	for _, j := range jobByID {
		if j.SessionID != sessionID {
			continue
		}
		out = append(out, j.snapshot())
	}
	return out
}

// ReadJobOutput returns the accumulated output of a session-owned job. ok is
// false when the id is unknown or owned by another session.
func ReadJobOutput(sessionID, id string) (string, bool) {
	j, ok := lookupJob(sessionID, id)
	if !ok {
		return "", false
	}
	return string(j.readOutput()), true
}

// KillJob stops a session-owned job and reports whether it was found. It
// settles the record first-wins and returns no error (a terminal job is a
// no-op kill).
func KillJob(sessionID, id string) bool {
	j, ok := lookupJob(sessionID, id)
	if !ok {
		return false
	}
	_ = j.kill()
	return true
}

var (
	jobMu   sync.Mutex
	jobSeq  = 0
	jobByID = map[string]*Job{}
)

// startJob registers and launches one background command owned by sessionID.
// The command runs detached from the tool-call context: it keeps executing
// after the call returns (upstream run_in_background semantics). The command
// is Started synchronously so cmd.Process (and its platform process group /
// Job Object) is guaranteed present before the caller can act on the job —
// in particular before job_kill runs, which must be able to tear down the
// whole tree without racing the spawn.
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

	if err := cmd.Start(); err != nil {
		// Failed to launch; settle the record immediately so job_output can
		// read a terminal status instead of hanging.
		job.mu.Lock()
		job.FinishedAt = time.Now().UnixMilli()
		job.Status = JobFailed
		job.Detail = err.Error()
		job.mu.Unlock()
		close(job.done)
		return job, nil
	}
	attachProcessGroup(cmd)

	go func() {
		err := cmd.Wait()
		releaseProcessGroup(cmd)
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
// first-wins. The Job Object (Windows) / process group (Unix) teardown
// releases the shell's working-directory handle, and the stdout/stderr pipes
// are closed so the pipe readers inside cmd.Run() observe EOF and the runner
// can reap the command.
func (j *Job) kill() error {
	j.mu.Lock()
	if j.Status == JobCompleted || j.Status == JobFailed || j.Status == JobKilled {
		j.mu.Unlock()
		return nil
	}
	j.Status = JobKilled
	j.Detail = "killed before exit"
	cmd := j.cmd
	j.mu.Unlock()
	_ = killProcessTree(cmd)
	closeJobPipes(cmd)
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
