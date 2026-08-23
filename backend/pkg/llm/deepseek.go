package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"dsh-go/pkg/session"
)

const (
	// DefaultDeepSeekBaseURL is the DeepSeek OpenAI-compatible chat origin.
	DefaultDeepSeekBaseURL = "https://api.deepseek.com"
	// DefaultDeepSeekModel is the default chat model advertised by the CLI.
	DefaultDeepSeekModel = "deepseek-chat"
	// deepSeekChatPath is the streaming Chat Completions route.
	deepSeekChatPath = "/chat/completions"

	// defaultDeepSeekTimeout bounds the whole request+stream lifecycle.
	defaultDeepSeekTimeout = 300 * time.Second
	// defaultDeepSeekWatchdog is the max silence between SSE bytes before abort.
	defaultDeepSeekWatchdog = 60 * time.Second
)

// Typed error sentinels so callers can distinguish retryable vs fatal failures.
var (
	// ErrDeepSeekMissingCredential mirrors upstream adapter.ts: without an API
	// key the adapter stays registered (routes browsable) but every stream call
	// fails with LlmError('MISSING_CREDENTIAL') rather than a silent mock reply.
	ErrDeepSeekMissingCredential = errors.New("deepseek: no API key configured, set DEEPSEEK_API_KEY and retry (MISSING_CREDENTIAL)")
	ErrDeepSeekAuth              = errors.New("deepseek: authentication failed (check DEEPSEEK_API_KEY)")
	ErrDeepSeekQuota             = errors.New("deepseek: quota/balance exhausted")
	ErrDeepSeekRateLimit         = errors.New("deepseek: rate limited")
	ErrDeepSeekContext           = errors.New("deepseek: context window exceeded")
	ErrDeepSeekBadRequest        = errors.New("deepseek: malformed request")
	ErrDeepSeekServer            = errors.New("deepseek: upstream server error")
	ErrDeepSeekStream            = errors.New("deepseek: malformed SSE stream")
	ErrDeepSeekWatchdog          = errors.New("deepseek: stream idle watchdog fired")
)

// DeepSeekConfig wires up the real deepseek-chat streaming adapter.
type DeepSeekConfig struct {
	APIKey     string
	BaseURL    string        // defaults to DefaultDeepSeekBaseURL
	Model      string        // defaults to DefaultDeepSeekModel
	Timeout    time.Duration // whole request+stream deadline; <=0 -> defaultDeepSeekTimeout
	Watchdog   time.Duration // max silence between SSE bytes; <=0 -> defaultDeepSeekWatchdog
	HTTPClient *http.Client  // nil -> http.DefaultClient
}

// DeepSeekAdapter implements LlmAdapter against the DeepSeek Chat Completions API.
type DeepSeekAdapter struct {
	cfg      DeepSeekConfig
	httpc    *http.Client
	baseURL  string
	model    string
	timeout  time.Duration
	watchdog time.Duration
}

// NewDeepSeekAdapter returns a configured adapter. Construction is offline: no
// network I/O happens until Stream is called.
func NewDeepSeekAdapter(cfg DeepSeekConfig) *DeepSeekAdapter {
	a := &DeepSeekAdapter{cfg: cfg, httpc: cfg.HTTPClient}
	if a.httpc == nil {
		a.httpc = http.DefaultClient
	}
	a.baseURL = strings.TrimRight(cfg.BaseURL, "/")
	if a.baseURL == "" {
		a.baseURL = DefaultDeepSeekBaseURL
	}
	a.model = cfg.Model
	if a.model == "" {
		a.model = DefaultDeepSeekModel
	}
	a.timeout = cfg.Timeout
	if a.timeout <= 0 {
		a.timeout = defaultDeepSeekTimeout
	}
	a.watchdog = cfg.Watchdog
	if a.watchdog <= 0 {
		a.watchdog = defaultDeepSeekWatchdog
	}
	return a
}

