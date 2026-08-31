// pkg/llm 架构（协议插件化）：
//
//   - 本包对外只暴露内部协议 dsh-internal/v1（internal_protocol.go）——内部协议即
//     LlmAdapter 接口本身：ModelRequest 进，StreamChunk 流 + error 通道出。
//     宿主 / Router / agent / 插件 LlmCapability 之间全部以内部协议对话。
//   - 线协议（openai-completions / openai-responses / anthropic-messages）是
//     注册进 ProtocolRegistry 的线格式构造块，是内部协议到外部线格式的翻译器
//     （中转）。三条线协议的播种即默认注册（见 internal_protocol.go）。
//   - DeepSeek 不是协议（它没有自有线格式，还是老三样之一）：它是
//     openai-completions 之上的默认 provider profile —— api.deepseek.com /
//     deepseek-v4-flash / DEEPSEEK_API_KEY 凭据引用，保留为网关种子 profile
//     的取值默认（见 profileFromDeepSeek）。
//
// 本文件持有 openai-completions 线协议的配置构造块、类型化错误与 HTTP 状态
// 映射（语义沿用 docs/deepseek-llm-contract.md）。
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
	// DefaultCompletionsBaseURL is the default openai-completions origin — the
	// seeded "deepseek" provider profile (a provider profile, NOT a protocol)
	// is what speaks here.
	DefaultCompletionsBaseURL = "https://api.deepseek.com"
	// DefaultCompletionsModel is the default chat model of the same
	// seeded provider profile (advertised by the CLI).
	DefaultCompletionsModel = "deepseek-v4-flash"
	// DefaultCompletionsChatPath is the streaming Chat Completions route the
	// api.deepseek.com origin serves at the host root (NOT /v1/chat/completions).
	DefaultCompletionsChatPath = "/chat/completions"

	// defaultTimeout is the connect+headers deadline applied by
	// startStream (upstream AbortSignal.timeout(300_000)); it does NOT bound
	// the streaming body — a healthy stream may outlive it (deepseek-llm-contract.md:93).
	defaultTimeout = 300 * time.Second
	// defaultWatchdog is the max silence between SSE bytes before
	// abort. 300s mirrors defaultTimeout: a reasoning-heavy generation
	// can legitimately stay quiet for minutes between visible bytes, and the
	// watchdog is an idle bound — not a whole-call deadline (a healthy stream
	// may outlive it as long as bytes keep arriving).
	defaultWatchdog = 300 * time.Second
)

