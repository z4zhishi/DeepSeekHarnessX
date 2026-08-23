package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Web tool limits mirroring the upstream web-fetch-http defaults.
const (
	webMaxURLChars      = 2048
	webMaxResponseBytes = 5_000_000
	webMaxBodyChars     = 100_000
	webFetchTimeout     = 30 * time.Second
	webMaxRedirects     = 5

	webSearchMaxResults = 8
	webSearchMaxQueries = 4
)

// webSource is one normalized search source (upstream WebSearchSource).
type webSource struct {
	URL         string `json:"url"`
	Title       string `json:"title,omitempty"`
	Snippet     string `json:"snippet,omitempty"`
	PublishedAt string `json:"publishedAt,omitempty"`
}

// webSearchResult is the normalized combined search outcome.
type webSearchResult struct {
	Content   string      `json:"content,omitempty"`
	Sources   []webSource `json:"sources"`
	Truncated bool        `json:"truncated"`
}

// webFetchResult is the normalized fetch outcome (upstream WebFetchResult).
type webFetchResult struct {
	URL        string       `json:"url"`
	StatusCode int          `json:"statusCode"`
	Body       webFetchBody `json:"body"`
	Truncated  bool         `json:"truncated"`
}

type webFetchBody struct {
	Kind    string `json:"kind"` // "html" | "text"
	Content string `json:"content"`
}

// RegisterWebTools registers the model-facing web_search and web_fetch tools
// (upstream @deepseek-ai/dsh-tool-web contract). Search providers are chosen
// at execution time: the DeepSeek official Anthropic-compatible Messages API
// when DEEPSEEK_API_KEY is set, otherwise Exa when EXA_API_KEY is set. Fetch
// is the anonymous local HTTP(S) provider with same-origin redirects and
// size/time caps.
func (r *ToolRegistry) RegisterWebTools() {
	r.Register(ToolDefinition{
		Name:        "web_search",
		Description: "Search the web for current information. Provide 1-4 queries in the required queries array. Returns an optional summary answer and a list of source URLs.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"queries": {
					"type": "array",
					"items": { "type": "string" },
					"description": "Required search queries; accepts 1-4 items and merges their results."
				}
			},
			"required": ["queries"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Queries []string `json:"queries"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, fmt.Errorf("invalid web_search arguments: %w", err)
			}
			queries, err := parseSearchArgs(args.Queries)
			if err != nil {
				return nil, err
			}
			return runSearchQueries(ctx.Context, queries)
		},
	})

	r.Register(ToolDefinition{
		Name:        "web_fetch",
		Description: "Fetch the content of a specific HTTP(S) URL and return it decoded to text.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"url": { "type": "string", "description": "The HTTP(S) URL to fetch." }
			},
			"required": ["url"]
		}`),
		Execute: func(ctx ToolExecutionContext, raw string) (any, error) {
			var args struct {
				URL string `json:"url"`
			}
			if err := json.Unmarshal([]byte(raw), &args); err != nil {
				return nil, fmt.Errorf("invalid web_fetch arguments: %w", err)
			}
			if strings.TrimSpace(args.URL) == "" {
				return nil, fmt.Errorf("url must be a non-empty string")
			}
			return runFetch(ctx.Context, args.URL)
		},
	})
}

// parseSearchArgs validates upstream's value constraints: non-empty, at most
// maxQueries, no blank strings; exact duplicates collapse.
func parseSearchArgs(queries []string) ([]string, error) {
	if len(queries) == 0 {
		return nil, fmt.Errorf("queries must contain at least one query")
	}
	if len(queries) > webSearchMaxQueries {
		return nil, fmt.Errorf("queries must contain at most %d queries", webSearchMaxQueries)
	}
	for _, q := range queries {
		if strings.TrimSpace(q) == "" {
			return nil, fmt.Errorf("each query must be a non-empty string")
		}
	}
	seen := make(map[string]bool, len(queries))
	out := make([]string, 0, len(queries))
	for _, q := range queries {
		if !seen[q] {
			seen[q] = true
			out = append(out, q)
		}
	}
	return out, nil
}

// searchProvider abstracts a web search backend.
type searchProvider interface {
	search(ctx context.Context, query string, maxResults int) (webSearchResult, error)
}

