package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"dsh-go/pkg/workflow"
)

// workflowRunArgs is the model-facing input schema for the workflow_run tool.
// It mirrors the model-authored workflow shape: an identity block (name,
// required description, optional whenToUse / phases — upstream WorkflowMeta
// field names) plus an ordered list of subagent steps, each carrying the
// subagent role/prompt that the Runner hands to the invoke_subagent seam and
// optional per-member provider/model target overrides. parallel toggles
// concurrent execution.
type workflowRunArgs struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	WhenToUse   string `json:"whenToUse,omitempty"`
	Phases      []struct {
		Title    string `json:"title"`
		Detail   string `json:"detail,omitempty"`
		Provider string `json:"provider,omitempty"`
		Model    string `json:"model,omitempty"`
	} `json:"phases,omitempty"`
	Parallel bool `json:"parallel"`
	Agents   []struct {
		Label    string `json:"label"`
		Role     string `json:"role"`
		Prompt   string `json:"prompt"`
		Phase    string `json:"phase,omitempty"`
		Provider string `json:"provider,omitempty"`
		Model    string `json:"model,omitempty"`
	} `json:"agents"`
}

// RegisterWorkflowTools registers the model-invocable `workflow_run` tool on r.
// It drives a workflow.Runner which emits the four durable tool-workflow/*
// lifecycle events (run-start / agent-start / agent-end / run-end) through the
// caller session's Emitter and executes each subagent through the registry's own
// `invoke_subagent` tool. The model supplies the workflow definition; parent
// session + cwd are taken from the execution context so every child is
// attributed to the calling session.
func (r *ToolRegistry) RegisterWorkflowTools() {
	r.Register(ToolDefinition{
		Name:         "workflow_run",
		Description:  "Orchestrate a multi-step workflow over subagents: the model provides an identity block (a name and a one-line description, plus optional whenToUse guidance and phase declarations) and an ordered list of agents (each with a role and an actionable prompt), and the runtime spawns each as an isolated subagent — sequentially by default or concurrently with parallel:true — then returns the aggregated per-agent outcomes. Each step runs as its own child subagent, so scoped delegation keeps the parent turn lean. Per-agent \"model\" overrides the child's LLM target (default: the parent session's current model); per-agent \"provider\" overrides are not supported by this deployment and fail the run loudly.",
		RequiresPerm: false,
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": { "type": "string", "description": "Short workflow name for the run record" },
				"description": { "type": "string", "description": "One-line description of what this workflow does" },
				"whenToUse": { "type": "string", "description": "Optional guidance on when this workflow applies" },
				"phases": {
					"type": "array",
					"description": "Optional phase declarations that agents group under via their phase field",
					"items": {
						"type": "object",
						"properties": {
							"title": { "type": "string", "description": "The phase title agents match by exact string" },
							"detail": { "type": "string", "description": "Optional one-line description of the phase" },
							"provider": { "type": "string", "description": "Optional provider this phase is expected to use" },
							"model": { "type": "string", "description": "Optional model this phase is expected to use" }
						},
						"required": ["title"]
					}
				},
				"parallel": { "type": "boolean", "description": "Run all agents concurrently (true) or in definition order (false, default)" },
				"agents": {
					"type": "array",
					"description": "Ordered subagent steps to orchestrate",
					"items": {
						"type": "object",
						"properties": {
							"label": { "type": "string", "description": "Display label for this agent" },
							"role": { "type": "string", "description": "Subagent role title (e.g. Code Reviewer, Researcher)" },
							"prompt": { "type": "string", "description": "Actionable instruction delivered to the subagent" },
							"phase": { "type": "string", "description": "Optional grouping phase title" },
							"model": { "type": "string", "description": "Optional per-agent model id override; defaults to the parent session's current model" },
							"provider": { "type": "string", "description": "Unsupported by this deployment: any non-empty provider fails the run before it starts" }
						},
						"required": ["role", "prompt"]
					}
				}
			},
			"required": ["name", "description", "agents"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args workflowRunArgs
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, fmt.Errorf("workflow_run: invalid arguments: %w", err)
			}
			if args.Name == "" {
				return nil, fmt.Errorf("workflow_run: name must be a non-empty string")
			}
			if args.Description == "" {
				return nil, fmt.Errorf("workflow_run: description must be a non-empty string (one line saying what this workflow does)")
			}
			if len(args.Agents) == 0 {
				return nil, fmt.Errorf("workflow_run: agents must be a non-empty array")
			}

			wf := workflow.Workflow{
				Meta: workflow.Meta{
					Name:        args.Name,
					Description: args.Description,
					WhenToUse:   args.WhenToUse,
				},
				Parallel: args.Parallel,
			}
			for _, p := range args.Phases {
				wf.Meta.Phases = append(wf.Meta.Phases, workflow.Phase{
					Title: p.Title, Detail: p.Detail, Provider: p.Provider, Model: p.Model,
				})
			}
			for _, a := range args.Agents {
				wf.Agents = append(wf.Agents, workflow.Agent{
					Label:    a.Label,
					Role:     a.Role,
					Prompt:   a.Prompt,
					Phase:    a.Phase,
					Provider: a.Provider,
					Model:    a.Model,
				})
			}

			// Per-agent provider overrides are rejected loudly before the run
			// opens (upstream misused-hook semantics): this gateway routes a
			// single adapter (llm.Router wraps one inner), and invoke_subagent
			// only carries a model override, so honoring provider would need
			// multi-adapter wiring that does not exist yet. Never silently
			// ignored.
			var offenders []string
			for _, a := range args.Agents {
				if a.Provider != "" {
					id := a.Label
					if id == "" {
						id = a.Role
					}
					offenders = append(offenders, id)
				}
			}
			if len(offenders) > 0 {
				return nil, fmt.Errorf("workflow_run: per-agent \"provider\" override is not supported by this deployment (single-provider adapter routing); remove \"provider\" from agents [%s] — use \"model\" instead", strings.Join(offenders, ", "))
			}

			// Spawn drives real in-process subagents by executing the registry's
			// own `invoke_subagent` tool for the calling session (upstream subagent
			// seam). The tool path carries the subagent manager's semantics (its 60s
			// default timeout) and blocks until the child settles. Per-member model
			// overrides ride invoke_subagent's own `model` parameter (explicit
			// override > parent's current model > default); an empty value is
			// semantically identical to absent there, so uncovered members keep
			// global routing. Depth/lineage are computed from the calling session
			// by the manager and are unaffected.
			parentCtx := ToolExecutionContext{
				Context:   ctx.Context,
				SessionID: ctx.SessionID,
				Cwd:       ctx.Cwd,
			}
			spawn := func(sctx context.Context, req workflow.SpawnRequest) (workflow.AgentRun, error) {
				execCtx := ToolExecutionContext{
					Context:   sctx,
					SessionID: parentCtx.SessionID,
					Cwd:       parentCtx.Cwd,
				}
				payload, err := json.Marshal(map[string]string{
					"role":   req.Role,
					"prompt": req.Prompt,
					"model":  req.Model,
				})
				if err != nil {
					return workflow.AgentRun{}, err
				}
				res, isErr, err := r.ExecutePipeline(execCtx, "invoke_subagent", string(payload))
				if err != nil {
					return workflow.AgentRun{}, err
				}
				if isErr {
					return workflow.AgentRun{Output: res}, errors.New(res)
				}
				return workflow.AgentRun{Output: res}, nil
			}

			runner := workflow.NewRunner(workflow.RunnerOptions{
				Emit:  ctx.Emit,
				Spawn: spawn,
			})
			res, err := runner.Run(ctx.Context, wf)
			if err != nil {
				return nil, fmt.Errorf("workflow_run: %w", err)
			}
			return res, nil
		},
	})
}
