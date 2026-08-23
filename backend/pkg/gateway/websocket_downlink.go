package gateway

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"dsh-go/pkg/session"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		// 与 HTTP 信任栅栏一致：只接受 loopback Host + loopback/无 Origin。
		// Godot 原生客户端（无 Origin）与本地浏览器（http://127.0.0.1）放行；
		// 跨站页面发起的 WS（CSRF/DNS rebinding）被拒。
		if !isLoopbackHost(r.Host) {
			return false
		}
		return originAllowed(r.Header.Get("Origin"))
	},
}

// DownlinkHub manages active WebSocket downlink connections.
type DownlinkHub struct {
	muxClients  map[*websocket.Conn]string // conn -> subscribed sessionId
	hostClients map[*websocket.Conn]bool
	mu          sync.RWMutex
}

// NewDownlinkHub creates a new downlink hub.
func NewDownlinkHub() *DownlinkHub {
	return &DownlinkHub{
		muxClients:  make(map[*websocket.Conn]string),
		hostClients: make(map[*websocket.Conn]bool),
	}
}

// HandleMux handles /api/events/mux downlinks (session events stream).
func (h *DownlinkHub) HandleMux(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	sessionID := r.URL.Query().Get("sessionId")

	h.mu.Lock()
	h.muxClients[conn] = sessionID
	h.mu.Unlock()

	// Downlink-only enforcement: if client sends any packet, reject with 1008 (policy violation)
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			break
		}
		// Client violated downlink-only rule
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(1008, "downlink only"),
			time.Now().Add(time.Second))
		break
	}

	h.mu.Lock()
	delete(h.muxClients, conn)
	h.mu.Unlock()
}

// HandleHost handles /api/events/host downlinks (workspace/host lifecycle stream).
func (h *DownlinkHub) HandleHost(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	h.mu.Lock()
	h.hostClients[conn] = true
	h.mu.Unlock()

	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			break
		}
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(1008, "downlink only"),
			time.Now().Add(time.Second))
		break
	}

	h.mu.Lock()
	delete(h.hostClients, conn)
	h.mu.Unlock()
}

// BroadcastSessionEvent sends a session event to clients subscribed to that session.
func (h *DownlinkHub) BroadcastSessionEvent(sessionID string, env *session.SessionEnvelope) {
	data, err := json.Marshal(map[string]any{
		"type":    "server-request",
		"method":  "session/event",
		"payload": env,
	})
	if err != nil {
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	for conn, subID := range h.muxClients {
		if subID == "" || subID == sessionID {
			_ = conn.WriteMessage(websocket.TextMessage, data)
		}
	}
}

// BroadcastHostEvent sends host-level updates to all /events/host clients.
func (h *DownlinkHub) BroadcastHostEvent(method string, payload any) {
	data, err := json.Marshal(map[string]any{
		"type":    "server-request",
		"method":  method,
		"payload": payload,
	})
	if err != nil {
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	for conn := range h.hostClients {
		_ = conn.WriteMessage(websocket.TextMessage, data)
	}
}