// newSearchProvider selects the configured provider: DeepSeek official when
// DEEPSEEK_API_KEY is present, else Exa when EXA_API_KEY is present.
func newSearchProvider() (searchProvider, error) {
	if key := os.Getenv("DEEPSEEK_API_KEY"); key != "" {
		return &deepseekSearchProvider{apiKey: key}, nil
	}
	if key := os.Getenv("EXA_API_KEY"); key != "" {
		return &exaSearchProvider{apiKey: key}, nil
	}
	return nil, fmt.Errorf("web_search requires DEEPSEEK_API_KEY or EXA_API_KEY to be set")
}

// runSearchQueries runs one or more searches concurrently and merges results
// round-robin, deduplicated, capped at webSearchMaxResults (upstream
// runSearchQueries / mergeSearchResults). A failed search aborts the rest via
// the shared context; the first failure wins after everything settles.
func runSearchQueries(ctx context.Context, queries []string) (webSearchResult, error) {
	provider, err := newSearchProvider()
	if err != nil {
		return webSearchResult{}, err
	}
	if len(queries) == 1 {
		return provider.search(ctx, queries[0], webSearchMaxResults)
	}
	searchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		mu      sync.Mutex
		results = make([]webSearchResult, len(queries))
		first   error
	)
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		go func(i int, q string) {
			defer wg.Done()
			res, err := provider.search(searchCtx, q, webSearchMaxResults)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if first == nil {
					first = err
				}
				cancel()
				return
			}
			results[i] = res
		}(i, q)
	}
	wg.Wait()
	if first != nil {
		return webSearchResult{}, first
	}
	return mergeSearchResults(queries, results), nil
}

// mergeSearchResults deduplicates and interleaves per-query sources
// (upstream mergeSearchResults).
func mergeSearchResults(queries []string, results []webSearchResult) webSearchResult {
	maxRank := 0
	for _, res := range results {
		if len(res.Sources) > maxRank {
			maxRank = len(res.Sources)
		}
	}
	seen := map[string]bool{}
	var sources []webSource
	dropped := false
merge:
	for rank := 0; rank < maxRank; rank++ {
		for _, res := range results {
			if rank >= len(res.Sources) {
				continue
			}
			src := res.Sources[rank]
			if seen[src.URL] {
				continue
			}
			seen[src.URL] = true
			if len(sources) == webSearchMaxResults {
				dropped = true
				break merge
			}
			sources = append(sources, src)
		}
	}
	var parts []string
	anyTruncated := dropped
	for i, res := range results {
		if res.Content != "" {
			parts = append(parts, fmt.Sprintf("### %s\n\n%s", queries[i], res.Content))
		}
		if res.Truncated {
			anyTruncated = true
		}
	}
	return webSearchResult{
		Content:   strings.Join(parts, "\n\n"),
		Sources:   sources,
		Truncated: anyTruncated,
	}
}

// deepseekSearchProvider calls the DeepSeek Anthropic-compatible Messages API
// with the native web_search_20250305 server tool (upstream web-search-deepseek).
type deepseekSearchProvider struct {
	apiKey string
}

type anthropicTextBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Citations []struct {
		URL       string `json:"url"`
		CitedText string `json:"cited_text"`
	} `json:"citations"`
}

