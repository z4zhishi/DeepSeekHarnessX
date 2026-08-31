// Package importcc implements the ecosystem-收编 session importer
// (docs/supremacy-plan.md §2 "会话历史", docs/product-strategy.md §3):
// `dshx import --from claude` reads Claude Code session jsonl files
// (`~/.claude/projects/<enc-cwd>/<session-uuid>.jsonl`) and sides them into
// the DSHX sqlite store through the NORMAL write path
// (storage.OpenSqliteStore + AppendEvents). No new storage surface, no schema
// change: imported logs are ordinary DSH event logs that DeriveMessages and
// the GUI fold already know how to replay.
//
// Idempotency contract: each import stamps the durable session header's
// Origin with "claude-import:<source-uuid>" (Origin is the provenance field —
// "team"/"headless"/"inbound" today — and the source uuid is precisely the
// provenance of an imported session). A second run skips any file whose
// filename uuid already has that marker, so re-running `dshx import` never
// duplicates a session.
package importcc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"dsh-go/pkg/session"
)

// OriginMarkerPrefix marks an imported session's provenance in
// SessionHeader.Origin; the suffix is the source harness's session uuid.
const OriginMarkerPrefix = "claude-import:"

// sourceUUIDOf extracts the original session uuid from an imported header's
// Origin ("claude-import:<uuid>" -> "<uuid>"); ok=false for non-import origins.
func sourceUUIDOf(origin string) (string, bool) {
	if !strings.HasPrefix(origin, OriginMarkerPrefix) {
		return "", false
	}
	uuid := strings.TrimPrefix(origin, OriginMarkerPrefix)
	if uuid == "" {
		return "", false
	}
	return uuid, true
}

// ---------------------------------------------------------------------------
// Claude Code session jsonl: physical line shapes
// ---------------------------------------------------------------------------

// ccLine is the tolerant shape of one CC session file line. Only the fields
// the importer consumes are decoded; everything else (origin, promptId,
// permissionMode, entrypoint, gitBranch, version, toolUseResult,
// file-history snapshots, ...) is intentionally ignored — unknown line types
// and unknown keys must never fail an import.
type ccLine struct {
	Type      string `json:"type"`
	UUID      string `json:"uuid,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	Cwd       string `json:"cwd,omitempty"`

	// Sidechain lines are subagent scratch transcripts; import skips and
	// counts them (display order uses the main chain only).
	IsSidechain bool `json:"isSidechain"`
	// isMeta / isCompactSummary / isVisibleInTranscriptOnly mark harness
	// injections (command caveats, goal-hook copy, compaction artifacts) that
	// are NOT user chat input; importing them would render pseudo user rows.
	IsMeta                    bool `json:"isMeta"`
	IsCompactSummary          bool `json:"isCompactSummary"`
	IsVisibleInTranscriptOnly bool `json:"isVisibleInTranscriptOnly"`

	// Title line variants (last one in file order wins, custom overrides AI).
	CustomTitle string `json:"customTitle,omitempty"`
	AITitle     string `json:"aiTitle,omitempty"`

	Message json.RawMessage `json:"message,omitempty"`
}

// ccBlock is one Anthropic wire content block (message.content element).
// tool_result.content is heterogeneous: a bare string, an array of blocks, or
// absent; it is kept raw and decoded per-block.
type ccBlock struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	Thinking   string          `json:"thinking,omitempty"`
	ID         string          `json:"id,omitempty"`      // tool_use call id
	Name       string          `json:"name,omitempty"`    // tool_use tool name
	Input      json.RawMessage `json:"input,omitempty"`   // tool_use arguments object
	ToolUseID  string          `json:"tool_use_id,omitempty"` // tool_result back-reference
	Content    json.RawMessage `json:"content,omitempty"`     // tool_result payload
	IsError    bool            `json:"is_error,omitempty"`
	Error      string          `json:"error,omitempty"` // tool_result textual error
}

// ccMessage is the raw Anthropic wire message on user/assistant lines.
type ccMessage struct {
	ID      string    `json:"id,omitempty"`
	Role    string    `json:"role,omitempty"`
	Model   string    `json:"model,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
	Usage   ccUsage   `json:"usage,omitempty"`
}

