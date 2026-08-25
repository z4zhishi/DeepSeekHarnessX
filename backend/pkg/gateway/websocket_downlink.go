package gateway

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"dsh-go/pkg/session"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		// 与 HTTP 信任栅栏同一谓词（trust.requestTrusted）：只接受 loopback
		// Host，浏览器 Origin 必须与请求 Host 权威精确一致，且
		// Sec-Fetch-Site 非 cross-site；无 Origin 的本地客户端（Godot/curl）放行。
		return requestTrusted(r.Host, r.Header.Get("Origin"), r.Header.Get("Sec-Fetch-Site"))
	},
}

// downlinkSendBuffer is the per-connection outbound frame backlog. Beyond this
// depth a slow consumer starts shedding frames instead of stalling every agent
// pump broadcasting through the hub (mux streams have store replay for
// catch-up; host frames are transient lifecycle notices).
const downlinkSendBuffer = 512

// downlinkWriteTimeout bounds each frame write so a dead TCP peer cannot pin
// the per-connection writer goroutine forever.
const downlinkWriteTimeout = 10 * time.Second

// downlinkConn couples one WebSocket with a single-writer outbound queue.
// gorilla/websocket forbids concurrent WriteMessage on one conn; serializing
// through one pump goroutine both restores that invariant (multiple agent
// pumps broadcast concurrently) and keeps slow clients from blocking callers.
type downlinkConn struct {
	conn      *websocket.Conn
	send      chan []byte
	closeOnce sync.Once
}

func newDownlinkConn(c *websocket.Conn) *downlinkConn {
	d := &downlinkConn{conn: c, send: make(chan []byte, downlinkSendBuffer)}
	go d.writePump()
	return d
}

// writePump is the ONLY writer of data frames on the conn. Control frames
// bypass the queue: gorilla documents WriteControl as safe alongside all
// other methods, and the downlink-only 1008 close relies on it.
func (d *downlinkConn) writePump() {
	for frame := range d.send {
		_ = d.conn.SetWriteDeadline(time.Now().Add(downlinkWriteTimeout))
		if err := d.conn.WriteMessage(websocket.TextMessage, frame); err != nil {
			// Unblock the handler's read loop; it unregisters and closes us.
			_ = d.conn.Close()
		}
	}
}

// enqueue drops a frame onto the outbound queue without ever blocking the
// publisher; an overfull queue sheds the frame (slow-consumer policy).
func (d *downlinkConn) enqueue(frame []byte) {
	select {
	case d.send <- frame:
	default:
	}
}

// close shuts the queue after unregister so the pump drains the backlog and
// exits exactly once.
func (d *downlinkConn) close() {
	d.closeOnce.Do(func() { close(d.send) })
}

// stagedFrame is a host-level frame kept for reconnect replay while its
// approval is in flight (currently: host/permission-request).
type stagedFrame struct {
	id   string
	data []byte
}

// DownlinkHub manages active WebSocket downlink connections.
type DownlinkHub struct {
	mu          sync.Mutex // guards the conn maps and stagedFrames; broadcasts enqueue under the lock so register-vs-publish is atomic
	muxClients  map[*downlinkConn]string
	hostClients map[*downlinkConn]bool
	// stagedFrames holds in-flight approval frames replayed to newly
	// connected downlinks so a refreshed/reconnected GUI re-sees pending
	// prompts instead of waiting for the timeout to cancel them.
	stagedFrames []*stagedFrame
	// Replay returns stored envelopes for mux catch-up. Invoked without
	// holding h.mu so a store query cannot deadlock the hub.
	Replay func(sessionID string, fromSeq int) []session.SessionEnvelope
}

// NewDownlinkHub creates a new downlink hub.
func NewDownlinkHub() *DownlinkHub {
	return &DownlinkHub{
		muxClients:  make(map[*downlinkConn]string),
		hostClients: make(map[*downlinkConn]bool),
	}
}

