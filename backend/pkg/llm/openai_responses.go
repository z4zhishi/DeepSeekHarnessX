package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"dsh-go/pkg/session"
)

// responsesAdapter implements LlmAdapter against the OpenAI Responses API
// (POST {base}/v1/responses SSE).
type responsesAdapter struct {
	mu      sync.Mutex
	profile ProviderProfile
	httpc   *http.Client
	baseURL string
	model   string
	timeout time.Duration
	watch   time.Duration
}

func newResponsesAdapter(p ProviderProfile) *responsesAdapter {
	p = normalizeProfile(p)
	return &responsesAdapter{
		profile: p,
		httpc:   p.HTTPClient,
		baseURL: p.BaseURL,
		model:   p.Model,
		timeout: p.Timeout,
		watch:   p.Watchdog,
	}
}

func responsesURL(baseURL string) string {
	base := trimBaseURL(baseURL)
	if pathHas(base, "/responses") {
		return base
	}
	if pathEnds(base, "/v1") {
		return base + "/responses"
	}
	return base + "/v1/responses"
}

func (a *responsesAdapter) Stream(ctx context.Context, req ModelRequest) (<-chan StreamChunk, <-chan error) {
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

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type responsesRequest struct {
	Model           string          `json:"model"`
	Input           []any           `json:"input"`
	Stream          bool            `json:"stream"`
	Instructions    string          `json:"instructions,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	MaxOutputTokens int             `json:"max_output_tokens,omitempty"`
	Tools           []responsesTool `json:"tools,omitempty"`
	Reasoning       *struct {
		Effort string `json:"effort,omitempty"`
	} `json:"reasoning,omitempty"`
}

type responsesEvent struct {
	Type        string `json:"type"`
	Delta       string `json:"delta"`
	OutputIndex int    `json:"output_index"`
	Item        *struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"item"`
	Response *struct {
		Status string `json:"status"`
		Usage  *struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			InputTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputTokensDetails *struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	} `json:"response"`
}

func buildResponsesRequest(req ModelRequest) responsesRequest {
	body := responsesRequest{
		Model:        req.Model,
		Input:        buildResponsesInput(req),
		Stream:       true,
		Instructions: req.System,
	}
	if req.Temperature != nil {
		body.Temperature = req.Temperature
	}
	// Output cap: when unset (0) the field is omitted on the wire so the
	// provider applies the model's real bound (OpenAI Responses tolerates
	// omission, unlike Anthropic Messages which requires it).
	body.MaxOutputTokens = effectiveMaxTokensFor(req.MaxTokens, false)
	for _, t := range req.Tools {
		params := t.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		body.Tools = append(body.Tools, responsesTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  params,
		})
	}
	// Reasoning mapping mirrors upstream resolveThinking: session-title omits
	// reasoning; "off" is the explicit opt-out; an omitted effort defaults ON
	// at DefaultReasoningEffort ("high"); "max" maps to the wire's top level.
	effort := req.ReasoningEffort
	if effort == "" {
		effort = DefaultReasoningEffort
	}
	if req.Purpose != "session-title" && effort != "off" {
		if effort == "max" {
			effort = "high"
		}
		body.Reasoning = &struct {
			Effort string `json:"effort,omitempty"`
		}{Effort: effort}
	}
	return body
}

func buildResponsesInput(req ModelRequest) []any {
	out := make([]any, 0, len(req.Messages)+1)
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			text := flattenTextBlocks(m.Content)
			if text != "" {
				out = append(out, map[string]any{"role": "system", "content": text})
			}
			continue
		case "assistant":
			var text strings.Builder
			for _, b := range m.Content {
				switch b.Type {
				case "text":
					text.WriteString(b.Text)
				case "tool-call":
					if text.Len() > 0 {
						out = append(out, map[string]any{"role": "assistant", "content": text.String()})
						text.Reset()
					}
					item := map[string]any{
						"type":      "function_call",
						"call_id":   b.ID,
						"name":      b.Name,
						"arguments": b.Arguments,
					}
					out = append(out, item)
				}
			}
			if text.Len() > 0 {
				out = append(out, map[string]any{"role": "assistant", "content": text.String()})
			}
			continue
		}
		// User-role messages: text rides as the user item and each
		// tool-result block expands into its own function_call_output item
		// after it (results stay verbatim in user messages).
		var text strings.Builder
		var outputs []any
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				text.WriteString(b.Text)
			case "tool-result":
				output := flattenTextBlocks(b.Content)
				if output == "" {
					output = "(no output)"
				}
				outputs = append(outputs, map[string]any{
					"type":    "function_call_output",
					"call_id": b.ToolCallID,
					"output":  output,
				})
			}
		}
		if text.Len() > 0 || len(outputs) == 0 {
			out = append(out, map[string]any{"role": "user", "content": text.String()})
		}
		out = append(out, outputs...)
	}
	return out
}

