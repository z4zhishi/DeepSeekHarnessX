package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"dsh-go/pkg/session"
)

// StreamChunk types in the LLM streaming protocol
const (
	ChunkBlockStart     = "block-start"
	ChunkTextDelta      = "text-delta"
	ChunkReasoningDelta = "reasoning-delta"
	ChunkToolCallDelta  = "tool-call-delta"
	ChunkBlockEnd       = "block-end"
	ChunkUsage          = "usage"
	ChunkFinish         = "finish"
)

// StreamChunk is a discriminated union of chunk events emitted by an LLM adapter.
type StreamChunk struct {
	Type           string                `json:"type"`
	Index          int                   `json:"index,omitempty"`
	BlockType      string                `json:"blockType,omitempty"`      // "text" | "reasoning" | "tool-call"
	BlockID        string                `json:"blockId,omitempty"`        // stable identity of the open block this chunk belongs to (parallel tool calls interleave)
	Text           string                `json:"text,omitempty"`           // for text-delta / reasoning-delta
	ID             string                `json:"id,omitempty"`             // for tool-call-delta
	Name           string                `json:"name,omitempty"`           // for tool-call-delta
	ArgumentsDelta string                `json:"argumentsDelta,omitempty"` // raw JSON string fragment
	Block          *session.ContentBlock `json:"block,omitempty"`          // for block-end
	Usage          *session.TokenUsage   `json:"usage,omitempty"`          // for usage
	FinishReason   string                `json:"finishReason,omitempty"`   // "stop" | "tool-calls" | "max-tokens" | "error" | "aborted"
}

// ModelRequest represents the parameters sent to an LLM provider.
type ModelRequest struct {
	Model           string                 `json:"model"`
	Messages        []session.ModelMessage `json:"messages"`
	System          string                 `json:"system,omitempty"`
	Temperature     *float64               `json:"temperature,omitempty"`
	MaxTokens       int                    `json:"maxTokens,omitempty"`
	Tools           []ToolDeclaration      `json:"tools,omitempty"`
	ReasoningEffort string                 `json:"reasoningEffort,omitempty"` // "off" | "low" | "high" | "max"
	SessionID       string                 `json:"sessionId,omitempty"`
	Purpose         string                 `json:"purpose,omitempty"` // "" | "session-title" | "compaction"
}

// ToolDeclaration represents a tool schema advertised to the model.
type ToolDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema
}

// LlmAdapter is the pluggable interface for LLM providers.
type LlmAdapter interface {
	Stream(ctx context.Context, req ModelRequest) (<-chan StreamChunk, <-chan error)
}

// BlockAssembler incrementally folds StreamChunks into complete ContentBlocks.
//
// Chunks carry a stable BlockID, so a stream may keep several blocks open at
// once (DeepSeek interleaves parallel tool-call deltas by wire index). Blocks
// commit in open order: block-end commits its own block wherever it sits in
// that order, and finish flushes every still-open remainder in open order.
type BlockAssembler struct {
	openOrder    []string                 // block IDs in first-open order — commit order
	active       map[string]*activeBlock  // currently open blocks keyed by BlockID
	legacyActive *activeBlock             // single anonymous block for ID-less legacy streams (anthropic / responses / mock)
	blocks       []session.ContentBlock
	usage        *session.TokenUsage
	finishReason string
}

// activeBlock is one in-flight block under assembly.
type activeBlock struct {
	block session.ContentBlock
}

// NewBlockAssembler creates a new incremental block aggregator.
func NewBlockAssembler() *BlockAssembler {
	return &BlockAssembler{
		active: make(map[string]*activeBlock),
	}
}

// target resolves the block a chunk applies to. Keyed chunks address their own
// open block; unkeyed chunks fall back to the most recent open block and only
// materialize a legacy singleton when nothing is open at all.
func (ba *BlockAssembler) target(chunk StreamChunk) *activeBlock {
	if chunk.BlockID != "" {
		if ab, ok := ba.active[chunk.BlockID]; ok {
			return ab
		}
		return nil
	}
	if len(ba.openOrder) > 0 {
		return ba.active[ba.openOrder[len(ba.openOrder)-1]]
	}
	return ba.legacyActive
}

// open registers a new block under an explicit or synthesized ID and returns it.
func (ba *BlockAssembler) open(id string) *activeBlock {
	if id == "" {
		id = fmt.Sprintf("block-%d", len(ba.openOrder)+len(ba.blocks))
	}
	ab := &activeBlock{block: session.ContentBlock{}}
	ba.active[id] = ab
	ba.openOrder = append(ba.openOrder, id)
	return ab
}

// commit closes the block registered under id and appends it to the results.
func (ba *BlockAssembler) commit(id string) {
	ab, ok := ba.active[id]
	if !ok {
		return
	}
	delete(ba.active, id)
	for i, openID := range ba.openOrder {
		if openID == id {
			ba.openOrder = append(ba.openOrder[:i], ba.openOrder[i+1:]...)
			break
		}
	}
	ba.blocks = append(ba.blocks, ab.block)
}

