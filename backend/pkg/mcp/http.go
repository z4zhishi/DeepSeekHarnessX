package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// HttpConfig Streamable HTTP 传输配置（对齐上游 StreamableHttpConfig）。
type HttpConfig struct {
	ServerName         string            `json:"serverName"`
	URL                string            `json:"url"`
	Headers            map[string]string `json:"headers,omitempty"`
	ToolCallTimeoutMs  int               `json:"toolCallTimeoutMs,omitempty"`
	FailOnStartupError bool              `json:"failOnStartupError,omitempty"`
	Reconnect          ReconnectConfig   `json:"reconnect,omitempty"`
}

func (c HttpConfig) validate() (HttpConfig, error) {
	if !serverNameRE.MatchString(c.ServerName) {
		return c, fmt.Errorf("mcp: serverName %q 非法，须匹配 %s", c.ServerName, serverNamePatternStr)
	}
	u, err := url.ParseRequestURI(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return c, fmt.Errorf("mcp: url %q 非法", c.URL)
	}
	if c.ToolCallTimeoutMs <= 0 {
		c.ToolCallTimeoutMs = defaultToolCallMs
	}
	return c, nil
}

// httpConn 是 Streamable HTTP 传输：POST 请求（JSON 或 SSE 响应体），
// 外加一个长命的背景 GET 流用于接收服务器主动通知。
//
// 断线语义（对齐上游 SDK onclose→generationDown 驱动的重连）：
//   - client 不设总 Timeout（总超时会掐死长命 SSE 体）；每个请求用 ctx 或
//     连接级 liveCtx 界定生命周期；
//   - 背景通知流终止后短退避重建；网络级失败/会话失效（404）标记连接死亡
//     （dead），done() 随之触发，Supervisor 的重连预算据此接手；
//   - 本地 close() 与远端死亡都会触发 done()，Supervisor 用自身 closed 标记
//     区分两者（与 stdioConn 行为一致）。
type httpConn struct {
	baseURL   string
	headers   map[string]string
	client    *http.Client
	pending   map[string]chan *rpcMsg
	notify    chan *rpcMsg
	reqID     atomic.Int64
	sIDMu     sync.Mutex
	sessionID string

	liveCtx    context.Context // 连接生命周期：close/markDead 时取消在途请求与通知流
	liveCancel context.CancelFunc
	mu         sync.Mutex
	closed     chan struct{} // 本地 Close 信号
	closeOnce  sync.Once
	dead       chan struct{} // 远端死亡信号（网络故障/会话失效）
	deadOnce   sync.Once
	doneCh     chan struct{} // closed ∪ dead；connection.done() 的实现
}

var _ connection = (*httpConn)(nil)

// bgStreamRetryDelay 是背景通知流终止后的重建退避（对齐上游 SDK 默认流重连间隔）。
const bgStreamRetryDelay = time.Second

func startHTTPConn(ctx context.Context, cfg HttpConfig) (*httpConn, error) {
	liveCtx, liveCancel := context.WithCancel(context.Background())
	conn := &httpConn{
		baseURL: cfg.URL,
		headers: cfg.Headers,
		client: &http.Client{
			// 故意不设 Timeout：它会计入响应体读取时长，把长命 SSE 流
			// （后台通知流 / SSE 长应答）在固定时限硬性掐断。请求寿命由
			// 各自的 request ctx（调用方）或 liveCtx（通知流/通知 POST）界定，
			// 应答等待另有 await 的 rpcTimeout 计时器兜底。
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     90 * time.Second,
				Proxy:               http.ProxyFromEnvironment,
			},
		},
		pending:    map[string]chan *rpcMsg{},
		notify:     make(chan *rpcMsg, 64),
		closed:     make(chan struct{}),
		dead:       make(chan struct{}),
		doneCh:     make(chan struct{}),
		liveCtx:    liveCtx,
		liveCancel: liveCancel,
	}
	go conn.watchDone()

	var out struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := conn.call(ctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"clientInfo": map[string]string{
			"name":    "dsh-go-mcp-client",
			"version": "1.0.0",
		},
		"capabilities": map[string]any{},
	}, &out); err != nil {
		conn.close()
		return nil, fmt.Errorf("mcp(%s): 初始化失败: %w", cfg.ServerName, err)
	}
	conn.notifyServer("notifications/initialized", map[string]any{})
	go conn.backgroundStream()
	return conn, nil
}

