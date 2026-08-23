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
// 外加一个容忍失败的背景 GET 流用于接收服务器主动通知。
type httpConn struct {
	baseURL   string
	headers   map[string]string
	client    *http.Client
	pending   map[string]chan *rpcMsg
	notify    chan *rpcMsg
	reqID     atomic.Int64
	sIDMu     sync.Mutex
	sessionID string
	mu        sync.Mutex
	closed    chan struct{}
	closeOnce sync.Once
}

var _ connection = (*httpConn)(nil)

func startHTTPConn(ctx context.Context, cfg HttpConfig) (*httpConn, error) {
	conn := &httpConn{
		baseURL: cfg.URL,
		headers: cfg.Headers,
		client: &http.Client{
			Timeout: rpcTimeout,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     90 * time.Second,
				Proxy:               http.ProxyFromEnvironment,
			},
		},
		pending: map[string]chan *rpcMsg{},
		notify:  make(chan *rpcMsg, 64),
		closed:  make(chan struct{}),
	}

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
		return fmt.Errorf("MCP HTTP 请求: %w", err)
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.sIDMu.Lock()
		c.sessionID = sid
		c.sIDMu.Unlock()
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("MCP HTTP 状态 %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "text/event-stream") {
		return c.consumeSSE(ctx, resp.Body)
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
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return
	}
	req, err := http.NewRequest(http.MethodPost, c.baseURL, bytes.NewReader(body))
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
		return
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.sIDMu.Lock()
		c.sessionID = sid
		c.sIDMu.Unlock()
	}
}

// backgroundStream 打开 GET 流以接收服务器主动通知；失败静默退出
// （POST 响应流仍是主通道，符合协议兼容）。
func (c *httpConn) backgroundStream() {
	req, err := http.NewRequest(http.MethodGet, c.baseURL, nil)
	if err != nil {
		return
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
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	_ = c.consumeSSE(context.Background(), resp.Body)
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
func (c *httpConn) done() <-chan struct{}         { return c.closed }

func (c *httpConn) close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
	})
	return nil
}
