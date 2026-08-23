package llm

import (
	"context"

	"dsh-go/pkg/session"
)

// MockLlmAdapter provides a simple mock LLM adapter for testing and offline runs.
type MockLlmAdapter struct {
	ResponseText string
}

// Stream streams mock response chunks.
func (m *MockLlmAdapter) Stream(ctx context.Context, req ModelRequest) (<-chan StreamChunk, <-chan error) {
	chunkChan := make(chan StreamChunk, 8)
	errChan := make(chan error, 1)

	resp := m.ResponseText
	if resp == "" {
		resp = "Hello from DeepSeek-Harness (DSH) Go Engine!"
	}

	go func() {
		defer close(chunkChan)
		defer close(errChan)

		chunkChan <- StreamChunk{Type: ChunkBlockStart, BlockType: "text"}
		chunkChan <- StreamChunk{Type: ChunkTextDelta, Text: resp}
		chunkChan <- StreamChunk{Type: ChunkBlockEnd}
		chunkChan <- StreamChunk{Type: ChunkUsage, Usage: &session.TokenUsage{InputTokens: 30, OutputTokens: 15}}
		chunkChan <- StreamChunk{Type: ChunkFinish, FinishReason: "stop"}
	}()

	return chunkChan, errChan
}
