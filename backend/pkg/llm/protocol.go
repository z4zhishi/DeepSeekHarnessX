package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Wire protocols a configured route may name. The order of SupportedProtocols
// is the table below (most-reached first): a configuration surface offering a
// choice presents the first as its default.
const (
	ProtocolOpenAICompletions = "openai-completions"
	ProtocolOpenAIResponses   = "openai-responses"
	ProtocolAnthropicMessages = "anthropic-messages"
)

// ProviderProfile is the construction-time snapshot of one provider route.
// Credentials may be supplied inline (APIKey) or resolved per Stream via
// APIKeyResolver; an empty key at stream time is MISSING_CREDENTIAL.
type ProviderProfile struct {
	Protocol       string
	BaseURL        string
	Model          string
	APIKey         string
	APIKeyResolver func() (string, error)
	ExtraHeaders   map[string]string
	HTTPClient     *http.Client
	Timeout        time.Duration
	Watchdog       time.Duration
}

// SupportedProtocols returns the three hand-declared wire protocols in stable
// order: openai-completions, openai-responses, anthropic-messages.
func SupportedProtocols() []string {
	return []string{
		ProtocolOpenAICompletions,
		ProtocolOpenAIResponses,
		ProtocolAnthropicMessages,
	}
}

// NewProtocolAdapter builds a streaming LlmAdapter for one profile.
// Unknown protocol → error. Empty Protocol defaults to openai-completions.
func NewProtocolAdapter(p ProviderProfile) (LlmAdapter, error) {
	proto := p.Protocol
	if proto == "" {
		proto = ProtocolOpenAICompletions
	}
	p.Protocol = proto
	switch proto {
	case ProtocolOpenAICompletions:
		return newCompletionsAdapter(p), nil
	case ProtocolOpenAIResponses:
		return newResponsesAdapter(p), nil
	case ProtocolAnthropicMessages:
		return newAnthropicAdapter(p), nil
	default:
		return nil, fmt.Errorf("llm: unknown protocol %q (supported: %s)", proto, strings.Join(SupportedProtocols(), ", "))
	}
}

func normalizeProfile(p ProviderProfile) ProviderProfile {
	if p.HTTPClient == nil {
		p.HTTPClient = http.DefaultClient
	}
	p.BaseURL = trimBaseURL(p.BaseURL)
	if p.Timeout <= 0 {
		p.Timeout = defaultDeepSeekTimeout
	}
	if p.Watchdog <= 0 {
		p.Watchdog = defaultDeepSeekWatchdog
	}
	return p
}

// FetchModelsFor lists models via GET {base}/models or GET {base}/v1/models
// (bearer for OpenAI protocols; x-api-key for anthropic-messages).
// On failure return (nil, err).
func FetchModelsFor(ctx context.Context, protocol, baseURL, apiKey string, httpc *http.Client) ([]ModelInfo, error) {
	if httpc == nil {
		httpc = http.DefaultClient
	}
	base := trimBaseURL(baseURL)
	if base == "" {
		return nil, fmt.Errorf("llm: empty base URL")
	}

	candidates := []string{base + "/models"}
	if !pathEnds(base, "/v1") && !pathEnds(base, "/models") {
		candidates = append(candidates, base+"/v1/models")
	}

	var lastErr error
	for _, u := range candidates {
		models, err := fetchModelsURL(ctx, httpc, protocol, u, apiKey)
		if err == nil {
			return models, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("llm: no model listing URL to try")
	}
	return nil, lastErr
}

func fetchModelsURL(ctx context.Context, httpc *http.Client, protocol, rawURL, apiKey string) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		if protocol == ProtocolAnthropicMessages {
			req.Header.Set("x-api-key", apiKey)
			req.Header.Set("anthropic-version", anthropicVersion)
		} else {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if !isSuccessStatus(resp.StatusCode) {
		return nil, fmt.Errorf("fetch models: upstream %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Data []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			DisplayName   string `json:"display_name"`
			ContextWindow int    `json:"context_window"`
			ContextLength int    `json:"context_length"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	if parsed.Data == nil {
		// Anthropic's listing uses the same `data` array; a missing array is
		// not a catalog, it is a malformed listing.
		return nil, fmt.Errorf("fetch models: listing has no data array")
	}
	out := make([]ModelInfo, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID == "" {
			continue
		}
		name := m.Name
		if name == "" {
			name = m.DisplayName
		}
		if name == "" {
			name = m.ID
		}
		window := m.ContextWindow
		if window <= 0 {
			window = m.ContextLength
		}
		if window <= 0 {
			window = 131072
		}
		out = append(out, ModelInfo{ID: m.ID, Name: name, ContextWindow: window, Modalities: []string{"text"}})
	}
	return out, nil
}
