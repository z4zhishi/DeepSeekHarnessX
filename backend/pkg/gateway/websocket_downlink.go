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

// downlinkWriteTimeout bounds each frame write so a dead TCP peer cannot pin
// the per-connection writer goroutine forever. 30s tolerates a GUI main-thread
// stall (JSON.parse + chat rebind) without force-closing the socket and forcing
// a reconnect churn.
const downlinkWriteTimeout = 30 * time.Second

// A downlink is best-effort only in the sense that it may be disconnected; it
// must never silently shed ordinary events. Once either bound is exceeded the
// slow peer is closed and removed from the hub, allowing publishers to proceed.
const (
	// Keep the queue bounded, but large enough to absorb a normal concurrent
	// broadcast burst while the single writer drains it. Slow peers still hit
	// the byte bound (or this frame bound) and are aborted rather than making
	// publishers wait forever.
	downlinkQueueMaxFrames  = 1024
	downlinkQueueMaxBytes   = 4 << 20
	downlinkReadIdleTimeout = 5 * time.Minute
)

// downlinkConn couples one WebSocket with a single-writer outbound queue.
// gorilla/websocket forbids concurrent WriteMessage on one conn; serializing
// through one pump goroutine both restores that invariant (multiple agent
// pumps broadcast concurrently) and keeps slow clients from blocking callers.
type downlinkConn struct {
	conn *websocket.Conn

	mu         sync.Mutex
	queue      [][]byte
	queueBytes int
	notify     chan struct{}
	done       chan struct{}
	closed     bool
	onFail     func()
	failOnce   sync.Once
}

func newDownlinkConn(c *websocket.Conn) *downlinkConn {
	return &downlinkConn{conn: c, notify: make(chan struct{}, 1), done: make(chan struct{})}
}

func (d *downlinkConn) start() {
	go d.writePump()
}

func (d *downlinkConn) writePump() {
	defer close(d.done)
	for {
		d.mu.Lock()
		if len(d.queue) == 0 {
			closed := d.closed
			d.mu.Unlock()
			if closed {
				return
			}
			<-d.notify
			continue
		}
		frame := d.queue[0]
		d.queue[0] = nil
		d.queue = d.queue[1:]
		d.queueBytes -= len(frame)
		d.mu.Unlock()

		_ = d.conn.SetWriteDeadline(time.Now().Add(downlinkWriteTimeout))
		if err := d.conn.WriteMessage(websocket.TextMessage, frame); err != nil {
			d.fail()
			return
		}
	}
}

func (d *downlinkConn) fail() {
	d.failOnce.Do(func() {
		d.mu.Lock()
		d.closed = true
		d.mu.Unlock()
		if d.conn != nil {
			_ = d.conn.Close()
		}
		if d.onFail != nil {
			d.onFail()
		}
	})
}

// enqueue accepts a frame or closes the slow connection when either queue
// bound is exceeded. It never silently drops an accepted ordinary event.
func (d *downlinkConn) enqueue(frame []byte) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	if len(d.queue) >= downlinkQueueMaxFrames || d.queueBytes+len(frame) > downlinkQueueMaxBytes {
		d.closed = true
		d.mu.Unlock()
		go d.fail()
		return
	}
	d.queue = append(d.queue, frame)
	d.queueBytes += len(frame)
	d.mu.Unlock()
	select {
	case d.notify <- struct{}{}:
	default:
	}
}

// close marks the queue closed. The writer drains all already-enqueued frames
// before exiting, while future enqueues are ignored safely.
func (d *downlinkConn) close() {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true
	d.mu.Unlock()
	select {
	case d.notify <- struct{}{}:
	default:
	}
}

// stagedFrame is a host-level frame kept for reconnect replay while its
// approval is in flight (currently: host/permission-request).
type stagedFrame struct {
	id   string
	data []byte
}

// lifecycleFrame is retained only across the short upgrade-to-registration
// window. It is deliberately bounded and expiring; it is not a host event log.
type lifecycleFrame struct {
	data      []byte
	createdAt time.Time
}

const (
	hostLifecycleBacklogLimit = 64
	hostLifecycleBacklogTTL   = 5 * time.Second
)

// DownlinkHub manages active WebSocket downlink connections.
type DownlinkHub struct {
	mu         sync.Mutex // guards the conn maps and stagedFrames; broadcasts enqueue under the lock so register-vs-publish is atomic
	muxClients map[*downlinkConn]string
	// muxPending buffers live events while a connection's replay snapshot is
	// being fetched. The connection is registered before the query, so events
	// published during the query cannot be lost; finishMuxReplay appends the
	// snapshot before this buffer to preserve stream order.
	muxPending  map[*downlinkConn][][]byte
	hostClients map[*downlinkConn]bool
	// stagedFrames holds in-flight approval frames replayed to newly
	// connected downlinks so a refreshed/reconnected GUI re-sees pending
	// prompts instead of waiting for the timeout to cancel them.
	stagedFrames []*stagedFrame
	// lifecycleFrames closes the small HTTP-upgrade/registration window for
	// host lifecycle events. They are drained by the next host registration;
	// unlike approvals, lifecycle notifications are not retained indefinitely.
	lifecycleFrames []*lifecycleFrame
	// Replay returns stored envelopes for mux catch-up. Invoked without
	// holding h.mu so a store query cannot deadlock the hub.
	Replay func(sessionID string, fromSeq int) []session.SessionEnvelope
}

// NewDownlinkHub creates a new downlink hub.
func NewDownlinkHub() *DownlinkHub {
	return &DownlinkHub{
		muxClients:  make(map[*downlinkConn]string),
		muxPending:  make(map[*downlinkConn][][]byte),
		hostClients: make(map[*downlinkConn]bool),
	}
}

