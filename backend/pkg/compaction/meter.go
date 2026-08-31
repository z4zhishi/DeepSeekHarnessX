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
		out[i] = llm.EstimateMessageTokens(message)
	}
	return out, nil
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
// blocks plus the provider-reported usage, the complete raw provider output,
// and llmStreamCall:true — the exact call envelope recorded on
// compaction/summary so the auxiliary call is reconstructible from the log
// (upstream summarizeWithLlm).
func (s LlmSummarizer) Summarize(ctx context.Context, input SummarizationInput) ([]session.ContentBlock, *session.TokenUsage, []session.ContentBlock, bool, error) {
	if s.Adapter == nil {
		return nil, nil, nil, false, fmt.Errorf("llm summarizer requires an adapter")
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
				return nil, nil, nil, false, err
			}
		case chunk, ok := <-chunkChan:
			if !ok {
				// A fatal buffered error must not be lost to select randomness:
				// failStream (missing credential etc.) closes both channels
				// with the error already buffered, so closed-chunkChan alone
				// is not proof of clean EOF — draining finds the buffered
				// failure; closed-empty means the stream really ended.
				select {
				case err, errOk := <-errChan:
					if errOk && err != nil {
						return nil, nil, nil, false, err
					}
				default:
				}
				streamDone = true
				break
			}
			assembler.IngestChunk(chunk)
		case <-ctx.Done():
			return nil, nil, nil, false, ctx.Err()
		}
	}
	blocks, usage, finish := assembler.Result()
	if finish == "error" || finish == "aborted" {
		return nil, nil, nil, false, fmt.Errorf("summarization finished with reason %q", finish)
	}
	rawOutput := append([]session.ContentBlock(nil), blocks...)
	// Keep only text before synthesizing the checkpoint (upstream summaryText
	// rejects visual output and filters to text blocks).
	var summary []session.ContentBlock
	for _, b := range blocks {
		if b.Type == "text" {
			summary = append(summary, b)
		}
	}
	if len(summary) == 0 {
		return nil, nil, rawOutput, true, fmt.Errorf("summarization produced no text summary content")
	}
	return summary, usage, rawOutput, true, nil
}