// Stream implements LlmAdapter. It returns a chunk channel and an error channel;
// the error channel carries exactly one fatal error (or none on clean EOF).
func (d *DeepSeekAdapter) Stream(ctx context.Context, req ModelRequest) (<-chan StreamChunk, <-chan error) {
	chunkChan := make(chan StreamChunk, 64)
	errChan := make(chan error, 1)

	// Upstream adapter.ts returns MISSING_CREDENTIAL at stream call time when no
	// key is configured (loading/catalog stay unaffected). Surface the same
	// error here instead of proceeding to an upstream 401.
	if d.cfg.APIKey == "" {
		errChan <- ErrDeepSeekMissingCredential
		close(chunkChan)
		close(errChan)
		return chunkChan, errChan
	}

	streamCtx, cancel := context.WithTimeout(ctx, d.timeout)
	go func() {
		defer close(chunkChan)
		defer close(errChan)
		defer cancel()
		d.stream(streamCtx, cancel, req, chunkChan, errChan)
	}()

	return chunkChan, errChan
}

// ---------------------------------------------------------------------------
// Wire request / response types (DeepSeek OpenAI-compatible JSON schema).
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

func (d *DeepSeekAdapter) endpoint() string {
	return d.baseURL + deepSeekChatPath
}

// DeepSeekProviderError is a typed provider failure carrying the normalized
// harness code, the provider's message, and the optional retry-after delay and
// request id the upstream LlmError attaches (adapter.ts:622-657). The stable
// code is what retry policy routes on.
type DeepSeekProviderError struct {
	Code               string // "AUTH" | "INVALID_REQUEST" | "QUOTA" | "RATE_LIMIT" | "CONTEXT_WINDOW_EXCEEDED" | "SERVER" | "HTTP_<status>"
	Message            string // provider message when JSON-parsable, else "DeepSeek API error (HTTP n)"
	Status             int
	ProviderRetryAfter time.Duration // 0 when absent/invalid
	RequestID          string        // "" when absent
}

func (e *DeepSeekProviderError) Error() string {
	return fmt.Sprintf("deepseek: %s (%s)", e.Message, e.Code)
}

// wireError is the DeepSeek error body shape (`WireError['error']`).
type wireError struct {
	Code    string `json:"code,omitempty"`
	Type    string `json:"type,omitempty"`
	Message string `json:"message,omitempty"`
}

// isContextWindowExceeded mirrors upstream error.ts isContextWindowExceededError:
// a conservative classifier over the joined code/type/message detail.
func isContextWindowExceeded(detail string) bool {
	if matchDetail(detail, `(?:^|[^a-z0-9])context[\s_-](?:length|window)[\s_-](?:exceed(?:ed|s)?|overflow(?:ed)?|limit[\s_-]exceeded)(?:$|[^a-z0-9])`) {
		return true
	}
	if matchDetail(detail, `\b(?:maximum|max)(?:\s+(?:allowed|supported))?\s+context\s+(?:length|window)\b`) {
		return true
	}
	if matchDetail(detail, `\b(?:request|prompt|input|messages?)\s+(?:is\s+|are\s+)?too\s+(?:large|long)\s+for\s+(?:(?:this|the)\s+)?(?:model(?:'s)?\s+)?context(?:\s+window)?\b`) {
		return true
	}
	if matchDetail(detail, `\b(?:input|prompt|request)\s+(?:is\s+)?too\s+(?:long|large)\s+for\s+(?:this|the)\s+model\b`) {
		return true
	}
	return matchDetail(detail, `\b(?:input|prompt|request|messages?)\b.{0,40}\b(?:exceed(?:s|ed)?|overflows?|is\s+larger\s+than)\b.{0,40}\b(?:the\s+)?(?:model(?:'s)?\s+)?context(?:\s+(?:length|window))?\b`)
}

// isQuotaExceededError mirrors the upstream classifier for exhausted account
// quota/balance/credits/budget (terminal, not transient request-rate limits).
func isQuotaExceeded(detail string) bool {
	patterns := []string{
		`\binsufficient[\s_-]+(?:quota|balance|credits?)\b`,
		`\b(?:quota|usage[\s_-]+limit)[\s_-]+(?:exceeded|exhausted|reached)\b`,
		`\bexceed(?:ed|s)?[\s_-]+(?:(?:your|the)[\s_-]+)?(?:current[\s_-]+)?quota\b`,
		`\b(?:balance|credits?)[\s_-]+(?:exhausted|depleted)\b`,
		`\bout[\s_-]+of[\s_-]+(?:credits?|budget)\b`,
	}
	for _, p := range patterns {
		if matchDetail(detail, p) {
			return true
		}
	}
	return false
}

