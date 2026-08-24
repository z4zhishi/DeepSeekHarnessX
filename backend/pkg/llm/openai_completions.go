package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"dsh-go/pkg/session"
)

// chatCompletionsURL resolves the Chat Completions POST URL for a base.
// If BaseURL already ends with /v1 → /chat/completions; if it already
// contains /chat/completions use as-is; else if host is api.deepseek.com →
// /chat/completions; else /v1/chat/completions.
func chatCompletionsURL(baseURL string) string {
	base := trimBaseURL(baseURL)
	if base == "" {
		base = DefaultDeepSeekBaseURL
	}
	if pathHas(base, "/chat/completions") {
		return base
	}
	if pathEnds(base, "/v1") {
		return base + "/chat/completions"
	}
	if urlHost(base) == "api.deepseek.com" {
		return base + deepSeekChatPath
	}
	return base + "/v1/chat/completions"
}

func newCompletionsAdapter(p ProviderProfile) *DeepSeekAdapter {
	p = normalizeProfile(p)
	if p.BaseURL == "" {
		p.BaseURL = DefaultDeepSeekBaseURL
	}
	if p.Model == "" {
		p.Model = DefaultDeepSeekModel
	}
	return &DeepSeekAdapter{
		cfg: DeepSeekConfig{
			APIKey:         p.APIKey,
			BaseURL:        p.BaseURL,
			Model:          p.Model,
			Timeout:        p.Timeout,
			Watchdog:       p.Watchdog,
			HTTPClient:     p.HTTPClient,
			APIKeyResolver: p.APIKeyResolver,
		},
		httpc:    p.HTTPClient,
		baseURL:  p.BaseURL,
		model:    p.Model,
		timeout:  p.Timeout,
		watchdog: p.Watchdog,
		extra:    p.ExtraHeaders,
	}
}

// Stream implements LlmAdapter. It returns a chunk channel and an error channel;
// the error channel carries exactly one fatal error (or none on clean EOF).
func (d *DeepSeekAdapter) Stream(ctx context.Context, req ModelRequest) (<-chan StreamChunk, <-chan error) {
	key, err := resolveStreamKey(d.cfg.APIKey, d.cfg.APIKeyResolver)
	if err != nil {
		return failStream(err)
	}

	d.mu.Lock()
	timeout := d.timeout
	d.mu.Unlock()

	return startStream(ctx, timeout, func(streamCtx context.Context, cancel context.CancelFunc, chunks chan<- StreamChunk) error {
		return d.streamCompletions(streamCtx, cancel, req, key, chunks)
	})
}

// ---------------------------------------------------------------------------
// Wire request / response types (OpenAI-compatible Chat Completions JSON).
// ---------------------------------------------------------------------------

type deepSeekToolCall struct {
	ID       string               `json:"id,omitempty"`
	Type     string               `json:"type,omitempty"` // "function"
	Function deepSeekFunctionCall `json:"function,omitempty"`
}

type deepSeekFunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"` // raw JSON string
}

type deepSeekMessage struct {
	Role             string             `json:"role"`
	Content          string             `json:"content,omitempty"` // "" for text-less turns, NEVER null
	ToolCallID       string             `json:"tool_call_id,omitempty"`
	ToolCalls        []deepSeekToolCall `json:"tool_calls,omitempty"`
	ReasoningContent string             `json:"reasoning_content,omitempty"`
}
type deepSeekTool struct {
	Type     string           `json:"type"` // "function"
	Function deepSeekFunction `json:"function"`
}

type deepSeekFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema
}

type deepSeekRequest struct {
	Model           string            `json:"model"`
	Messages        []deepSeekMessage `json:"messages"`
	Stream          bool              `json:"stream"`
	Temperature     *float64          `json:"temperature,omitempty"`
	MaxTokens       int               `json:"max_tokens,omitempty"`
	Tools           []deepSeekTool    `json:"tools,omitempty"`
	ToolChoice      string            `json:"tool_choice,omitempty"` // "auto" when tools present
	Thinking        *deepSeekThinking `json:"thinking,omitempty"`
	ReasoningEffort string            `json:"reasoning_effort,omitempty"`
	StreamOptions   struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
}

type deepSeekThinking struct {
	Type string `json:"type"` // "enabled" | "disabled"
}

