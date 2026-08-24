package plugin

// Claude Code 风格 hooks 运行时（对齐 CK/packages/hooks：hook-protocol +
// hooks-claude-code，生态兼容 P1）。
//
// 本文件是文件所有者：实现
//   - ParseHooks / LoadHooks：解析 CC 风格 hooks.json（事件 → matcher 组 → command 钩子）；
//   - ${CLAUDE_PLUGIN_ROOT} / ${CLAUDE_PROJECT_DIR} 变量替换（parse 期）；
//   - CommandHook 执行：shell 命令 + 每钩子超时 + 错误隔离（单个钩子失败不阻断主流程）；
//   - Hooks.Dispatch：在某拦截点对 subject 运行匹配钩子，回调上报 invoked/result 载荷。
//
// hook/invoked + hook/result 事件（session/events.go:32-33 已预埋）由本包以相同字符串
// 在拦截点真实发出——接线见 eventbus.go 的 EventBus.DispatchHook。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// CC 拦截点事件名（dialect "claude-code"）。本运行时真实分发全部 7 个拦截点。
const (
	HookPointSessionStart     = "SessionStart"
	HookPointUserPromptSubmit = "UserPromptSubmit"
	HookPointPreToolUse       = "PreToolUse"
	HookPointPostToolUse      = "PostToolUse"
	HookPointStop             = "Stop"
	HookPointSubagentStart    = "SubagentStart"
	HookPointSubagentStop     = "SubagentStop"
)

// HookDialectClaudeCode 是运行钩子的桥接方言，写进 hook/invoked 载荷。
const HookDialectClaudeCode = "claude-code"

// 真实分发（执行命令）的拦截点；全部 7 个 CC 事件点均分发。
var dispatchedHookPoints = map[string]bool{
	HookPointSessionStart:     true,
	HookPointUserPromptSubmit: true,
	HookPointPreToolUse:       true,
	HookPointPostToolUse:      true,
	HookPointStop:             true,
	HookPointSubagentStart:    true,
	HookPointSubagentStop:     true,
}

// 无 matcher 的拦截点（CC 丢弃其 matcher 字段，matcher 一律视为匹配全部）。
var matcherlessHookPoints = map[string]bool{
	HookPointUserPromptSubmit: true,
	HookPointStop:             true,
}

// 全部受支持的 CC 事件名（用于解析；dialect 通用清单）。
var allHookPoints = []string{
	HookPointSessionStart, HookPointUserPromptSubmit, HookPointPreToolUse,
	HookPointPostToolUse, HookPointStop, HookPointSubagentStart, HookPointSubagentStop,
}

// 默认钩子超时（对齐 hook-protocol DEFAULT_HOOK_TIMEOUT_MS = 10 分钟）。
const defaultHookTimeout = 10 * time.Minute

// CommandHook 是一条配置的命令钩子（{type:"command", command, timeout?}）。
// 非 command 类型（prompt/agent/http）在解析时跳过并记为 skipped。
type CommandHook struct {
	Command    string `json:"command"`
	TimeoutSec int    `json:"timeout,omitempty"` // 秒；0 = 用默认超时
}

// MatcherGroup 是一个 matcher 组：matcher 模式（缺省/”/'*' = 匹配全部）+ 命中命令。
type MatcherGroup struct {
	Pattern string        `json:"matcher,omitempty"`
	Hooks   []CommandHook `json:"hooks"`
}

// Hooks 是解析后的 CC hooks 配置：事件名 → matcher 组。构造后只读（Dispatch 可并发）。
type Hooks struct {
	groups     map[string][]MatcherGroup
	pluginRoot string
	projectDir string
	shell      string
	seq        atomic.Int64 // handlerId 序列，保证 invoked/result 可配对
}

