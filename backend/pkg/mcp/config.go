package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
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

// LoadConfigFile 读取并校验 DSHX 原生 JSON 配置（{"servers":[...]}）。
// 社区配置形状（mcpServers 映射 / VS Code servers 映射 / Codex TOML）请走
// ImportConfig，或直接用 MountConfigFile（对社区形状透明导入）。
func LoadConfigFile(path string) (*FileConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("mcp: 读取配置 %s: %w", path, err)
	}
	return unmarshalNativeConfig(data, path)
}

// unmarshalNativeConfig 解析 DSHX 原生 JSON 配置（无导入回退）。
func unmarshalNativeConfig(data []byte, path string) (*FileConfig, error) {
	var cfg FileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("mcp: 解析配置 %s: %w", path, err)
	}
	if len(cfg.Servers) == 0 {
		return nil, fmt.Errorf("mcp: 配置 %s 未包含任何服务器", path)
	}
	return &cfg, nil
}

// loadConfigForMount 为挂载加载配置：DSHX 原生格式优先（向后兼容）；.toml
// 与"原生解析零服务器且呈社区形状"的 JSON 文件改走导入器（生态收编 #1）。
func loadConfigForMount(path string) (*FileConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("mcp: 读取配置 %s: %w", path, err)
	}
	if strings.EqualFold(filepath.Ext(path), ".toml") {
		cfg, err := importTOMLConfig(data, path)
		if err != nil {
			return nil, err
		}
		if len(cfg.Servers) == 0 {
			return nil, fmt.Errorf("mcp: 配置 %s 未包含任何服务器", path)
		}
		return cfg, nil
	}
	native, nativeErr := unmarshalNativeConfig(data, path)
	if nativeErr == nil {
		return native, nil
	}
	if hasImportShapeJSON(data) {
		return importJSONConfig(data, path)
	}
	return nil, nativeErr
}

// hasImportShapeJSON 报告 data 是否呈社区配置形状：mcpServers 键，或以
// {名称: {定义}} 映射出现的 servers 键（数组值的 servers 是原生形状）。
func hasImportShapeJSON(data []byte) bool {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return false
	}
	if _, ok := top["mcpServers"]; ok {
		return true
	}
	if raw, ok := top["servers"]; ok {
		var dict map[string]json.RawMessage
		if err := json.Unmarshal(raw, &dict); err == nil {
			return true
		}
	}
	return false
}

// MountConfigFile 按配置文件挂载全部 MCP 服务器（每个实例独立命名空间）。
// DSHX 原生 {"servers":[...]} 格式仍然优先原样解析；文件呈社区形状时
// （Claude/Cursor/Gemini/Windsurf mcpServers 映射、VS Code servers 映射、
// Codex TOML）透明经 ImportConfig 翻译后挂载（生态收编 #1）。
// 任一配置/命名空间错误会回滚全部已挂载实例并返回错误；连接失败按
// 各实例 failOnStartupError 语义处理（默认转后台重连）。
func MountConfigFile(ctx context.Context, path string, reg *tools.ToolRegistry, logger *log.Logger) ([]*Supervisor, error) {
	cfg, err := loadConfigForMount(path)
	if err != nil {
		return nil, err
	}
	return MountConfig(ctx, cfg, reg, logger)
}

// MountConfig 把加载/导入后的配置挂载进 reg（MountConfigFile 的核心循环）。
// 供 ACP session/new 内联 mcpServers 透传等内存配置来源复用。
// 任一配置/命名空间错误会回滚全部已挂载实例并返回错误。
func MountConfig(ctx context.Context, cfg *FileConfig, reg *tools.ToolRegistry, logger *log.Logger) ([]*Supervisor, error) {
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
