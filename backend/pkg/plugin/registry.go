package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"dsh-go/pkg/llm"
	"dsh-go/pkg/mcp"
	"dsh-go/pkg/tools"
)

// Registry 收敛"扁平单例"为"能力 + 注册表 + 事件总线"的入口（目标架构
// §registry.go）。它持有共享的 ToolRegistry/CommandRegistry/EventBus，
// 并统一装配两类插件：
//
//   - 内置（编译期）能力：实现 Capability 接口，通过 Register 注册，
//     Reconcile 时 Mount 到共享注册表。
//   - 外部子进程插件：JSON-RPC server + manifest，通过 AddExternal 登记配置，
//     Reconcile 时拉起 Host 子进程并同步工具/命令/事件。
//
// 上手规则（docs/plugin-onboarding.md 摘要）：
//  1. 内置插件：实现 Capability{Name, Mount}，在 main.go 装配处 registry.Register。
//  2. 外部插件：任意语言写 JSON-RPC server（方法集见 host.go）+ 一份 manifest，
//     把可执行路径放进插件目录，Registry.Reconcile 扫描并挂载。
//  3. 两者共享同一 ToolRegistry/CommandRegistry/EventBus，行为即验收。
type Registry struct {
	mu          sync.Mutex
	reconcileMu sync.Mutex
	tools       *tools.ToolRegistry
	cmds        *tools.CommandRegistry
	bus         *EventBus
	llm         llm.LlmAdapter
	llmOwner    string
	store       any
	storeOwner  string
	hooks       *Hooks
	logger      interface{ Printf(string, ...any) }
	// LibRegistry 是计划中阶段 4 的 lib 依赖层：manifest.libs 声明在此解析。
	Libs *LibRegistry

	builtins   map[string]Capability
	external   map[string]External
	mounted    map[string]Disposer
	hosts      map[string]*Host
	pluginDir  string          // ScanDir/Install 的插件根；.disabled 持久化于此
	disabled   map[string]bool // 停用名；Reconcile 跳过拉起
	libBlocked map[string]bool // lib 依赖未满足；Reconcile 跳过拉起（面板显示 error）
	installed  map[string][]string
	lastErr    map[string]string
}

// NewRegistry 构造空注册表，注入共享的单例句柄。
func NewRegistry(toolReg *tools.ToolRegistry, cmds *tools.CommandRegistry, bus *EventBus, logger interface{ Printf(string, ...any) }) *Registry {
	return &Registry{
		tools:      toolReg,
		cmds:       cmds,
		bus:        bus,
		logger:     logger,
		Libs:       NewLibRegistry(),
		builtins:   map[string]Capability{},
		external:   map[string]External{},
		mounted:    map[string]Disposer{},
		hosts:      map[string]*Host{},
		disabled:   map[string]bool{},
		libBlocked: map[string]bool{},
		installed:  map[string][]string{},
		lastErr:    map[string]string{},
	}
}

// Register 注册一个编译期能力（内置插件）。返回是否已存在同名（替换）。
func (r *Registry) Register(c Capability) bool {
	if c == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	name := c.Name()
	_, existed := r.builtins[name]
	r.builtins[name] = c
	return existed
}

// External 描述一个待拉起的子进程插件配置（manifest + 传输）。
type External struct {
	Name         string
	Command      string
	Args         []string
	Env          map[string]string
	Cwd          string
	RequiresPerm *bool
	Reconnect    mcp.ReconnectConfig
	ABIVersion   int
	Capabilities []string
}

// AddExternal 登记一个外部子进程插件配置（Reconcile 时拉起）。
func (r *Registry) AddExternal(ext External) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ext.Name == "" || strings.TrimSpace(ext.Command) == "" {
		return
	}
	r.external[ext.Name] = ext
}