func (a *responsesAdapter) stream(
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

	body, err := json.Marshal(buildResponsesRequest(req))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDeepSeekBadRequest, err)
	}
	httpReq, err := http.NewRequestWithContext(streamCtx, http.MethodPost, responsesURL(base), bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	applyExtraHeaders(httpReq.Header, extra)

	emit := func(c StreamChunk) { emitChunk(streamCtx, chunks, c) }

	opened := map[int]string{} // output_index -> block type
	openedCount := 0
	sawTool := false
	var pendingUsage *session.TokenUsage
	var pendingFinish string
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
		opened = map[int]string{}
	}
	ensure := func(idx int, kind, id, name string) {
		if cur, ok := opened[idx]; ok {
			if cur == kind {
				return
			}
			closeIndex(idx)
		}
		openedCount++
		opened[idx] = kind
		emit(StreamChunk{Type: ChunkBlockStart, Index: idx, BlockType: kind, ID: id, Name: name})
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
		if reason == "" {
			reason = "stop"
		}
		if reason == "stop" && sawTool {
			reason = "tool-calls"
		}
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
		var ev responsesEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return false, fmt.Errorf("%w: %v", ErrDeepSeekStream, err)
		}
		switch ev.Type {
		case "response.output_item.added":
			if ev.Item == nil {
				return false, nil
			}
			switch ev.Item.Type {
			case "message":
				ensure(ev.OutputIndex, "text", "", "")
			case "reasoning":
				ensure(ev.OutputIndex, "reasoning", ev.Item.ID, "")
			case "function_call", "custom_tool_call":
				sawTool = true
				ensure(ev.OutputIndex, "tool-call", firstNonEmpty(ev.Item.CallID, ev.Item.ID), ev.Item.Name)
			}
		case "response.output_text.delta", "response.refusal.delta":
			ensure(ev.OutputIndex, "text", "", "")
			if ev.Delta != "" {
				emit(StreamChunk{Type: ChunkTextDelta, Index: ev.OutputIndex, Text: ev.Delta})
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			ensure(ev.OutputIndex, "reasoning", "", "")
			if ev.Delta != "" {
				emit(StreamChunk{Type: ChunkReasoningDelta, Index: ev.OutputIndex, Text: ev.Delta})
			}
		case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
			sawTool = true
			id, name := "", ""
			if ev.Item != nil {
				id = firstNonEmpty(ev.Item.CallID, ev.Item.ID)
				name = ev.Item.Name
			}
			ensure(ev.OutputIndex, "tool-call", id, name)
			if ev.Delta != "" {
				emit(StreamChunk{Type: ChunkToolCallDelta, Index: ev.OutputIndex, ID: id, Name: name, ArgumentsDelta: ev.Delta})
			}
		case "response.output_item.done":
			if ev.Item != nil && (ev.Item.Type == "function_call" || ev.Item.Type == "custom_tool_call") {
				sawTool = true
			}
			closeIndex(ev.OutputIndex)
		case "response.completed", "response.incomplete":
			if ev.Response != nil {
				switch ev.Response.Status {
				case "completed":
					pendingFinish = "stop"
				case "incomplete":
					pendingFinish = "max-tokens"
				case "failed":
					pendingFinish = "error"
				default:
					if ev.Response.Status != "" {
						pendingFinish = "error"
					}
				}
				if ev.Response.Usage != nil {
					cacheRead := 0
					if ev.Response.Usage.InputTokensDetails != nil {
						cacheRead = ev.Response.Usage.InputTokensDetails.CachedTokens
					}
					reasoning := 0
					if ev.Response.Usage.OutputTokensDetails != nil {
						reasoning = ev.Response.Usage.OutputTokensDetails.ReasoningTokens
					}
					pendingUsage = &session.TokenUsage{
						InputTokens:     ev.Response.Usage.InputTokens - cacheRead,
						OutputTokens:    ev.Response.Usage.OutputTokens,
						CacheReadTokens: cacheRead,
						ReasoningTokens: reasoning,
					}
				}
			} else if ev.Type == "response.completed" {
				pendingFinish = "stop"
			}
			finish()
			return true, nil
		case "response.failed", "error":
			pendingFinish = "error"
			finish()
			return true, nil
		}
		return false, nil
	})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