// commitLegacy closes the anonymous singleton block, if any.
func (ba *BlockAssembler) commitLegacy() {
	if ba.legacyActive != nil {
		ba.blocks = append(ba.blocks, ba.legacyActive.block)
		ba.legacyActive = nil
	}
}

// IngestChunk consumes a chunk and updates assembled content state.
func (ba *BlockAssembler) IngestChunk(chunk StreamChunk) {
	switch chunk.Type {
	case ChunkBlockStart:
		if chunk.BlockID != "" {
			if _, exists := ba.active[chunk.BlockID]; !exists {
				ab := ba.open(chunk.BlockID)
				ab.block = session.ContentBlock{
					Type: chunk.BlockType,
					ID:   chunk.ID,
					Name: chunk.Name,
				}
			}
			return
		}
		// Legacy ID-less start: exactly one anonymous block may be open;
		// a second start replaces it (pre-overhaul semantics preserved).
		if ba.legacyActive != nil {
			ba.commitLegacy()
		} else if len(ba.openOrder) > 0 {
			ba.commit(ba.openOrder[len(ba.openOrder)-1])
		}
		ba.legacyActive = &activeBlock{block: session.ContentBlock{
			Type: chunk.BlockType,
			ID:   chunk.ID,
			Name: chunk.Name,
		}}

	case ChunkTextDelta:
		if ab := ba.target(chunk); ab != nil {
			ab.block.Text += chunk.Text
			return
		}
		if chunk.BlockID != "" {
			ab := ba.open(chunk.BlockID)
			ab.block.Type = "text"
			ab.block.Text += chunk.Text
			return
		}
		if ba.legacyActive == nil {
			ba.legacyActive = &activeBlock{}
		}
		ba.legacyActive.block.Type = "text"
		ba.legacyActive.block.Text += chunk.Text

	case ChunkReasoningDelta:
		if ab := ba.target(chunk); ab != nil {
			ab.block.Text += chunk.Text
			return
		}
		if chunk.BlockID != "" {
			ab := ba.open(chunk.BlockID)
			ab.block.Type = "reasoning"
			ab.block.Text += chunk.Text
			return
		}
		if ba.legacyActive == nil {
			ba.legacyActive = &activeBlock{}
		}
		ba.legacyActive.block.Type = "reasoning"
		ba.legacyActive.block.Text += chunk.Text

	case ChunkToolCallDelta:
		var ab *activeBlock
		if chunk.BlockID != "" {
			if a, ok := ba.active[chunk.BlockID]; ok {
				ab = a
			} else {
				// Delta before its block-start: synthesize the tool-call block
				// so arguments are never dropped.
				ab = ba.open(chunk.BlockID)
				ab.block.Type = "tool-call"
			}
		} else if len(ba.openOrder) > 0 {
			ab = ba.active[ba.openOrder[len(ba.openOrder)-1]]
		} else {
			if ba.legacyActive == nil {
				ba.legacyActive = &activeBlock{block: session.ContentBlock{Type: "tool-call"}}
			}
			ab = ba.legacyActive
		}
		if chunk.ID != "" && ab.block.ID == "" {
			ab.block.ID = chunk.ID
		}
		if chunk.Name != "" && ab.block.Name == "" {
			ab.block.Name = chunk.Name
		}
		ab.block.Arguments += chunk.ArgumentsDelta

	case ChunkBlockEnd:
		switch {
		case chunk.BlockID != "":
			// Commit this block only; sibling blocks stay open. An end for an
			// unknown ID carries a complete fallback block — append it verbatim
			// (deferred-close adapters ship the finished block this way).
			if _, ok := ba.active[chunk.BlockID]; !ok && chunk.Block != nil {
				ba.blocks = append(ba.blocks, *chunk.Block)
				return
			}
			ba.commit(chunk.BlockID)
		case chunk.Block != nil:
			ba.blocks = append(ba.blocks, *chunk.Block)
		default:
			if len(ba.openOrder) > 0 {
				ba.commit(ba.openOrder[len(ba.openOrder)-1])
				return
			}
			ba.commitLegacy()
		}

	case ChunkUsage:
		if chunk.Usage != nil {
			ba.usage = chunk.Usage
		}

	case ChunkFinish:
		ba.finishReason = chunk.FinishReason
		// Flush remainders in open order, then any anonymous legacy block.
		for _, id := range ba.openOrder {
			ba.commit(id)
		}
		ba.commitLegacy()
	}
}

// Result returns all assembled content blocks and token usage.
func (ba *BlockAssembler) Result() ([]session.ContentBlock, *session.TokenUsage, string) {
	return ba.blocks, ba.usage, ba.finishReason
}

// FormatDeepSeekSystemPrompt prepares the system message including tool definitions for DeepSeek API.
func FormatDeepSeekSystemPrompt(baseSystem string, tools []ToolDeclaration) string {
	if len(tools) == 0 {
		return baseSystem
	}

	var sb strings.Builder
	sb.WriteString(baseSystem)
	sb.WriteString("\n\n## Available Tools\n")
	for _, t := range tools {
		sb.WriteString(fmt.Sprintf("\n### Tool: %s\n%s\nParameters Schema:\n```json\n%s\n```\n",
			t.Name, t.Description, string(t.Parameters)))
	}
	return sb.String()
}