// ccUsage maps the Anthropic usage counters onto DSHX TokenUsage fields.
type ccUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// ---------------------------------------------------------------------------
// Conversion result
// ---------------------------------------------------------------------------

// ConvertResult is the pure CC-file -> envelope translation output. No store
// is involved; the caller (Import) turns it into a durable append.
type ConvertResult struct {
	// SourceID is the file's session uuid (filename stem). SessionID is the
	// DSH session id used for the header (first in-file sessionId, else the
	// filename stem).
	SourceID  string
	SessionID string

	Envelopes  []*session.SessionEnvelope
	CreatedAt  int64 // unix ms; first in-file timestamp, else wall clock
	Cwd        string
	Title      string

	Lines           int            // non-empty lines attempted
	Skipped         int            // lines producing zero envelopes (excl. sidechains)
	Sidechain       int            // isSidechain lines (skipped)
	BadJSON         int            // unparseable lines (also counted in Skipped)
	Counts          map[string]int // skipped/observed line kinds, by kind
}

// Header builds the durable session header for a converted file. The import
// marker lives in Origin; Version stays 0 like every other caller.
func (r *ConvertResult) Header() *session.SessionHeader {
	return &session.SessionHeader{
		Version:   0,
		ID:        r.SessionID,
		CreatedAt: r.CreatedAt,
		Cwd:       r.Cwd,
		Origin:    OriginMarkerPrefix + r.SourceID,
	}
}

// ---------------------------------------------------------------------------
// Line-level conversion
// ---------------------------------------------------------------------------

// claudeConverter carries per-file translation state (seq/turn/step/timestamps
// and the observed-kind counts).
type claudeConverter struct {
	seq      int
	turn     int // synthetic DSH turn: bumped on each real user message
	step     int // synthetic step: bumped on each assistant message
	prevTime int64

	createdAt int64
	cwd       string
	sessID    string
	title     string
	haveTitle bool

	result ConvertResult
}

const maxCCLineBytes = 64 << 20 // CC tool payloads can be multi-MB; cap generously

// ConvertClaudeJSONL converts one CC session file body (an io.Reader of jsonl
// lines) into DSH envelopes in file order. Unknown/failed lines are
// skip-counted, never fatal. A body with zero mappable lines yields a result
// with no envelopes (the caller reports it as "empty" and writes nothing).
func ConvertClaudeJSONL(r io.Reader, fileUUID string) (*ConvertResult, error) {
	c := &claudeConverter{
		result: ConvertResult{
			SourceID: fileUUID,
			Counts:   map[string]int{},
		},
	}
	if fileUUID == "" {
		c.result.SourceID = "unknown"
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxCCLineBytes)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		c.convertLine(line)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading claude session file: %w", err)
	}
	c.finish()
	return &c.result, nil
}

func (c *claudeConverter) count(kind string) {
	c.result.Counts[kind]++
}

