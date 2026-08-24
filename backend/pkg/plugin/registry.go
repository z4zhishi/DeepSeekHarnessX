package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
	mu     sync.Mutex
	tools  *tools.ToolRegistry
	cmds   *tools.CommandRegistry
	bus    *EventBus
	llm    llm.LlmAdapter
	store  any
	logger interface{ Printf(string, ...any) }

	builtins  map[string]Capability
	external  map[string]External
	mounted   map[string]Disposer
	hosts     map[string]*Host
	pluginDir string            // ScanDir/Install 的插件根；.disabled 持久化于此
	disabled  map[string]bool   // 停用名；Reconcile 跳过拉起
	installed map[string][]string
	lastErr   map[string]string
}

// NewRegistry 构造空注册表，注入共享的单例句柄。
func NewRegistry(toolReg *tools.ToolRegistry, cmds *tools.CommandRegistry, bus *EventBus, logger interface{ Printf(string, ...any) }) *Registry {
	return &Registry{
		tools:    toolReg,
		cmds:     cmds,
		bus:      bus,
		logger:   logger,
		builtins:  map[string]Capability{},
		external:  map[string]External{},
		mounted:   map[string]Disposer{},
		hosts:     map[string]*Host{},
		disabled:  map[string]bool{},
		installed: map[string][]string{},
		lastErr:   map[string]string{},
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
		ext, err := LoadExternalManifest(path)
		if err != nil {
			r.logf("plugin: 解析 %s 失败：%v", path, err)
			continue
		}
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
func (r *Registry) Reconcile(ctx context.Context) {
	r.mu.Lock()
	for name, cap := range r.builtins {
		if r.disabled[name] {
			continue
		}
		if _, ok := r.mounted[name]; ok {
			continue
		}
		if err := r.mountBuiltin(cap); err != nil {
			r.lastErr[name] = err.Error()
			r.logf("plugin(%s): 内置挂载失败：%v", name, err)
			continue
		}
		delete(r.lastErr, name)
	}
	exts := make([]External, 0, len(r.external))
	for _, ext := range r.external {
		if r.disabled[ext.Name] {
			continue
		}
		exts = append(exts, ext)
	}
	r.mu.Unlock()

	for _, ext := range exts {
		r.mu.Lock()
		if r.disabled[ext.Name] {
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

// mountBuiltin 挂载一个内置能力并记录其 disposer；若实现 LlmCapability 则替换适配器。
func (r *Registry) mountBuiltin(cap Capability) error {
	ctx := &Capabilities{
		Tools:    r.tools,
		Commands: r.cmds,
		Events:   r.bus,
	}
	d, err := cap.Mount(ctx)
	if err != nil {
		return err
	}
	if d == nil {
		d = NopDisposer()
	}
	name := cap.Name()
	r.mounted[name] = d
	if lc, ok := cap.(LlmCapability); ok {
		r.llm = lc.LlmAdapter()
	}
	if sc, ok := cap.(SessionStoreCapability); ok {
		r.store = sc.SessionStore()
	}
	return nil
}

// LlmAdapter 返回被能力替换的 LLM 适配器（无则 nil）。
func (r *Registry) LlmAdapter() llm.LlmAdapter {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.llm
}

// SessionStore 返回被能力替换的会话存储（无则 nil）。
func (r *Registry) SessionStore() any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.store
}

// Unload 卸载指定插件（内置或外部），释放其 disposer。
func (r *Registry) Unload(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d, ok := r.mounted[name]; ok {
		d()
		delete(r.mounted, name)
	}
	if h, ok := r.hosts[name]; ok {
		_ = h.Close()
		delete(r.hosts, name)
	}
}

// Reload 卸载后重新挂载指定插件（热重载；外部=重连新子进程）。
func (r *Registry) Reload(ctx context.Context, name string) {
	r.Unload(name)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.disabled[name] {
		return
	}
	if cap, ok := r.builtins[name]; ok {
		if err := r.mountBuiltin(cap); err != nil {
			r.logf("plugin(%s): 重载内置失败：%v", name, err)
		}
		return
	}
	if ext, ok := r.external[name]; ok {
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
			return
		}
		r.hosts[name] = h
		r.mounted[name] = func() { _ = h.Close() }
	}
}

// Close 卸载全部插件（内置 disposer + 外部 Host）。
func (r *Registry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name := range r.mounted {
		if d := r.mounted[name]; d != nil {
			d()
		}
		delete(r.mounted, name)
	}
	for name, h := range r.hosts {
		_ = h.Close()
		delete(r.hosts, name)
	}
}