// deepSeekChunk is one SSE `data:` payload.
type deepSeekChunk struct {
	ID      string `json:"id"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens          int `json:"prompt_tokens"`
		CompletionTokens      int `json:"completion_tokens"`
		PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
		PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
		PromptTokensDetails   *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

// ---------------------------------------------------------------------------
// Message translation.
// ---------------------------------------------------------------------------

func buildDeepSeekMessages(req ModelRequest) []deepSeekMessage {
	// Mirrors upstream serializeMessages: system content is flattened text;
	// assistant messages flatten text, pass reasoning back as
	// reasoning_content, and map tool-call blocks to tool_calls; user-role
	// messages contribute their text first and each tool-result block
	// becomes its own role:"tool" wire message (empty output -> "(no output)").
	out := make([]deepSeekMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		out = append(out, deepSeekMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			out = append(out, deepSeekMessage{Role: "system", Content: flattenTextBlocks(m.Content)})
		case "assistant":
			dsm := deepSeekMessage{Role: "assistant"}
			var text, reasoning strings.Builder
			for _, b := range m.Content {
				switch b.Type {
				case "text":
					text.WriteString(b.Text)
				case "reasoning":
					reasoning.WriteString(b.Text)
				case "tool-call":
					dsm.ToolCalls = append(dsm.ToolCalls, deepSeekToolCall{
						ID:       b.ID,
						Type:     "function",
						Function: deepSeekFunctionCall{Name: b.Name, Arguments: b.Arguments},
					})
				}
			}
			// Text-less turns send "" — NEVER null; reasoning-only turns carry
			// the reasoning channel back (upstream contract).
			dsm.Content = text.String()
			if reasoning.Len() > 0 {
				dsm.ReasoningContent = reasoning.String()
			}
			out = append(out, dsm)
		case "tool":
			// Harness tool messages carry exactly one tool-result block whose
			// nested content holds the result blocks; expand text verbatim.
			for _, b := range m.Content {
				if b.Type != "tool-result" {
					continue
				}
				text := flattenTextBlocks(b.Content)
				if text == "" {
					text = "(no output)"
				}
				out = append(out, deepSeekMessage{
					Role:       "tool",
					ToolCallID: b.ToolCallID,
					Content:    text,
				})
			}
		case "user":
			dsm := deepSeekMessage{Role: "user", Content: flattenTextBlocks(m.Content)}
			out = append(out, dsm)
		}
	}
	return out
}

// flattenTextBlocks joins the text blocks of a message (upstream flattenText).
func flattenTextBlocks(blocks []session.ContentBlock) string {
	var text strings.Builder
	for _, b := range blocks {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	return text.String()
}

func buildDeepSeekRequest(req ModelRequest) deepSeekRequest {
	body := deepSeekRequest{
		Model:    req.Model,
		Messages: buildDeepSeekMessages(req),
		Stream:   true,
	}
	if req.Temperature != nil {
		body.Temperature = req.Temperature
	}
	if req.MaxTokens > 0 {
		body.MaxTokens = req.MaxTokens
	}
	for _, t := range req.Tools {
		body.Tools = append(body.Tools, deepSeekTool{
			Type: "function",
			Function: deepSeekFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	if len(body.Tools) > 0 {
		body.ToolChoice = "auto"
	}
	body.StreamOptions.IncludeUsage = true
	// Thinking-mode mapping mirrors upstream resolveThinking: session-title
	// disables thinking; "off" effort disables; low/high/max enable with the
	// effort on the wire.
	if req.Purpose == "session-title" {
		body.Thinking = &deepSeekThinking{Type: "disabled"}
	} else if req.ReasoningEffort != "" {
		switch req.ReasoningEffort {
		case "off":
			body.Thinking = &deepSeekThinking{Type: "disabled"}
		case "low", "high", "max":
			body.Thinking = &deepSeekThinking{Type: "enabled"}
			body.ReasoningEffort = req.ReasoningEffort
		}
	}
	return body
}

func (d *DeepSeekAdapter) completionsEndpoint() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return chatCompletionsURL(d.baseURL)
}

func (d *DeepSeekAdapter) streamCompletions(
	streamCtx context.Context,
	cancel context.CancelFunc,
	req ModelRequest,
	apiKey string,
	chunkChan chan<- StreamChunk,
) error {
	if req.Model == "" {
		req.Model = d.Model()
	}
	body, err := json.Marshal(buildDeepSeekRequest(req))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDeepSeekBadRequest, err)
	}

	httpReq, err := http.NewRequestWithContext(streamCtx, http.MethodPost, d.completionsEndpoint(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	// Attribution headers mirror upstream adapter.ts: a stable harness user id,
	// the session id when the call carries one, and the compaction marker.
	httpReq.Header.Set("x-deepseek-harness-user-id", "dsh-go")
	if req.SessionID != "" {
		httpReq.Header.Set("x-deepseek-harness-session-id", req.SessionID)
	}
	if req.Purpose == "compaction" {
		httpReq.Header.Set("x-deepseek-harness-compact", "1")
	}
	d.mu.Lock()
	extra := d.extra
	watchdog := d.watchdog
	httpc := d.httpc
	d.mu.Unlock()
	applyExtraHeaders(httpReq.Header, extra)

	contentType := "" // "" | "text" | "reasoning"
	toolOpened := map[int]bool{}
	openedBlocks := 0

	emit := func(c StreamChunk) {
		emitChunk(streamCtx, chunkChan, c)
	}
	closeContent := func() {
		if contentType != "" {
			emit(StreamChunk{Type: ChunkBlockEnd})
			contentType = ""
		}
	}
	setContent := func(t string) {
		if contentType == t {
			return
		}
		closeContent()
		openedBlocks++
		emit(StreamChunk{Type: ChunkBlockStart, BlockType: t})
		contentType = t
	}
	closeTools := func() {
		for idx := range toolOpened {
			if toolOpened[idx] {
				emit(StreamChunk{Type: ChunkBlockEnd})
			}
		}
		toolOpened = map[int]bool{}
	}
	var pendingUsage *session.TokenUsage
	var pendingFinish *string
	finished := false

	finish := func() {
		if finished {
			return
		}
		finished = true
		closeContent()
		closeTools()
		if pendingUsage != nil {
			emit(StreamChunk{Type: ChunkUsage, Usage: pendingUsage})
		}
		reason := "stop"
		if pendingFinish != nil {
			reason = mapFinishReason(*pendingFinish)
		}
		if reason == "stop" && openedBlocks == 0 {
			reason = "error"
		}
		emit(StreamChunk{Type: ChunkFinish, FinishReason: reason})
	}

	return consumeSSE(streamCtx, cancel, httpc, watchdog, httpReq, func(data string) (bool, error) {
		if data == "[DONE]" {
			finish()
			return true, nil
		}
		var c deepSeekChunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			return false, fmt.Errorf("%w: %v", ErrDeepSeekStream, err)
		}

		for _, choice := range c.Choices {
			delta := choice.Delta
			for _, tc := range delta.ToolCalls {
				idx := tc.Index
				if !toolOpened[idx] {
					closeContent()
					openedBlocks++
					emit(StreamChunk{
						Type:      ChunkBlockStart,
						BlockType: "tool-call",
						ID:        tc.ID,
						Name:      tc.Function.Name,
					})
					toolOpened[idx] = true
				}
				emit(StreamChunk{
					Type:           ChunkToolCallDelta,
					Index:          idx,
					ID:             tc.ID,
					Name:           tc.Function.Name,
					ArgumentsDelta: tc.Function.Arguments,
				})
			}
			if delta.ReasoningContent != "" {
				setContent("reasoning")
				emit(StreamChunk{Type: ChunkReasoningDelta, Text: delta.ReasoningContent})
			}
			if delta.Content != "" {
				setContent("text")
				emit(StreamChunk{Type: ChunkTextDelta, Text: delta.Content})
			}
			if choice.FinishReason != nil {
				pendingFinish = choice.FinishReason
			}
		}

		if c.Usage != nil {
			cacheRead := c.Usage.PromptCacheHitTokens
			if cacheRead == 0 && c.Usage.PromptTokensDetails != nil {
				cacheRead = c.Usage.PromptTokensDetails.CachedTokens
			}
			reasoning := 0
			if c.Usage.CompletionTokensDetails != nil {
				reasoning = c.Usage.CompletionTokensDetails.ReasoningTokens
			}
			pendingUsage = &session.TokenUsage{
				InputTokens:     c.Usage.PromptTokens - cacheRead,
				OutputTokens:    c.Usage.CompletionTokens,
				CacheReadTokens: cacheRead,
				ReasoningTokens: reasoning,
			}
		}
		return false, nil
	})
}

func mapFinishReason(r string) string {
	switch r {
	case "stop":
		return "stop"
	case "tool_calls":
		return "tool-calls"
	case "length":
		return "max-tokens"
	default:
		// content_filter, insufficient_system_resource, future additions.
		return "error"
	}
}
