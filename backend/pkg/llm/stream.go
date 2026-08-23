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
type BlockAssembler struct {
	activeBlock  *session.ContentBlock
	blocks       []session.ContentBlock
	usage        *session.TokenUsage
	finishReason string
}

// NewBlockAssembler creates a new incremental block aggregator.
func NewBlockAssembler() *BlockAssembler {
	return &BlockAssembler{}
}

// IngestChunk consumes a chunk and updates assembled content state.
func (ba *BlockAssembler) IngestChunk(chunk StreamChunk) {
	switch chunk.Type {
	case ChunkBlockStart:
		ba.activeBlock = &session.ContentBlock{
			Type: chunk.BlockType,
			ID:   chunk.ID,
			Name: chunk.Name,
		}

	case ChunkTextDelta:
		if ba.activeBlock == nil {
			ba.activeBlock = &session.ContentBlock{Type: "text"}
		}
		ba.activeBlock.Text += chunk.Text

	case ChunkReasoningDelta:
		if ba.activeBlock == nil {
			ba.activeBlock = &session.ContentBlock{Type: "reasoning"}
		}
		ba.activeBlock.Text += chunk.Text

	case ChunkToolCallDelta:
		if ba.activeBlock == nil {
			ba.activeBlock = &session.ContentBlock{
				Type: "tool-call",
				ID:   chunk.ID,
				Name: chunk.Name,
			}
		}
		if chunk.ID != "" && ba.activeBlock.ID == "" {
			ba.activeBlock.ID = chunk.ID
		}
		if chunk.Name != "" && ba.activeBlock.Name == "" {
			ba.activeBlock.Name = chunk.Name
		}
		ba.activeBlock.Arguments += chunk.ArgumentsDelta

	case ChunkBlockEnd:
		if ba.activeBlock != nil {
			ba.blocks = append(ba.blocks, *ba.activeBlock)
			ba.activeBlock = nil
		} else if chunk.Block != nil {
			ba.blocks = append(ba.blocks, *chunk.Block)
		}

	case ChunkUsage:
		if chunk.Usage != nil {
			ba.usage = chunk.Usage
		}

	case ChunkFinish:
		ba.finishReason = chunk.FinishReason
		if ba.activeBlock != nil {
			ba.blocks = append(ba.blocks, *ba.activeBlock)
			ba.activeBlock = nil
		}
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