// ParseOptions 是解析期替换与运行时参数。
type ParseOptions struct {
	// PluginRoot 替换 ${CLAUDE_PLUGIN_ROOT}。
	PluginRoot string
	// ProjectDir 替换 ${CLAUDE_PROJECT_DIR}。
	ProjectDir string
	// Shell 是钩子命令的 shell；空用默认（sh，PATH 无则回退 cmd）。
	Shell string
}

// SkippedHook 记录被跳过的非 command 钩子（宿主可告警）。
type SkippedHook struct {
	Event string
	Type  string
}

// LoadHooks 读取一份磁盘 hooks.json（CC 事件映射或 {hooks:{...}} 设置文件）并解析。
func LoadHooks(path string) (*Hooks, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	h, _, err := ParseHooks(data, ParseOptions{})
	return h, err
}

// ParseHooks 解析 CC 风格 hooks.json。畸形条目被忽略而非导致启动失败（对齐
// parseClaudeCodeConfig 的宽容语义）。返回可运行钩子组与跳过的非 command 钩子。
func ParseHooks(data []byte, opts ParseOptions) (*Hooks, []SkippedHook, error) {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil, fmt.Errorf("解析 hooks.json 失败: %w", err)
	}
	h := &Hooks{
		groups:     map[string][]MatcherGroup{},
		pluginRoot: opts.PluginRoot,
		projectDir: opts.ProjectDir,
		shell:      opts.Shell,
	}
	skipped, err := h.parse(raw)
	if err != nil {
		return nil, nil, err
	}
	return h, skipped, nil
}

// parse 把解析后的 JSON 折叠为事件 → matcher 组（仅 command 钩子），并应用替换。
func (h *Hooks) parse(raw any) ([]SkippedHook, error) {
	var skipped []SkippedHook
	root := asObject(raw)
	hooksMap := root
	if root != nil {
		if inner := asObject(root["hooks"]); inner != nil {
			hooksMap = inner
		}
	}
	if hooksMap == nil {
		return skipped, nil
	}
	for _, point := range allHookPoints {
		rawGroups, ok := hooksMap[point]
		if !ok {
			continue
		}
		groups, err := h.parseEventGroups(point, rawGroups, &skipped)
		if err != nil {
			return nil, err
		}
		if len(groups) > 0 {
			h.groups[point] = groups
		}
	}
	return skipped, nil
}

// parseEventGroups 解析一个事件的 matcher 组数组，应用变量替换；对不支持的 matcher
// 正则返回错误（宿主可在注册监听前整体拒绝）。
func (h *Hooks) parseEventGroups(point string, rawGroups any, skipped *[]SkippedHook) ([]MatcherGroup, error) {
	arr, ok := rawGroups.([]any)
	if !ok {
		return nil, nil
	}
	var groups []MatcherGroup
	for _, rawGroup := range arr {
		group := asObject(rawGroup)
		if group == nil {
			continue
		}
		rawHooks, ok := group["hooks"].([]any)
		if !ok {
			continue
		}
		var cmds []CommandHook
		for _, rawHook := range rawHooks {
			hook := asObject(rawHook)
			if hook == nil {
				continue
			}
			typ, _ := hook["type"].(string)
			if typ == "" {
				typ = "command"
			}
			if typ != "command" {
				*skipped = append(*skipped, SkippedHook{Event: point, Type: typ})
				continue
			}
			cmd, ok := hook["command"].(string)
			if !ok {
				continue
			}
			ch := CommandHook{Command: h.substitute(cmd)}
			if to, ok := hook["timeout"].(float64); ok {
				ch.TimeoutSec = int(to)
			}
			cmds = append(cmds, ch)
		}
		if len(cmds) == 0 {
			continue
		}
		var matcher string
		if !matcherlessHookPoints[point] {
			if s, ok := group["matcher"].(string); ok {
				matcher = s
			}
		}
		if !isMatchAll(matcher) && !isClaudeLiteral(matcher) && !validRegex(matcher) {
			return nil, fmt.Errorf("invalid claude-code regex matcher %q on event %q", matcher, point)
		}
		groups = append(groups, MatcherGroup{Pattern: matcher, Hooks: cmds})
	}
	return groups, nil
}