// HandleMux handles /api/events/mux downlinks (session events stream).
func (h *DownlinkHub) HandleMux(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	d := newDownlinkConn(conn)
	defer h.unregisterMux(d)

	sessionID := r.URL.Query().Get("sessionId")
	fromSeq := 0
	if v := r.URL.Query().Get("fromSeq"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			fromSeq = n
		}
	}

	// Replay historical envelopes before registering for live events so a
	// reconnecting GUI sees the log. Query the store without holding h.mu.
	var history []session.SessionEnvelope
	if replay := h.Replay; replay != nil && sessionID != "" {
		history = replay(sessionID, fromSeq)
	}
	for i := range history {
		if data, err := encodeSessionEvent(&history[i]); err == nil {
			d.enqueue(data)
		}
	}

	// Register atomically with the staged-frame snapshot so an approval
	// published concurrently is delivered exactly once (live or replayed),
	// and pending prompts reach the fresh connection before the live stream.
	for _, frame := range h.registerMux(d, sessionID) {
		d.enqueue(frame)
	}

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
}

// HandleHost handles /api/events/host downlinks (workspace/host lifecycle stream).
func (h *DownlinkHub) HandleHost(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	d := newDownlinkConn(conn)
	defer h.unregisterHost(d)

	for _, frame := range h.registerHost(d) {
		d.enqueue(frame)
	}

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
}

func encodeSessionEvent(env *session.SessionEnvelope) ([]byte, error) {
	return json.Marshal(map[string]any{
		"type":    "server-request",
		"method":  "session/event",
		"payload": env,
	})
}

func encodeHostEvent(method string, payload any) ([]byte, error) {
	return json.Marshal(map[string]any{
		"type":    "server-request",
		"method":  method,
		"payload": payload,
	})
}

// BroadcastSessionEvent sends a session event to clients subscribed to that session.
func (h *DownlinkHub) BroadcastSessionEvent(sessionID string, env *session.SessionEnvelope) {
	data, err := encodeSessionEvent(env)
	if err != nil {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	for conn, subID := range h.muxClients {
		if subID == "" || subID == sessionID {
			conn.enqueue(data)
		}
	}
}

// BroadcastHostEvent sends host-level updates to all /events/host clients.
func (h *DownlinkHub) BroadcastHostEvent(method string, payload any) {
	data, err := encodeHostEvent(method, payload)
	if err != nil {
		return
	}
	h.broadcastHostData(data)
}

// broadcastHostData enqueues a pre-encoded frame to every host client. The
// critical section is the publish side of the register/snapshot handshake,
// making replay-vs-live delivery exactly-once per connection.
func (h *DownlinkHub) broadcastHostData(data []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for conn := range h.hostClients {
		conn.enqueue(data)
	}
}

// stageReplay records an in-flight approval frame for reconnect replay. Must
// be called BEFORE the frame is broadcast so any downlink connecting after
// publication sees it in its registration snapshot.
func (h *DownlinkHub) stageReplay(id string, data []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stagedFrames = append(h.stagedFrames, &stagedFrame{id: id, data: data})
}

// unstageReplay drops the frame once the approval resolves (answered or timed
// out) so later reconnects do not resurrect a dead prompt.
func (h *DownlinkHub) unstageReplay(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, f := range h.stagedFrames {
		if f.id == id {
			h.stagedFrames = append(h.stagedFrames[:i], h.stagedFrames[i+1:]...)
			return
		}
	}
}

// registerMux registers a mux downlink and returns the staged approval frames
// snapshot taken under the same lock — the consume side of the handshake.
func (h *DownlinkHub) registerMux(d *downlinkConn, sessionID string) [][]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.muxClients[d] = sessionID
	return h.stagedSnapshotLocked()
}

// registerHost registers a host downlink and returns the staged approval
// frames snapshot taken under the same lock.
func (h *DownlinkHub) registerHost(d *downlinkConn) [][]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hostClients[d] = true
	return h.stagedSnapshotLocked()
}

func (h *DownlinkHub) stagedSnapshotLocked() [][]byte {
	if len(h.stagedFrames) == 0 {
		return nil
	}
	out := make([][]byte, len(h.stagedFrames))
	for i, f := range h.stagedFrames {
		out[i] = f.data
	}
	return out
}

func (h *DownlinkHub) unregisterMux(d *downlinkConn) {
	h.mu.Lock()
	delete(h.muxClients, d)
	h.mu.Unlock()
	d.close()
}

func (h *DownlinkHub) unregisterHost(d *downlinkConn) {
	h.mu.Lock()
	delete(h.hostClients, d)
	h.mu.Unlock()
	d.close()
}