// convertLine handles one physical line. Line-level failures are skip-counted,
// never fatal (import must tolerate unknown CC line types and torn lines).
func (c *claudeConverter) convertLine(line string) {
	c.result.Lines++

	var ln ccLine
	if err := json.Unmarshal([]byte(line), &ln); err != nil {
		c.result.Skipped++
		c.result.BadJSON++
		c.count("badjson")
		return
	}
	c.noteMeta(&ln)

	// Subagent sidechains are separate scratch transcripts, not main-chain
	// display history; skip and count, never fail.
	if ln.IsSidechain {
		c.result.Sidechain++
		c.result.Skipped++
		c.count("sidechain")
		return
	}

	switch ln.Type {
	case "user":
		c.translateUser(&ln)
	case "assistant":
		c.translateAssistant(&ln)
	case "custom-title":
		if t := strings.TrimSpace(ln.CustomTitle); t != "" {
			c.title, c.haveTitle = t, true
		}
	case "ai-title":
		if !c.haveTitle {
			if t := strings.TrimSpace(ln.AITitle); t != "" {
				c.title, c.haveTitle = t, true
			}
		}
	default:
		// system, attachment, mode, permission-mode, queue-operation,
		// file-history-snapshot, last-prompt, agent-name, summary, and any
		// future CC line type: skip by kind, never fail.
		c.result.Skipped++
		kind := ln.Type
		if kind == "" {
			kind = "<typeless>"
		}
		c.count(kind)
	}
}

// noteMe records the file-level metadata the header needs (first cwd, first
// timestamp, session id) from ANY line, mappable or not.
func (c *claudeConverter) noteMeta(ln *ccLine) {
	if ln.SessionID != "" && c.result.SessionID == "" {
		c.result.SessionID = ln.SessionID
	}
	if ln.Cwd != "" && c.result.Cwd == "" {
		c.result.Cwd = ln.Cwd
	}
	if ts := parseCCCTimestamp(ln.Timestamp); ts > 0 && c.createdAt == 0 {
		c.createdAt = ts
	}
}

// envelopeTime translates a line timestamp into envelope Time (unix ms),
// falling back to the previous line's time so a log never regresses. A file
// with no timestamps at all ends up with plain 0 times — acceptable for a
// historical import.
func (c *claudeConverter) envelopeTime(ts string) int64 {
	if t := parseCCCTimestamp(ts); t > 0 {
		c.prevTime = t
		return t
	}
	return c.prevTime
}