type anthropicWebResult struct {
	Type    string `json:"type"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	PageAge string `json:"page_age"`
}

type anthropicBlock struct {
	Type    string               `json:"type"`
	Content []anthropicWebResult `json:"content"`
}

type anthropicResponse struct {
	Content []json.RawMessage `json:"content"`
}

func (p *deepseekSearchProvider) search(ctx context.Context, query string, maxResults int) (webSearchResult, error) {
	body := map[string]any{
		"model":      "deepseek-v4-flash",
		"max_tokens": 4096,
		"messages": []map[string]any{{
			"role":    "user",
			"content": []map[string]string{{"type": "text", "text": "Perform a web search for the query: " + query}},
		}},
		"tools": []map[string]any{{
			"type":     "web_search_20250305",
			"name":     "web_search",
			"max_uses": 5,
		}},
	}
	rawBody, err := json.Marshal(body)
	if err != nil {
		return webSearchResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.deepseek.com/anthropic/v1/messages", strings.NewReader(string(rawBody)))
	if err != nil {
		return webSearchResult{}, fmt.Errorf("deepseek search request: %w", err)
	}
	req.Header.Set("x-api-key", p.apiKey)
	req.Header.Set("authorization", "Bearer "+p.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")
	req.Header.Set("user-agent", "deepseek-harness/0.0.1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return webSearchResult{}, fmt.Errorf("deepseek search request failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return webSearchResult{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := fmt.Sprintf("DeepSeek API error (HTTP %d)", resp.StatusCode)
		var e struct {
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal(respBody, &e) == nil && len(e.Error) > 0 {
			var s string
			if json.Unmarshal(e.Error, &s) == nil && s != "" {
				msg = s
			} else {
				var m struct {
					Message string `json:"message"`
				}
				if json.Unmarshal(e.Error, &m) == nil && m.Message != "" {
					msg = m.Message
				}
			}
		}
		return webSearchResult{}, fmt.Errorf("%s", msg)
	}

	var payload anthropicResponse
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return webSearchResult{}, fmt.Errorf("deepseek returned an unprocessable response body: %w", err)
	}
	return mapAnthropicResponse(payload), nil
}

// mapAnthropicResponse maps Messages blocks to normalized sources, joining
// citation snippets from text blocks (upstream mapAnthropicResponse).
func mapAnthropicResponse(payload anthropicResponse) webSearchResult {
	snippets := map[string]string{}
	for _, raw := range payload.Content {
		var tb anthropicTextBlock
		if json.Unmarshal(raw, &tb) != nil || tb.Type != "text" {
			continue
		}
		for _, cite := range tb.Citations {
			if cite.URL != "" && cite.CitedText != "" {
				if _, ok := snippets[cite.URL]; !ok {
					snippets[cite.URL] = cite.CitedText
				}
			}
		}
	}
	seen := map[string]bool{}
	var sources []webSource
	for _, raw := range payload.Content {
		var block anthropicBlock
		if json.Unmarshal(raw, &block) != nil || block.Type != "web_search_tool_result" {
			continue
		}
		for _, item := range block.Content {
			if item.Type != "web_search_result" || item.URL == "" || seen[item.URL] {
				continue
			}
			seen[item.URL] = true
			src := webSource{URL: item.URL}
			if item.Title != "" {
				src.Title = item.Title
			}
			if s := snippets[item.URL]; s != "" {
				src.Snippet = s
			}
			if item.PageAge != "" {
				src.PublishedAt = item.PageAge
			}
			sources = append(sources, src)
		}
	}
	return webSearchResult{Sources: sources, Truncated: false}
}

// exaSearchProvider calls the Exa /search API (upstream web-search-exa).
type exaSearchProvider struct {
	apiKey string
}

type exaResponse struct {
	Results []struct {
		URL           string   `json:"url"`
		Title         string   `json:"title"`
		Highlights    []string `json:"highlights"`
		PublishedDate string   `json:"publishedDate"`
	} `json:"results"`
}

func (p *exaSearchProvider) search(ctx context.Context, query string, maxResults int) (webSearchResult, error) {
	body := map[string]any{
		"query":      query,
		"type":       "auto",
		"contents":   map[string]any{"highlights": map[string]int{"highlightsPerUrl": 1}},
		"numResults": maxResults,
	}
	rawBody, err := json.Marshal(body)
	if err != nil {
		return webSearchResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.exa.ai/search", strings.NewReader(string(rawBody)))
	if err != nil {
		return webSearchResult{}, fmt.Errorf("exa search request: %w", err)
	}
	req.Header.Set("authorization", "Bearer "+p.apiKey)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")
	req.Header.Set("user-agent", "deepseek-harness/0.0.1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return webSearchResult{}, fmt.Errorf("exa search request failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return webSearchResult{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := fmt.Sprintf("Exa API error (HTTP %d)", resp.StatusCode)
		var e struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(respBody, &e) == nil {
			if e.Error != "" {
				msg = e.Error
			} else if e.Message != "" {
				msg = e.Message
			}
		}
		return webSearchResult{}, fmt.Errorf("%s", msg)
	}
	var payload exaResponse
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return webSearchResult{}, fmt.Errorf("exa returned an unprocessable response body: %w", err)
	}
	var sources []webSource
	for _, r := range payload.Results {
		snippet := ""
		for _, h := range r.Highlights {
			if strings.TrimSpace(h) != "" {
				snippet = h
				break
			}
		}
		if snippet == "" {
			continue
		}
		src := webSource{URL: r.URL, Snippet: snippet}
		if r.Title != "" {
			src.Title = r.Title
		}
		if r.PublishedDate != "" {
			src.PublishedAt = r.PublishedDate
		}
		sources = append(sources, src)
	}
	return webSearchResult{Sources: sources, Truncated: false}, nil
}

// runFetch implements the anonymous HTTP(S) fetch provider: URL validation,
// same-origin redirects up to the hop cap, byte cap, charset decoding,
// content-type classification, and HTML闁愁偅澧穉rkdown conversion (upstream
// web-fetch-http + tool-web fetch formatting).
func runFetch(ctx context.Context, rawURL string) (webFetchResult, error) {
	u, err := validateFetchURL(rawURL)
	if err != nil {
		return webFetchResult{}, err
	}
	client := &http.Client{
		Timeout: webFetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= webMaxRedirects {
				return fmt.Errorf("exceeded the maximum of %d redirects", webMaxRedirects)
			}
			cur := via[len(via)-1].URL
			if !sameOrigin(cur, req.URL) {
				return fmt.Errorf("cross-origin redirect to %s is not followed automatically; retry against that URL directly", req.URL.Host)
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return webFetchResult{}, fmt.Errorf("invalid URL: %w", err)
	}
	req.Header.Set("user-agent", "deepseek-harness/0.0.1 (+https://github.com/deepseek-ai)")
	req.Header.Set("accept", "text/html,application/xhtml+xml,text/*;q=0.9,application/json;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return webFetchResult{}, fmt.Errorf("web fetch failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, webMaxResponseBytes))
	if err != nil {
		return webFetchResult{}, fmt.Errorf("web fetch failed: %w", err)
	}

	contentType := resp.Header.Get("content-type")
	kind := classifyContentType(contentType)
	if kind == "" {
		return webFetchResult{}, fmt.Errorf("unsupported content type %q", contentType)
	}
	text := decodeText(bodyBytes, contentType)
	truncated := false
	if kind == "html" {
		text = htmlToMarkdown(text)
	}
	if len(text) > webMaxBodyChars {
		text = text[:webMaxBodyChars] + "\n\n...[truncated]"
		truncated = true
	}
	finalURL := u.String()
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	return webFetchResult{
		URL:        finalURL,
		StatusCode: resp.StatusCode,
		Body:       webFetchBody{Kind: kind, Content: text},
		Truncated:  truncated,
	}, nil
}

// validateFetchURL enforces http(s) only, no credentials, bounded length.
func validateFetchURL(input string) (*url.URL, error) {
	if len(input) > webMaxURLChars {
		return nil, fmt.Errorf("URL exceeds the maximum length of %d", webMaxURLChars)
	}
	u, err := url.Parse(input)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %s", input)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported URL scheme %q (only http and https are allowed)", u.Scheme)
	}
	if u.User != nil && (u.User.Username() != "" || passwordSet(u.User)) {
		return nil, fmt.Errorf("credentials in URLs are not allowed")
	}
	return u, nil
}

func passwordSet(u *url.Userinfo) bool {
	_, ok := u.Password()
	return ok
}

func sameOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if u.Port() != "" {
			return u.Port()
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return a.Scheme == b.Scheme && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}

// classifyContentType maps a Content-Type to "html" | "text" | "" (unsupported).
func classifyContentType(contentType string) string {
	ct, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		ct = strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0])
	}
	switch {
	case ct == "text/html" || ct == "application/xhtml+xml":
		return "html"
	case strings.HasPrefix(ct, "text/"):
		return "text"
	case ct == "application/json" || ct == "application/xml" || strings.HasSuffix(ct, "+json") || strings.HasSuffix(ct, "+xml"):
		return "text"
	default:
		return ""
	}
}

// decodeText decodes a body with its declared charset, defaulting to UTF-8.
func decodeText(body []byte, contentType string) string {
	_, params, err := mime.ParseMediaType(contentType)
	cs := ""
	if err == nil {
		cs = strings.ToLower(strings.Trim(params["charset"], `"`))
	}
	switch cs {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		return string(body)
	case "iso-8859-1", "latin1", "windows-1252":
		out := make([]rune, 0, len(body))
		for _, b := range body {
			out = append(out, rune(b))
		}
		return string(out)
	default:
		// Unknown encodings degrade to UTF-8 with replacement characters
		// instead of failing the whole fetch.
		return strings.ToValidUTF8(string(body), "\uFFFD")
	}
}