// substitute 替换 ${CLAUDE_PLUGIN_ROOT} / ${CLAUDE_PROJECT_DIR}；未设置变量保持原样。
func (h *Hooks) substitute(s string) string {
	if h.pluginRoot != "" {
		s = strings.ReplaceAll(s, "${CLAUDE_PLUGIN_ROOT}", h.pluginRoot)
	}
	if h.projectDir != "" {
		s = strings.ReplaceAll(s, "${CLAUDE_PROJECT_DIR}", h.projectDir)
	}
	return s
}

// --- matcher 语义（对齐 matcher.ts，mode = claude-code）---

// isMatchAll 判定缺省/空/'*' 的匹配全部哨兵。
func isMatchAll(m string) bool { return m == "" || m == "*" }

// claudeLiteralRegex 是 CC 字面量判别器：纯词字符 + '|'。
var claudeLiteralRegex = regexp.MustCompile(`^[A-Za-z0-9_|]+$`)

func isClaudeLiteral(m string) bool { return claudeLiteralRegex.MatchString(m) }

// validRegex 报告 m 能否编译为未锚定正则。
func validRegex(m string) bool {
	_, err := regexp.Compile(m)
	return err == nil
}

// matches 判定 matcher 是否命中 subject（CC 语义：纯字面量=管道分隔精确匹配，
// 其余=未锚定正则；无效正则视为不匹配）。
func matches(m, subject string) bool {
	if isMatchAll(m) {
		return true
	}
	if isClaudeLiteral(m) {
		for _, alt := range strings.Split(m, "|") {
			if alt == subject {
				return true
			}
		}
		return false
	}
	re, err := regexp.Compile(m)
	if err != nil {
		return false
	}
	return re.MatchString(subject)
}

// --- 结果类型 ---

// HookOutcome 是单次钩子运行的解码结果（对齐 HookOutput 的决策语义）。任一钩子失败
// 仅记录（Err 非空 / exit 2），不阻断后续钩子与主流程。
type HookOutcome struct {
	ExitCode   int
	Decision   string // "block"(exit2) / "pass"(干净退出) / 无；Err 非空时恒 "pass"
	Stdout     string
	Stderr     string
	DurationMs int64
	Err        error // 命令无法运行（缺 shell 等基础设施错误）时非 nil
}

// HookInvokedPayload 是 hook/invoked 载荷。
type HookInvokedPayload struct {
	Turn      int    `json:"turn"`
	Point     string `json:"point"`
	Dialect   string `json:"dialect"`
	Matcher   string `json:"matcher,omitempty"`
	HandlerID string `json:"handlerId"`
}

// HookResultPayload 是 hook/result 载荷。
type HookResultPayload struct {
	Turn          int    `json:"turn"`
	Point         string `json:"point"`
	HandlerID     string `json:"handlerId"`
	Decision      string `json:"decision"`
	ExitCode      int    `json:"exitCode,omitempty"`
	StderrSummary string `json:"stderrSummary,omitempty"`
	DurationMs    int64  `json:"durationMs"`
}

