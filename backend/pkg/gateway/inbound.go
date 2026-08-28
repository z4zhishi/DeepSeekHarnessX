package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"dsh-go/pkg/agent"
	"dsh-go/pkg/llm"
	"dsh-go/pkg/session"
)

// inboundSessionID resolves a session id from X-Session-Id, body metadata,
// or a custom session_id field. Empty means the caller should create ephemeral.
func inboundSessionID(r *http.Request, body map[string]any) string {
	if r != nil {
		if id := strings.TrimSpace(r.Header.Get("X-Session-Id")); id != "" {
			return id
		}
		if id := strings.TrimSpace(r.Header.Get("X-Session-ID")); id != "" {
			return id
		}
	}
	if body == nil {
		return ""
	}
	if id, _ := body["session_id"].(string); strings.TrimSpace(id) != "" {
		return strings.TrimSpace(id)
	}
	if id, _ := body["sessionId"].(string); strings.TrimSpace(id) != "" {
		return strings.TrimSpace(id)
	}
	meta, _ := body["metadata"].(map[string]any)
	if meta == nil {
		return ""
	}
	if id, _ := meta["session_id"].(string); strings.TrimSpace(id) != "" {
		return strings.TrimSpace(id)
	}
	if id, _ := meta["sessionId"].(string); strings.TrimSpace(id) != "" {
		return strings.TrimSpace(id)
	}
	return ""
}

// inboundEnsureSession creates or reuses a live actor for an inbound protocol
// call. freshLog reports whether the underlying durable log carries no prior
// conversation: only then may the caller safely prepend projected client-side
// history. Reused live actors and stored sessions derive history from the log,
// so they must keep the legacy single-last-user-text delivery.
func (s *Server) inboundEnsureSession(id string) (*agent.Agent, string, bool, error) {
	if s.inboundEnsureSessionFn != nil {
		return s.inboundEnsureSessionFn(id)
	}
	freshLog := true
	if id == "" {
		id = fmt.Sprintf("ephemeral-%d", time.Now().UnixNano())
	} else {
		if ag, err := s.ensureLiveAgent(id); err == nil {
			return ag, id, false, nil
		}
		// A stored-but-stopped session: its durable log already carries the
		// prior turns, so the incoming request must not duplicate them.
		if s.Store != nil {
			if evs, gerr := s.Store.GetEvents(id, 0); gerr == nil && len(evs) > 0 {
				freshLog = false
			}
		}
	}
	header := session.SessionHeader{
		ID:        id,
		CreatedAt: time.Now().UnixMilli(),
		Cwd:       ".",
		Origin:    "inbound",
	}
	if s.Store != nil {
		_ = s.Store.PutSession(&header)
	}
	s.spawnAgent(header)
	s.mu.RLock()
	ag := s.agents[id]
	s.mu.RUnlock()
	if ag == nil || !ag.Alive() {
		return nil, id, false, fmt.Errorf("failed to start session %s", id)
	}
	return ag, id, freshLog, nil
}

func lastUserTextFromMessages(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var messages []map[string]any
	if err := json.Unmarshal(raw, &messages); err != nil {
		return ""
	}
	for i := len(messages) - 1; i >= 0; i-- {
		role, _ := messages[i]["role"].(string)
		if role != "user" {
			continue
		}
		if t := contentToText(messages[i]["content"]); t != "" {
			return t
		}
	}
	return ""
}

func contentToText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			m, ok := part.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			if typ == "" || typ == "text" || typ == "input_text" || typ == "output_text" {
				if t, _ := m["text"].(string); t != "" {
					if b.Len() > 0 {
						b.WriteByte('\n')
					}
					b.WriteString(t)
				}
			}
		}
		return b.String()
	case map[string]any:
		if t, _ := v["text"].(string); t != "" {
			return t
		}
	}
	return ""
}

// --- 入站多轮上下文投影（stateless messages 完整投影） ---
//
// Agent 的 ModelRequest.Messages 完全由持久化日志派生，pkg/agent（请求级
// 注入点的所在）不在本修复白名单内；向存储预写客户端伪造的 assistant 轮次
// 又会污染持久化会话日志。因此对全新会话采用规格允许的降级方案：把 system
// + 多轮 user/assistant 文本拼接为单条驱动 user 消息投影。会话复用
// （X-Session-Id 命中 live actor 或存储已有历史）保持原行为——仅投递最后一条
// user 文本，历史由日志自然携带。

type inboundTurnText struct {
	role string
	text string
}

// parseInboundTurns extracts (role, text) pairs from an OpenAI Chat
// Completions / Anthropic / Responses-style messages array. Responses input
// items ({type:"message", role, content}) share the shape; entries without a
// role or extractable text are skipped.
func parseInboundTurns(raw json.RawMessage) []inboundTurnText {
	if len(raw) == 0 {
		return nil
	}
	var messages []map[string]any
	if err := json.Unmarshal(raw, &messages); err != nil {
		return nil
	}
	out := make([]inboundTurnText, 0, len(messages))
	for _, m := range messages {
		role, _ := m["role"].(string)
		role = strings.ToLower(strings.TrimSpace(role))
		text := contentToText(m["content"])
		if role == "" || text == "" {
			continue
		}
		out = append(out, inboundTurnText{role: role, text: text})
	}
	return out
}

