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

// inboundEnsureSession creates or reuses a live actor for an inbound protocol call.
func (s *Server) inboundEnsureSession(id string) (*agent.Agent, string, error) {
	if id == "" {
		id = fmt.Sprintf("ephemeral-%d", time.Now().UnixNano())
	} else if ag, err := s.ensureLiveAgent(id); err == nil {
		return ag, id, nil
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
		return nil, id, fmt.Errorf("failed to start session %s", id)
	}
	return ag, id, nil
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
