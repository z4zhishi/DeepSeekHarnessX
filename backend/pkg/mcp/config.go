package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"dsh-go/pkg/tools"
)

// FileConfig 磁盘 JSON 配置（对齐上游 cordis.yml 多实例语义：一个文件多服务器）。
type FileConfig struct {
	Servers []ServerConfig `json:"servers"`
}

// ServerConfig 一个 MCP 服务器实例（stdio 或 streamable-http）。
type ServerConfig struct {
	Transport          string            `json:"transport"` // "stdio" | "streamable-http"
	ServerName         string            `json:"serverName"`
	Command            string            `json:"command,omitempty"`
	Args               []string          `json:"args,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
	Cwd                string            `json:"cwd,omitempty"`
	URL                string            `json:"url,omitempty"`
	Headers            map[string]string `json:"headers,omitempty"`
	ToolCallTimeoutMs  int               `json:"toolCallTimeoutMs,omitempty"`
	FailOnStartupError bool              `json:"failOnStartupError,omitempty"`
	Reconnect          ReconnectConfig   `json:"reconnect,omitempty"`
}

// LoadConfigFile 读取并校验 MCP 配置文件。
func LoadConfigFile(path string) (*FileConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("mcp: 读取配置 %s: %w", path, err)
	}
	var cfg FileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("mcp: 解析配置 %s: %w", path, err)
	}
	if len(cfg.Servers) == 0 {
		return nil, fmt.Errorf("mcp: 配置 %s 未包含任何服务器", path)
	}
	return &cfg, nil
}

// MountConfigFile 按配置挂载全部 MCP 服务器（每个实例独立命名空间）。
// 任一配置/命名空间错误会回滚全部已挂载实例并返回错误；连接失败按
// 各实例 failOnStartupError 语义处理（默认转后台重连）。
func MountConfigFile(ctx context.Context, path string, reg *tools.ToolRegistry, logger *log.Logger) ([]*Supervisor, error) {
	cfg, err := LoadConfigFile(path)
	if err != nil {
		return nil, err
	}
	sups := make([]*Supervisor, 0, len(cfg.Servers))
	for _, sc := range cfg.Servers {
		opts := BridgeOptions{
			ServerName:          sc.ServerName,
			RegistrationFailure: "contain",
			Logger:              logger,
		}
		if sc.ToolCallTimeoutMs > 0 {
			opts.ToolCallTimeout = time.Duration(sc.ToolCallTimeoutMs) * time.Millisecond
		}
		switch sc.Transport {
		case "stdio":
			s, err := NewStdioSupervisor(ctx, StdioConfig{
				ServerName:         sc.ServerName,
				Command:            sc.Command,
				Args:               sc.Args,
				Env:                sc.Env,
				Cwd:                sc.Cwd,
				ToolCallTimeoutMs:  sc.ToolCallTimeoutMs,
				FailOnStartupError: sc.FailOnStartupError,
				Reconnect:          sc.Reconnect,
			}, reg, opts)
			if err != nil {
				for _, prev := range sups {
					_ = prev.Close()
				}
				return nil, err
			}
			sups = append(sups, s)
		case "streamable-http":
			s, err := NewHttpSupervisor(ctx, HttpConfig{
				ServerName:         sc.ServerName,
				URL:                sc.URL,
				Headers:            sc.Headers,
				ToolCallTimeoutMs:  sc.ToolCallTimeoutMs,
				FailOnStartupError: sc.FailOnStartupError,
				Reconnect:          sc.Reconnect,
			}, reg, opts)
			if err != nil {
				for _, prev := range sups {
					_ = prev.Close()
				}
				return nil, err
			}
			sups = append(sups, s)
		default:
			for _, prev := range sups {
				_ = prev.Close()
			}
			return nil, fmt.Errorf("mcp: 服务器 %q 的 transport %q 非法（须 stdio 或 streamable-http）", sc.ServerName, sc.Transport)
		}
	}
	return sups, nil
}