// Dispatch 在某拦截点对 subject 运行匹配的钩子命令。逐个隔离执行：任一钩子失败都
// 记录但不阻断后续钩子与主流程。每次钩子调用先回调 onInvoked 再运行，运行后回调
// onResult，让宿主持序发出 hook/invoked + hook/result 事件（按 handlerId 配对）。
//
// point 不受支持（非 7 个 CC 拦截点）时不运行（返回空）。UserPromptSubmit/Stop
// 忽略 subject（其 matcher 为空，恒命中）。
func (h *Hooks) Dispatch(ctx context.Context, point, subject string, turn int,
	onInvoked func(HookInvokedPayload), onResult func(HookResultPayload)) []HookOutcome {
	if h == nil || !dispatchedHookPoints[point] {
		return nil
	}
	var outcomes []HookOutcome
	for _, g := range h.groups[point] {
		if !matches(g.Pattern, subject) {
			continue
		}
		for _, hook := range g.Hooks {
			handlerID := fmt.Sprintf("%s-%d", point, h.seq.Add(1))
			inv := HookInvokedPayload{
				Turn: turn, Point: point, Dialect: HookDialectClaudeCode,
				HandlerID: handlerID,
			}
			if !isMatchAll(g.Pattern) {
				inv.Matcher = g.Pattern
			}
			if onInvoked != nil {
				onInvoked(inv)
			}
			out := h.runOne(ctx, hook)
			res := HookResultPayload{
				Turn: turn, Point: point, HandlerID: handlerID,
				Decision:   summarizeDecision(out),
				ExitCode:   out.ExitCode,
				DurationMs: out.DurationMs,
			}
			if s := summarizeStderr(out.Stderr); s != "" {
				res.StderrSummary = s
			}
			if onResult != nil {
				onResult(res)
			}
			outcomes = append(outcomes, out)
		}
	}
	return outcomes
}

// runOne 执行单个钩子命令（超时 + 错误隔离）。
func (h *Hooks) runOne(ctx context.Context, hook CommandHook) HookOutcome {
	timeout := defaultHookTimeout
	if hook.TimeoutSec > 0 {
		timeout = time.Duration(hook.TimeoutSec) * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	shell := h.Shell()
	start := time.Now()
	cmd := exec.CommandContext(runCtx, shell, "-c", hook.Command)
	cmd.Env = h.hookEnv()
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	dur := time.Since(start)

	outcome := HookOutcome{
		Stdout:     strings.TrimSpace(outBuf.String()),
		Stderr:     strings.TrimSpace(errBuf.String()),
		DurationMs: dur.Milliseconds(),
	}
	if err != nil {
		// 命令无法运行 / 非零退出 / 超时——非阻断，记录即可。
		outcome.Err = err
		outcome.Decision = "pass"
		return outcome
	}
	outcome.Decision = "pass"
	if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 2 {
		outcome.Decision = "block"
	}
	outcome.ExitCode = 0
	if cmd.ProcessState != nil {
		outcome.ExitCode = cmd.ProcessState.ExitCode()
	}
	return outcome
}

// summarizeDecision 把一次运行折叠为 durable decision（对齐 appendHookResult）：
// 干净退出默认 "pass"，阻塞（退出码 2）为 "block"；基础设施错误恒 "pass"。
func summarizeDecision(out HookOutcome) string { return out.Decision }

// summarizeStderr 裁剪 stderr 用于 hook/result（对齐 summarizeStderr，500 字符上限）。
func summarizeStderr(stderr string) string {
	t := strings.TrimSpace(stderr)
	if t == "" {
		return ""
	}
	const maxChars = 500
	if len(t) > maxChars {
		return t[:maxChars] + "…"
	}
	return t
}

// asObject 返回普通（非 null、非数组）对象，否则 nil。
func asObject(v any) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return m
}

// Shell 返回运行时 shell（显式设置或默认解析）。
func (h *Hooks) Shell() string {
	if h.shell != "" {
		return h.shell
	}
	if _, err := exec.LookPath("sh"); err == nil {
		return "sh"
	}
	if _, err := exec.LookPath("cmd"); err == nil {
		return "cmd"
	}
	return "sh"
}

// hookEnv 构建钩子进程环境（继承宿主并注入替换变量）。
func (h *Hooks) hookEnv() []string {
	env := os.Environ()
	if h.pluginRoot != "" {
		env = append(env, "CLAUDE_PLUGIN_ROOT="+h.pluginRoot)
	}
	if h.projectDir != "" {
		env = append(env, "CLAUDE_PROJECT_DIR="+h.projectDir)
	}
	return env
}
