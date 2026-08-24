package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"dsh-go/pkg/llm"
	"dsh-go/pkg/session"
)

// Session title generation for DSH Go.
//
// This mirrors the upstream `@deepseek-ai/dsh-session-title` service in its
// *pure* (state-free) surface: message collection, deterministic fallback, and
// the model-backed title call. Wiring into the agent loop (when to schedule a
// title, how to persist the `session/title` event) is deliberately OUT of
// scope here and belongs to Phase 2 — this file only ships the generator.
//
// Failure policy: title generation is never load-bearing. Every LLM failure
// silently degrades to the deterministic fallback (or the empty string when no
// eligible user text exists); it never returns an error for a downstream
// caller to propagate into the main loop.

// TitleUserMessage is one eligible human text message extracted from the log.
type TitleUserMessage struct {
	// Seq is the source user/message event sequence.
	Seq int
	// Text is the exact concatenated text-block content of the message.
	Text string
}

// TitleLimits are the byte/word budgets enforced during normalization. The
// zero value selects the package defaults.
type TitleLimits struct {
	// MaxWords caps whitespace-delimited words in the deterministic fallback.
	MaxWords int
	// MaxBytes caps UTF-8 bytes in the deterministic fallback title.
	MaxBytes int
}

// Apply returns the resolved limits with sensible defaults filled in.
func (l TitleLimits) Apply() (maxWords, maxBytes int) {
	maxWords = l.MaxWords
	if maxWords <= 0 {
		maxWords = 8
	}
	maxBytes = l.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 80
	}
	return maxWords, maxBytes
}

// TitleGenerateOptions tune the LLM-backed call.
type TitleGenerateOptions struct {
	// Model is the model id to request; empty leaves the adapter's default.
	Model string
	// MaxTokens caps the auxiliary output; <=0 uses a small default (title
	// only, no tools, so a tiny budget is enough).
	MaxTokens int
	// Limits configure the deterministic fallback used on any LLM failure.
	Limits TitleLimits
}

