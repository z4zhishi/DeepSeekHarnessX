package mcp

// MCP 配置导入器（生态收编）：把社区 MCP 配置形状翻译为 DSHX 的
// FileConfig/ServerConfig，让用户既有配置文件原样挂载（docs/supremacy-plan.md
// §2 生态收编 #1）。
//
// 按扩展名分派（ImportConfig / ImportConfigFile）：
//
//  1. .json —— {"mcpServers": {"name": {...}}}（Claude Desktop / Cursor /
//     Gemini / Windsurf）。变体键：远端 "url" + 可选 "headers"（可无 type）；
//     Windsurf 旧键 "serverUrl"；Gemini 流式远端键 "httpUrl"、超时 "timeout"
//     （毫秒）；"type" 可选（"stdio" | "http"，显式给出则校验与推断一致）。
//  2. .json —— {"servers": {"name": {...}}}（VS Code 形状；键为映射。值为
//     数组时是 DSHX 原生形状，走直通，不翻译）。
//  3. .toml —— Codex config.toml 子集：[mcp_servers.NAME] command/args/
//     env_vars/tool_timeout_sec（秒）+ [mcp_servers.NAME.env] K = "v"；远端
//     url + bearer_token_env_var + http_headers 内联表；文件内其它节一律忽略。
//
// 统一翻译规则（translateServerEntry）：
//   - map 键 → serverName；
//   - transport 推断：command → stdio；url/serverUrl/httpUrl → streamable-http；
//   - 启动（导入）期展开 ${VAR} / ${env:VAR} / ${VAR:-default}（进程环境；
//     未定义且无默认值 → 空串），作用于 command/args/env/cwd/url/headers；
//   - Codex tool_timeout_sec（秒）→ toolCallTimeoutMs（毫秒）；env_vars 按名
//     解析自宿主环境（未定义者省略）；Gemini timeout（毫秒）→ toolCallTimeoutMs；
//   - includeTools/excludeTools/trust/startup_timeout_sec/envFile 等未建模字段
//     静默忽略（ServerConfig v1 无 raw passthrough 字段）；
//   - 未识别 "type"、矛盾字段、空服务器清单、未知扩展名 → fail loud。
//
// TOML 解析为有意最小化的手写子集（toml_subset.go，零新依赖），仅覆盖上述
// 配置形状；子集之外的构造报错并给出行号。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	transportStdio          = "stdio"
	transportStreamableHTTP = "streamable-http"
)

// ImportConfig 把 r 中的社区 MCP 配置翻译为 FileConfig；格式按 filename 的
// 扩展名分派（.json / .toml）。错误消息始终携带 filename；TOML 尽力给出行号。
func ImportConfig(r io.Reader, filename string) (*FileConfig, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("mcp: 读取配置 %s: %w", filename, err)
	}
	var cfg *FileConfig
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".json":
		cfg, err = importJSONConfig(data, filename)
	case ".toml":
		cfg, err = importTOMLConfig(data, filename)
	default:
		err = fmt.Errorf("mcp: 配置 %s 的扩展名无法识别（支持 .json / .toml）", filename)
	}
	if err != nil {
		return nil, err
	}
	if len(cfg.Servers) == 0 {
		return nil, fmt.Errorf("mcp: 配置 %s 未包含任何服务器", filename)
	}
	return cfg, nil
}

// ImportConfigFile 读取磁盘上的社区 MCP 配置并翻译为 FileConfig。
func ImportConfigFile(path string) (*FileConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("mcp: 读取配置 %s: %w", path, err)
	}
	return ImportConfig(bytes.NewReader(data), path)
}

// importJSONConfig 分派 JSON 形状：mcpServers 映射 → servers 映射（VS Code）
// → DSHX 原生 {"servers":[...]} 直通。三者皆不匹配时按原生语义报错。
func importJSONConfig(data []byte, filename string) (*FileConfig, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("mcp: 解析配置 %s: %w", filename, err)
	}
	if raw, ok := top["mcpServers"]; ok && string(raw) != "null" {
		var dict map[string]json.RawMessage
		if err := json.Unmarshal(raw, &dict); err != nil {
			return nil, fmt.Errorf("mcp: 配置 %s: mcpServers 必须是 {名称: {定义}} 映射: %v", filename, err)
		}
		return importServersDict(dict, filename)
	}
	if raw, ok := top["servers"]; ok && string(raw) != "null" {
		var dict map[string]json.RawMessage
		if err := json.Unmarshal(raw, &dict); err == nil {
			// VS Code 形状：servers 是 {名称: {定义}} 映射；数组值落入原生直通。
			return importServersDict(dict, filename)
		}
	}
	var cfg FileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("mcp: 解析配置 %s: %w", filename, err)
	}
	return &cfg, nil // 空服务器清单由 ImportConfig 收口报错（原生同语义）
}