// projectInboundConversation renders system + multi-turn user/assistant history
// as the driving user text for a stateless request. A single-turn, system-free
// payload round-trips verbatim (legacy byte-compat); anything richer becomes
// labeled transcript lines ending on the latest user turn.
func projectInboundConversation(raw json.RawMessage, extraSystem string) string {
	var sysParts []string
	if s := strings.TrimSpace(extraSystem); s != "" {
		sysParts = append(sysParts, s)
	}
	var convo []inboundTurnText
	for _, t := range parseInboundTurns(raw) {
		switch t.role {
		case "system", "developer":
			sysParts = append(sysParts, t.text)
		case "user", "assistant":
			convo = append(convo, t)
		}
	}
	sys := strings.Join(sysParts, "\n\n")
	switch {
	case len(convo) == 0:
		return ""
	case sys == "" && len(convo) == 1:
		return convo[0].text
	}
	var b strings.Builder
	if sys != "" {
		b.WriteString("system: ")
		b.WriteString(sys)
		b.WriteString("\n\n")
	}
	for _, t := range convo {
		b.WriteString(t.role)
		b.WriteString(": ")
		b.WriteString(t.text)
		b.WriteString("\n\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func responsesInputText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	if t := lastUserTextFromMessages(raw); t != "" {
		return t
	}
	return contentToText(rawToAny(raw))
}

func rawToAny(raw json.RawMessage) any {
	var v any
	_ = json.Unmarshal(raw, &v)
	return v
}

func decodeJSONBody(r *http.Request) (map[string]any, []byte, error) {
	if r.Body == nil {
		return map[string]any{}, nil, nil
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, nil, err
	}
	if len(data) == 0 {
		return map[string]any{}, data, nil
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, data, err
	}
	if body == nil {
		body = map[string]any{}
	}
	return body, data, nil
}

func boolField(body map[string]any, key string) bool {
	v, ok := body[key]
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

func stringField(body map[string]any, key, def string) string {
	if v, _ := body[key].(string); v != "" {
		return v
	}
	return def
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeSSEData(w http.ResponseWriter, flusher http.Flusher, event string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var b strings.Builder
	if event != "" {
		b.WriteString("event: ")
		b.WriteString(event)
		b.WriteByte('\n')
	}
	b.WriteString("data: ")
	b.Write(data)
	b.WriteString("\n\n")
	if _, err := io.WriteString(w, b.String()); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

func writeSSERaw(w http.ResponseWriter, flusher http.Flusher, line string) error {
	if _, err := io.WriteString(w, line); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

func chunkDeltaText(env *session.SessionEnvelope) string {
	if env == nil || env.Type != session.EventAssistantChunk {
		return ""
	}
	var payload struct {
		Chunk llm.StreamChunk `json:"chunk"`
	}
	if err := json.Unmarshal(env.Data, &payload); err != nil {
		return ""
	}
	if payload.Chunk.Type == llm.ChunkTextDelta {
		return payload.Chunk.Text
	}
	return ""
}

func assistantMessageText(env *session.SessionEnvelope) string {
	if env == nil || env.Type != session.EventAssistantMessage {
		return ""
	}
	var p session.AssistantMessagePayload
	if err := json.Unmarshal(env.Data, &p); err != nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range p.Message.Content {
		if blk.Type == "text" && blk.Text != "" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

func turnEndKind(env *session.SessionEnvelope) string {
	if env == nil || env.Type != session.EventTurnEnd {
		return ""
	}
	var p session.TurnEndPayload
	if err := json.Unmarshal(env.Data, &p); err != nil {
		return ""
	}
	return p.Reason.Kind
}

// inboundTurn drives one user prompt through the agent and invokes onEvent
// for every envelope after the prompt is admitted, until turn/end.
func (s *Server) inboundTurn(ctx context.Context, ag *agent.Agent, userText string, onEvent func(*session.SessionEnvelope)) error {
	if s.inboundTurnFn != nil {
		return s.inboundTurnFn(ctx, ag, userText, onEvent)
	}
	if ag == nil {
		return fmt.Errorf("session actor missing")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sub := ag.Subscribe()
	defer ag.Unsubscribe(sub)

	posted := false
	tryPost := func() {
		if posted || !ag.Alive() {
			return
		}
		if ag.IsRunning() {
			return
		}
		ag.PostUserMessage(session.UserMessagePayload{
			ID:   fmt.Sprintf("inb-%d", time.Now().UnixNano()),
			Role: "user",
			Content: []session.ContentBlock{
				{Type: "text", Text: userText},
			},
			Source: session.MessageSource{Kind: "user"},
		})
		posted = true
	}
	tryPost()

	timeout := time.NewTimer(90 * time.Second)
	defer timeout.Stop()

	for {
		select {
		case <-ctx.Done():
			ag.AbortTurn()
			return ctx.Err()
		case <-timeout.C:
			ag.AbortTurn()
			return fmt.Errorf("inbound turn timeout")
		case env, ok := <-sub:
			if !ok {
				return fmt.Errorf("session closed")
			}
			if !posted {
				if env.Type == session.EventTurnEnd || !ag.IsRunning() {
					tryPost()
				}
				continue
			}
			onEvent(env)
			if env.Type == session.EventTurnEnd {
				if kind := turnEndKind(env); kind != "" && kind != "completed" {
					var reason session.TurnEndReason
					var payload session.TurnEndPayload
					if err := json.Unmarshal(env.Data, &payload); err == nil {
						reason = payload.Reason
					}
					message := reason.Message
					if message == "" {
						message = fmt.Sprintf("inbound turn ended with %s", kind)
					}
					return fmt.Errorf("%s: %s", kind, message)
				}
				return nil
			}
			if !ag.Alive() {
				return fmt.Errorf("session stopped")
			}
		}
	}
}

func prepareSSE(w http.ResponseWriter) http.Flusher {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	return flusher
}
