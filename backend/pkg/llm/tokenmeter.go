package llm

import (
	"encoding/json"
	"fmt"

	"dsh-go/pkg/session"
)

// tokenmeter.go elevates the compaction heuristic into a reusable, replay-aware
// token service. It prices a session's projected model transcript with the
// fixed ~4-characters-per-token heuristic plus per-block structural overhead,
// and returns a context projection (tokenUsage / contextPressure /
// projectedTokens / contextBreakdown) that compaction and the UI can share.
//
// The arithmetic mirrors upstream `@deepseek-ai/dsh-token-meter/estimate`:
// text/reasoning price ceil(len/4)+BLOCK_OVERHEAD per block, tool-call adds
// its name and arguments, tool-result recurses into the nested content array,
// every message carries ROLE_OVERHEAD. This is the single source of truth for
// the heuristic; the compaction package delegates here (its HeuristicMeter is
// a thin per-node adapter).

// Fixed-density heuristic constants (upstream estimate.ts).
const (
	// charsPerToken is the fixed text-density estimate used until exact
	// tokenization is needed.
	charsPerToken = 4
	// blockOverhead is the per-block structural overhead for JSON framing and
	// type tags.
	blockOverhead = 4
	// roleOverhead is the role-field framing overhead added to every priced
	// message (upstream ROLE_OVERHEAD).
	roleOverhead = 4
)

func ceilDiv(n int) int { return (n + charsPerToken - 1) / charsPerToken }

// EstimateContentTokens prices content blocks recursively under the fixed
// density heuristic (upstream estimateContent): tool-result blocks recurse
// into their nested content so a large tool output is priced by its actual
// size instead of a fixed constant.
func EstimateContentTokens(blocks []session.ContentBlock) int {
	tokens := 0
	for i := range blocks {
		block := &blocks[i]
		switch block.Type {
		case "text", "reasoning":
			tokens += ceilDiv(len(block.Text)) + blockOverhead
		case "tool-call":
			tokens += ceilDiv(len(block.Name)) + ceilDiv(len(block.Arguments)) + blockOverhead
		case "tool-result":
			tokens += EstimateContentTokens(block.Content) + blockOverhead
		default:
			// Unknown merge-extensible blocks keep a conservative structural
			// JSON price under the fixed heuristic (upstream default arm).
			raw, err := json.Marshal(block)
			if err != nil {
				tokens += blockOverhead
				continue
			}
			tokens += blockOverhead + ceilDiv(len(raw))
		}
	}
	return tokens
}

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
		switch {
		case msg.Role == "assistant":
			breakdown.AssistantTokens += tokens
		case hasToolResultBlock(msg.Content):
			// Tool results ride in verbatim projected messages; the
			// breakdown classifies them by block shape, not role.
			breakdown.ToolTokens += tokens
		default: // plain "user" text (and any "system" surface node)
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

// hasToolResultBlock reports whether any block in the message is a tool
// result (the shape the tool-token breakdown bucket tracks).
func hasToolResultBlock(blocks []session.ContentBlock) bool {
	for i := range blocks {
		if blocks[i].Type == "tool-result" {
			return true
		}
	}
	return false
}

// EstimateMessageTokens prices one projected model message with the fixed
// heuristic: recursive per-block pricing plus the role-field framing overhead
// (upstream estimateMessage). This is the shared single-source arithmetic;
// compaction's HeuristicMeter delegates to it.
func EstimateMessageTokens(message *session.ModelMessage) int {
	total := EstimateContentTokens(message.Content) + roleOverhead
	if total == 0 {
		total = 1
	}
	return total
}
