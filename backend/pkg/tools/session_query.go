package tools

// session_query.go
//
// Model-facing session-history read tools (upstream
// `CK/packages/session-query/tool-session-query`): let the model look back
// over a session's ordered event log to find prior work, trace lineage, and
// read one unabridged event.
//
// Data access resolves the caller's own session history through
// `ToolExecutionContext.Events()` when present (the live ring + persisted
// replay the agent loop injects); any other `session_id` resolves through the
// registered per-session event provider (`eventsForSession`), which hosts the
// same stream. When no stream is reachable the tool reports an empty result
// rather than fabricating data.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"dsh-go/pkg/session"
)

// sessionQueryDefaults mirror upstream tool-session-query bounds.
const (
	defaultMaxSearchResults = 100
)

// sessionSurfaceClass maps one envelope to the upstream surface taxonomy used
// in search/read output: "current" (append surface), "shadowed" (replacement
// copy), or "log-only" (no surface eligibility).
func sessionSurfaceClass(env session.SessionEnvelope) string {
	if env.IsAppendSurfaceEvent() {
		return "current"
	}
	if env.IsReplacementSurfaceEvent() {
		return "shadowed"
	}
	return "log-only"
}

// sessionQueryText extracts the model-facing text of one event for full-text
// search and neighbor snippets. Known message-bearing payloads project their
// text blocks; anything else falls back to the raw JSON payload.
func sessionQueryText(env session.SessionEnvelope) string {
	switch env.Type {
	case session.EventUserMessage:
		var m session.UserMessagePayload
		if err := json.Unmarshal(env.Data, &m); err == nil {
			return sessionQueryTextBlocks(m.Content)
		}
	case session.EventAssistantMessage:
		var m session.AssistantMessagePayload
		if err := json.Unmarshal(env.Data, &m); err == nil {
			return sessionQueryTextBlocks(m.Message.Content)
		}
	case session.EventToolResult:
		var m session.ToolResultPayload
		if err := json.Unmarshal(env.Data, &m); err == nil {
			return sessionQueryTextBlocks(m.Message.Content)
		}
	}
	return string(env.Data)
}

// sessionQueryTextBlocks concatenates the text content blocks of a message,
// mirroring the session package's text projection for search snippets.
func sessionQueryTextBlocks(blocks []session.ContentBlock) string {
	var res string
	for _, b := range blocks {
		if b.Type == "text" {
			res += b.Text
		}
	}
	return res
}

// sessionSnippet returns a compact single-line excerpt around the first
// occurrence of the query, or a leading window when the query is absent.
func sessionSnippet(text, query string, width int) string {
	if width <= 0 {
		width = 120
	}
	norm := strings.Join(strings.Fields(text), " ")
	if query == "" {
		return truncateUTF8(norm, width)
	}
	idx := strings.Index(norm, query)
	if idx < 0 {
		return truncateUTF8(norm, width)
	}
	start := idx - 20
	if start < 0 {
		start = 0
	}
	end := start + width
	if end > len(norm) {
		end = len(norm)
	}
	prefix := ""
	if start > 0 {
		prefix = "…"
	}
	suffix := ""
	if end < len(norm) {
		suffix = "…"
	}
	return prefix + norm[start:end] + suffix
}

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Keep whole UTF-8 runes by slicing on the last boundary <= n.
	for n > 0 && n < len(s) && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}

