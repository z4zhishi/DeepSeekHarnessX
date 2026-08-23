package compaction

import (
	"context"
	"fmt"

	"dsh-go/pkg/llm"
	"dsh-go/pkg/session"
)

// HeuristicMeter estimates per-node token prices from projected message
// content. It is the fixed estimator used by the pressure policy and the
// shadow price recorded on compaction/summary (a stand-in for the upstream
// token-meter seam).
type HeuristicMeter struct{}

// MeasureNodes returns one token estimate per surface node, in surface order.
func (HeuristicMeter) MeasureNodes(events []session.SessionEnvelope, nodes []int) ([]int, error) {
	bySeq := make(map[int]*session.SessionEnvelope, len(events))
	for i := range events {
		bySeq[events[i].Seq] = &events[i]
	}
	out := make([]int, len(nodes))
	for i, seq := range nodes {
		env, ok := bySeq[seq]
		if !ok {
			return nil, fmt.Errorf("compaction: surface seq %d has no matching session event", seq)
		}
		message, err := projectEvent(env)
		if err != nil {
			return nil, err
		}
		if message == nil {
			out[i] = 0
			continue
		}
		out[i] = estimateMessageTokens(message)
	}
	return out, nil
}

// estimateMessageTokens prices one projected model message with the fixed
// heuristic: ~4 chars per token for text, plus fixed per-block overhead.
func estimateMessageTokens(message *session.ModelMessage) int {
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

// LlmSummarizer runs the compaction summarization through a real LLM adapter,
// mirroring the upstream summarizer's one-shot call with the replayed
// conversation prefix in front of the compaction instruction.
type LlmSummarizer struct {
	Adapter   llm.LlmAdapter
	Model     string
	MaxTokens int
}

// CompactionInstruction is the summarization directive delivered as the final
// user message after the replayed conversation (upstream COMPACTION_INSTRUCTION).
const CompactionInstruction = `You are now acting as a compaction engine for this AI coding assistant. Condense the conversation ABOVE into a structured checkpoint that lets another model resume the work with no loss of essential context.

Output EXACTLY the Markdown structure below: keep every section, in order. Use terse bullets, not prose paragraphs. Write "(none)" for an empty section — never drop a section.

## Primary Request and Intent
- [the user's original and evolving goals; quote verbatim where the exact wording matters]

## Key Technical Concepts
- [technologies, frameworks, patterns, and conventions in play]

## Files and Code
- [exact path: why it matters, key changes or snippets]

## Errors and Fixes
- [error: how it was resolved, plus any related user feedback]

## Pending Jobs
- [explicitly requested work not yet completed]

## Current Work
- [precisely what was in progress at this checkpoint]

## Next Step
- [the single next action, directly in line with the most recent request, or "(none)"]

## Critical Context
- [decisions and their rationale, constraints, user preferences, open questions, data needed to continue]

Rules:
- Preserve exact file paths, commands, error strings, identifiers, numeric values, and syntax fragments.
- Capture user feedback and explicit instructions faithfully, especially corrections.
- Do NOT mention this summarization request or that the context was compacted.
- Output only the checkpoint text: do not call any tool or take any other action.`

// Summarize streams one summarization request and returns the assembled text
// blocks plus the provider-reported usage.
func (s LlmSummarizer) Summarize(ctx context.Context, input SummarizationInput) ([]session.ContentBlock, *session.TokenUsage, error) {
	if s.Adapter == nil {
		return nil, nil, fmt.Errorf("llm summarizer requires an adapter")
	}
	messages := append([]session.ModelMessage(nil), input.Messages...)
	messages = append(messages, session.ModelMessage{
		Role: "user",
		Content: []session.ContentBlock{
			{Type: "text", Text: CompactionInstruction},
		},
	})
	req := llm.ModelRequest{
		Model:     s.Model,
		Messages:  messages,
		System:    input.System,
		Tools:     input.Tools,
		MaxTokens: s.MaxTokens,
		Purpose:   "compaction",
	}
	chunkChan, errChan := s.Adapter.Stream(ctx, req)
	assembler := llm.NewBlockAssembler()
	streamDone := false
	for !streamDone {
		select {
		case err, ok := <-errChan:
			if ok && err != nil {
				return nil, nil, err
			}
		case chunk, ok := <-chunkChan:
			if !ok {
				streamDone = true
				break
			}
			assembler.IngestChunk(chunk)
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	blocks, usage, finish := assembler.Result()
	if finish == "error" || finish == "aborted" {
		return nil, nil, fmt.Errorf("summarization finished with reason %q", finish)
	}
	if len(blocks) == 0 {
		return nil, nil, fmt.Errorf("summarization produced no content")
	}
	return blocks, usage, nil
}
