package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"dsh-go/pkg/session"
)

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
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
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	msgsRaw := jsonRaw(body["messages"], raw, "messages")
	userText := lastUserTextFromMessages(msgsRaw)
	if userText == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "messages must include a user text turn"}})
		return
	}
	ag, sessionID, freshLog, err := s.inboundEnsureSession(inboundSessionID(r, body))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	w.Header().Set("X-Session-Id", sessionID)
	model := stringField(body, "model", s.configuredModel())
	id := fmt.Sprintf("chatcmpl-%s-%d", sessionID, time.Now().UnixNano())
	created := time.Now().Unix()
	stream := boolField(body, "stream")

	// 无状态多轮投影：全新会话把 system + 全部历史轮次拼进本轮驱动文本；
	// 会话复用（X-Session-Id）保持仅投递最后一条 user 文本的既有行为。
	driveText := userText
	if freshLog {
		if proj := projectInboundConversation(msgsRaw, ""); proj != "" {
			driveText = proj
		}
	}

	if stream {
		flusher := prepareSSE(w)
		var text string
		finish := "stop"
		err = s.inboundTurn(r.Context(), ag, driveText, func(env *session.SessionEnvelope) {
			if delta := chunkDeltaText(env); delta != "" {
				text += delta
				_ = writeSSEData(w, flusher, "", map[string]any{
					"id":      id,
					"object":  "chat.completion.chunk",
					"created": created,
					"model":   model,
					"choices": []map[string]any{{
						"index": 0,
						"delta": map[string]any{
							"role":    "assistant",
							"content": delta,
						},
						"finish_reason": nil,
					}},
				})
			}
			if env.Type == session.EventTurnEnd {
				if kind := turnEndKind(env); kind == "error" {
					finish = "stop"
				} else if kind == "aborted" {
					finish = "stop"
				}
			}
		})
		if err != nil {
			_ = writeSSEData(w, flusher, "", map[string]any{"error": map[string]any{"message": err.Error(), "type": "api_error"}})
			finish = "error"
		}
		_ = writeSSEData(w, flusher, "", map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": finish,
			}},
		})
		_ = writeSSERaw(w, flusher, "data: [DONE]\n\n")
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
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": text,
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
	})
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
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
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	inputRaw := jsonRaw(body["input"], raw, "input")
	userText := responsesInputText(inputRaw)
	if userText == "" {
		userText = lastUserTextFromMessages(jsonRaw(body["messages"], raw, "messages"))
	}
	if userText == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "input must include user text"}})
		return
	}
	ag, sessionID, freshLog, err := s.inboundEnsureSession(inboundSessionID(r, body))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	w.Header().Set("X-Session-Id", sessionID)
	model := stringField(body, "model", s.configuredModel())
	respID := fmt.Sprintf("resp-%s-%d", sessionID, time.Now().UnixNano())
	msgID := fmt.Sprintf("msg_%d", time.Now().UnixNano())
	stream := boolField(body, "stream")

	// 无状态多轮投影（同 chat/completions）：instructions 视为 system 前置；
	// input 为纯字符串时无历史概念，保持原样投递。
	sysExtra := contentToText(body["instructions"])
	driveText := userText
	if freshLog {
		proj := projectInboundConversation(inputRaw, sysExtra)
		if proj == "" {
			proj = projectInboundConversation(jsonRaw(body["messages"], raw, "messages"), sysExtra)
		}
		if proj != "" {
			driveText = proj
		}
	}

	if stream {
		flusher := prepareSSE(w)
		_ = writeSSEData(w, flusher, "response.created", map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": respID, "object": "response", "status": "in_progress", "model": model},
		})
		var text string
		err = s.inboundTurn(r.Context(), ag, driveText, func(env *session.SessionEnvelope) {
			if delta := chunkDeltaText(env); delta != "" {
				text += delta
				_ = writeSSEData(w, flusher, "response.output_text.delta", map[string]any{
					"type":          "response.output_text.delta",
					"item_id":       msgID,
					"output_index":  0,
					"content_index": 0,
					"delta":         delta,
				})
			}
		})
		if err != nil {
			_ = writeSSEData(w, flusher, "error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": err.Error()}})
			return
		}
		_ = writeSSEData(w, flusher, "response.completed", map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":     respID,
				"object": "response",
				"status": "completed",
				"model":  model,
				"output": []map[string]any{{
					"type":    "message",
					"id":      msgID,
					"role":    "assistant",
					"content": []map[string]any{{"type": "output_text", "text": text}},
				}},
			},
		})
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
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":     respID,
		"object": "response",
		"status": "completed",
		"model":  model,
		"output": []map[string]any{{
			"type": "message",
			"id":   msgID,
			"role": "assistant",
			"content": []map[string]any{
				{"type": "output_text", "text": text},
			},
		}},
		"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
	})
}

func jsonRaw(v any, full []byte, key string) json.RawMessage {
	if v == nil && len(full) > 0 {
		var top map[string]json.RawMessage
		if json.Unmarshal(full, &top) == nil {
			return top[key]
		}
	}
	b, _ := json.Marshal(v)
	return b
}