// ScanDir 扫描插件目录，把其中的 *.json 文件解析为外部插件 manifest 并登记。
// 读取 dir/.disabled（一行一个插件名），这些插件仍登记配置但 Reconcile 不会拉起。
// 目录不存在时静默返回（未配置插件目录即空操作）。
func (r *Registry) ScanDir(ctx context.Context, dir string) error {
	if dir == "" {
		return nil
	}
	r.mu.Lock()
	r.pluginDir = dir
	r.disabled = loadDisabled(dir)
	r.mu.Unlock()

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		// 一次读取：manifest + libs 约束都在 parseDiskPlugin 结果里。
		ext, man, err := loadPluginManifest(path)
		if err != nil {
			r.logf("plugin: 解析 %s 失败：%v", path, err)
			continue
		}
		// libs 声明在登记前解析（plan §Lib registry）：缺失/版本不满足即拒载。
		// 拒载 = libBlocked 阻止 Reconcile 拉起 + lastErr 面板可见，但
		// AddExternal 保留登记，使插件面板看到 error + lastErr 而非"凭空消失"。
		if len(man.Libs) > 0 && r.Libs != nil {
			if rerr := r.Libs.Resolve(man.Libs); rerr != nil {
				r.logf("plugin(%s): lib 解析失败：%v", ext.Name, rerr)
				r.mu.Lock()
				r.lastErr[ext.Name] = rerr.Error()
				r.libBlocked[ext.Name] = true
				r.mu.Unlock()
				r.AddExternal(ext)
				continue
			}
		}
		r.mu.Lock()
		wasBlocked := r.libBlocked[ext.Name]
		delete(r.libBlocked, ext.Name)
		// lib 约束通过：清除旧 lib 错误，使面板从 error 恢复为 installed。
		if wasBlocked {
			delete(r.lastErr, ext.Name)
		}
		r.mu.Unlock()
		r.AddExternal(ext)
	}
	return nil
}

// LoadExternalManifest 读取一份磁盘 manifest JSON 并构造 External 配置。
func LoadExternalManifest(path string) (External, error) {
	ext, _, err := loadPluginManifest(path)
	return ext, err
}

// diskPlugin 是磁盘 manifest 的完整 JSON 形状（契约字段 + 子进程启动字段）。
type diskPlugin struct {
	Name         string            `json:"name"`
	ABIVersion   int               `json:"abiVersion"`
	Command      string            `json:"command"`
	Args         []string          `json:"args"`
	Cwd          string            `json:"cwd"`
	Env          map[string]string `json:"env"`
	Capabilities []CapabilitySpec  `json:"capabilities"`
	Deps         []string          `json:"deps"`
	Libs         []LibDecl         `json:"libs"`
	Policy       *struct {
		RequiresPerm *bool                `json:"requiresPerm"`
		Reconnect    *mcp.ReconnectConfig `json:"reconnect"`
	} `json:"policy"`
}

func loadPluginManifest(path string) (External, Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return External{}, Manifest{}, err
	}
	ext, man, err := parseDiskPlugin(data)
	if err != nil {
		return External{}, man, err
	}
	ext.Command = resolveCommand(ext.Command, filepath.Dir(path), false)
	return ext, man, nil
}

func parseDiskPlugin(data []byte) (External, Manifest, error) {
	var m diskPlugin
	if err := json.Unmarshal(data, &m); err != nil {
		return External{}, Manifest{}, err
	}
	abi := m.ABIVersion
	if abi == 0 {
		abi = ABIVersion
	}
	man := Manifest{
		Name:         m.Name,
		ABIVersion:   abi,
		Capabilities: m.Capabilities,
		Deps:         m.Deps,
		Libs:         m.Libs,
	}
	if m.Policy != nil {
		man.Policy = &Policy{RequiresPerm: m.Policy.RequiresPerm}
		if m.Policy.Reconnect != nil {
			rc := m.Policy.Reconnect
			man.Policy.Reconnect = &Reconnect{
				Enabled:        rc.Enabled,
				InitialDelayMs: rc.InitialDelayMs,
				MaxDelayMs:     rc.MaxDelayMs,
				MaxAttempts:    rc.MaxAttempts,
			}
		}
	}
	if err := man.Validate(); err != nil {
		return External{}, man, err
	}
	ext := External{
		Name:         m.Name,
		Command:      m.Command,
		Args:         m.Args,
		Cwd:          m.Cwd,
		Env:          m.Env,
		ABIVersion:   abi,
		Capabilities: capNames(m.Capabilities),
	}
	if m.Policy != nil {
		ext.RequiresPerm = m.Policy.RequiresPerm
		if m.Policy.Reconnect != nil {
			ext.Reconnect = *m.Policy.Reconnect
		}
	}
	return ext, man, nil
}

