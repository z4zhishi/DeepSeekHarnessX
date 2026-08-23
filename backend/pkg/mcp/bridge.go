package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"dsh-go/pkg/tools"
)

// 上游契约（CK/packages/mcp/mcp-client/src/tools.ts，@b150a55）：
// - MCP 工具稳定身份 (serverName, rawName)；模型可见名 mcp__<serverName>__<rawName>
// - DeepSeek 函数名契约：≤64 字符，仅 [A-Za-z0-9_-]；有损归一化时追加身份 SHA-256 前 12 位
// - 两阶段同步：fetch 阶段完整构建下一整代，失败不动现有注册；
//   swap 阶段先释放上一代再注册新一代，注册冲突整代回滚，绝不出现半套工具
// - isError:true 的 tools/call 结果抛错，让执行管线产出 isError 结果给模型

const (
	maxPublicNameLength  = 64
	nameHashLength       = 12
	serverNamePatternStr = `^[A-Za-z0-9_-]{1,32}$`
	defaultToolCallMs    = 60_000
)

var invalidNameRE = regexp.MustCompile(`[^A-Za-z0-9_-]`)
var serverNameRE = regexp.MustCompile(serverNamePatternStr)

// publicToolName 推导模型可见工具名（纯函数，确定性强）。
// 干净情形原样返回 mcp__<serverName>__<rawName>；发生字符替换或截断时，
// 追加 (serverName, rawName) 身份的 SHA-256 前 12 位 hex，杜绝身份坍缩。
func publicToolName(serverName, rawName string) string {
	joined := "mcp__" + serverName + "__" + rawName
	normalized := invalidNameRE.ReplaceAllString(joined, "_")
	if normalized == joined && len(normalized) <= maxPublicNameLength {
		return normalized
	}
	sum := sha256.Sum256([]byte(serverName + "\x00" + rawName))
	hash := hex.EncodeToString(sum[:])[:nameHashLength]
	// 与上游 JS slice 语义一致：截断位自动 clamp（短名不截断，直接追加 hash）。
	cut := len(normalized)
	if cut > maxPublicNameLength-nameHashLength-1 {
		cut = maxPublicNameLength - nameHashLength - 1
	}
	return normalized[:cut] + "_" + hash
}

// McpContentBlock 读取 MCP content 块；位于网络信任边界，字段从宽。
type McpContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
	Name     string `json:"name,omitempty"`
	URI      string `json:"uri,omitempty"`
}

// McpResult 是暴露给模型/调用方的规范化 MCP 结果。
type McpResult struct {
	Content           []McpContentBlock `json:"content"`
	StructuredContent json.RawMessage   `json:"structuredContent,omitempty"`
}

// ListToolsResult 对齐 MCP tools/list 结果（分页）。
type ListToolsResult struct {
	Tools      []ToolSpec `json:"tools"`
	NextCursor string     `json:"nextCursor,omitempty"`
}

// ToolSpec 是远端 MCP 工具描述。
type ToolSpec struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
}

// CallToolResult 对齐 MCP tools/call 结果。
type CallToolResult struct {
	Content           []McpContentBlock `json:"content"`
	StructuredContent json.RawMessage   `json:"structuredContent,omitempty"`
	IsError           bool              `json:"isError"`
}

// extractText 拼接文本块（含 image 块的诊断占位）。
func extractText(content []McpContentBlock, rawName string) string {
	var b strings.Builder
	for _, block := range content {
		switch block.Type {
		case "text":
			b.WriteString(block.Text)
		case "image":
			mt := block.MimeType
			if mt == "" {
				mt = "unknown media type"
			}
			fmt.Fprintf(&b, "[image unavailable: %s; raw image data remains available to programmatic callers]", mt)
		case "resource":
			if block.URI != "" {
				fmt.Fprintf(&b, "[resource: %s]", block.URI)
			}
		default:
			fmt.Fprintf(&b, "[block type %s]", block.Type)
		}
	}
	return b.String()
}

// bridgeOptions 是同步期的桥接参数（由 BridgeOptions 派生）。
type bridgeOptions struct {
	serverName          string
	toolCallTimeout     time.Duration
	registrationFailure string // "contain" | "throw"
}

func (o BridgeOptions) bridge() bridgeOptions {
	return bridgeOptions{
		serverName:          o.ServerName,
		toolCallTimeout:     o.ToolCallTimeout,
		registrationFailure: o.RegistrationFailure,
	}
}

// fetchGeneration 分页拉取远端工具列表并构建下一代的定义。
// 任何失败（网络、重复 rawName）返回错误，注册表保持原样。
func fetchGeneration(ctx context.Context, conn connection, opts bridgeOptions) (map[string]tools.ToolDefinition, error) {
	definitions := map[string]tools.ToolDefinition{}
	cursor := ""
	for {
		page, err := conn.listTools(ctx, cursor)
		if err != nil {
			return nil, fmt.Errorf("mcp(%s): tools/list 失败: %w", opts.serverName, err)
		}
		for _, tool := range page.Tools {
			pub := publicToolName(opts.serverName, tool.Name)
			if _, dup := definitions[pub]; dup {
				return nil, fmt.Errorf("mcp(%s): 服务器重复列出工具 %q", opts.serverName, tool.Name)
			}
			schema := tool.InputSchema
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			rawName := tool.Name
			definitions[pub] = tools.ToolDefinition{
				Name:           pub,
				Description:    tool.Description,
				ParametersJSON: schema,
				RequiresPerm:   true,
				Execute: func(ctx tools.ToolExecutionContext, argsJSON string) (any, error) {
					var arguments map[string]any
					_ = json.Unmarshal([]byte(argsJSON), &arguments)
					if arguments == nil {
						arguments = map[string]any{}
					}
					result, err := conn.callTool(ctx.Context, rawName, arguments)
					if err != nil {
						return nil, fmt.Errorf("mcp tool %s: %w", pub, err)
					}
					if result.IsError {
						return nil, fmt.Errorf("mcp tool %s: %s", pub, extractText(result.Content, rawName))
					}
					m := McpResult{Content: result.Content}
					if len(result.StructuredContent) > 0 {
						m.StructuredContent = result.StructuredContent
					}
					return m, nil
				},
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return definitions, nil
}
