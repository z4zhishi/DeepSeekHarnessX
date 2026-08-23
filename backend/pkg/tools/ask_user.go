package tools

import (
	"encoding/json"
	"fmt"
	"strings"
)

// askUserQuestionArgs mirrors the upstream tool-ask-user schema: one or more
// questions, each with a stable id, optional header, optional choices, and
// optional multi-select.
type askUserQuestionArgs struct {
	Questions []askQuestion `json:"questions"`
}

type askQuestion struct {
	ID          string      `json:"id"`
	Question    string      `json:"question"`
	Header      string      `json:"header"`
	Options     []askOption `json:"options"`
	MultiSelect bool        `json:"multi_select"`
}

type askOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// askAnswer is one answered question (upstream answer shape).
type askAnswer struct {
	ID       string   `json:"id"`
	Selected []string `json:"selected"`
	Custom   string   `json:"custom,omitempty"`
}

// RegisterAskUserTool registers the ask_user_question tool (upstream
// @deepseek-ai/dsh-tool-ask-user). Execution pauses until a UI provider
// returns a human answer; the answer feeds back into the agent loop as an
// ordinary tool result. Without a provider hook the call fails closed.
func (r *ToolRegistry) RegisterAskUserTool() {
	r.Register(ToolDefinition{
		Name:        "ask_user_question",
		Description: "Ask the user a concise question when you need confirmation, a choice, or missing information before proceeding. Send one or more questions, each with a stable id that will be echoed in the answer.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"questions": {
					"type": "array",
					"items": {
						"type": "object",
						"properties": {
							"id": { "type": "string", "description": "Stable id for this question; echoed in the answer" },
							"question": { "type": "string", "description": "The specific question to ask the user" },
							"header": { "type": "string", "description": "Optional short heading for the question" },
							"options": {
								"type": "array",
								"items": {
									"type": "object",
									"properties": {
										"label": { "type": "string" },
										"description": { "type": "string" }
									},
									"required": ["label"]
								}
							},
							"multi_select": { "type": "boolean", "description": "Whether the user may select more than one option (default false)" }
						},
						"required": ["id", "question"]
					}
				}
			},
			"required": ["questions"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args askUserQuestionArgs
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			if len(args.Questions) == 0 {
				return nil, fmt.Errorf("questions must contain at least one question")
			}
			// The RequestUser hook is the shared human bridge (TUI modal,
			// Godot approval modal, ACP permission request). Ask for each
			// question in order; the hook returns the selected label.
			var answers []askAnswer
			for _, q := range args.Questions {
				if q.ID == "" || q.Question == "" {
					return nil, fmt.Errorf("each question requires id and question")
				}
				options := make([]string, 0, len(q.Options))
				for _, o := range q.Options {
					options = append(options, o.Label)
				}
				var prompt strings.Builder
				if q.Header != "" {
					prompt.WriteString("[" + q.Header + "] ")
				}
				prompt.WriteString(q.Question)
				if len(options) > 0 {
					prompt.WriteString(" Options: " + strings.Join(options, " | "))
				}
				if ctx.RequestUser == nil {
					// No answerer composed: fail closed like the approval
					// chain (upstream: missing answerers fail closed).
					return nil, fmt.Errorf("ask_user_question: no user-answerer is available in this host")
				}
				decision, err := ctx.RequestUser(prompt.String(), options)
				answer := askAnswer{ID: q.ID}
				switch decision {
				case ApprovalAllowOnce:
					answer.Selected = []string{"approved"}
				case ApprovalDeny:
					answer.Selected = []string{"denied"}
				case ApprovalCancel:
					answer.Custom = "cancelled"
				}
				_ = err
				answers = append(answers, answer)
			}
			b, _ := json.MarshalIndent(map[string]any{"answers": answers}, "", "  ")
			return string(b), nil
		},
	})
}