func configureDownlinkRead(conn *websocket.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(downlinkReadIdleTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(downlinkReadIdleTimeout))
	})
}

// HandleMux handles /api/events/mux downlinks (session events stream).
func (h *DownlinkHub) HandleMux(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	configureDownlinkRead(conn)
	d := newDownlinkConn(conn)
	d.onFail = func() { h.unregisterMux(d) }
	d.start()
	defer h.unregisterMux(d)

	sessionID := r.URL.Query().Get("sessionId")
	fromSeq := 0
	if v := r.URL.Query().Get("fromSeq"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			fromSeq = n
		}
	}

	// Register before querying storage. BroadcastSessionEvent buffers every
	// event published during the query, and finishMuxReplay orders the stored
	// snapshot before that buffer. This closes the history-query/live gap.
	h.registerMuxPending(d, sessionID)
	var history []session.SessionEnvelope
	if replay := h.Replay; replay != nil && sessionID != "" {
		history = replay(sessionID, fromSeq)
	}
	h.finishMuxReplay(d, history)

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
	configureDownlinkRead(conn)
	d := newDownlinkConn(conn)
	d.onFail = func() { h.unregisterHost(d) }
	d.start()
	defer h.unregisterHost(d)

	// Register atomically with the staged-frame snapshot.
	h.registerHost(d)

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
			if _, pending := h.muxPending[conn]; pending {
				h.muxPending[conn] = append(h.muxPending[conn], data)
			} else {
				conn.enqueue(data)
			}
		}
	}
}

// BroadcastHostEvent sends host-level updates to all /events/host clients.
func (h *DownlinkHub) BroadcastHostEvent(method string, payload any) {
	data, err := encodeHostEvent(method, payload)
	if err != nil {
		return
	}
	h.broadcastHostData(data, isHostLifecycleMethod(method))
}

func isHostLifecycleMethod(method string) bool {
	switch method {
	case "host/session-added", "host/session-deleted", "host/subagent-started", "host/subagent-finished":
		return true
	default:
		return false
	}
}

// broadcastHostData enqueues a pre-encoded frame to every host client. The
// critical section is the publish side of the register/snapshot handshake,
// making replay-vs-live delivery exactly-once per connection. Only lifecycle
// events use the bounded registration-window backlog; approval replay has its
// own explicit stagedFrames semantics.
func (h *DownlinkHub) broadcastHostData(data []byte, lifecycle bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if len(h.hostClients) == 0 {
		if lifecycle {
			now := time.Now()
			h.pruneLifecycleLocked(now)
			if len(h.lifecycleFrames) >= hostLifecycleBacklogLimit {
				copy(h.lifecycleFrames, h.lifecycleFrames[len(h.lifecycleFrames)-hostLifecycleBacklogLimit+1:])
				h.lifecycleFrames = h.lifecycleFrames[:hostLifecycleBacklogLimit-1]
			}
			h.lifecycleFrames = append(h.lifecycleFrames, &lifecycleFrame{data: data, createdAt: now})
		}
		return
	}
	for conn := range h.hostClients {
		conn.enqueue(data)
	}
}

func (h *DownlinkHub) pruneLifecycleLocked(now time.Time) {
	cutoff := now.Add(-hostLifecycleBacklogTTL)
	keep := h.lifecycleFrames[:0]
	for _, f := range h.lifecycleFrames {
		if f.createdAt.After(cutoff) {
			keep = append(keep, f)
		}
	}
	h.lifecycleFrames = keep
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

func (h *DownlinkHub) registerMuxPending(d *downlinkConn, sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.muxClients[d] = sessionID
	h.muxPending[d] = nil
	h.enqueueStagedLocked(d)
}

func sessionEventSeq(data []byte) (int, bool) {
	var frame struct {
		Payload struct {
			Seq int `json:"seq"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(data, &frame); err != nil || frame.Payload.Seq <= 0 {
		return 0, false
	}
	return frame.Payload.Seq, true
}

func (h *DownlinkHub) finishMuxReplay(d *downlinkConn, history []session.SessionEnvelope) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.muxClients[d]; !ok {
		return
	}
	seen := make(map[int]struct{}, len(history))
	for i := range history {
		if history[i].Seq > 0 {
			if _, exists := seen[history[i].Seq]; exists {
				continue
			}
			seen[history[i].Seq] = struct{}{}
		}
		if data, err := encodeSessionEvent(&history[i]); err == nil {
			d.enqueue(data)
		}
	}
	for _, data := range h.muxPending[d] {
		if seq, ok := sessionEventSeq(data); ok {
			if _, exists := seen[seq]; exists {
				continue
			}
			seen[seq] = struct{}{}
		}
		d.enqueue(data)
	}
	delete(h.muxPending, d)
}

// registerHost registers a host downlink and enqueues the staged approval
// snapshot while holding the hub lock.
func (h *DownlinkHub) registerHost(d *downlinkConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hostClients[d] = true
	h.enqueueStagedLocked(d)
	h.pruneLifecycleLocked(time.Now())
	for _, f := range h.lifecycleFrames {
		d.enqueue(f.data)
	}
	h.lifecycleFrames = nil
}

func (h *DownlinkHub) enqueueStagedLocked(d *downlinkConn) {
	for _, f := range h.stagedFrames {
		d.enqueue(f.data)
	}
}

func (h *DownlinkHub) unregisterMux(d *downlinkConn) {
	h.mu.Lock()
	delete(h.muxClients, d)
	delete(h.muxPending, d)
	h.mu.Unlock()
	d.close()
}

func (h *DownlinkHub) unregisterHost(d *downlinkConn) {
	h.mu.Lock()
	delete(h.hostClients, d)
	h.mu.Unlock()
	d.close()
}