func matchDetail(detail, pattern string) bool {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(detail)
}

// providerRetryAfterMs parses the `retry-after` header: a pure number is
// seconds*1000, an HTTP-date is its offset from now; invalid/<=0 yields 0.
func providerRetryAfterMs(value string) time.Duration {
	if value == "" {
		return 0
	}
	if strings.TrimSpace(value) != "" {
		// Pure numeric: seconds.
		if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
			if n > 0 {
				return time.Duration(n) * time.Second
			}
			return 0
		}
		// HTTP-date: offset from now.
		if t, err := http.ParseTime(strings.TrimSpace(value)); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
		}
	}
	return 0
}

// mapDeepSeekStatus converts an HTTP status plus the parsed provider error
// body into a structured DeepSeekProviderError, exactly mirroring upstream
// httpErrorCode + requestId + providerRetryAfterMs (adapter.ts:333-345).
func mapDeepSeekStatus(code int, providerError *wireError, retryAfter string, requestID string) *DeepSeekProviderError {
	if code >= 200 && code < 300 {
		return nil
	}
	message := fmt.Sprintf("DeepSeek API error (HTTP %d)", code)
	if providerError != nil && providerError.Message != "" {
		message = providerError.Message
	}
	detail := ""
	if providerError != nil {
		parts := []string{providerError.Code, providerError.Type, providerError.Message}
		detail = strings.Join(nonEmpty(parts), " ")
	}
	httpCode := "HTTP_" + strconv.Itoa(code)
	switch {
	case code == 401 || code == 403:
		httpCode = "AUTH"
	case code == 413:
		httpCode = "INVALID_REQUEST"
	case isQuotaExceeded(detail):
		httpCode = "QUOTA"
	case code == 429:
		httpCode = "RATE_LIMIT"
	case code == 400:
		if isContextWindowExceeded(detail) {
			httpCode = "CONTEXT_WINDOW_EXCEEDED"
		} else {
			httpCode = "INVALID_REQUEST"
		}
	case code >= 500:
		httpCode = "SERVER"
	}
	return &DeepSeekProviderError{
		Code:               httpCode,
		Message:            message,
		Status:             code,
		ProviderRetryAfter: providerRetryAfterMs(retryAfter),
		RequestID:          requestID,
	}
}

