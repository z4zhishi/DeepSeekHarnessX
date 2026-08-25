package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"dsh-go/pkg/session"
)

const (
	anthropicVersion = "2023-06-01"
)

// anthropicAdapter implements LlmAdapter against the Anthropic Messages API
// (POST {base}/v1/messages SSE).
type anthropicAdapter struct {
	mu      sync.Mutex
	profile ProviderProfile
	httpc   *http.Client
	baseURL string
	model   string
	timeout time.Duration
	watch   time.Duration
}

func newAnthropicAdapter(p ProviderProfile) *anthropicAdapter {
	p = normalizeProfile(p)
	return &anthropicAdapter{
		profile: p,
		httpc:   p.HTTPClient,
		baseURL: p.BaseURL,
		model:   p.Model,
		timeout: p.Timeout,
		watch:   p.Watchdog,
	}
}

func anthropicMessagesURL(baseURL string) string {
	base := trimBaseURL(baseURL)
	if pathEnds(base, "/messages") || pathHas(base, "/v1/messages") {
		return base
	}
	if pathEnds(base, "/v1") {
		return base + "/messages"
	}
	return base + "/v1/messages"
}

func (a *anthropicAdapter) Stream(ctx context.Context, req ModelRequest) (<-chan StreamChunk, <-chan error) {
	key, err := resolveStreamKey(a.profile.APIKey, a.profile.APIKeyResolver)
	if err != nil {
		return failStream(err)
	}
	a.mu.Lock()
	timeout := a.timeout
	a.mu.Unlock()
	return startStream(ctx, timeout, func(streamCtx context.Context, cancel context.CancelFunc, chunks chan<- StreamChunk) error {
		return a.stream(streamCtx, cancel, req, key, chunks)
	})
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicMsg struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type anthropicRequest struct {
	Model       string          `json:"model"`
	System      string          `json:"system,omitempty"`
	Messages    []anthropicMsg  `json:"messages"`
	Stream      bool            `json:"stream"`
	MaxTokens   int             `json:"max_tokens"`
	Temperature *float64        `json:"temperature,omitempty"`
	Tools       []anthropicTool `json:"tools,omitempty"`
}

type anthropicEvent struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
		Text string `json:"text"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
	Message *struct {
		Usage *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

func buildAnthropicRequest(req ModelRequest) anthropicRequest {
	body := anthropicRequest{
		Model:     req.Model,
		System:    req.System,
		Messages:  buildAnthropicMessages(req),
		Stream:    true,
		// max_tokens is REQUIRED on the Messages wire: an unset (<=0) request
		// falls back to DefaultMaxTokens, never leaves the harness unbounded.
		MaxTokens: effectiveMaxTokens(req.MaxTokens),
	}
	if req.Temperature != nil {
		body.Temperature = req.Temperature
	}
	for _, t := range req.Tools {
		schema := t.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		body.Tools = append(body.Tools, anthropicTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		})
	}
	return body
}

func buildAnthropicMessages(req ModelRequest) []anthropicMsg {
	out := make([]anthropicMsg, 0, len(req.Messages))
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			// Top-level `system` already carries req.System; inline system
			// turns are folded into a user message so the wire stays
			// user/assistant alternating.
			text := flattenTextBlocks(m.Content)
			if text != "" {
				out = append(out, anthropicMsg{Role: "user", Content: text})
			}
			continue
		case "assistant":
			blocks := make([]map[string]any, 0, len(m.Content))
			for _, b := range m.Content {
				switch b.Type {
				case "text":
					blocks = append(blocks, map[string]any{"type": "text", "text": b.Text})
				case "reasoning":
					blocks = append(blocks, map[string]any{"type": "thinking", "thinking": b.Text})
				case "tool-call":
					var input any = map[string]any{}
					if b.Arguments != "" {
						if err := json.Unmarshal([]byte(b.Arguments), &input); err != nil {
							input = map[string]any{}
						}
					}
					blocks = append(blocks, map[string]any{
						"type":  "tool_use",
						"id":    b.ID,
						"name":  b.Name,
						"input": input,
					})
				}
			}
			if len(blocks) == 0 {
				out = append(out, anthropicMsg{Role: "assistant", Content: ""})
			} else {
				out = append(out, anthropicMsg{Role: "assistant", Content: blocks})
			}
			continue
		}
		// User-role messages: text blocks come first, then each tool-result
		// block becomes an Anthropic tool_result riding in the same user
		// message (results stay verbatim in the projected history).
		var texts []map[string]any
		var results []map[string]any
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				if b.Text != "" {
					texts = append(texts, map[string]any{"type": "text", "text": b.Text})
				}
			case "tool-result":
				text := flattenTextBlocks(b.Content)
				if text == "" {
					text = "(no output)"
				}
				results = append(results, map[string]any{
					"type":        "tool_result",
					"tool_use_id": b.ToolCallID,
					"content":     text,
				})
			}
		}
		blocks := append(append([]map[string]any(nil), texts...), results...)
		if len(blocks) > 0 {
			out = append(out, anthropicMsg{Role: "user", Content: blocks})
		}
	}
	return mergeAnthropicRoles(out)
}

// mergeAnthropicRoles concatenates consecutive same-role messages so the
// transcript alternates user/assistant as the Messages API requires.
func mergeAnthropicRoles(in []anthropicMsg) []anthropicMsg {
	if len(in) == 0 {
		return in
	}
	out := make([]anthropicMsg, 0, len(in))
	for _, m := range in {
		if len(out) == 0 || out[len(out)-1].Role != m.Role {
			out = append(out, m)
			continue
		}
		prev := &out[len(out)-1]
		prev.Content = concatAnthropicContent(prev.Content, m.Content)
	}
	return out
}

func concatAnthropicContent(a, b any) any {
	as := asAnthropicBlocks(a)
	bs := asAnthropicBlocks(b)
	return append(as, bs...)
}

func asAnthropicBlocks(v any) []map[string]any {
	switch t := v.(type) {
	case string:
		if t == "" {
			return nil
		}
		return []map[string]any{{"type": "text", "text": t}}
	case []map[string]any:
		return t
	default:
		return nil
	}
}

func mapAnthropicStop(reason string) string {
	switch reason {
	case "", "end_turn", "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool-calls"
	case "max_tokens":
		return "max-tokens"
	default:
		return "error"
	}
}

func (a *anthropicAdapter) stream(
	streamCtx context.Context,
	cancel context.CancelFunc,
	req ModelRequest,
	apiKey string,
	chunks chan<- StreamChunk,
) error {
	a.mu.Lock()
	base := a.baseURL
	model := a.model
	httpc := a.httpc
	watch := a.watch
	extra := a.profile.ExtraHeaders
	a.mu.Unlock()
	if req.Model == "" {
		req.Model = model
	}

	body, err := json.Marshal(buildAnthropicRequest(req))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDeepSeekBadRequest, err)
	}
	httpReq, err := http.NewRequestWithContext(streamCtx, http.MethodPost, anthropicMessagesURL(base), bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("x-api-key", apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	applyExtraHeaders(httpReq.Header, extra)

	emit := func(c StreamChunk) { emitChunk(streamCtx, chunks, c) }

	type slot struct {
		kind string
		id   string
		name string
	}
	opened := map[int]slot{}
	openedCount := 0
	var pendingUsage *session.TokenUsage
	pendingFinish := "stop"
	finished := false

	closeIndex := func(idx int) {
		if _, ok := opened[idx]; ok {
			emit(StreamChunk{Type: ChunkBlockEnd, Index: idx})
			delete(opened, idx)
		}
	}
	closeAll := func() {
		for idx := range opened {
			emit(StreamChunk{Type: ChunkBlockEnd, Index: idx})
		}
		opened = map[int]slot{}
	}
	applyUsage := func(input, output, cacheRead, cacheWrite int) {
		if pendingUsage == nil {
			pendingUsage = &session.TokenUsage{}
		}
		if input > 0 {
			pendingUsage.InputTokens = input
		}
		if output > 0 {
			pendingUsage.OutputTokens = output
		}
		if cacheRead > 0 {
			pendingUsage.CacheReadTokens = cacheRead
		}
		if cacheWrite > 0 {
			pendingUsage.CacheWriteTokens = cacheWrite
		}
	}
	finish := func() {
		if finished {
			return
		}
		finished = true
		closeAll()
		if pendingUsage != nil {
			emit(StreamChunk{Type: ChunkUsage, Usage: pendingUsage})
		}
		reason := pendingFinish
		if reason == "stop" && openedCount == 0 {
			reason = "error"
		}
		emit(StreamChunk{Type: ChunkFinish, FinishReason: reason})
	}

	return consumeSSE(streamCtx, cancel, httpc, watch, httpReq, func(data string) (bool, error) {
		if data == "[DONE]" {
			finish()
			return true, nil
		}
		var ev anthropicEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return false, fmt.Errorf("%w: %v", ErrDeepSeekStream, err)
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil && ev.Message.Usage != nil {
				u := ev.Message.Usage
				applyUsage(u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens)
			}
		case "content_block_start":
			if ev.ContentBlock == nil {
				return false, nil
			}
			kind := "text"
			id, name := "", ""
			switch ev.ContentBlock.Type {
			case "text":
				kind = "text"
			case "thinking", "redacted_thinking":
				kind = "reasoning"
			case "tool_use":
				kind = "tool-call"
				id = ev.ContentBlock.ID
				name = ev.ContentBlock.Name
			default:
				return false, nil
			}
			openedCount++
			opened[ev.Index] = slot{kind: kind, id: id, name: name}
			emit(StreamChunk{Type: ChunkBlockStart, Index: ev.Index, BlockType: kind, ID: id, Name: name})
			if kind == "text" && ev.ContentBlock.Text != "" {
				emit(StreamChunk{Type: ChunkTextDelta, Index: ev.Index, Text: ev.ContentBlock.Text})
			}
		case "content_block_delta":
			if ev.Delta == nil {
				return false, nil
			}
			s := opened[ev.Index]
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" {
					emit(StreamChunk{Type: ChunkTextDelta, Index: ev.Index, Text: ev.Delta.Text})
				}
			case "thinking_delta":
				if ev.Delta.Thinking != "" {
					emit(StreamChunk{Type: ChunkReasoningDelta, Index: ev.Index, Text: ev.Delta.Thinking})
				}
			case "input_json_delta":
				if ev.Delta.PartialJSON != "" {
					emit(StreamChunk{
						Type:           ChunkToolCallDelta,
						Index:          ev.Index,
						ID:             s.id,
						Name:           s.name,
						ArgumentsDelta: ev.Delta.PartialJSON,
					})
				}
			}
		case "content_block_stop":
			closeIndex(ev.Index)
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				pendingFinish = mapAnthropicStop(ev.Delta.StopReason)
			}
			if ev.Usage != nil {
				applyUsage(ev.Usage.InputTokens, ev.Usage.OutputTokens, ev.Usage.CacheReadInputTokens, ev.Usage.CacheCreationInputTokens)
			}
		case "message_stop":
			finish()
			return true, nil
		}
		return false, nil
	})
}
