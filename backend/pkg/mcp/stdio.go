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

// rpcMsg 一行 JSON-RPC 消息；id==nil 表示通知。它是 RPC 的别名，使现有
// MCP 内部代码与泛化的 plugin Host 共享同一线格式。
type rpcMsg = RPC

// stdioConn 是 stdio 传输的 JSON-RPC 连接：专用 reader goroutine
// 把响应按 id 分发到 pending channel，通知投递到 notify channel。
// 请求可并发发出（上游依赖允许并发 request）。
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

var (
	_ connection = (*stdioConn)(nil)
	_ Transport  = (*stdioConn)(nil)
)

// rpcTimeout 是单次调用的最大等待（工具调用级超时由 bridge 配置另行约束）。
const rpcTimeout = 120 * time.Second

// spawnStdio 建立子进程与 stdio 管道，启动 readLoop，不做任何协议握手。
// 具体协议握手（MCP initialize 或插件 initialize）由调用方在返回的连接上执行。
func spawnStdio(cfg StdioConfig) (*stdioConn, error) {
	env := os.Environ()
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
	}
	// 注意：不能用 CommandContext 绑定调用方 ctx——重连路径的临时 ctx 会在
	// reconnectNow 返回时取消，从而杀掉刚启动的子进程。进程生命周期由
	// Supervisor/Host.Close 统一管理（close 时 kill）。
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
	return conn, nil
}

// startStdio 建立 stdio 连接并执行 MCP 握手（initialize → initialized）。
func startStdio(ctx context.Context, cfg StdioConfig) (*stdioConn, error) {
	conn, err := spawnStdio(cfg)
	if err != nil {
		return nil, err
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
	return conn, nil
}

// StartStdioTransport 建立一条裸 stdio JSON-RPC 传输，不做任何协议握手。
// 供 plugin Host 使用：Host 在返回的 Transport 上自行执行插件 initialize
// 握手。此入口复用 stdioConn（同一进程生命周期/重连/分发语义），MCP 路径
// 不受影响（startStdio 仍走 MCP 握手）。
func StartStdioTransport(cfg StdioConfig) (Transport, error) {
	conn, err := spawnStdio(cfg)
	if err != nil {
		return nil, err
	}
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

// --- Transport 泛化方法：使 stdioConn 同时满足泛化 Transport 接口，
// 供 plugin Host 与既有 MCP Supervisor 共用同一 stdio 传输。 ---

// Call 是 Transport.Call 的实现：转发到 MCP 侧既有的 call。
func (c *stdioConn) Call(ctx context.Context, method string, params any, out any) error {
	return c.call(ctx, method, params, out)
}

// Notify 是 Transport.Notify 的实现：转发到 notifyServer。
func (c *stdioConn) Notify(method string, params any) { c.notifyServer(method, params) }

// Close 是 Transport.Close 的实现：转发到内部 close。
func (c *stdioConn) Close() error { return c.close() }

// Done 是 Transport.Done 的实现：转发到内部 done。
func (c *stdioConn) Done() <-chan struct{} { return c.done() }

// Notifications 是 Transport.Notifications 的实现：以公共 RPC 类型暴露
// 通知流（channel 元素类型与内部 rpcMsg 同一）。
func (c *stdioConn) Notifications() <-chan *RPC { return c.notify }

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