// watchDone 汇聚本地关闭与远端死亡两个信号源并取消连接生命周期 ctx。
func (c *httpConn) watchDone() {
	select {
	case <-c.closed:
	case <-c.dead:
	}
	c.liveCancel()
	close(c.doneCh)
}

// markDead 标记连接因网络级故障/会话失效而死亡（幂等）。
func (c *httpConn) markDead() {
	c.deadOnce.Do(func() { close(c.dead) })
}

// isDown 报告连接是否已进入本地关闭或远端死亡状态。
func (c *httpConn) isDown() bool {
	select {
	case <-c.closed:
		return true
	case <-c.dead:
		return true
	default:
		return false
	}
}

// classifyFailure 把一次传输级失败归类为连接死亡：网络层错误（连接拒绝/
// 复位等）意味着本代连接不可恢复；调用方主动取消/超时与本地关闭引发的
// 连锁错误不算。
func (c *httpConn) classifyFailure(err error) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	if c.isDown() {
		return
	}
	c.markDead()
}

func (c *httpConn) call(ctx context.Context, method string, params any, out any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id := c.reqID.Add(1)
	reqBody, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return err
	}

	ch := make(chan *rpcMsg, 1)
	c.mu.Lock()
	select {
	case <-c.closed:
		c.mu.Unlock()
		return errors.New("MCP 连接已关闭")
	default:
		c.pending[fmt.Sprint(id)] = ch
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, fmt.Sprint(id))
		c.mu.Unlock()
	}()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range c.headers {
		httpReq.Header.Set(k, v)
	}
	c.sIDMu.Lock()
	if c.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	c.sIDMu.Unlock()

	resp, err := c.client.Do(httpReq)
	if err != nil {
		c.classifyFailure(err)
		return fmt.Errorf("MCP HTTP 请求: %w", err)
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.sIDMu.Lock()
		c.sessionID = sid
		c.sIDMu.Unlock()
	}
	if resp.StatusCode == http.StatusNotFound {
		// 会话已失效（典型：服务器重启丢失 session）。本代连接作废，交由
		// Supervisor 重连建立新会话（对齐 streamable-http 规范的 404 语义）。
		c.markDead()
		return fmt.Errorf("MCP HTTP 状态 %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("MCP HTTP 状态 %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "text/event-stream") {
		err := c.consumeSSE(ctx, resp.Body)
		if err != nil {
			// SSE 应答体中途损坏通常是网络级故障；调用方取消（ctx）除外。
			c.classifyFailure(err)
			return err
		}
		return nil
	}
	var msg rpcMsg
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		return err
	}
	c.dispatch(&msg)
	return c.await(ctx, fmt.Sprint(id), ch, out)
}

// await 等待指定请求 id 的响应或失败。
func (c *httpConn) await(ctx context.Context, id string, ch chan *rpcMsg, out any) error {
	timeout := time.NewTimer(rpcTimeout)
	defer timeout.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return errors.New("MCP 连接已关闭")
	case <-timeout.C:
		return errors.New("MCP 请求超时")
	case msg := <-ch:
		if len(msg.Error) > 0 && string(msg.Error) != "null" {
			return fmt.Errorf("MCP 调用失败: %s", string(msg.Error))
		}
		if out != nil {
			return json.Unmarshal(msg.Result, out)
		}
		return nil
	}
}

// dispatch 将一条 rpcMsg 投递到 pending 或 notify。
func (c *httpConn) dispatch(msg *rpcMsg) {
	if msg.ID != nil {
		key := fmt.Sprint(*msg.ID)
		c.mu.Lock()
		ch := c.pending[key]
		delete(c.pending, key)
		c.mu.Unlock()
		if ch != nil {
			select {
			case ch <- msg:
			default:
			}
		}
		return
	}
	select {
	case c.notify <- msg:
	case <-c.closed:
	case <-c.dead:
	}
}

