package gateway

import (
	"fmt"
	"net/http"
	"time"

	"dsh-go/pkg/session"
)

func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, raw, err := decodeJSONBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": err.Error()}})
		return
	}
	msgsRaw := jsonRaw(body["messages"], raw, "messages")
	userText := lastUserTextFromMessages(msgsRaw)
	if userText == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": "messages must include a user text turn"}})
		return
	}
	ag, sessionID, freshLog, err := s.inboundEnsureSession(inboundSessionID(r, body))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": err.Error()}})
		return
	}
	w.Header().Set("X-Session-Id", sessionID)
	model := stringField(body, "model", s.configuredModel())
	msgID := fmt.Sprintf("msg_%d", time.Now().UnixNano())
	stream := boolField(body, "stream")

	// 无状态多轮投影（同 chat/completions）：Anthropic 顶层 system（字符串
	// 或块数组）作为 system 前置参与拼接；会话复用路径保持原行为。
	sysExtra := contentToText(body["system"])
	driveText := userText
	if freshLog {
		if proj := projectInboundConversation(msgsRaw, sysExtra); proj != "" {
			driveText = proj
		}
	}

	if stream {
		flusher := prepareSSE(w)
		_ = writeSSEData(w, flusher, "message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":      msgID,
				"type":    "message",
				"role":    "assistant",
				"content": []any{},
				"model":   model,
				"usage":   map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		})
		_ = writeSSEData(w, flusher, "content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         0,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		var text string
		_ = s.inboundTurn(r.Context(), ag, driveText, func(env *session.SessionEnvelope) {
			if delta := chunkDeltaText(env); delta != "" {
				text += delta
				_ = writeSSEData(w, flusher, "content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": 0,
					"delta": map[string]any{"type": "text_delta", "text": delta},
				})
			}
		})
		_ = writeSSEData(w, flusher, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		_ = writeSSEData(w, flusher, "message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": 0},
		})
		_ = writeSSEData(w, flusher, "message_stop", map[string]any{"type": "message_stop"})
		_ = text
		return
	}

	var fromChunks, fromMessage string
	err = s.inboundTurn(r.Context(), ag, driveText, func(env *session.SessionEnvelope) {
		if t := assistantMessageText(env); t != "" {
			fromMessage += t
		}
		if d := chunkDeltaText(env); d != "" {
			fromChunks += d
		}
	})
	text := fromMessage
	if text == "" {
		text = fromChunks
	}
	if err != nil && text == "" {
		writeJSON(w, http.StatusBadGateway, map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": err.Error()}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":          msgID,
		"type":        "message",
		"role":        "assistant",
		"content":     []map[string]any{{"type": "text", "text": text}},
		"model":       model,
		"stop_reason": "end_turn",
		"usage": map[string]any{
			"input_tokens":  0,
			"output_tokens": 0,
		},
	})
}