var (
	webScriptRe    = regexp.MustCompile(`(?is)<script\b.*?</script\s*>`)
	webStyleRe     = regexp.MustCompile(`(?is)<style\b.*?</style\s*>`)
	webNoScriptRe  = regexp.MustCompile(`(?is)<noscript\b.*?</noscript\s*>`)
	webCommentRe   = regexp.MustCompile(`(?s)<!--.*?-->`)
	webTagRe       = regexp.MustCompile(`(?is)<(/?)([a-zA-Z][a-zA-Z0-9]*)([^>]*)>|([^<]+)`)
	webHrefRe      = regexp.MustCompile(`(?i)(?:href|src)=["']([^"']+)["']`)
	webSpacesRe    = regexp.MustCompile(`[ \t]+`)
	webEmptyLineRe = regexp.MustCompile(`(?m)^\n+$`)
)

// htmlToMarkdown is a lightweight deterministic HTML闁愁偅澧穉rkdown converter
// (atx headings, links, fenced code, lists, GFM-ish tables) matching the
// upstream presentation conventions without a DOM dependency.
func htmlToMarkdown(html string) string {
	html = webScriptRe.ReplaceAllString(html, "")
	html = webStyleRe.ReplaceAllString(html, "")
	html = webNoScriptRe.ReplaceAllString(html, "")
	html = webCommentRe.ReplaceAllString(html, "")
	var b strings.Builder
	var linkHrefs []string
	depth := 0
	for _, m := range webTagRe.FindAllStringSubmatch(html, -1) {
		if m[0] == "" {
			continue
		}
		if m[4] != "" {
			text := webSpacesRe.ReplaceAllString(m[4], " ")
			b.WriteString(text)
			continue
		}
		tag := strings.ToLower(m[2])
		attrs := m[3]
		if m[1] == "/" {
			switch tag {
			case "h1", "h2", "h3", "h4", "h5", "h6", "li":
				b.WriteString("\n")
			case "pre":
				b.WriteString("\n```\n")
				depth--
			case "code":
				if depth == 0 {
					b.WriteString("`")
				}
			case "a", "img":
				href := ""
				if n := len(linkHrefs); n > 0 {
					href = linkHrefs[n-1]
					linkHrefs = linkHrefs[:n-1]
				}
				b.WriteString(fmt.Sprintf("](%s)", href))
			case "p", "div", "td", "th":
				b.WriteString("\n")
			}
			continue
		}
		href := ""
		if hm := webHrefRe.FindStringSubmatch(attrs); hm != nil {
			href = hm[1]
		}
		switch tag {
		case "h1", "h2", "h3", "h4", "h5", "h6":
			b.WriteString(strings.Repeat("#", int(tag[1]-'0')))
			b.WriteString(" ")
		case "li":
			b.WriteString("- ")
		case "pre":
			b.WriteString("\n```\n")
			depth++
		case "code":
			if depth == 0 {
				b.WriteString("`")
			}
		case "p", "br":
			b.WriteString("\n\n")
		case "a", "img":
			if tag == "img" {
				b.WriteString("![")
			} else {
				b.WriteString("[")
			}
			linkHrefs = append(linkHrefs, href)
		case "tr":
			b.WriteString("\n")
		case "td", "th":
			b.WriteString(" | ")
		}
	}
	out := b.String()
	out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	out = webEmptyLineRe.ReplaceAllString(out, "")
	return strings.TrimSpace(out)
}
