package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// StdioConfig 子进程 stdio 传输配置（对齐上游 StdioConfig）。
type StdioConfig struct {
	ServerName         string            `json:"serverName"`
	Command            string            `json:"command"`
	Args               []string          `json:"args,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
	Cwd                string            `json:"cwd,omitempty"`
	ToolCallTimeoutMs  int               `json:"toolCallTimeoutMs,omitempty"`
	FailOnStartupError bool              `json:"failOnStartupError,omitempty"`
	Reconnect          ReconnectConfig   `json:"reconnect,omitempty"`
}

func (c StdioConfig) validate() (StdioConfig, error) {
	if !serverNameRE.MatchString(c.ServerName) {
		return c, fmt.Errorf("mcp: serverName %q 非法，须匹配 %s", c.ServerName, serverNamePatternStr)
	}
	if strings.TrimSpace(c.Command) == "" {
		return c, errors.New("mcp: stdio command 不能为空")
	}
	if c.ToolCallTimeoutMs <= 0 {
		c.ToolCallTimeoutMs = defaultToolCallMs
	}
	return c, nil
}

// rpcMsg 一行 JSON-RPC 消息；id==nil 表示通知。
type rpcMsg struct {
	ID     *int64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

// stdioConn 是 stdio 传输的 JSON-RPC 连接：专用 reader goroutine
// 把响应按 id 分发到 pending channel，通知投递到 notify channel。
// 请求可并发发出（上游 SDK 允许并发 request）。
type stdioConn struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    *bufio.Reader
	pending   map[int64]chan *rpcMsg
	notify    chan *rpcMsg
	reqID     atomic.Int64
	mu        sync.Mutex
	closed    chan struct{}
	closeOnce sync.Once
	readErr   error
}

var _ connection = (*stdioConn)(nil)

// rpcTimeout 是单次调用的最大等待（工具调用级超时由 bridge 配置另行约束）。
const rpcTimeout = 120 * time.Second

func startStdio(ctx context.Context, cfg StdioConfig) (*stdioConn, error) {
	env := os.Environ()
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
	}
	// 注意：不能用 CommandContext 绑定调用方 ctx——重连路径的临时 ctx 会在
	// reconnectNow 返回时取消，从而杀掉刚启动的子进程。进程生命周期由
	// Supervisor.Close 统一管理（close 时 kill）。
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Env = env
	if cfg.Cwd != "" {
		cmd.Dir = cfg.Cwd
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr // 服务器诊断直接透出
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp(%s): 启动失败: %w", cfg.ServerName, err)
	}

	conn := &stdioConn{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  bufio.NewReader(stdout),
		pending: map[int64]chan *rpcMsg{},
		notify:  make(chan *rpcMsg, 64),
		closed:  make(chan struct{}),
	}
	go conn.readLoop()

	// 握手：initialize → notifications/initialized
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
	return conn, nil
}

func (c *stdioConn) readLoop() {
	defer close(c.closed)
	for {
		line, err := c.stdout.ReadBytes('\n')
		if err != nil {
			return
		}
		line = bytes.TrimPrefix(line, []byte{0xEF, 0xBB, 0xBF}) // UTF-8 BOM
		var msg rpcMsg
		if err := json.Unmarshal(line, &msg); err != nil {
			continue // 畸形行忽略（上游契约）
		}
		if msg.ID != nil {
			c.mu.Lock()
			ch := c.pending[*msg.ID]
			delete(c.pending, *msg.ID)
			c.mu.Unlock()
			if ch != nil {
				select {
				case ch <- &msg:
				default: // 调用方已超时离开
				}
			}
			continue
		}
		select {
		case c.notify <- &msg:
		case <-c.closed:
			return
		}
	}
}

func (c *stdioConn) call(ctx context.Context, method string, params any, out any) error {
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
		c.pending[id] = ch
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if _, err := c.stdin.Write(append(reqBody, '\n')); err != nil {
		return fmt.Errorf("写入 MCP 请求: %w", err)
	}

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
			return fmt.Errorf("MCP %s 失败: %s", method, string(msg.Error))
		}
		if out != nil {
			return json.Unmarshal(msg.Result, out)
		}
		return nil
	}
}

// notifyServer 发送单向通知（无响应）。
func (c *stdioConn) notifyServer(method string, params any) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return
	}
	_, _ = c.stdin.Write(append(body, '\n'))
}

func (c *stdioConn) listTools(ctx context.Context, cursor string) (*ListToolsResult, error) {
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

func (c *stdioConn) callTool(ctx context.Context, rawName string, args map[string]any) (*CallToolResult, error) {
	var out CallToolResult
	if err := c.call(ctx, "tools/call", map[string]any{
		"name":      rawName,
		"arguments": args,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *stdioConn) notifications() <-chan *rpcMsg { return c.notify }

// done 在子进程退出/流终止时触发。
func (c *stdioConn) done() <-chan struct{} { return c.closed }

// close 关闭输入、等待子进程退出（5s 上限），兜底 kill；只执行一次。
func (c *stdioConn) close() error {
	c.closeOnce.Do(func() {
		_ = c.stdin.Close()
		done := make(chan struct{})
		go func() {
			_ = c.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = c.cmd.Process.Kill()
			<-done
		}
	})
	return nil
}