// importServersDict 翻译 {名称: {定义}} 映射（Claude/VS Code/Codex 归一后）。
// 按 map 键稳定排序，保证挂载顺序与报错顺序确定。
func importServersDict(dict map[string]json.RawMessage, filename string) (*FileConfig, error) {
	names := make([]string, 0, len(dict))
	for name := range dict {
		names = append(names, name)
	}
	sort.Strings(names)
	cfg := &FileConfig{Servers: make([]ServerConfig, 0, len(names))}
	for _, name := range names {
		sc, err := translateServerEntry(name, dict[name], filename)
		if err != nil {
			return nil, err
		}
		cfg.Servers = append(cfg.Servers, sc)
	}
	return cfg, nil
}

// importTOMLConfig 解析 Codex TOML 子集并抽取 [mcp_servers.*] 节（其余节
// 忽略），归一为通用 JSON 形状后走 importServersDict 统一翻译。
func importTOMLConfig(data []byte, filename string) (*FileConfig, error) {
	root, err := parseTOMLSubset(data)
	if err != nil {
		return nil, fmt.Errorf("mcp: 解析配置 %s: %s", filename, err)
	}
	servers, ok := root["mcp_servers"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("mcp: 配置 %s 未包含 [mcp_servers.*] 节（仅解析 Codex 形状，文件其余部分被忽略）", filename)
	}
	dict := make(map[string]json.RawMessage, len(servers))
	for name, value := range servers {
		entry, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("mcp: 配置 %s: [mcp_servers.%s] 的定义必须是键值表", filename, name)
		}
		norm, err := normalizeCodexEntry(filename, name, entry)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(norm)
		if err != nil {
			return nil, fmt.Errorf("mcp: 配置 %s: [mcp_servers.%s] 无法翻译: %w", filename, name, err)
		}
		dict[name] = raw
	}
	return importServersDict(dict, filename)
}

// normalizeCodexEntry 把 Codex TOML 专属键归一为通用 JSON 形状：
//   - env_vars = ["K", ...] → 按名从宿主环境透传（未定义的变量省略）；
//   - http_headers → headers；
//   - bearer_token_env_var → headers["Authorization"] = "Bearer ${VAR}" 模板
//     （与既有 Authorization 头冲突报错），随统一 ${VAR} 展开在导入期解析；
//   - tool_timeout_sec（秒）→ toolCallTimeoutMs（毫秒）；
//   - startup_timeout_sec 等未建模字段静默忽略。
func normalizeCodexEntry(filename, name string, entry map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(entry))
	for k, v := range entry {
		out[k] = v
	}
	if raw, ok := out["env_vars"]; ok {
		names, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("mcp: 配置 %s: [mcp_servers.%s] 的 env_vars 必须是字符串数组", filename, name)
		}
		env, _ := out["env"].(map[string]any)
		for _, item := range names {
			key, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("mcp: 配置 %s: [mcp_servers.%s] 的 env_vars 元素必须是字符串", filename, name)
			}
			if v, ok := os.LookupEnv(key); ok {
				if env == nil {
					env = map[string]any{}
				}
				env[key] = v
			}
		}
		out["env"] = env
		delete(out, "env_vars")
	}
	if raw, ok := out["http_headers"]; ok {
		headers, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("mcp: 配置 %s: [mcp_servers.%s] 的 http_headers 必须是内联表", filename, name)
		}
		out["headers"] = headers
		delete(out, "http_headers")
	}
	if raw, ok := out["bearer_token_env_var"]; ok {
		envKey, ok := raw.(string)
		if !ok || strings.TrimSpace(envKey) == "" {
			return nil, fmt.Errorf("mcp: 配置 %s: [mcp_servers.%s] 的 bearer_token_env_var 必须是非空字符串", filename, name)
		}
		headers, _ := out["headers"].(map[string]any)
		if headers == nil {
			headers = map[string]any{}
		}
		if _, conflict := headers["Authorization"]; conflict {
			return nil, fmt.Errorf("mcp: 配置 %s: [mcp_servers.%s] 的 bearer_token_env_var 与 http_headers 中的 Authorization 头冲突", filename, name)
		}
		headers["Authorization"] = "Bearer ${" + envKey + "}"
		out["headers"] = headers
		delete(out, "bearer_token_env_var")
	}
	if raw, ok := out["tool_timeout_sec"]; ok {
		secs, ok := raw.(int64)
		if !ok || secs < 0 {
			return nil, fmt.Errorf("mcp: 配置 %s: [mcp_servers.%s] 的 tool_timeout_sec 必须是非负整数秒", filename, name)
		}
		out["toolCallTimeoutMs"] = int(secs) * 1000
		delete(out, "tool_timeout_sec")
	}
	delete(out, "startup_timeout_sec")
	return out, nil
}