// Deprecated aliases for one release so out-of-tree callers (and pkg/gateway,
// whose own reference migration is Track 2's job) keep compiling unchanged.
// Do not write new code against these names.
const (
	// Deprecated: use DefaultCompletionsBaseURL. DeepSeek 是 provider profile
	// 而非协议，基础 URL 常量本就属于 openai-completions 线协议构造块。
	DefaultDeepSeekBaseURL = DefaultCompletionsBaseURL
	// Deprecated: use DefaultCompletionsModel.
	DefaultDeepSeekModel = DefaultCompletionsModel
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
	{ID: "deepseek-v4-flash", Name: "DeepSeek-V4-Flash", ContextWindow: 1048576, Modalities: []string{"text"}},
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
// sentinels 由三条线协议适配器共用，错误文案保持原样（错误文本是行为契约的
// 一部分，不做无提示的字符串变更）。
var (
	// ErrMissingCredential mirrors upstream adapter.ts: without an API key the
	// adapter stays registered (routes browsable) but every stream call fails
	// with LlmError('MISSING_CREDENTIAL') rather than a silent mock reply.
	ErrMissingCredential  = errors.New("deepseek: no API key configured, set DEEPSEEK_API_KEY and retry (MISSING_CREDENTIAL)")
	ErrDeepSeekAuth       = errors.New("deepseek: authentication failed (check DEEPSEEK_API_KEY)")
	ErrDeepSeekQuota      = errors.New("deepseek: quota/balance exhausted")
	ErrDeepSeekRateLimit  = errors.New("deepseek: rate limited")
	ErrDeepSeekContext    = errors.New("deepseek: context window exceeded")
	ErrDeepSeekBadRequest = errors.New("deepseek: malformed request")
	ErrDeepSeekServer     = errors.New("deepseek: upstream server error")
	ErrDeepSeekStream     = errors.New("deepseek: malformed SSE stream")
	ErrDeepSeekWatchdog   = errors.New("deepseek: stream idle watchdog fired")
)

// Deprecated: ErrDeepSeekMissingCredential is the pre-rename name of
// ErrMissingCredential (kept one release so existing callers compile).
var ErrDeepSeekMissingCredential = ErrMissingCredential

// CompletionsConfig wires up the openai-completions streaming adapter via the
// legacy constructor (NewDeepSeekAdapter). The profile-based
// NewProtocolAdapter is the default construction entry.
type CompletionsConfig struct {
	APIKey     string
	BaseURL    string        // defaults to DefaultCompletionsBaseURL
	Model      string        // defaults to DefaultCompletionsModel
	Timeout    time.Duration // whole request+stream deadline; <=0 -> defaultTimeout
	Watchdog   time.Duration // max silence between SSE bytes; <=0 -> defaultWatchdog
	HTTPClient *http.Client  // nil -> http.DefaultClient
	// APIKeyResolver, when set, is consulted at Stream time when APIKey is
	// empty. It lets the host resolve the key through the credential seam
	// (default reference DEEPSEEK_API_KEY) so a changed credential reaches the
	// next operation without a restart. It returns the resolved value and an
	// error; an empty resolved value still reports MISSING_CREDENTIAL.
	APIKeyResolver func() (string, error)
}

// Deprecated: DeepSeekConfig is the pre-rename alias of CompletionsConfig
// ("deepseek" 是 provider profile，不是协议；类型归属 openai-completions 线协议).
type DeepSeekConfig = CompletionsConfig

// CompletionsAdapter implements LlmAdapter against the OpenAI Chat Completions
// wire protocol (ProtocolOpenAICompletions). It IS the 中转 adapter: it speaks
// the internal protocol (dsh-internal/v1, i.e. the LlmAdapter interface) on
// this side and openai-completions on the wire. DeepSeek 是走这条线协议的默认
// provider profile（origin https://api.deepseek.com, path /chat/completions）。
type CompletionsAdapter struct {
	mu       sync.Mutex
	cfg      CompletionsConfig
	httpc    *http.Client
	baseURL  string
	model    string
	timeout  time.Duration
	watchdog time.Duration
	extra    map[string]string
}

// Deprecated: DeepSeekAdapter is the pre-rename alias of CompletionsAdapter
// (kept one release; pkg/gateway type-switches still name the old type).
type DeepSeekAdapter = CompletionsAdapter

// NewDeepSeekAdapter returns a configured adapter. Construction is offline: no
// network I/O happens until Stream is called. It is the openai-completions
// convenience constructor (ProtocolOpenAICompletions + DefaultCompletionsBaseURL).
//
// Deprecated: DeepSeek 没有自有线协议——DeepSeek 走 OpenAI Chat Completions。
// 请改用 NewProtocolAdapter（Protocol "openai-completions"）；DeepSeek 只是
// 该协议之上的 provider profile（DefaultCompletionsBaseURL /
// DefaultCompletionsModel / DEEPSEEK_API_KEY）。本构造函数保留一个发布周期
// 以便存量调用方（pkg/gateway 迁移属 Track 2）编译平滑。
func NewDeepSeekAdapter(cfg CompletionsConfig) LlmAdapter {
	p := profileFromDeepSeek(cfg)
	if a, err := NewProtocolAdapter(p); err == nil {
		return a
	}
	return newCompletionsAdapter(p)
}

// profileFromDeepSeek builds the seeded "deepseek" PROVIDER PROFILE on the
// openai-completions wire protocol: api.deepseek.com / deepseek-v4-flash /
// DEEPSEEK_API_KEY credential reference. 名字保留 "deepseek"：它构造的就是
// DeepSeek 这个 provider 的 profile（协议归属不变：ProtocolOpenAICompletions）。
func profileFromDeepSeek(cfg CompletionsConfig) ProviderProfile {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = DefaultCompletionsBaseURL
	}
	model := cfg.Model
	if model == "" {
		model = DefaultCompletionsModel
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
func (d *CompletionsAdapter) SetEndpoint(baseURL string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if baseURL == "" {
		return
	}
	d.baseURL = strings.TrimRight(baseURL, "/")
}

// SetModel swaps the default model id at runtime (thread-safe). It takes
// effect on the next Stream call.
func (d *CompletionsAdapter) SetModel(model string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if model == "" {
		return
	}
	d.model = model
}

// Endpoint returns the current chat-completions endpoint (thread-safe).
func (d *CompletionsAdapter) Endpoint() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return chatCompletionsURL(d.baseURL)
}

// Model returns the current default model id (thread-safe).
func (d *CompletionsAdapter) Model() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.model
}

// FetchModels lists the models a base URL advertises (OpenAI-compatible
// GET {baseURL}/models). The key is resolved through the adapter's resolver
// (or cfg.APIKey) so a configured credential is honored. On any failure it
// returns the error; callers fall back to the static DefaultModels catalog.
func (d *CompletionsAdapter) FetchModels(ctx context.Context) ([]ModelInfo, error) {
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

// ProviderError is a typed provider failure carrying the normalized
// harness code, the provider's message, and the optional retry-after delay and
// request id the upstream LlmError attaches (adapter.ts:622-657). The stable
// code is what retry policy routes on.
type ProviderError struct {
	Code               string // "AUTH" | "INVALID_REQUEST" | "QUOTA" | "RATE_LIMIT" | "CONTEXT_WINDOW_EXCEEDED" | "SERVER" | "HTTP_<status>"
	Message            string // provider message when JSON-parsable, else "DeepSeek API error (HTTP n)"
	Status             int
	ProviderRetryAfter time.Duration // 0 when absent/invalid
	RequestID          string        // "" when absent
}

// Deprecated: DeepSeekProviderError is the pre-rename alias of ProviderError
// （sentinels/错误文案不变，供一版过渡）.
type DeepSeekProviderError = ProviderError

func (e *ProviderError) Error() string {
	return fmt.Sprintf("deepseek: %s (%s)", e.Message, e.Code)
}

// Unwrap exposes the matching sentinel so errors.Is(err, ErrDeepSeekAuth)
// (and RateLimit / BadRequest / Server / …) still classifies HTTP failures.
func (e *ProviderError) Unwrap() error {
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

// wireError is the provider error body shape (`WireError['error']`).
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

// mapProviderStatus converts an HTTP status plus the parsed provider error
// body into a structured ProviderError, exactly mirroring upstream
// httpErrorCode + requestId + providerRetryAfterMs (adapter.ts:333-345).
func mapProviderStatus(code int, providerError *wireError, retryAfter string, requestID string) *ProviderError {
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
	return &ProviderError{
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
