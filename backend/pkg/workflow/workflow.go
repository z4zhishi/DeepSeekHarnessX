// Package workflow provides a minimal runtime for model-authored workflows over
// subagents (upstream CK/packages/workflow/* — workflow core + tool-workflow
// + tool-ralph). A Runner drives an ordered list of subagents — sequentially or
// concurrently — on goroutines and aggregates their outputs, emitting the four
// durable `tool-workflow/*` events (run-start / agent-start / agent-end /
// run-end) through a caller-supplied emitter so they land in the parent session
// log exactly as upstream persists them.
//
// The event payload structs (RunStartData / AgentStartData / AgentEndData /
// RunEndData) are byte-for-byte the upstream `tool-workflow/types.ts` data
// shapes, so persistence/replay paths that already understand those event
// types (session/events.go:53-56) keep working untouched.
package workflow

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"dsh-go/pkg/session"
)

// RunID brands one workflow run (upstream WorkflowRunId).
type RunID string

// Phase is one declared workflow phase (progress vocabulary only; imposes no
// execution structure) — upstream WorkflowPhase.
type Phase struct {
	Title    string `json:"title"`
	Detail   string `json:"detail,omitempty"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

// Meta is a workflow's identity block (upstream WorkflowMeta).
type Meta struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	WhenToUse   string  `json:"whenToUse,omitempty"`
	Phases      []Phase `json:"phases,omitempty"`
}

// ---------------------------------------------------------------------------
// tool-workflow event payloads (mirror upstream tool-workflow/types.ts).
// ---------------------------------------------------------------------------

// RunStartData opens one durable workflow run record (upstream
// ToolWorkflowRunStartData).
type RunStartData struct {
	RunID RunID  `json:"runId"`
	Name  string `json:"name"`
}

// AgentStartData records one workflow member after its child session is
// published (upstream ToolWorkflowAgentStartData).
type AgentStartData struct {
	RunID   RunID  `json:"runId"`
	Seq     int    `json:"seq"`
	Label   string `json:"label"`
	Phase   string `json:"phase,omitempty"`
	ChildID string `json:"childId"`
}

// AgentEndData settles one previously started workflow member (upstream
// ToolWorkflowAgentEndData). Outcome is one of completed | failed | cancelled.
type AgentEndData struct {
	RunID   RunID  `json:"runId"`
	Seq     int    `json:"seq"`
	Outcome string `json:"outcome"`
}

// RunEndData closes one workflow record after quiescence (upstream
// ToolWorkflowRunEndData). StopReason is one of completed | cancelled | error.
type RunEndData struct {
	RunID      RunID  `json:"runId"`
	StopReason string `json:"stopReason"`
}

// Outcome / StopReason vocabulary (upstream WorkflowAgentOutcome /
// WorkflowStopReason; both are CLOSED unions).
const (
	OutcomeCompleted = "completed"
	OutcomeFailed    = "failed"
	OutcomeCancelled = "cancelled"

	StopCompleted = "completed"
	StopCancelled = "cancelled"
	StopError     = "error"
)

// ---------------------------------------------------------------------------
// Workflow definition and execution.
// ---------------------------------------------------------------------------

// Agent is one subagent step in a model-authored workflow.
type Agent struct {
	// Label is the display label (shown in agent-start).
	Label string
	// Role is the subagent role title (e.g. "Code Reviewer").
	Role string
	// Prompt is the actionable instruction delivered to the subagent.
	Prompt string
	// Phase optionally groups this agent under a declared meta.Phase title.
	Phase string
}

// Workflow is a model-authored workflow definition.
type Workflow struct {
	// ID brands the run (the caller may leave it empty; Runner synthesizes one).
	ID RunID
	// Meta is the workflow identity block (validated before the body runs).
	Meta Meta
	// Agents are the subagents to orchestrate, in definition order.
	Agents []Agent
	// Parallel, when true, runs all agents concurrently; otherwise they run in
	// definition order (sequential). Parallel agents still emit agent-start in
	// definition order; agent-end follows completion order.
	Parallel bool
	// MaxParallel caps how many agents run at once when Parallel is true.
	// 0 (default) means unbounded concurrency.
	MaxParallel int
}

// AgentOutput aggregates one agent's settlement for the Runner's Result.
type AgentOutput struct {
	Seq     int    `json:"seq"`
	Label   string `json:"label"`
	Outcome string `json:"outcome"`
	Output  string `json:"output"`
}

// Result is a settled workflow run, aggregated across all agents. It follows
// the upstream contract that a run's result "never rejects": a terminal
// failure is carried in StopReason/Error rather than as a Go error.
type Result struct {
	RunID         RunID         `json:"runId"`
	Value         string        `json:"value"`
	StopReason    string        `json:"stopReason"`
	Error         string        `json:"error,omitempty"`
	AgentsStarted int           `json:"agentsStarted"`
	Outputs       []AgentOutput `json:"outputs"`
}

// AgentSpawn executes one subagent and returns its settlement. It is the seam
// through which the Runner drives a child agent.
type AgentSpawn func(ctx context.Context, role, prompt string) (AgentRun, error)

// AgentRun is the settlement of one spawned subagent.
type AgentRun struct {
	// Output is the subagent's final text output.
	Output string
	// ChildID is the child session id (may be empty; the Runner emits a
	// deterministic fallback in agent-start).
	ChildID string
}

// Emitter appends one durable session event (eventType, payload). It has the
// same signature as agent.EmitEvent and tools.ToolExecutionContext.Emit, so
// the gateway / loop can wire it directly. nil emitters are no-ops.
type Emitter func(eventType string, payload any)

// Runner drives model-authored workflows over subagents on goroutines and
// appends the four tool-workflow events through an Emitter.
type Runner struct {
	emit  Emitter
	spawn AgentSpawn
	seq   atomic.Int64
}

// RunnerOptions configures a Runner.
type RunnerOptions struct {
	// Emit appends one durable event; required to persist tool-workflow/*.
	Emit Emitter
	// Spawn executes one subagent. When nil, the Runner still emits lifecycle
	// events but settles every agent as failed with "no spawner configured".
	Spawn AgentSpawn
}

// NewRunner creates a Runner.
func NewRunner(opts RunnerOptions) *Runner {
	return &Runner{emit: opts.Emit, spawn: opts.Spawn}
}

// Run executes the workflow and returns its aggregate result. A terminal
// failure is carried in Result.StopReason/Result.Error; a canceled ctx settles
// the run as cancelled. It always emits run-end (upstream "result never
// rejects" / append-on-quiescence contract).
func (r *Runner) Run(ctx context.Context, wf Workflow) (Result, error) {
	if wf.ID == "" {
		wf.ID = RunID(fmt.Sprintf("run-%d", r.seq.Add(1)))
	}
	if len(wf.Agents) == 0 {
		r.dispatch(session.EventToolWorkflowRunStart, RunStartData{RunID: wf.ID, Name: wf.Meta.Name})
		r.dispatch(session.EventToolWorkflowRunEnd, RunEndData{RunID: wf.ID, StopReason: StopCompleted})
		return Result{RunID: wf.ID, StopReason: StopCompleted}, nil
	}

	// run-start
	r.dispatch(session.EventToolWorkflowRunStart, RunStartData{RunID: wf.ID, Name: wf.Meta.Name})

	// Publish every member (agent-start) in definition order before any work
	// begins. This keeps member publication deterministic regardless of
	// concurrency, matching the seam contract that agent-start records a
	// published child before its settlement.
	starts := make([]AgentStartData, len(wf.Agents))
	for i := range wf.Agents {
		ag := &wf.Agents[i]
		start := AgentStartData{
			RunID: wf.ID, Seq: i + 1, Label: ag.Label, Phase: ag.Phase,
			ChildID: childID(wf.ID, i+1),
		}
		starts[i] = start
		r.dispatch(session.EventToolWorkflowAgentStart, start)
	}

	outputs := make([]AgentOutput, len(wf.Agents))
	stopReason := StopCompleted
	var runErr string

	if wf.Parallel {
		max := wf.MaxParallel
		if max <= 0 {
			max = len(wf.Agents)
		}
		sem := make(chan struct{}, max)
		type done struct {
			idx    int
			output AgentOutput
		}
		results := make(chan done, len(wf.Agents))
		var wg sync.WaitGroup
		for i := range wf.Agents {
			i := i
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				results <- done{idx: i, output: r.runOne(ctx, wf.ID, &wf.Agents[i], starts[i])}
			}()
		}
		wg.Wait()
		close(results)
		for d := range results {
			outputs[d.idx] = d.output
			stopReason, runErr = settle(stopReason, runErr, d.output)
		}
	} else {
		for i := range wf.Agents {
			out := r.runOne(ctx, wf.ID, &wf.Agents[i], starts[i])
			outputs[i] = out
			stopReason, runErr = settle(stopReason, runErr, out)
		}
	}

	// run-end always emits (upstream append on quiescence).
	r.dispatch(session.EventToolWorkflowRunEnd, RunEndData{RunID: wf.ID, StopReason: stopReason})

	res := Result{
		RunID: wf.ID, StopReason: stopReason,
		AgentsStarted: len(wf.Agents), Outputs: outputs,
	}
	if stopReason != StopCompleted {
		res.Error = runErr
	}
	return res, nil
}

// runOne drives a single agent's settlement: runs the subagent, then emits
// agent-end with the outcome. agent-start was already published by Run (in
// definition order) before the work began. It never panics: a spawn failure or
// a canceled context settles as failed/cancelled rather than surfacing an
// error.
func (r *Runner) runOne(ctx context.Context, runID RunID, ag *Agent, start AgentStartData) AgentOutput {
	if err := ctx.Err(); err != nil {
		r.dispatch(session.EventToolWorkflowAgentEnd, AgentEndData{RunID: runID, Seq: start.Seq, Outcome: OutcomeCancelled})
		return AgentOutput{Seq: start.Seq, Label: ag.Label, Outcome: OutcomeCancelled}
	}
	if r.spawn == nil {
		r.dispatch(session.EventToolWorkflowAgentEnd, AgentEndData{RunID: runID, Seq: start.Seq, Outcome: OutcomeFailed})
		return AgentOutput{Seq: start.Seq, Label: ag.Label, Outcome: OutcomeFailed, Output: "no spawner configured"}
	}

	run, err := r.spawn(ctx, ag.Role, ag.Prompt)
	outcome := OutcomeCompleted
	out := run.Output
	switch {
	case err != nil:
		outcome = OutcomeFailed
		if out == "" {
			out = err.Error()
		}
	case ctx.Err() != nil:
		outcome = OutcomeCancelled
	}
	r.dispatch(session.EventToolWorkflowAgentEnd, AgentEndData{RunID: runID, Seq: start.Seq, Outcome: outcome})
	return AgentOutput{Seq: start.Seq, Label: ag.Label, Outcome: outcome, Output: out}
}

// dispatch sends one event through the emitter; nil emitters are no-ops.
func (r *Runner) dispatch(typ string, payload any) {
	if r == nil || r.emit == nil {
		return
	}
	r.emit(typ, payload)
}

// settle folds one agent's outcome into the run-level settlement: cancellation
// wins, otherwise the first failure sets the error message (subsequent
// failures are ignored so the run keeps the earliest authoritative error).
func settle(stop, runErr string, out AgentOutput) (string, string) {
	switch out.Outcome {
	case OutcomeCancelled:
		return StopCancelled, runErr
	case OutcomeFailed:
		if stop != StopCancelled {
			return StopError, out.Output
		}
		return stop, runErr
	}
	return stop, runErr
}

// childID synthesizes a deterministic child-session id for an agent. The
// spawner may override it via AgentRun.ChildID in a future wiring; the Runner
// emits this fallback best-effort, matching the seam contract that the id
// identify the child Session.
func childID(runID RunID, seq int) string {
	return fmt.Sprintf("%s/agent-%d", runID, seq)
}
