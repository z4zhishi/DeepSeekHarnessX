package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"

	"dsh-go/pkg/tools"
)

// Client connects to an external MCP (Model Context Protocol) server over stdio.
type Client struct {
	serverName string
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	stdout     *bufio.Reader
	reqCounter atomic.Int64
	mu         sync.Mutex
}

// StartStdioClient spawns an MCP server subprocess and connects stdio JSON-RPC.
func StartStdioClient(ctx context.Context, serverName string, command string, args ...string) (*Client, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start MCP server %s: %w", serverName, err)
	}

	client := &Client{
		serverName: serverName,
		cmd:        cmd,
		stdin:      stdin,
		stdout:     bufio.NewReader(stdout),
	}

	// Initialize handshake
	if err := client.initialize(); err != nil {
		client.Close()
		return nil, err
	}

	return client, nil
}

func (c *Client) initialize() error {
	var resp struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}

	err := c.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"clientInfo": map[string]string{
			"name":    "dsh-go-mcp-client",
			"version": "1.0.0",
		},
		"capabilities": map[string]any{},
	}, &resp)
	return err
}

// DiscoverAndRegisterTools queries `tools/list` on the MCP server and mounts tools on the registry.
func (c *Client) DiscoverAndRegisterTools(r *tools.ToolRegistry) error {
	var resp struct {
		Result struct {
			Tools []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}

	if err := c.call("tools/list", map[string]any{}, &resp); err != nil {
		return err
	}

	for _, t := range resp.Result.Tools {
		mcpToolName := fmt.Sprintf("mcp__%s__%s", c.serverName, t.Name)
		rawToolName := t.Name

		r.Register(tools.ToolDefinition{
			Name:           mcpToolName,
			Description:    t.Description,
			ParametersJSON: t.InputSchema,
			RequiresPerm:   true,
			Execute: func(ctx tools.ToolExecutionContext, argsJSON string) (any, error) {
				var arguments map[string]any
				_ = json.Unmarshal([]byte(argsJSON), &arguments)

				var callResp struct {
					Result struct {
						Content []struct {
							Type string `json:"type"`
							Text string `json:"text"`
						} `json:"content"`
						IsError bool `json:"isError"`
					} `json:"result"`
				}

				err := c.call("tools/call", map[string]any{
					"name":      rawToolName,
					"arguments": arguments,
				}, &callResp)
				if err != nil {
					return nil, err
				}

				var out string
				for _, block := range callResp.Result.Content {
					if block.Type == "text" {
						out += block.Text
					}
				}
				return out, nil
			},
		})
	}

	return nil
}

func (c *Client) call(method string, params any, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	reqID := int(c.reqCounter.Add(1))
	reqBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      reqID,
		"method":  method,
		"params":  params,
	})

	if _, err := c.stdin.Write(append(reqBody, '\n')); err != nil {
		return err
	}

	line, err := c.stdout.ReadBytes('\n')
	if err != nil {
		return err
	}

	return json.Unmarshal(line, out)
}

// Close terminates the MCP client subprocess.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.stdin.Close()
	if c.cmd != nil && c.cmd.Process != nil {
		return c.cmd.Process.Kill()
	}
	return nil
}