func nonEmpty(parts []string) []string {
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func isSuccessStatus(code int) bool { return code >= 200 && code < 300 }

// parseWireError parses the provider error body; returns nil when the JSON is
// malformed or has no error object (the HTTP status stays authoritative).
func parseWireError(raw []byte, err error) *wireError {
	if err != nil || len(raw) == 0 {
		return nil
	}
	var body struct {
		Error wireError `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil
	}
	if body.Error.Message == "" && body.Error.Code == "" && body.Error.Type == "" {
		return nil
	}
	return &body.Error
}

// requestIDOf extracts `x-request-id` ?? `x-deepseek-request-id` (upstream
// requestId helper).
func requestIDOf(resp *http.Response) string {
	if v := resp.Header.Get("X-Request-Id"); v != "" {
		return v
	}
	return resp.Header.Get("X-DeepSeek-Request-Id")
}

// ---------------------------------------------------------------------------
// Streaming loop.
// ---------------------------------------------------------------------------

func (d *DeepSeekAdapter) stream(
	streamCtx context.Context,
	cancel context.CancelFunc,
	req ModelRequest,
	chunkChan chan<- StreamChunk,
	errChan chan<- error,
) {
	body, err := json.Marshal(buildDeepSeekRequest(req))
	if err != nil {
		errChan <- fmt.Errorf("%w: %v", ErrDeepSeekBadRequest, err)
		return
	}

	httpReq, err := http.NewRequestWithContext(streamCtx, http.MethodPost, d.endpoint(), bytes.NewReader(body))
	if err != nil {
		errChan <- err
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+d.cfg.APIKey)
	// Attribution headers mirror upstream adapter.ts: a stable harness user id,
	// the session id when the call carries one, and the compaction marker.
	httpReq.Header.Set("x-deepseek-harness-user-id", "dsh-go")
	if req.SessionID != "" {
		httpReq.Header.Set("x-deepseek-harness-session-id", req.SessionID)
	}
	if req.Purpose == "compaction" {
		httpReq.Header.Set("x-deepseek-harness-compact", "1")
	}

	resp, err := d.httpc.Do(httpReq)
	if err != nil {
		if streamCtx.Err() != nil {
			errChan <- streamCtx.Err() // parent canceled
		} else {
			errChan <- err
		}
		return
	}
	defer resp.Body.Close()

	if !isSuccessStatus(resp.StatusCode) {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		merr := mapDeepSeekStatus(
			resp.StatusCode,
			parseWireError(raw, nil),
			resp.Header.Get("Retry-After"),
			requestIDOf(resp),
		)
		errChan <- merr
		return
	}
	// Reader goroutine: decodes lines, watches for silence.
	lineCh := make(chan string, 64)
	readErr := make(chan error, 1)
	go readSSELines(streamCtx, resp.Body, lineCh, readErr)

	watchdog := time.NewTimer(d.watchdog)
	defer watchdog.Stop()

	// Incremental block state machine.
	contentType := "" // "" | "text" | "reasoning"
	toolOpened := map[int]bool{}
	openedBlocks := 0 // opened block count; drives EMPTY_RESPONSE detection

	emit := func(c StreamChunk) {
		select {
		case chunkChan <- c:
		case <-streamCtx.Done():
		}
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
	// Upstream translate.ts defers usage and finish to the `[DONE]` sentinel:
	// finish reason and the LATEST usage are flushed there, covering both
	// finish-attached and trailing usage-only shapes while guaranteeing no
	// chunk follows `finish`.
	var pendingUsage *session.TokenUsage
	var pendingFinish *string

	finish := func() {
		closeContent()
		closeTools()
		if pendingUsage != nil {
			emit(StreamChunk{Type: ChunkUsage, Usage: pendingUsage})
		}
		reason := "stop"
		if pendingFinish != nil {
			reason = mapFinishReason(*pendingFinish)
		}
		// Degenerate completion: a stop (or absent) finish with no opened
		// blocks is an EMPTY_RESPONSE error finish, never a successful empty
		// message (upstream translate.ts).
		if reason == "stop" && openedBlocks == 0 {
			reason = "error"
		}
		emit(StreamChunk{Type: ChunkFinish, FinishReason: reason})
	}

	streamDone := false
	for !streamDone {
		select {
		case <-streamCtx.Done():
			return
		case err := <-readErr:
			errChan <- err
			return
		case <-watchdog.C:
			cancel()
			errChan <- ErrDeepSeekWatchdog
			return
		case line, ok := <-lineCh:
			if !ok {
				// EOF before [DONE]: the model call cannot be trusted.
				errChan <- ErrDeepSeekStream
				return
			}
			watchdog.Reset(d.watchdog)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" {
				continue
			}
			if data == "[DONE]" {
				finish()
				streamDone = true
				continue
			}
			var c deepSeekChunk
			if err := json.Unmarshal([]byte(data), &c); err != nil {
				errChan <- fmt.Errorf("%w: %v", ErrDeepSeekStream, err)
				cancel()
				return
			}

			// A usage-only chunk carries an empty choices array; iterate all
			// choices (upstream `for (const choice of chunk.choices ?? [])`).
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

			// Usage may arrive attached to the finish chunk or as a trailing
			// usage-only chunk — keep the latest.
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

		}
	}
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

// readSSELines reads lines from the SSE body and pushes them onto lineCh.
// It blocks until the stream is exhausted or streamCtx is canceled; a read
// failure is reported on readErr (nil is not sent on clean EOF).
func readSSELines(ctx context.Context, r io.Reader, lineCh chan<- string, readErr chan<- error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		select {
		case lineCh <- sc.Text():
		case <-ctx.Done():
			return
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		select {
		case readErr <- err:
		case <-ctx.Done():
		}
		return
	}
	// Clean EOF: signal the end of the byte stream so the caller can detect a
	// missing [DONE] sentinel (truncated response). Close, not nil-send: a
	// receive on a closed channel is the canonical EOF marker here.
	close(lineCh)
}