// translateServerEntry 把一条社区服务器定义翻译为 ServerConfig。字段类型不符、
// transport 无法唯一推断/矛盾、type 无法识别、负超时都 fail loud，报错携带
// 位置（where = 文件路径）与服务器名。
func translateServerEntry(name string, raw json.RawMessage, where string) (ServerConfig, error) {
	var e struct {
		Type              string            `json:"type"`
		Command           string            `json:"command"`
		Args              []string          `json:"args"`
		Env               map[string]string `json:"env"`
		Cwd               string            `json:"cwd"`
		URL               string            `json:"url"`
		ServerURL         string            `json:"serverUrl"`   // Windsurf 旧键
		HTTPURL           string            `json:"httpUrl"`     // Gemini 流式远端
		Headers           map[string]string `json:"headers"`
		TimeoutMs         *int              `json:"timeout"` // Gemini，毫秒
		DSHXToolCallMs    *int              `json:"toolCallTimeoutMs"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return ServerConfig{}, fmt.Errorf("mcp: %s: 服务器 %q 定义不合法: %w", where, name, err)
	}
	sc := ServerConfig{
		ServerName: name,
		Command:    strings.TrimSpace(e.Command),
		Args:       e.Args,
		Env:        e.Env,
		Cwd:        e.Cwd,
		Headers:    e.Headers,
	}
	sc.URL = strings.TrimSpace(e.URL)
	if sc.URL == "" {
		sc.URL = strings.TrimSpace(e.ServerURL)
	}
	if sc.URL == "" {
		sc.URL = strings.TrimSpace(e.HTTPURL)
	}
	// transport 推断：command → stdio；url/serverUrl/httpUrl → streamable-http。
	switch {
	case sc.Command != "" && sc.URL != "":
		return ServerConfig{}, fmt.Errorf("mcp: %s: 服务器 %q 同时定义了 command 与 url/serverUrl/httpUrl，transport 无法唯一推断", where, name)
	case sc.Command != "":
		sc.Transport = transportStdio
	case sc.URL != "":
		sc.Transport = transportStreamableHTTP
	default:
		return ServerConfig{}, fmt.Errorf("mcp: %s: 服务器 %q 缺少 command（stdio）或 url/serverUrl/httpUrl（streamable-http）", where, name)
	}
	// 显式 type 校验（与推断矛盾即报错）。
	switch e.Type {
	case "":
	case "stdio":
		if sc.Transport != transportStdio {
			return ServerConfig{}, fmt.Errorf("mcp: %s: 服务器 %q 的 type=%q 与定义字段矛盾", where, name, e.Type)
		}
	case "http":
		if sc.Transport != transportStreamableHTTP {
			return ServerConfig{}, fmt.Errorf("mcp: %s: 服务器 %q 的 type=%q 与定义字段矛盾", where, name, e.Type)
		}
	default:
		return ServerConfig{}, fmt.Errorf("mcp: %s: 服务器 %q 的 type %q 无法识别（支持 stdio / http）", where, name, e.Type)
	}
	// 超时：DSHX 原生毫秒键优先，其次 Gemini timeout（毫秒）。
	if e.DSHXToolCallMs != nil {
		sc.ToolCallTimeoutMs = *e.DSHXToolCallMs
	} else if e.TimeoutMs != nil {
		sc.ToolCallTimeoutMs = *e.TimeoutMs
	}
	if sc.ToolCallTimeoutMs < 0 {
		return ServerConfig{}, fmt.Errorf("mcp: %s: 服务器 %q 的超时不能为负", where, name)
	}
	// ${VAR} / ${env:VAR} / ${VAR:-default} 导入期展开（原生直通路径不展开）。
	sc.Command = expandEnvRefs(sc.Command)
	sc.Cwd = expandEnvRefs(sc.Cwd)
	sc.URL = expandEnvRefs(sc.URL)
	for i, arg := range sc.Args {
		sc.Args[i] = expandEnvRefs(arg)
	}
	for k, v := range sc.Env {
		sc.Env[k] = expandEnvRefs(v)
	}
	for k, v := range sc.Headers {
		sc.Headers[k] = expandEnvRefs(v)
	}
	return sc, nil
}

// envRefRE 匹配 ${env:VAR} / ${VAR:-default} / ${VAR}（RE2 左优先分支；
// VAR 字符类不含 ':'，两类构造无歧义）。
var envRefRE = regexp.MustCompile(`\$\{env:([A-Za-z_][A-Za-z0-9_]*)\}|\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^{}]*))?\}`)

// expandEnvRefs 在导入期展开环境变量引用：${VAR} 与 ${env:VAR} 取进程环境
// （未定义 → 空串）；${VAR:-default} 在 VAR 未定义或为空时取默认值。
// 无 ${ 的串原样返回；不匹配的文本（如无花括号的 $VAR）保持字面。
func expandEnvRefs(s string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	return envRefRE.ReplaceAllStringFunc(s, func(match string) string {
		groups := envRefRE.FindStringSubmatch(match)
		if groups[1] != "" { // ${env:VAR}
			v, _ := os.LookupEnv(groups[1])
			return v
		}
		// ${VAR} / ${VAR:-default}：未定义或空值 → 默认值（无默认 → 空串）。
		if v, ok := os.LookupEnv(groups[2]); ok && v != "" {
			return v
		}
		return groups[3]
	})
}