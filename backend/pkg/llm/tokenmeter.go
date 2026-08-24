package llm

import (
	"fmt"

	"dsh-go/pkg/session"
)

// tokenmeter.go elevates the compaction heuristic into a reusable, replay-aware
// token service. It prices a session's projected model transcript with the
// fixed ~4-characters-per-token heuristic plus per-block structural overhead,
// and returns a context projection (tokenUsage / contextPressure /
// projectedTokens / contextBreakdown) that compaction and the UI can share.
//
// The pricing here is the single source of truth for the heuristic. The
// compaction package's HeuristicMeter remains a thin per-node adapter over the
// same per-block arithmetic (it must keep its local copy to avoid an
// import cycle; keep the two in sync when the heuristic changes).

// Meter is the replay-aware token meter. The zero value is usable; set
// ContextLimit to have Measure report a normalized pressure ratio.
type Meter struct {
	// ContextLimit is the nominal context window the pressure ratio is
	// computed against (typically the selected model's window in tokens).
	// 0 disables pressure reporting (ContextPressure stays 0).
	ContextLimit int
}

// ContextBreakdown is the projected transcript split by message role and by
// input/output side.
type ContextBreakdown struct {
	// InputTokens is the projected size of the transcript as the context a
	// model would receive. It equals ProjectedTokens: output is not estimated
	// from a static transcript, so the whole projection is input-side.
	InputTokens int `json:"inputTokens"`
	// OutputTokens is always 0; a transcript cannot predict the next call's
	// generated output. Kept for a symmetric shape.
	OutputTokens int `json:"outputTokens"`
	// UserTokens / AssistantTokens / ToolTokens are the projected totals of
	// the user, assistant, and tool messages respectively.
	UserTokens      int `json:"userTokens"`
	AssistantTokens int `json:"assistantTokens"`
	ToolTokens      int `json:"toolTokens"`
}

// ContextMetrics is the complete projection for one session transcript.
type ContextMetrics struct {
	// TokenUsage carries the projected consumption in the canonical session
	// TokenUsage shape (InputTokens holds the total projection).
	TokenUsage session.TokenUsage `json:"tokenUsage"`
	// ContextPressure is TokenUsage.InputTokens / ContextLimit, clamped to
	// [0,1]; 0 when ContextLimit is unset or the transcript is empty.
	ContextPressure float64 `json:"contextPressure"`
	// ContextLimit is the window ContextPressure was computed against.
	ContextLimit int `json:"contextLimit"`
	// ProjectedTokens is the projected size of the whole transcript.
	ProjectedTokens int `json:"projectedTokens"`
	// MessageCount is the number of projected model messages priced.
	MessageCount int `json:"messageCount"`
	// Breakdown is the per-role and input/output split of ProjectedTokens.
	Breakdown ContextBreakdown `json:"contextBreakdown"`
}

// Measure prices the session's model-visible transcript. It folds the canonical
// surface and replays every surviving node to its model message (the same
// replay `session.DeriveMessages` performs), then prices each message with the
// fixed heuristic. Events must arrive in contiguous seq order starting at the
// session base seq.
func (m Meter) Measure(events []session.SessionEnvelope) (ContextMetrics, error) {
	messages, err := session.DeriveMessages(events)
	if err != nil {
		return ContextMetrics{}, fmt.Errorf("tokenmeter: project session transcript: %w", err)
	}

	metrics := ContextMetrics{ContextLimit: m.ContextLimit}
	var breakdown ContextBreakdown
	for i := range messages {
		msg := &messages[i]
		tokens := EstimateMessageTokens(msg)
		metrics.ProjectedTokens += tokens
		metrics.MessageCount++
		switch msg.Role {
		case "assistant":
			breakdown.AssistantTokens += tokens
		case "tool":
			breakdown.ToolTokens += tokens
		default: // "user" (and any "system" surface node)
			breakdown.UserTokens += tokens
		}
	}

	breakdown.InputTokens = metrics.ProjectedTokens
	// OutputTokens cannot be derived from a transcript; left at 0.
	metrics.Breakdown = breakdown

	metrics.TokenUsage.InputTokens = metrics.ProjectedTokens
	if m.ContextLimit > 0 && metrics.ProjectedTokens > 0 {
		p := float64(metrics.ProjectedTokens) / float64(m.ContextLimit)
		if p > 1 {
			p = 1
		}
		metrics.ContextPressure = p
	}
	return metrics, nil
}

// EstimateMessageTokens prices one projected model message with the fixed
// heuristic: ~4 chars per token for text, plus fixed per-block structural
// overhead. This is the same arithmetic compaction's HeuristicMeter uses; keep
// the two in lockstep.
func EstimateMessageTokens(message *session.ModelMessage) int {
	total := 0
	for _, block := range message.Content {
		switch block.Type {
		case "text", "reasoning":
			total += (len(block.Text) + 3) / 4
		case "tool-call":
			total += 8 + (len(block.Arguments)+3)/4
		case "tool-result":
			total += 16 + (len(block.Text)+3)/4
		default:
			total += 4
		}
	}
	if total == 0 {
		total = 1
	}
	return total
}