func capNames(specs []CapabilitySpec) []string {
	if len(specs) == 0 {
		return nil
	}
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		if s.Name != "" {
			out = append(out, s.Name)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Reconcile 挂载全部插件：内置能力先挂载，外部子进程随后拉起。
// 任一挂载失败记录到日志，不阻断其余插件（插件间隔离）。
// 插件代码（Mount / NewHost）在 registry 锁外执行。
func (r *Registry) Reconcile(ctx context.Context) {
	r.reconcileMu.Lock()
	defer r.reconcileMu.Unlock()

	type builtinJob struct {
		name string
		cap  Capability
	}
	r.mu.Lock()
	builtins := make([]builtinJob, 0, len(r.builtins))
	for name, cap := range r.builtins {
		if r.disabled[name] {
			continue
		}
		if _, ok := r.mounted[name]; ok {
			continue
		}
		builtins = append(builtins, builtinJob{name: name, cap: cap})
	}
	exts := make([]External, 0, len(r.external))
	for _, ext := range r.external {
		if r.disabled[ext.Name] || r.libBlocked[ext.Name] {
			continue
		}
		exts = append(exts, ext)
	}
	r.mu.Unlock()
	sort.SliceStable(builtins, func(i, j int) bool { return builtins[i].name < builtins[j].name })

	for _, job := range builtins {
		if err := r.mountBuiltin(job.cap); err != nil {
			r.mu.Lock()
			r.lastErr[job.name] = err.Error()
			r.mu.Unlock()
			r.logf("plugin(%s): 内置挂载失败：%v", job.name, err)
			continue
		}
		r.mu.Lock()
		delete(r.lastErr, job.name)
		r.mu.Unlock()
	}

	for _, ext := range exts {
		r.mu.Lock()
		if r.disabled[ext.Name] || r.libBlocked[ext.Name] {
			r.mu.Unlock()
			continue
		}
		if _, ok := r.mounted[ext.Name]; ok {
			r.mu.Unlock()
			continue
		}
		r.mu.Unlock()
		h, err := NewHost(ctx, hostConfig{
			Name:         ext.Name,
			Command:      ext.Command,
			Args:         ext.Args,
			Env:          ext.Env,
			Cwd:          ext.Cwd,
			RequiresPerm: ext.RequiresPerm,
			Reconnect:    ext.Reconnect,
			Logger:       logAdapter{r.logger},
		}, r.tools, r.cmds, r.bus)
		if err != nil {
			r.logf("plugin(%s): 挂载失败：%v", ext.Name, err)
			r.mu.Lock()
			r.lastErr[ext.Name] = err.Error()
			r.mu.Unlock()
			continue
		}
		r.mu.Lock()
		if r.disabled[ext.Name] {
			r.mu.Unlock()
			_ = h.Close()
			continue
		}
		delete(r.lastErr, ext.Name)
		r.hosts[ext.Name] = h
		r.mounted[ext.Name] = func() { _ = h.Close() }
		r.mu.Unlock()
	}
}

// logAdapter 适配 *log.Logger / nil 的 Printf 签名。
type logAdapter struct {
	l interface{ Printf(string, ...any) }
}

func (a logAdapter) Printf(format string, args ...any) {
	if a.l == nil {
		return
	}
	a.l.Printf(format, args...)
}

func (r *Registry) logf(format string, args ...any) {
	if r.logger == nil {
		return
	}
	r.logger.Printf(format, args...)
}

// NewWebCapability 是内置 web 能力的公共构造函数，避免其它包依赖未导出类型。
func NewWebCapability() Capability { return webCapability{} }

// FilesystemCapability is the builtin filesystem tool family.
func FilesystemCapability() Capability {
	return ToolFamilyCapability("filesystem", func(r *tools.ToolRegistry) {
		r.RegisterFSTools()
		r.RegisterGlobTool()
	}, "read_file", "write_file", "replace_file_content", "list_dir", "grep_search", "find_by_name", "glob")
}

// PolicyCapability mounts policy tools.
func PolicyCapability() Capability {
	return ToolFamilyCapability("policy", func(r *tools.ToolRegistry) {
		r.RegisterPolicyTools()
	}, "set_sandbox_mode", "set_approval_policy", "set_permission_preset")
}

// SessionQueryCapability mounts session query tools.
func SessionQueryCapability() Capability {
	return ToolFamilyCapability("session-query", func(r *tools.ToolRegistry) {
		r.RegisterSessionQueryTools()
	}, "session_search", "session_trace", "session_event_read")
}

// NewBuiltinToolsCapability remounts the legacy catch-all RegisterBuiltinTools
// unit. Production must not register this: it re-Registers family tools and
// wipes their owners. Kept for tests that still exercise the migration helper.
func NewBuiltinToolsCapability() Capability {
	return ToolFamilyCapability("builtin-tools", func(r *tools.ToolRegistry) {
		r.RegisterBuiltinTools()
	})
}

// ToolFamilyCapability creates a capability for an existing tool family.
func ToolFamilyCapability(name string, mount func(*tools.ToolRegistry), toolNames ...string) Capability {
	return toolFamilyCapability{name: name, mount: mount, tools: toolNames}
}

// TerminalCapability mounts terminal and persistent shell tools as one owner.
func TerminalCapability() Capability {
	return ToolFamilyCapability("terminal", func(r *tools.ToolRegistry) {
		r.RegisterRunCommandTool()
		r.RegisterTerminalTools()
		r.RegisterPersistentShellTools()
		r.RegisterPwshTools()
	}, "run_command", "terminal_open", "terminal_send", "terminal_read", "terminal_list", "terminal_signal", "terminal_close", "bash_persistent", "bash_reset", "pwsh_persistent", "pwsh_reset")
}

// CoreToolsCapability mounts leftover production tools that are not part of a
// dedicated family (slash commands, ask-user, plan-exit, schedules).
func CoreToolsCapability() Capability {
	return ToolFamilyCapability("core-tools", func(r *tools.ToolRegistry) {
		r.RegisterAskUserTool()
		r.RegisterExitPlanModeTool()
		r.RegisterScheduleTools()
		if r.Commands != nil {
			r.Commands.RegisterBuiltinCommands()
		}
	}, "ask_user_question", "exit_plan_mode", "schedule_create", "schedule_list", "schedule_delete")
}

// WorkflowCapability mounts long-running workflow orchestration tools.
func WorkflowCapability() Capability {
	return ToolFamilyCapability("workflow", func(r *tools.ToolRegistry) {
		r.RegisterWorkflowTools()
	}, "workflow_run")
}

// TeamCapability mounts agent team orchestration tools.
func TeamCapability() Capability {
	return ToolFamilyCapability("team", func(r *tools.ToolRegistry) {
		r.RegisterTeamTools()
	}, "spawn_teammate")
}

// TaskCapability mounts generic task helpers.
func TaskCapability() Capability {
	return ToolFamilyCapability("task", func(r *tools.ToolRegistry) {
		r.RegisterTaskTools()
	}, "todo_write", "plan_update")
}

// JobCapability mounts long-running job tools.
func JobCapability() Capability {
	return ToolFamilyCapability("jobs", func(r *tools.ToolRegistry) {
		r.RegisterJobTools()
	}, "job_output", "job_list", "job_kill")
}

// GoalCapability mounts goal tracking tools.
func GoalCapability() Capability {
	return ToolFamilyCapability("goal", func(r *tools.ToolRegistry) {
		r.RegisterGoalTools()
	}, "get_goal", "create_goal", "update_goal")
}

// ImageCapability mounts vision tooling.
func ImageCapability() Capability {
	return ToolFamilyCapability("image", func(r *tools.ToolRegistry) {
		r.RegisterImageTools()
	}, "read_image")
}

// SkillCapability mounts skill discovery and invocation tools.
func SkillCapability() Capability {
	return ToolFamilyCapability("skills", func(r *tools.ToolRegistry) {
		r.RegisterSkillTools()
	}, "skill", "skill_list")
}

type toolFamilyCapability struct {
	name  string
	mount func(*tools.ToolRegistry)
	tools []string
}

func (c toolFamilyCapability) Name() string { return c.name }
func (c toolFamilyCapability) Mount(cap *Capabilities) (Disposer, error) {
	if cap == nil || cap.Tools == nil || c.mount == nil {
		return nil, fmt.Errorf("plugin(%s): tool capability unavailable", c.name)
	}
	before := cap.Tools.Names()
	var cmdBefore []string
	if cap.Tools.Commands != nil {
		cmdBefore = cap.Tools.Commands.Names()
	}
	c.mount(cap.Tools)
	beforeSet := make(map[string]bool, len(before))
	for _, name := range before {
		beforeSet[name] = true
	}
	names := cap.Tools.Names()
	owned := make([]string, 0, len(names))
	for _, name := range names {
		if !beforeSet[name] || contains(c.tools, name) {
			owned = append(owned, name)
		}
	}
	cap.Tools.ClaimOwner(c.name, owned...)
	// Commands mounted by this family (e.g. RegisterBuiltinCommands inside
	// RegisterBuiltinTools) receive the same owner so uninstall removes the
	// whole family atomically instead of leaving stale command handlers.
	var ownedCmds []string
	if cap.Tools.Commands != nil {
		cmdBeforeSet := make(map[string]bool, len(cmdBefore))
		for _, name := range cmdBefore {
			cmdBeforeSet[name] = true
		}
		for _, name := range cap.Tools.Commands.Names() {
			if !cmdBeforeSet[name] {
				ownedCmds = append(ownedCmds, name)
			}
		}
		cap.Tools.Commands.ClaimOwner(c.name, ownedCmds...)
	}
	return func() {
		for _, name := range owned {
			cap.Tools.UnregisterOwned(c.name, name)
		}
		for _, name := range ownedCmds {
			cap.Tools.Commands.UnregisterOwned(c.name, name)
		}
	}, nil
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func (r *Registry) mountBuiltin(cap Capability) error {
	if cap == nil {
		return fmt.Errorf("plugin: nil capability")
	}
	name := cap.Name()
	ctx := &Capabilities{
		Tools:    r.tools,
		Commands: r.cmds,
		Events:   r.bus,
		Bridge:   NewCoreBridge(r.tools, r.cmds, r.bus),
		SetHooks: r.setHooks,
	}
	d, err := cap.Mount(ctx)
	if err != nil {
		return err
	}
	if d == nil {
		d = NopDisposer()
	}
	r.mu.Lock()
	if r.disabled[name] {
		r.mu.Unlock()
		d()
		return nil
	}
	r.mounted[name] = d
	if lc, ok := cap.(LlmCapability); ok {
		r.llm = lc.LlmAdapter()
		r.llmOwner = name
	}
	if sc, ok := cap.(SessionStoreCapability); ok {
		r.store = sc.SessionStore()
		r.storeOwner = name
	}
	r.mu.Unlock()
	return nil
}

func (r *Registry) OwnedTools(plugin string) []string {
	if r.tools == nil || plugin == "" {
		return nil
	}
	owners := r.tools.ToolOwners()
	out := make([]string, 0)
	for name, owner := range owners {
		if owner == plugin {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// OwnsTool reports whether a tool name is currently owned by plugin.
func (r *Registry) OwnsTool(plugin, tool string) bool {
	if r.tools == nil || plugin == "" || tool == "" {
		return false
	}
	owners := r.tools.ToolOwners()
	return owners[tool] == plugin
}

func (r *Registry) LlmAdapter() llm.LlmAdapter {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.llm
}

// ProtocolRegistry 返回内置 protocols 能力发布的线协议注册表，未注册该
// 能力（免插件运行时）返回 nil。
//
// 指针身份在能力整个注册期内恒定（实例属于 Capability，remount 复用同一
// 实例）：enable/disable 改变的是注册表内容（Mount 登记 / Disposer 注销
// 登记项），而不是句柄。disable 后 List()==空、Build 报 unknown protocol，
// 调用方（网关）因此不会把"协议已下线"静默降级为包默认适配器。每次构造
// 适配器前重读本指针即可，无缓存。
func (r *Registry) ProtocolRegistry() ProtocolRegistry {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.builtins[ProtocolCapabilityName]; ok {
		if pc, ok := c.(ProtocolsCapability); ok {
			return pc.Protocols()
		}
	}
	return nil
}

// EventBus is the process-shared plugin bus. Agents attach it as HookBus so
// TUI/ACP/SDK/headless/team/subagent dispatchHook is live, not gateway-only.
func (r *Registry) EventBus() *EventBus {
	if r == nil {
		return nil
	}
	return r.bus
}

// Hooks returns the live hooks.json runtime. nil means the hooks capability
// is unmounted or inert; callers must re-read this pointer on each dispatch.
func (r *Registry) Hooks() *Hooks {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hooks
}

func (r *Registry) setHooks(h *Hooks) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.hooks = h
	r.mu.Unlock()
}

// IsMounted reports whether name currently has a live disposer (builtin or external).
func (r *Registry) IsMounted(name string) bool {
	if r == nil || name == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.mounted[name]
	return ok
}

// HasBuiltin reports whether name is a registered compile-time capability.
func (r *Registry) HasBuiltin(name string) bool {
	if r == nil || name == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.builtins[name]
	return ok
}

// SessionStore 返回被能力替换的会话存储（无则 nil）。
func (r *Registry) SessionStore() any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.store
}

// Unload 卸载指定插件（内置或外部），释放其 disposer。
// Only the plugin that supplied the process LLM/store pointers may clear them.
func (r *Registry) Unload(name string) {
	r.mu.Lock()
	d := r.mounted[name]
	h := r.hosts[name]
	delete(r.mounted, name)
	delete(r.hosts, name)
	if r.llmOwner == name {
		r.llm = nil
		r.llmOwner = ""
	}
	if r.storeOwner == name {
		r.store = nil
		r.storeOwner = ""
	}
	r.mu.Unlock()
	if d != nil {
		d()
	}
	if h != nil {
		_ = h.Close()
	}
}

// Reload 卸载后重新挂载指定插件（热重载；外部=重连新子进程）。
func (r *Registry) Reload(ctx context.Context, name string) {
	r.reconcileMu.Lock()
	defer r.reconcileMu.Unlock()
	r.Unload(name)

	r.mu.Lock()
	if r.disabled[name] || r.libBlocked[name] {
		r.mu.Unlock()
		return
	}
	cap, isBuiltin := r.builtins[name]
	ext, isExternal := r.external[name]
	r.mu.Unlock()

	if isBuiltin {
		if err := r.mountBuiltin(cap); err != nil {
			r.logf("plugin(%s): 重载内置失败：%v", name, err)
			r.mu.Lock()
			r.lastErr[name] = err.Error()
			r.mu.Unlock()
			return
		}
		r.mu.Lock()
		delete(r.lastErr, name)
		r.mu.Unlock()
		return
	}
	if isExternal {
		h, err := NewHost(ctx, hostConfig{
			Name:         ext.Name,
			Command:      ext.Command,
			Args:         ext.Args,
			Env:          ext.Env,
			Cwd:          ext.Cwd,
			RequiresPerm: ext.RequiresPerm,
			Reconnect:    ext.Reconnect,
			Logger:       logAdapter{r.logger},
		}, r.tools, r.cmds, r.bus)
		if err != nil {
			r.logf("plugin(%s): reload 失败：%v", name, err)
			r.mu.Lock()
			r.lastErr[name] = err.Error()
			r.mu.Unlock()
			return
		}
		r.mu.Lock()
		if r.disabled[name] {
			r.mu.Unlock()
			_ = h.Close()
			return
		}
		delete(r.lastErr, name)
		r.hosts[name] = h
		r.mounted[name] = func() { _ = h.Close() }
		r.mu.Unlock()
	}
}

// Close 卸载全部插件（内置 disposer + 外部 Host）。
func (r *Registry) Close() {
	r.mu.Lock()
	disposers := make([]Disposer, 0)
	hosts := make([]*Host, 0)
	for name := range r.mounted {
		if d := r.mounted[name]; d != nil {
			disposers = append(disposers, d)
		}
		delete(r.mounted, name)
	}
	for name, h := range r.hosts {
		hosts = append(hosts, h)
		delete(r.hosts, name)
	}
	r.llm = nil
	r.store = nil
	r.hooks = nil
	r.mu.Unlock()
	for i := len(disposers) - 1; i >= 0; i-- {
		disposers[i]()
	}
	for _, h := range hosts {
		_ = h.Close()
	}
}