// CleanControlSequences removes OSC/CSI/other ESC sequences and remaining
// control/directional characters, collapsing internal whitespace to single
// spaces. This is the Go port of upstream normalize.ts cleanTitleText (RE2 has
// no lookahead, so the trailing-sentinel removal is handled with optional
// groups instead of a negative lookahead).
func CleanControlSequences(input string) string {
	// OSC (OSC-payload, optional ST or BEL terminator).
	reOSC := regexp.MustCompile(`\x1b](?:[^\x07\x1b\\]|\\)*(?:\x07|\x1b\\)?`)
	// CSI (SGR and other control-sequence-introducer escapes).
	reCSI := regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	// Remaining two-byte ESC control sequences.
	reESC := regexp.MustCompile(`\x1b[@-_]`)
	// Non-whitespace C0/C1 control characters.
	reControl := regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x7f-\x9f]`)
	// Directional and invisible controls that can make a displayed title deceptive
	// (zero-width space, LRM/RLM, LRE/RLO/LRI/RLI/FSI/PDI, word joiner, BOM).
	reDirectional := regexp.MustCompile(`[\x{200B}\x{200E}\x{200F}\x{202A}-\x{202E}\x{2060}-\x{2064}\x{2066}-\x{206F}\x{FEFF}]`)

	s := input
	s = reOSC.ReplaceAllString(s, "")
	s = reCSI.ReplaceAllString(s, "")
	s = reESC.ReplaceAllString(s, "")
	s = reControl.ReplaceAllString(s, "")
	s = reDirectional.ReplaceAllString(s, "")
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimSpace(s)
}

// CleanInput trims surrounding whitespace from a raw title candidate. It is a
// thin alias so callers that already cleaned escape sequences can still
// normalize spacing via the same entry point.
func CleanInput(input string) string { return CleanControlSequences(input) }

// TruncateTitleUtf8 truncates a string to a UTF-8 byte budget without splitting
// a Unicode code point (Go port of upstream truncateTitleUtf8).
func TruncateTitleUtf8(input string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(input) <= maxBytes {
		return input
	}
	used := 0
	var out strings.Builder
	for _, r := range input {
		bytes := len(string(r))
		if used+bytes > maxBytes {
			break
		}
		out.WriteRune(r)
		used += bytes
	}
	return out.String()
}

// NormalizeTitle cleans control sequences and enforces a UTF-8 byte budget,
// returning a terminal-safe one-line title (possibly empty after sanitization).
// Go port of upstream normalizeSessionTitle.
func NormalizeTitle(input string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	return strings.TrimRight(TruncateTitleUtf8(CleanControlSequences(input), maxBytes), " \t")
}

// FallbackTitle derives the deterministic first-prompt fallback from the first
// eligible user message (Go port of upstream fallbackSessionTitle).
func FallbackTitle(input string, limits TitleLimits) string {
	maxWords, maxBytes := limits.Apply()
	cleaned := CleanControlSequences(input)
	words := strings.Fields(cleaned)
	if len(words) > maxWords {
		words = words[:maxWords]
	}
	return strings.TrimRight(TruncateTitleUtf8(strings.Join(words, " "), maxBytes), " \t")
}

// CollectTitleMessages returns eligible human text-bearing user messages in log
// order (Go port of upstream collectSessionTitleMessages). Only `user/message`
// events whose source.kind is "user" qualify; empty-after-clean text is skipped.
func CollectTitleMessages(events []session.SessionEnvelope, throughSeq int) []TitleUserMessage {
	messages := []TitleUserMessage{}
	for _, event := range events {
		if throughSeq >= 0 && event.Seq > throughSeq {
			break
		}
		if event.Type != session.EventUserMessage {
			continue
		}
		var payload session.UserMessagePayload
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			continue
		}
		if payload.Source.Kind != "user" {
			continue
		}
		var text strings.Builder
		for _, block := range payload.Content {
			if block.Type == "text" {
				text.WriteString(block.Text)
				text.WriteString("\n")
			}
		}
		joined := text.String()
		// The join separator contributes a trailing newline; normalize away for
		// emptiness so a message with only whitespace is not eligible.
		if NormalizeTitle(joined, 1<<20) == "" {
			continue
		}
		messages = append(messages, TitleUserMessage{Seq: event.Seq, Text: strings.TrimRight(joined, "\n")})
	}
	return messages
}

// TitleUserMessageFromLog collects eligible user messages with no seq boundary.
func TitleUserMessageFromLog(events []session.SessionEnvelope) []TitleUserMessage {
	return CollectTitleMessages(events, -1)
}

// systemPrompt builds the stable language-aware instruction mirroring upstream
// systemPrompt; the target sizes are internal defaults.
func titleSystemPrompt() string {
	return strings.Join([]string{
		"Create a concise title for an AI coding-assistant session from the supplied human messages.",
		"Return only the title on one line, in plain text of natural language, with no quotes, prefix, explanation, Markdown, XML, or terminal control codes. No code is allowed.",
		"Use the language of the messages.",
		"Aim for about 6 words in non-CJK languages or 4 CJK characters.",
	}, "\n")
}

// frameTitleMessages frames exact messages as JSON so user text cannot break
// structural delimiters (upstream frameMessages).
func frameTitleMessages(messages []TitleUserMessage) string {
	data, _ := json.Marshal(messages)
	return "Generate the session title from this JSON array of human messages:\n" + string(data)
}

// readTitleText drives one adapter.Stream call, assembling only the text blocks
// and checking the terminal finish reason. It returns the assembled raw text.
func readTitleText(ctx context.Context, adapter llm.LlmAdapter, req llm.ModelRequest) (string, error) {
	chunkChan, errChan := adapter.Stream(ctx, req)
	assembler := llm.NewBlockAssembler()
	done := false
	for !done {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case err, ok := <-errChan:
			if ok && err != nil {
				return "", err
			}
			done = true
		case chunk, ok := <-chunkChan:
			if !ok {
				done = true
				break
			}
			assembler.IngestChunk(chunk)
		}
	}
	blocks, _, finish := assembler.Result()
	if finish != "stop" && finish != "" {
		return "", fmt.Errorf("session-title: LLM finish reason %q", finish)
	}
	var text strings.Builder
	for _, block := range blocks {
		if block.Type == "text" {
			if text.Len() > 0 {
				text.WriteString(" ")
			}
			text.WriteString(block.Text)
		}
	}
	return text.String(), nil
}

// GenerateTitle generates a short session title from the conversation log using
// the LLM adapter, degrading silently to the deterministic fallback on any
// failure. It returns "" (no error) when the log has no eligible user message,
// so the caller can simply stop — title generation never blocks the main flow.
//
// The generated title is normalized (single line, control-clean, within the byte
// budget). The returned error is non-nil ONLY for an empty/zero adapter or an
// empty log; LLM stream failures are swallowed in favor of the fallback.
func GenerateTitle(ctx context.Context, adapter llm.LlmAdapter, events []session.SessionEnvelope) (string, error) {
	return GenerateTitleWithOpts(ctx, adapter, events, TitleGenerateOptions{})
}

// GenerateTitleWithOpts is GenerateTitle with explicit tuning.
func GenerateTitleWithOpts(
	ctx context.Context,
	adapter llm.LlmAdapter,
	events []session.SessionEnvelope,
	opts TitleGenerateOptions,
) (string, error) {
	if adapter == nil {
		return "", fmt.Errorf("session-title: nil LLM adapter")
	}
	messages := TitleUserMessageFromLog(events)
	if len(messages) == 0 {
		return "", nil
	}
	first := messages[0]
	limits := opts.Limits
	_, maxBytes := limits.Apply()
	fallback := FallbackTitle(first.Text, limits)
	normalizedFallback := NormalizeTitle(fallback, maxBytes)

	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 48
	}
	req := llm.ModelRequest{
		Model:     opts.Model,
		Purpose:   "session-title", // adapter disables thinking for this purpose
		MaxTokens: maxTokens,
		System:    titleSystemPrompt(),
		Messages: []session.ModelMessage{{
			Role: "user",
			Content: []session.ContentBlock{{
				Type: "text",
				Text: frameTitleMessages(messages),
			}},
		}},
	}
	text, err := readTitleText(ctx, adapter, req)
	if err != nil || NormalizeTitle(text, maxBytes) == "" {
		// Silent degradation: never propagate an LLM failure upstream.
		return normalizedFallback, nil
	}
	title := NormalizeTitle(text, maxBytes)
	if title == "" {
		return normalizedFallback, nil
	}
	return title, nil
}

// NormalizeTitleText is an exported alias of NormalizeTitle for callers that
// already have a plain title string.
func NormalizeTitleText(input string, maxBytes int) string { return NormalizeTitle(input, maxBytes) }
