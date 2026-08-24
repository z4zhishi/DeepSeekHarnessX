package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"dsh-go/pkg/workflow"
)

// workflowRunArgs is the model-facing input schema for the workflow_run tool.
// It mirrors the model-authored workflow shape: a name + an ordered list of
// subagent steps, each carrying the subagent role/prompt that the Runner hands
// to the invoke_subagent seam. parallel toggles concurrent execution.
type workflowRunArgs struct {
	Name     string `json:"name"`
	Parallel bool   `json:"parallel"`
	Agents   []struct {
		Label  string `json:"label"`
		Role   string `json:"role"`
		Prompt string `json:"prompt"`
		Phase  string `json:"phase,omitempty"`
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
		Description:  "Orchestrate a multi-step workflow over subagents: the model provides a name and an ordered list of agents (each with a role and an actionable prompt), and the runtime spawns each as an isolated subagent — sequentially by default or concurrently with parallel:true — then returns the aggregated per-agent outcomes. Each step runs as its own child subagent, so scoped delegation keeps the parent turn lean.",
		RequiresPerm: false,
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": { "type": "string", "description": "Short workflow name for the run record" },
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
							"phase": { "type": "string", "description": "Optional grouping phase title" }
						},
						"required": ["role", "prompt"]
					}
				}
			},
			"required": ["name", "agents"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args workflowRunArgs
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, fmt.Errorf("workflow_run: invalid arguments: %w", err)
			}
			if args.Name == "" {
				return nil, fmt.Errorf("workflow_run: name must be a non-empty string")
			}
			if len(args.Agents) == 0 {
				return nil, fmt.Errorf("workflow_run: agents must be a non-empty array")
			}

			wf := workflow.Workflow{
				Meta:     workflow.Meta{Name: args.Name},
				Parallel: args.Parallel,
			}
			for _, a := range args.Agents {
				wf.Agents = append(wf.Agents, workflow.Agent{
					Label:  a.Label,
					Role:   a.Role,
					Prompt: a.Prompt,
					Phase:  a.Phase,
				})
			}

			// Spawn drives real in-process subagents by executing the registry's
			// own `invoke_subagent` tool for the calling session (upstream subagent
			// seam). The tool path carries the subagent manager's semantics (its 60s
			// default timeout) and blocks until the child settles.
			parentCtx := ToolExecutionContext{
				Context:   ctx.Context,
				SessionID: ctx.SessionID,
				Cwd:       ctx.Cwd,
			}
			spawn := func(sctx context.Context, role, prompt string) (workflow.AgentRun, error) {
				execCtx := ToolExecutionContext{
					Context:   sctx,
					SessionID: parentCtx.SessionID,
					Cwd:       parentCtx.Cwd,
				}
				args, err := json.Marshal(map[string]string{"role": role, "prompt": prompt})
				if err != nil {
					return workflow.AgentRun{}, err
				}
				res, isErr, err := r.ExecutePipeline(execCtx, "invoke_subagent", string(args))
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