func formatEventTime(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

// RegisterSessionQueryTools registers session_search, session_trace, and
// session_event_read (upstream tool-session-query, current-session subset).
func (r *ToolRegistry) RegisterSessionQueryTools() {
	r.Register(ToolDefinition{
		Name:        "session_search",
		Description: "Search the session event history for relevant prior work and return the strongest matching event from each searched session.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": { "type": "string", "description": "Literal full-text query over session history" },
				"session_ids": { "type": "array", "items": { "type": "string" }, "description": "Optional session ids to search; omit to search the current session" },
				"event_types": { "type": "array", "items": { "type": "string" }, "description": "Event types to include" }
			},
			"required": ["query"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Query      string   `json:"query"`
				SessionIDs []string `json:"session_ids"`
				EventTypes []string `json:"event_types"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			query := normalizeQuery(args.Query)
			typeFilter := map[string]bool{}
			for _, t := range args.EventTypes {
				typeFilter[t] = true
			}
			sessionIDs := args.SessionIDs
			if len(sessionIDs) == 0 {
				sessionIDs = []string{ctx.SessionID}
			}
			var lines []string
			for _, sid := range sessionIDs {
				events := sessionQueryEvents(ctx, sid)
				if len(events) == 0 {
					continue
				}
				best := -1
				bestText := ""
				for i, env := range events {
					if len(typeFilter) > 0 && !typeFilter[env.Type] {
						continue
					}
					text := sessionQueryText(env)
					if strings.Contains(text, query) {
						if best < 0 || strings.Index(text, query) < strings.Index(bestText, query) {
							best = i
							bestText = text
						}
					}
				}
				if best < 0 {
					continue
				}
				env := events[best]
				lines = append(lines,
					fmt.Sprintf("Session %s", sid),
					fmt.Sprintf("  Best match: seq %d | %s | %s | %s", env.Seq, env.Type, sessionSurfaceClass(env), formatEventTime(env.Time)),
					fmt.Sprintf("  Snippet: %s", sessionSnippet(bestText, query, 120)),
				)
			}
			if len(lines) == 0 {
				return "No prior session matches found.", nil
			}
			lines = append([]string{fmt.Sprintf("Session search results (%d):", len(lines)/3)}, lines...)
			return strings.Join(lines, "\n"), nil
		},
	})

	r.Register(ToolDefinition{
		Name:        "session_trace",
		Description: "Read the authorized session lineage and event summary around one session, including its surface history and any parent relationship.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"session_id": { "type": "string", "description": "Target session id. Omit for the current session." }
			},
			"required": []
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				SessionID string `json:"session_id"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			sid := args.SessionID
			if sid == "" {
				sid = ctx.SessionID
			}
			events := sessionQueryEvents(ctx, sid)
			ptrs := make([]*session.SessionEnvelope, len(events))
			for i := range events {
				ptrs[i] = &events[i]
			}
			var lines []string
			lines = append(lines, fmt.Sprintf("Session %s", sid))
			if len(events) == 0 {
				lines = append(lines, "  No events available.")
				return strings.Join(lines, "\n"), nil
			}
			first := events[0]
			last := events[len(events)-1]
			lines = append(lines,
				fmt.Sprintf("  Created: %s", formatEventTime(first.Time)),
				fmt.Sprintf("  Events: %d (seq %d..%d)", len(events), first.Seq, last.Seq),
			)
			fold, err := session.FoldSurface(ptrs, first.Seq)
			if err == nil {
				current, shadowed, logOnly := 0, 0, 0
				for i := range events {
					switch sessionSurfaceClass(events[i]) {
					case "current":
						current++
					case "shadowed":
						shadowed++
					default:
						logOnly++
					}
				}
				lines = append(lines,
					fmt.Sprintf("  Surface: %d current, %d shadowed, %d log-only", current, shadowed, logOnly),
					fmt.Sprintf("  Replacements: %d", fold.ReplaceGeneration),
				)
			}
			return strings.Join(lines, "\n"), nil
		},
	})

	r.Register(ToolDefinition{
		Name:        "session_event_read",
		Description: "Read one full unabridged event and optional neighboring raw-event summaries from an authorized session.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"session_id": { "type": "string", "description": "Target session id. Omit for the current session." },
				"seq": { "type": "integer", "description": "Target event sequence number." },
				"before": { "type": "integer", "description": "Number of preceding raw events to summarize. Omit for none." },
				"after": { "type": "integer", "description": "Number of following raw events to summarize. Omit for none." }
			},
			"required": ["seq"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				SessionID string `json:"session_id"`
				Seq       int    `json:"seq"`
				Before    int    `json:"before"`
				After     int    `json:"after"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			if args.Seq < 0 {
				return nil, fmt.Errorf("seq must be non-negative")
			}
			sid := args.SessionID
			if sid == "" {
				sid = ctx.SessionID
			}
			events := sessionQueryEvents(ctx, sid)
			var target *session.SessionEnvelope
			for i := range events {
				if events[i].Seq == args.Seq {
					target = &events[i]
					break
				}
			}
			if target == nil {
				return nil, fmt.Errorf("no event with seq %d in session %s", args.Seq, sid)
			}
			var lines []string
			lines = append(lines,
				fmt.Sprintf("Session %s", sid),
				fmt.Sprintf("Target event seq %d:", target.Seq),
				"```json",
				formatJSON(target.Data),
				"```",
			)
			if args.Before > 0 {
				lines = append(lines, "", "Before:")
				for _, env := range events {
					if env.Seq >= target.Seq {
						break
					}
					if args.Before <= 0 {
						break
					}
					args.Before--
					lines = append(lines, formatNeighbor(&env))
				}
			}
			if args.After > 0 {
				lines = append(lines, "", "After:")
				started := false
				for i := range events {
					if !started {
						if events[i].Seq == target.Seq {
							started = true
						}
						continue
					}
					if args.After <= 0 {
						break
					}
					args.After--
					lines = append(lines, formatNeighbor(&events[i]))
				}
			}
			return strings.Join(lines, "\n"), nil
		},
	})
}

// sessionQueryEvents resolves the ordered event stream for one session,
// preferring the caller's injected stream, then the registered provider.
func sessionQueryEvents(ctx ToolExecutionContext, sessionID string) []session.SessionEnvelope {
	if ctx.Events != nil && sessionID == ctx.SessionID {
		events := ctx.Events()
		if events != nil {
			out := make([]session.SessionEnvelope, 0, len(events))
			for _, e := range events {
				if e != nil {
					out = append(out, *e)
				}
			}
			return out
		}
	}
	events := eventsForSession(sessionID)
	if events == nil {
		return nil
	}
	out := make([]session.SessionEnvelope, 0, len(events))
	for _, e := range events {
		if e != nil {
			out = append(out, *e)
		}
	}
	return out
}

func formatNeighbor(env *session.SessionEnvelope) string {
	text := strings.TrimSpace(sessionQueryText(*env))
	line := fmt.Sprintf("- seq %d | %s | %s", env.Seq, env.Type, formatEventTime(env.Time))
	if text == "" {
		return line + " | (no semantic text)"
	}
	return line + "\n  " + strings.ReplaceAll(text, "\n", "\n  ")
}

// normalizeQuery trims and collapses whitespace in a literal search query.
func normalizeQuery(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// formatJSON renders event payload bytes as indented JSON, tolerating raw
// text payloads by quoting them.
func formatJSON(data json.RawMessage) string {
	if len(data) == 0 {
		return "{}"
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, data, "", "  "); err == nil {
		return buf.String()
	}
	b, _ := json.MarshalIndent(string(data), "", "  ")
	return string(b)
}