// consumeSSE 解析 SSE 流（data: 多行拼接，空行 flush）。
func (c *httpConn) consumeSSE(ctx context.Context, body io.Reader) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var data strings.Builder
	flush := func() {
		if data.Len() == 0 {
			return
		}
		var msg rpcMsg
		if err := json.Unmarshal([]byte(data.String()), &msg); err == nil {
			c.dispatch(&msg)
		}
		data.Reset()
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "event:") || strings.HasPrefix(line, "id:") || strings.HasPrefix(line, "retry:") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data.WriteString(strings.TrimPrefix(line, "data:"))
			data.WriteString("\n")
		}
	}
	flush()
	return scanner.Err()
}

func (c *httpConn) notifyServer(method string, params any) {
	if c.isDown() {
		return
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(c.liveCtx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	c.sIDMu.Lock()
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	c.sIDMu.Unlock()
	resp, err := c.client.Do(req)
	if err != nil {
		// 单向通知 best-effort，但网络级失败同样说明连接已死。
		c.classifyFailure(err)
		return
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.sIDMu.Lock()
		c.sessionID = sid
		c.sIDMu.Unlock()
	}
}

// errStreamUnsupported 表示服务器按 streamable-http 规范拒绝独立 GET 通知流
// （405）：POST 是唯一通道，连接仍然健康，不应重建也不应判死。
var errStreamUnsupported = errors.New("mcp: 服务器不支持独立通知流")

// backgroundStream 维持长命 GET 通知流：终止后短退避重建（修复旧实现被客户端
// 总超时掐死后静默消失的问题）；网络级失败标记连接死亡并由 Supervisor 的重连
// 预算接手；405 为协议允许的降级——安静停止重建。
func (c *httpConn) backgroundStream() {
	for {
		if c.isDown() {
			return
		}
		err := c.streamOnce()
		if errors.Is(err, errStreamUnsupported) {
			return
		}
		if c.isDown() {
			return
		}
		select {
		case <-c.closed:
			return
		case <-c.dead:
			return
		case <-time.After(bgStreamRetryDelay):
		}
	}
}

// streamOnce 执行一次 GET 通知流直到其终止。返回 nil 表示流干净结束
// （可重建）；errStreamUnsupported 表示永久放弃；其余错误伴随连接死亡标记
// 或可重试的瞬态状态码。
func (c *httpConn) streamOnce() error {
	req, err := http.NewRequestWithContext(c.liveCtx, http.MethodGet, c.baseURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	c.sIDMu.Lock()
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	c.sIDMu.Unlock()
	resp, err := c.client.Do(req)
	if err != nil {
		if !c.isDown() {
			// 拨号/读流网络失败：连接死亡信号，驱动 Supervisor 重连。
			c.markDead()
		}
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusMethodNotAllowed:
		return errStreamUnsupported
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("通知流状态 %d", resp.StatusCode)
	}
	if err := c.consumeSSE(c.liveCtx, resp.Body); err != nil {
		if !c.isDown() && !errors.Is(err, context.Canceled) {
			c.markDead()
		}
		return err
	}
	return nil // 干净 EOF：稍后重建
}

func (c *httpConn) listTools(ctx context.Context, cursor string) (*ListToolsResult, error) {
	params := map[string]any{}
	if cursor != "" {
		params["cursor"] = cursor
	}
	var out ListToolsResult
	if err := c.call(ctx, "tools/list", params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *httpConn) callTool(ctx context.Context, rawName string, args map[string]any) (*CallToolResult, error) {
	var out CallToolResult
	if err := c.call(ctx, "tools/call", map[string]any{
		"name":      rawName,
		"arguments": args,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *httpConn) notifications() <-chan *rpcMsg { return c.notify }
func (c *httpConn) done() <-chan struct{}         { return c.doneCh }

func (c *httpConn) close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
	})
	return nil
}