// parseCCCTimestamp parses a CC RFC3339 timestamp ("2026-08-28T13:30:54.630Z")
// into unix milliseconds; 0 when absent/invalid.
func parseCCCTimestamp(ts string) int64 {
	if ts == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// nextSeq assigns the next log seq; DSH logs are contiguous from 0.
func (c *claudeConverter) nextSeq() int {
	seq := c.seq
	c.seq++
	return seq
}

// logEnvelope appends a log-only (non-surface) envelope.
func (c *claudeConverter) logEnvelope(ts int64, eventType string, payload any) *session.SessionEnvelope {
	env := c.envelope(ts, eventType, payload)
	c.result.Envelopes = append(c.result.Envelopes, env)
	return env
}

func (c *claudeConverter) envelope(ts int64, eventType string, payload any) *session.SessionEnvelope {
	data, err := json.Marshal(payload)
	if err != nil {
		// Payloads are our own structs; a marshal failure is an import bug,
		// tolerated as a skipped line rather than a corrupted log.
		return nil
	}
	env := &session.SessionEnvelope{
		Seq:  c.nextSeq(),
		Time: ts,
		Type: eventType,
		Data: data,
	}
	return env
}

// effectiveTurn lazily opens turn 1 for logs that start mid-conversation
// (assistant/tool-result first lines after a skipped compaction preamble).
func (c *claudeConverter) effectiveTurn() int {
	if c.turn == 0 {
		c.turn = 1
	}
	return c.turn
}

// ---------------------------------------------------------------------------
// user lines
// ---------------------------------------------------------------------------

// translateUser maps one CC user line. Real user text becomes ONE user/message
// envelope (exact DSHX user-turn payload shape); tool_result blocks become
// tool/result envelopes (the pair the GUI fold and DeriveMessages expect for
// tool traffic). A line whose only content is non-textual (images) is
// skip-counted.
func (c *claudeConverter) translateUser(ln *ccLine) {
	// Harness-injected pseudo user lines (command caveats, hook/goal
	// injections, compaction summaries) are not user chat input; importing
	// them would render fake user rows. Skip with kind counts.
	if ln.IsMeta || ln.IsCompactSummary || ln.IsVisibleInTranscriptOnly {
		c.skipLine("user(isMeta)")
		return
	}
	msg, ok := decodeCCMessage(ln.Message)
	if !ok {
		c.skipLine("user")
		return
	}
	content, err := decodeBlocks(msg.Content)
	if err != nil {
		// Content is neither a string nor an array of blocks: preserve the
		// line kind for diagnosis, import nothing.
		c.skipLine("user")
		return
	}

	var textParts []string
	var toolResults []ccBlock
	for _, b := range content {
		switch b.Type {
		case "text":
			if s := strings.TrimSpace(b.Text); s != "" {
				textParts = append(textParts, b.Text)
			}
		case "tool_result":
			toolResults = append(toolResults, b)
		case "image":
			c.count("block:image")
		default:
			c.count("block:" + b.Type)
		}
	}

	ts := c.envelopeTime(ln.Timestamp)
	for i, tr := range toolResults {
		env := c.envelope(ts, session.EventToolResult, c.toolResultPayload(ln, msg, tr, i))
		if env == nil {
			continue
		}
		op := session.AppendSurfaceOp
		env.SurfaceOp = &op
		c.result.Envelopes = append(c.result.Envelopes, env)
	}

	if len(textParts) == 0 {
		if len(toolResults) > 0 {
			return // tool-result-only lines mapped fully above
		}
		c.skipLine("user")
		return
	}

	// A real user turn: bump the synthetic turn (upstream turns are 1-based),
	// zero the step counter for the coming assistant steps.
	c.turn++
	c.step = 0
	env := c.envelope(ts, session.EventUserMessage, session.UserMessagePayload{
		ID:   msgIDOr(ln.UUID, fmt.Sprintf("user-%d", c.seq)),
		Role: "user",
		Content: []session.ContentBlock{
			{Type: "text", Text: strings.Join(textParts, "\n")},
		},
		Source: session.MessageSource{Kind: "user"},
	})
	if env == nil {
		c.skipLine("user")
		return
	}
	op := session.AppendSurfaceOp
	env.SurfaceOp = &op
	c.result.Envelopes = append(c.result.Envelopes, env)
}

// toolResultPayload builds the DSHX tool/result payload for one CC
// tool_result block — the same shape the live loop emits (user-role message
// carrying a tool-result block, per-call id, ToolCallID back-reference).
func (c *claudeConverter) toolResultPayload(ln *ccLine, msg *ccMessage, tr ccBlock, idx int) session.ToolResultPayload {
	callID := tr.ToolUseID
	if callID == "" {
		callID = msgIDOr(ln.UUID, fmt.Sprintf("user-%d", c.seq))
		if idx > 0 {
			callID = fmt.Sprintf("%s-%d", callID, idx)
		}
	}
	return session.ToolResultPayload{
		Turn: c.effectiveTurn(),
		Step: c.step,
		Message: session.WireMessage{
			ID:      fmt.Sprintf("tool-%d-%d-%s", c.effectiveTurn(), c.step, callID),
			Role:    "user",
			Content: []session.ContentBlock{toolResultBlockOf(tr)},
			Source:  session.MessageSource{Kind: "tool", CallID: callID},
		},
	}
}

// toolResultBlockOf converts one CC tool_result block. Its content is either a
// plain string or an array of content blocks; both fold into text blocks. A
// result without content still renders (empty text), matching live tools that
// produce empty output.
func toolResultBlockOf(tr ccBlock) session.ContentBlock {
	out := session.ContentBlock{
		Type:      "tool-result",
		ToolCallID: tr.ToolUseID,
		IsError:   tr.IsError,
	}
	content := strings.TrimSpace(string(tr.Content))
	switch {
	case content == "" || content == "null":
		out.Content = []session.ContentBlock{{Type: "text", Text: ""}}
	case strings.HasPrefix(content, `"`):
		var s string
		_ = json.Unmarshal(tr.Content, &s)
		out.Content = []session.ContentBlock{{Type: "text", Text: s}}
	default:
		var blocks []map[string]any
		if err := json.Unmarshal(tr.Content, &blocks); err != nil {
			out.Content = []session.ContentBlock{{Type: "text", Text: content}}
			break
		}
		for _, b := range blocks {
			btype, _ := b["type"].(string)
			switch btype {
			case "text", "":
				text, _ := b["text"].(string)
				out.Content = append(out.Content, session.ContentBlock{Type: "text", Text: text})
			case "image":
				// Attachment blobs have no DSHX attachment backing; keep a
				// visibly honest placeholder instead of dropping silently.
				out.Content = append(out.Content, session.ContentBlock{Type: "text", Text: "[image attachment imported from Claude Code]"})
			default:
				out.Content = append(out.Content, session.ContentBlock{Type: "text", Text: "[" + btype + "]"})
			}
		}
		if len(out.Content) == 0 {
			out.Content = []session.ContentBlock{{Type: "text", Text: ""}}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// assistant lines
// ---------------------------------------------------------------------------

// translateAssistant maps one CC assistant line to at most ONE assistant/
// message envelope (text + thinking blocks folded in source order — the
// pragmatic v0 contract: CC lines are not streaming chunks) followed by one
// tool/call envelope per tool_use block (log-only, exactly like the live loop
// which emits the message first, then the calls).
func (c *claudeConverter) translateAssistant(ln *ccLine) {
	msg, ok := decodeCCMessage(ln.Message)
	if !ok {
		c.skipLine("assistant")
		return
	}
	blocks, err := decodeBlocks(msg.Content)
	if err != nil {
		c.skipLine("assistant")
		return
	}

	ts := c.envelopeTime(ln.Timestamp)
	model := msg.Model

	var content []session.ContentBlock
	var toolUses []ccBlock
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			content = append(content, session.ContentBlock{Type: "text", Text: b.Text})
		case "thinking":
			if strings.TrimSpace(b.Thinking) != "" {
				content = append(content, session.ContentBlock{Type: "reasoning", Text: b.Thinking})
			}
		case "tool_use":
			toolUses = append(toolUses, b)
		case "redacted_thinking":
			// Opaque encrypted blob, not renderable text; skip with count.
			c.count("block:redacted_thinking")
		case "image":
			c.count("block:image")
		default:
			c.count("block:" + b.Type)
		}
	}

	// assistant/message first (live ordering), only when it has content — an
	// empty-content assistant/message is a max-tokens usage carrier upstream
	// and must not inject a content-less history turn.
	if len(content) > 0 {
		c.step++ // synthetic step identity for display ordering only
		env := c.envelope(ts, session.EventAssistantMessage, session.AssistantMessagePayload{
			Turn: c.effectiveTurn(),
			Step: c.step,
			Message: session.WireMessage{
				ID:      msgIDOr(msg.ID, msgIDOr(ln.UUID, fmt.Sprintf("asst-%d-%d", c.turn, c.step))),
				Role:    "assistant",
				Content: content,
				// Provider/Model mirror the live loop's (Provider == Model)
				// wire shape; the model is the CC harness's own model id.
				Source: session.MessageSource{Kind: "model", Provider: model, Model: model},
			},
			Usage: &session.TokenUsage{
				InputTokens:      msg.Usage.InputTokens,
				OutputTokens:     msg.Usage.OutputTokens,
				CacheReadTokens:  msg.Usage.CacheReadInputTokens,
				CacheWriteTokens: msg.Usage.CacheCreationInputTokens,
			},
		})
		if env != nil {
			op := session.AppendSurfaceOp
			env.SurfaceOp = &op
			c.result.Envelopes = append(c.result.Envelopes, env)
		}
	}

	for _, tu := range toolUses {
		args := strings.TrimSpace(string(tu.Input))
		if args == "" {
			args = "{}"
		}
		callID := tu.ID
		if callID == "" {
			callID = msgIDOr(ln.UUID, fmt.Sprintf("call-%d", c.seq))
		}
		c.logEnvelope(ts, session.EventToolCall, session.ToolCallPayload{
			Turn:      c.effectiveTurn(),
			Step:      c.step,
			CallID:    callID,
			Name:      tu.Name,
			Arguments: args,
		})
	}

	if len(content) == 0 && len(toolUses) == 0 {
		c.skipLine("assistant")
	}
}

// ---------------------------------------------------------------------------
// finishing
// ---------------------------------------------------------------------------

// finish closes the conversion: resolves remaining identity defaults and
// appends the CC title as a single log-only session/title snapshot (same
// payload shape the gateway's session.rename writes; the GUI renders the
// latest title event as the session name).
func (c *claudeConverter) finish() {
	if c.result.SessionID == "" {
		c.result.SessionID = c.result.SourceID
	}
	if c.createdAt == 0 {
		c.createdAt = time.Now().UnixMilli()
	}
	c.result.CreatedAt = c.createdAt
	c.result.Title = c.title
	if strings.TrimSpace(c.title) == "" {
		return
	}
	title := strings.TrimSpace(c.title)
	data, err := json.Marshal(map[string]any{
		"title":       title,
		"messageSeqs": []int{},
		"source":      map[string]any{"kind": "user"},
	})
	if err != nil {
		return
	}
	c.result.Envelopes = append(c.result.Envelopes, &session.SessionEnvelope{
		Seq:  c.nextSeq(),
		Time: c.prevTime,
		Type: session.EventSessionTitle,
		Data: data,
	})
}

func msgIDOr(primary, fallback string) string {
	if p := strings.TrimSpace(primary); p != "" {
		return p
	}
	return fallback
}

// skipLine records a mappable-type line that ultimately produced no envelope.
func (c *claudeConverter) skipLine(kind string) {
	if kind == "" {
		kind = "<typeless>"
	}
	c.count(kind)
	c.result.Skipped++
}

// ---------------------------------------------------------------------------
// message/content decoding
// ---------------------------------------------------------------------------

// decodeCCMessage parses the raw `message` object on a user/assistant line.
// Lines without a well-formed message object fail the mapping (skip-counted
// by the caller); they never abort the import.
func decodeCCMessage(raw json.RawMessage) (*ccMessage, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var msg ccMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, false
	}
	return &msg, true
}

type rawContentBlock = map[string]any

// decodeBlocks normalizes message.content, which CC encodes either as a plain
// string, an array of typed blocks, or (rarely) null. Returns a canonical
// block list; string content becomes a single text block.
func decodeBlocks(raw json.RawMessage) ([]ccBlock, error) {
	if len(raw) == 0 {
		return nil, nil // null / absent content
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "null" {
		return nil, nil
	}
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return []ccBlock{{Type: "text", Text: s}}, nil
	}
	var arr []rawContentBlock
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, err
	}
	out := make([]ccBlock, 0, len(arr))
	for _, m := range arr {
		b := ccBlock{}
		b.Type, _ = m["type"].(string)
		b.Text, _ = m["text"].(string)
		b.Thinking, _ = m["thinking"].(string)
		b.ID, _ = m["id"].(string)
		b.Name, _ = m["name"].(string)
		b.ToolUseID, _ = m["tool_use_id"].(string)
		if v, ok := m["is_error"].(bool); ok {
			b.IsError = v
		}
		if v, ok := m["input"]; ok {
			b.Input, _ = json.Marshal(v)
		}
		if v, ok := m["content"]; ok {
			b.Content, _ = json.Marshal(v)
		}
		out = append(out, b)
	}
	return out, nil
}