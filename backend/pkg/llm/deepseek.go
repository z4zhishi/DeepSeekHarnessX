package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultDeepSeekBaseURL is the DeepSeek OpenAI-compatible chat origin.
	DefaultDeepSeekBaseURL = "https://api.deepseek.com"
	// DefaultDeepSeekModel is the default chat model advertised by the CLI.
	DefaultDeepSeekModel = "deepseek-v4-flash"
	// deepSeekChatPath is the streaming Chat Completions route DeepSeek serves
	// at the origin (NOT /v1/chat/completions).
	deepSeekChatPath = "/chat/completions"

	// defaultDeepSeekTimeout bounds the whole request+stream lifecycle.
	defaultDeepSeekTimeout = 300 * time.Second
	// defaultDeepSeekWatchdog is the max silence between SSE bytes before abort.
	defaultDeepSeekWatchdog = 60 * time.Second
)

// ModelInfo describes one selectable model in the llm.models catalog served to
// the frontend model picker. It mirrors the upstream DEFAULT_MODELS shape.
type ModelInfo struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	ContextWindow int      `json:"contextWindow"`
	Modalities    []string `json:"modalities"`
}

// DefaultModels is the fallback catalog for the llm.models picker. Live
// provider listings are merged in front of these entries; unknown selected
// ids are kept as-is (ContextLimitForModel still uses the first window).
var DefaultModels = []ModelInfo{
	{ID: "deepseek-v4-flash", Name: "DeepSeek-V4-Flash", ContextWindow: 131072, Modalities: []string{"text"}},
	{ID: "deepseek-v4-pro", Name: "DeepSeek-V4-Pro", ContextWindow: 131072, Modalities: []string{"text", "reasoning"}},
	{ID: "deepseek-chat", Name: "DeepSeek Chat", ContextWindow: 131072, Modalities: []string{"text"}},
	{ID: "deepseek-reasoner", Name: "DeepSeek Reasoner", ContextWindow: 131072, Modalities: []string{"text", "reasoning"}},
}

// ContextLimitForModel returns the context window (tokens) of a catalog model
// for the token meter's pressure ratio. Unknown ids fall back to the default
// chat model's window so a stale selection never zeroes pressure reporting.
func ContextLimitForModel(model string) int {
	if model == "" {
		return DefaultModels[0].ContextWindow
	}
	for _, m := range DefaultModels {
		if m.ID == model {
			return m.ContextWindow
		}
	}
	return DefaultModels[0].ContextWindow
}

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
	// APIKeyResolver, when set, is consulted at Stream time when APIKey is
	// empty. It lets the host resolve the key through the credential seam
	// (default reference DEEPSEEK_API_KEY) so a changed credential reaches the
	// next operation without a restart. It returns the resolved value and an
	// error; an empty resolved value still reports MISSING_CREDENTIAL.
	APIKeyResolver func() (string, error)
}

// DeepSeekAdapter implements LlmAdapter against the OpenAI Chat Completions
// API. DeepSeek is a provider that speaks openai-completions (origin
// https://api.deepseek.com, path /chat/completions).
type DeepSeekAdapter struct {
	mu       sync.Mutex
	cfg      DeepSeekConfig
	httpc    *http.Client
	baseURL  string
	model    string
	timeout  time.Duration
	watchdog time.Duration
	extra    map[string]string
}

// NewDeepSeekAdapter returns a configured adapter. Construction is offline: no
// network I/O happens until Stream is called. It is the openai-completions
// convenience constructor (ProtocolOpenAICompletions + DefaultDeepSeekBaseURL).
func NewDeepSeekAdapter(cfg DeepSeekConfig) *DeepSeekAdapter {
	a, err := NewProtocolAdapter(profileFromDeepSeek(cfg))
	if err == nil {
		if d, ok := a.(*DeepSeekAdapter); ok {
			return d
		}
	}
	return newCompletionsAdapter(profileFromDeepSeek(cfg))
}

func profileFromDeepSeek(cfg DeepSeekConfig) ProviderProfile {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = DefaultDeepSeekBaseURL
	}
	model := cfg.Model
	if model == "" {
		model = DefaultDeepSeekModel
	}
	return ProviderProfile{
		Protocol:       ProtocolOpenAICompletions,
		BaseURL:        base,
		Model:          model,
		APIKey:         cfg.APIKey,
		APIKeyResolver: cfg.APIKeyResolver,
		HTTPClient:     cfg.HTTPClient,
		Timeout:        cfg.Timeout,
		Watchdog:       cfg.Watchdog,
	}
}

// SetEndpoint swaps the upstream base URL at runtime (thread-safe). A trailing
// slash is trimmed. It takes effect on the next Stream call.
func (d *DeepSeekAdapter) SetEndpoint(baseURL string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if baseURL == "" {
		return
	}
	d.baseURL = strings.TrimRight(baseURL, "/")
}

// SetModel swaps the default model id at runtime (thread-safe). It takes
// effect on the next Stream call.
func (d *DeepSeekAdapter) SetModel(model string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if model == "" {
		return
	}
	d.model = model
}

// Endpoint returns the current chat-completions endpoint (thread-safe).
func (d *DeepSeekAdapter) Endpoint() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return chatCompletionsURL(d.baseURL)
}

// Model returns the current default model id (thread-safe).
func (d *DeepSeekAdapter) Model() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.model
}

// FetchModels lists the models a base URL advertises (OpenAI-compatible
// GET {baseURL}/models). The key is resolved through the adapter's resolver
// (or cfg.APIKey) so a configured credential is honored. On any failure it
// returns the error; callers fall back to the static DefaultModels catalog.
func (d *DeepSeekAdapter) FetchModels(ctx context.Context) ([]ModelInfo, error) {
	key := d.cfg.APIKey
	if key == "" && d.cfg.APIKeyResolver != nil {
		if rk, rerr := d.cfg.APIKeyResolver(); rerr != nil {
			return nil, rerr
		} else {
			key = rk
		}
	}
	d.mu.Lock()
	base := d.baseURL
	httpc := d.httpc
	d.mu.Unlock()
	return FetchModelsFor(ctx, ProtocolOpenAICompletions, base, key, httpc)
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

// Unwrap exposes the matching sentinel so errors.Is(err, ErrDeepSeekAuth)
// (and RateLimit / BadRequest / Server / …) still classifies HTTP failures.
func (e *DeepSeekProviderError) Unwrap() error {
	if e == nil {
		return nil
	}
	switch e.Code {
	case "AUTH":
		return ErrDeepSeekAuth
	case "QUOTA":
		return ErrDeepSeekQuota
	case "RATE_LIMIT":
		return ErrDeepSeekRateLimit
	case "CONTEXT_WINDOW_EXCEEDED":
		return ErrDeepSeekContext
	case "INVALID_REQUEST":
		return ErrDeepSeekBadRequest
	case "SERVER":
		return ErrDeepSeekServer
	default:
		return nil
	}
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
