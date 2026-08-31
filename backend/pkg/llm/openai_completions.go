// openai-completions 线格式构造块：dsh-internal/v1（LlmAdapter）到 OpenAI
// Chat Completions SSE 的翻译器（中转）。协议插件化后本文件只负责这一条线
// 协议；注册进默认 ProtocolRegistry（internal_protocol.go）。DeepSeek 是走
// 本线协议的默认 provider profile（无自有线协议），语义沿用
// docs/deepseek-llm-contract.md。
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
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
		base = DefaultCompletionsBaseURL
	}
	if pathHas(base, "/chat/completions") {
		return base
	}
	if pathEnds(base, "/v1") {
		return base + "/chat/completions"
	}
	if urlHost(base) == "api.deepseek.com" {
		return base + DefaultCompletionsChatPath
	}
	return base + "/v1/chat/completions"
}

func newCompletionsAdapter(p ProviderProfile) *CompletionsAdapter {
	p = normalizeProfile(p)
	if p.BaseURL == "" {
		p.BaseURL = DefaultCompletionsBaseURL
	}
	if p.Model == "" {
		p.Model = DefaultCompletionsModel
	}
	return &CompletionsAdapter{
		cfg: CompletionsConfig{
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
func (d *CompletionsAdapter) Stream(ctx context.Context, req ModelRequest) (<-chan StreamChunk, <-chan error) {
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
	Role string `json:"role"`
	// Content is "" for text-less turns — NEVER null. It stays a pointer so
	// MarshalJSON below can distinguish "omit the key entirely" (never used
	// for assistant) from the explicit empty string.
	Content          *string            `json:"-"`
	ToolCallID       string             `json:"tool_call_id,omitempty"`
	ToolCalls        []deepSeekToolCall `json:"tool_calls,omitempty"`
	ReasoningContent string             `json:"reasoning_content,omitempty"`
}

// MarshalJSON mirrors upstream serialize.ts:213-231 exactly:
//
//	system/user/tool → {"role":…,"content":"<text>"}          (content always present)
//	assistant        → {"role":"assistant","content":"<text or \"\">",
//	                    …reasoning?{"reasoning_content":…},
//	                    …toolCalls?{"tool_calls":[…]}}
//
// The assistant content key is ALWAYS emitted (empty string for pure
// tool-call turns). A missing key decodes as null on the wire, which some
// gateways reject with a 400 — and since the message sits durably in the
// session log, that bricks every later turn of the session.
func (m deepSeekMessage) MarshalJSON() ([]byte, error) {
	type alias struct {
		Role             string             `json:"role"`
		Content          string             `json:"content"`
		ToolCallID       string             `json:"tool_call_id,omitempty"`
		ToolCalls        []deepSeekToolCall `json:"tool_calls,omitempty"`
		ReasoningContent string             `json:"reasoning_content,omitempty"`
	}
	a := alias{
		Role:             m.Role,
		Content:          derefContent(m.Content),
		ToolCallID:       m.ToolCallID,
		ToolCalls:        m.ToolCalls,
		ReasoningContent: m.ReasoningContent,
	}
	return json.Marshal(a)
}

func derefContent(c *string) string {
	if c == nil {
		return ""
	}
	return *c
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
	// reasoning_content, and map tool-call blocks to tool_calls. Tool-result
	// blocks ride in verbatim projected messages (the harness keeps them in
	// user-role messages, upstream surface.ts passes them through as-is);
	// every tool-result block expands into its own role:"tool" wire message
	// (empty output -> "(no output)").
	out := make([]deepSeekMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		out = append(out, deepSeekMessage{Role: "system", Content: strPtr(req.System)})
	}
	for _, m := range req.Messages {
		assertTextOnlyBlocks(m.Role, m.Content)
		switch m.Role {
		case "system":
			out = append(out, deepSeekMessage{Role: "system", Content: strPtr(flattenTextBlocks(m.Content))})
			continue
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
			dsm.Content = strPtr(text.String())
			if reasoning.Len() > 0 {
				dsm.ReasoningContent = reasoning.String()
			}
			out = append(out, dsm)
			continue
		}
		// User-role messages contribute their text first and each tool-result
		// block becomes its own role:"tool" wire message after the text
		// (upstream serializeMessages ordering).
		var text strings.Builder
		var results []deepSeekMessage
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				text.WriteString(b.Text)
			case "tool-result":
				result := flattenTextBlocks(b.Content)
				if result == "" {
					result = "(no output)"
				}
				results = append(results, deepSeekMessage{
					Role:       "tool",
					ToolCallID: b.ToolCallID,
					Content:    strPtr(result),
				})
			}
		}
		if text.Len() > 0 || len(results) == 0 {
			out = append(out, deepSeekMessage{Role: "user", Content: strPtr(text.String())})
		}
		out = append(out, results...)
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

// strPtr hands a value to deepSeekMessage.Content (pointer so MarshalJSON can
// always emit the content key — "" included, never null).
func strPtr(s string) *string {
	return &s
}

// assertTextOnlyBlocks rejects image content before any text-flattening path
// can silently erase it (upstream serialize.ts assertTextOnly). The chat
// completions adapter is text-only until multimodal support lands; an image
// block must fail the request loudly instead of disappearing into "".
func assertTextOnlyBlocks(role string, blocks []session.ContentBlock) {
	for _, b := range blocks {
		if b.Type == "image" || containsImageBlock(b.Content) {
			panic(fmt.Sprintf(
				"deepseek chat-completions: %s message carries unsupported image content (UNSUPPORTED_CONTENT)",
				role,
			))
		}
	}
}

// containsImageBlock walks nested tool-result blocks for image leaves.
func containsImageBlock(blocks []session.ContentBlock) bool {
	for _, b := range blocks {
		if b.Type == "image" || containsImageBlock(b.Content) {
			return true
		}
	}
	return false
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
	// Output cap: OpenAI-family chat completions OMITS max_tokens when unset
	// (omitempty + 0 from effectiveMaxTokensFor(false)), letting the provider
	// apply the model's real bound. Only Anthropic, where max_tokens is
	// REQUIRED, pulls the DefaultMaxTokens fallback.
	body.MaxTokens = effectiveMaxTokensFor(req.MaxTokens, false)
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
	// Thinking-mode mapping mirrors upstream resolveThinking (serialize.ts:80-96,
	// index.ts:103-104): session-title disables thinking; an omitted effort
	// resolves to DefaultReasoningEffort ("high") so thinking is ON by default;
	// "off" is the explicit opt-out; low/high/max enable with the effort on the
	// wire.
	if req.Purpose == "session-title" {
		body.Thinking = &deepSeekThinking{Type: "disabled"}
	} else {
		effort := req.ReasoningEffort
		if effort == "" {
			effort = DefaultReasoningEffort
		}
		switch effort {
		case "off":
			body.Thinking = &deepSeekThinking{Type: "disabled"}
		case "low", "high", "max":
			body.Thinking = &deepSeekThinking{Type: "enabled"}
			body.ReasoningEffort = effort
		}
	}
	return body
}

func (d *CompletionsAdapter) completionsEndpoint() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return chatCompletionsURL(d.baseURL)
}

func (d *CompletionsAdapter) streamCompletions(
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

	// One stateful harness block per content, reasoning, or tool-call index,
	// mirroring upstream translate.ts:90-104/152-170. Parallel tool calls
	// stream INTERLEAVED deltas keyed by their wire index, so several blocks
	// stay open at once — closing one early would split its siblings' deltas
	// across blocks. Every block-end is therefore deferred to [DONE] and
	// emitted in first-open order, each carrying its fully assembled block.
	textOpen := false
	reasoningOpen := false
	type openTool struct {
		id     string // stable harness BlockID ("tool-<wire index>")
		callID string
		name   string
		args   strings.Builder
	}
	textBuf := strings.Builder{}
	reasoningBuf := strings.Builder{}
	openTools := map[int]*openTool{}
	var order []string // BlockIDs in first-open order — commit order

	emit := func(c StreamChunk) {
		emitChunk(streamCtx, chunkChan, c)
	}
	openBlock := func(id, kind string) {
		order = append(order, id)
		emit(StreamChunk{
			Type:      ChunkBlockStart,
			Index:     len(order) - 1,
			BlockID:   id,
			BlockType: kind,
		})
	}
	// blockIndex reports a block's position in the open order (-1 unknown).
	blockIndex := func(id string) int {
		for i, v := range order {
			if v == id {
				return i
			}
		}
		return -1
	}

	var pendingUsage *session.TokenUsage
	var pendingFinish *string
	finished := false

	finish := func() {
		if finished {
			return
		}
		finished = true
		// Deferred closes, strictly in first-open order.
		for i, id := range order {
			var blk *session.ContentBlock
			switch {
			case id == "text":
				blk = &session.ContentBlock{Type: "text", Text: textBuf.String()}
			case id == "reasoning":
				blk = &session.ContentBlock{Type: "reasoning", Text: reasoningBuf.String()}
			default: // "tool-<n>"
				n, _ := strconv.Atoi(strings.TrimPrefix(id, "tool-"))
				if t := openTools[n]; t != nil {
					blk = &session.ContentBlock{Type: "tool-call", ID: t.callID, Name: t.name, Arguments: t.args.String()}
				}
			}
			emit(StreamChunk{Type: ChunkBlockEnd, Index: i, BlockID: id, Block: blk})
		}
		if pendingUsage != nil {
			emit(StreamChunk{Type: ChunkUsage, Usage: pendingUsage})
		}
		reason := "stop"
		if pendingFinish != nil {
			reason = mapFinishReason(*pendingFinish)
		}
		if reason == "stop" && len(order) == 0 {
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
			// Reasoning before text: thinking mode interleaves it that way,
			// matching upstream ordering.
			if delta.ReasoningContent != "" {
				if !reasoningOpen {
					reasoningOpen = true
					openBlock("reasoning", "reasoning")
				}
				reasoningBuf.WriteString(delta.ReasoningContent)
				emit(StreamChunk{
					Type:    ChunkReasoningDelta,
					Index:   blockIndex("reasoning"),
					BlockID: "reasoning",
					Text:    delta.ReasoningContent,
				})
			}
			if delta.Content != "" {
				if !textOpen {
					textOpen = true
					openBlock("text", "text")
				}
				textBuf.WriteString(delta.Content)
				emit(StreamChunk{
					Type:    ChunkTextDelta,
					Index:   blockIndex("text"),
					BlockID: "text",
					Text:    delta.Content,
				})
			}
			for _, tc := range delta.ToolCalls {
				idx := tc.Index
				t := openTools[idx]
				if t == nil {
					t = &openTool{id: fmt.Sprintf("tool-%d", idx), callID: tc.ID, name: tc.Function.Name}
					openTools[idx] = t
					openBlock(t.id, "tool-call")
				} else {
					if tc.ID != "" && t.callID == "" {
						t.callID = tc.ID
					}
					if tc.Function.Name != "" && t.name == "" {
						t.name = tc.Function.Name
					}
				}
				t.args.WriteString(tc.Function.Arguments)
				emit(StreamChunk{
					Type:           ChunkToolCallDelta,
					Index:          blockIndex(t.id),
					BlockID:        t.id,
					ID:             t.callID,
					Name:           t.name,
					ArgumentsDelta: tc.Function.Arguments,
				})
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
