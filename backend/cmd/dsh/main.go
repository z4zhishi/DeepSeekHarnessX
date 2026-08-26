package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"dsh-go/pkg/acp"
	"dsh-go/pkg/agent"
	"dsh-go/pkg/credential"
	"dsh-go/pkg/embedgui"
	"dsh-go/pkg/feedback"
	"dsh-go/pkg/gateway"
	"dsh-go/pkg/llm"
	"dsh-go/pkg/mcp"
	"dsh-go/pkg/plugin"
	"dsh-go/pkg/session"
	"dsh-go/pkg/settings"
	"dsh-go/pkg/storage"
	"dsh-go/pkg/subagent"
	"dsh-go/pkg/tools"
	"dsh-go/pkg/tui"
	"dsh-go/pkg/workspace"
)

// version is injected at build time via `-ldflags "-X main.version=..."`.
var version = "dev"

// knownModes is the whitelist of launch modes. Anything else (e.g. a typo)
// is rejected with the list of valid values instead of silently launching
// the desktop GUI.
var knownModes = []string{"gui", "tui", "server", "web", "acp", "mcp", "sdk", "headless"}

func isKnownMode(m string) bool {
	for _, k := range knownModes {
		if m == k {
			return true
		}
	}
	return false
}

// modeDeps lazily assembles the heavyweight runtime (storage, tool registry,
// LLM adapter, subagent tools) exactly once, on first use. --help/--version
// short-circuit before any of this is built, keeping them sub-millisecond.
type modeDeps struct {
	once      sync.Once
	needStore bool
	store     gateway.SessionStore
	useBbolt  bool // true -> legacy bbolt backend; default sqlite
	mock      bool // true -> explicitly use the MockLlmAdapter (test/demo only)
	toolReg   *tools.ToolRegistry
	adapter   llm.LlmAdapter
	subagents *subagent.Manager
	plugins   *plugin.Registry
	dataDir   string
	model     string
	// pluginDir is scanned for external subprocess plugins.
	pluginDir string
	// workspaceRoots are the workspace roots seeded into the workspace manager
	// at startup (usually the configured data dir / cwd); empty uses the cwd.
	workspaceRoots []string
	// hookBus is the shared plugin event bus captured at build time so the
	// hooks runtime can dispatch through the same bus plugins use. Set in
	// build(); may be nil for tooling-only modes.
	hookBus *plugin.EventBus
	// mcpConfig is the --mcp-config path honored by every mode ("" = no MCP).
	mcpConfig string
	// mcpMount tracks the async MCP mounting kicked off by build() (nil when
	// no config was given). Collect() after Done() yields supervisors to close.
	mcpMount *mcp.ProductMount
	initErr  error
}

// mcpMountInitTimeout bounds one async MCP config mount's initial-connect
// budget. The mount runs off the startup path entirely; this only decides how
// long a slow/hung server may hold the initial generation before its supervisor
// falls back to the background reconnect loop (or the whole mount degrades).
const mcpMountInitTimeout = 10 * time.Second

// mcpCloseWait caps how long Close blocks on an in-flight async MCP mount.
const mcpCloseWait = 15 * time.Second

func newModeDeps(needStore, mock bool, dataDir, model, storeKind, pluginDir, mcpConfig string) *modeDeps {
	d := &modeDeps{needStore: needStore, mock: mock, dataDir: dataDir, model: model, useBbolt: storeKind == "bbolt", pluginDir: pluginDir, mcpConfig: mcpConfig}
	// Seed the workspace manager's default root with the process working
	// directory (a real, browsable tree; the storage data dir is a flat file
	// store, not a good picker root). Empty workspaceRoots later falls back to
	// the cwd at scan time, matching the legacy workspace.list fallback.
	if cwd, err := os.Getwd(); err == nil {
		d.workspaceRoots = []string{cwd}
	}
	return d
}

// build lazily assembles tooling and (when needStore) storage. Mode code calls
// the accessor matching what it needs; storage is only opened for modes that
// actually persist.
func (m *modeDeps) build() {
	m.once.Do(func() {
		if m.needStore {
			// Default persistence is the schema-17 SQLite engine; bbolt remains
			// selectable via --store bbolt for legacy workspaces.
			if m.useBbolt {
				store, err := storage.OpenBboltStore(m.dataDir)
				if err != nil {
					m.initErr = fmt.Errorf("failed to initialize storage: %w", err)
					return
				}
				m.store = store
			} else {
				store, err := storage.OpenSqliteStore(m.dataDir)
				if err != nil {
					m.initErr = fmt.Errorf("failed to initialize storage: %w", err)
					return
				}
				m.store = &storage.SqliteGatewayStore{SqliteStore: store}
			}
		}

		m.toolReg = tools.NewToolRegistry()

		// Upstream credential semantics: no key is not a load-time failure and
		// does not silently fall back to a mock. Only an explicit --mock opts
		// into the demo adapter; otherwise we build a keyless DeepSeekAdapter
		// whose Stream() reports MISSING_CREDENTIAL at call time, resolving the
		// key through the credential seam (default reference DEEPSEEK_API_KEY)
		// once per operation so a changed credential takes effect without a
		// restart.
		var adapter llm.LlmAdapter
		if m.mock {
			adapter = &llm.MockLlmAdapter{}
		} else {
			creds := credential.NewManager(credential.Options{})
			adapter = llm.NewDeepSeekAdapter(llm.DeepSeekConfig{
				APIKey: "",
				Model:  m.model,
				APIKeyResolver: func() (string, error) {
					return creds.ResolveValue("DEEPSEEK_API_KEY")
				},
			})
		}
		m.adapter = llm.NewRouter(adapter)

		// 子代理管理器：接入父会话同一持久化 store（子会话事件落库，不再
		// 只在内存），并继承当前模型路由（每次 spawn 时查询，热切换生效）。
		subOpts := []subagent.Option{subagent.WithModelGetter(func() string { return m.model })}
		if m.store != nil {
			subOpts = append(subOpts, subagent.WithStore(m.store))
		}
		m.subagents = subagent.NewManager(m.toolReg, m.adapter, subOpts...)
		m.subagents.RegisterSubagentTools(m.toolReg)

		// Agent Teams runtime wiring: the process-global TeamService spawns
		// teammate children as full DSH agents (shared store/registry/adapter)
		// whenever spawn_teammate is invoked. Wire lazily through build() so
		// both server and headless hosts get a live provider.
		tools.SetTeamSpawner(teamSpawner(m.store, m.toolReg, m.adapter, m.model))

		// 插件边界：把"扁平单例"收敛为"能力 + 注册表 + 事件总线"。
		// 内置能力经 Registry.Register 编译期注册；外部子进程插件经
		// Registry.ScanDir 扫描插件目录（*.json manifest）后由 Reconcile 拉起。
		// 两者共享同一 ToolRegistry/CommandRegistry/EventBus。注册表持有唯一的
		// EventBus（hookBus），hooks 运行时与插件监听共享同一总线。
		m.hookBus = plugin.NewEventBus()
		m.plugins = plugin.NewRegistry(m.toolReg, m.toolReg.Commands, m.hookBus, log.Default())
		if m.pluginDir != "" {
			if err := m.plugins.ScanDir(context.Background(), m.pluginDir); err != nil {
				m.initErr = fmt.Errorf("扫描插件目录失败: %w", err)
				return
			}
		}
		m.plugins.Reconcile(context.Background())

		// MCP 产品会话挂载：存在 --mcp-config 时把服务器工具异步挂进主工具
		// 注册表（gui/tui/server/acp/sdk/headless 全模式共享本注册表）。配置
		// 缺失/解析失败/挂载错误一律静默降级为无 MCP，不阻断启动；挂载在
		// 后台 goroutine 完成，主启动路径零等待。专职 `dshx mcp` 校验模式
		// 不走这里（mcpConfig 置空），由其分支同步 MountConfigFile。
		if m.mcpConfig != "" {
			m.mcpMount = mcp.MountAsync(m.mcpConfig, m.toolReg, log.Default(), mcp.MountConfigFile, mcpMountInitTimeout)
		}
	})
}

// Tooling returns the tool registry and adapter without touching storage.
func (m *modeDeps) Tooling() (*tools.ToolRegistry, llm.LlmAdapter, error) {
	m.build()
	return m.toolReg, m.adapter, m.initErr
}

// Full returns the store, tool registry, and adapter, opening storage lazily.
func (m *modeDeps) Full() (gateway.SessionStore, *tools.ToolRegistry, llm.LlmAdapter, error) {
	m.build()
	return m.store, m.toolReg, m.adapter, m.initErr
}

// Subagents returns the process-level subagent manager (built with tooling).
func (m *modeDeps) Subagents() *subagent.Manager {
	m.build()
	return m.subagents
}

// Close releases the async MCP mount, the plugin registry, and the store if it
// was opened. Safe to call unconditionally.
func (m *modeDeps) Close() {
	if m.mcpMount != nil {
		// 挂载仍在后台进行时按上限等待，避免退出被慢 MCP 服务器无限拖延；
		// 超时放弃的实例随进程退出回收，但插件与存储的收尾不受影响。
		select {
		case <-m.mcpMount.Done():
			mcp.CloseSupervisors(m.mcpMount.Collect(), log.Default())
		case <-time.After(mcpCloseWait):
			log.Printf("mcp: 挂载未在 %v 内完成，跳过其收尾（随进程退出回收）", mcpCloseWait)
		}
	}
	if m.plugins != nil {
		m.plugins.Close()
	}
	if closer, ok := m.store.(interface{ Close() error }); ok {
		_ = closer.Close()
		m.store = nil
	}
}

func main() {
	var (
		port       = flag.Int("port", 3080, "HTTP/WS API server port")
		host       = flag.String("host", "127.0.0.1", "HTTP/WS API server bind address (server/gui modes)")
		profile    = flag.String("profile", "", "Profile to launch: gui | tui | web | headless | server | acp")
		model      = flag.String("model", "deepseek-v4-flash", "Default model name")
		storeKind  = flag.String("store", "sqlite", "Storage engine: sqlite (default) | bbolt")
		dataDir    = flag.String("data-dir", ".dsh-data", "Storage data directory")
		systemText = flag.String("system", "You are DSHX Assistant.", "System prompt")
		showVer    = flag.Bool("version", false, "Print version and exit")
		mockLlm    = flag.Bool("mock", false, "Use the mock LLM adapter (test/demo only; default behavior reports MISSING_CREDENTIAL when DEEPSEEK_API_KEY is unset)")
		mcpConfig  = flag.String("mcp-config", "", "MCP servers JSON config file; honored by every mode (missing/invalid file degrades to no MCP; dedicated validation mode: 'dshx mcp')")
		pluginDir  = flag.String("plugin-dir", "", "External plugin directory (JSON-RPC subprocess plugins; *.json manifests)")
	)
	flag.Parse()

	// Fast paths: --help (handled by flag package) and --version return before
	// any storage/tools/adapter work is done.
	if *showVer {
		fmt.Printf("DSHX %s\n", version)
		os.Exit(0)
	}

	args := flag.Args()

	// Default mode is adaptive: prefer the desktop GUI when a display session
	// is available, otherwise fall back to the TUI (original-design.md:13
	// "dsh（自适应/TUI）"). On Linux without DISPLAY/WAYLAND there is no GUI to
	// launch, so TUI keeps the no-arg invocation usable on headless servers.
	mode := "gui"
	if *profile != "" {
		mode = *profile
	} else if len(args) > 0 {
		mode = args[0]
		args = args[1:]
	}
	if mode == "gui" && !guiAvailable() {
		if runtime.GOOS == "windows" {
			// Windows always reports a desktop session in practice; if it does
			// not (service context), still fall back rather than hang.
			fmt.Fprintln(os.Stderr, "[DSHX] No desktop display session detected; falling back to TUI.")
			mode = "tui"
		} else {
			fmt.Fprintln(os.Stderr, "[DSHX] No display environment detected (DISPLAY/WAYLAND unset); falling back to TUI. Use 'dshx gui' to force GUI mode.")
			mode = "tui"
		}
	}

	// Unknown subcommands must fail loudly: silently launching a desktop
	// window for a typo makes scripted use and debugging impossible.
	if !isKnownMode(mode) {
		fmt.Fprintf(os.Stderr, "Error: unknown mode %q. Valid modes: %s\n", mode, strings.Join(knownModes, ", "))
		os.Exit(2)
	}

	// ACP is automation-only over stdio: it never opens storage. Every other
	// mode persists, so it is the only mode that constructs deps without a store.
	needStore := mode != "acp" && mode != "mcp"
	if *pluginDir == "" {
		*pluginDir = filepath.Join(*dataDir, "plugins")
	}
	// The dedicated `dshx mcp` validation mode mounts its config synchronously
	// in its own branch; blank it here so deps.build() does not mount a second
	// time (duplicate serverName would be a namespace-conflict error).
	depsMcpConfig := *mcpConfig
	if mode == "mcp" {
		depsMcpConfig = ""
	}
	deps := newModeDeps(needStore, *mockLlm, *dataDir, *model, *storeKind, *pluginDir, depsMcpConfig)
	defer deps.Close()

	switch mode {
	case "gui":
		store, toolReg, adapter, err := deps.Full()
		if err != nil {
			fatal(err)
		}
		// server/gui 共用进程级 subagent 管理器：host 下行广播
		// host/subagent-started|finished，Godot 谱系树据此渲染。
		srv := gateway.NewServerWithVersion(store, toolReg, adapter, version)
		wireSettings(srv)
		wireServerExt(srv, deps)
		srv.AttachSubagentManager(deps.Subagents())
		if err := embedgui.LaunchAllInOneGUIWithServer(*host, *port, srv); err != nil {
			fmt.Fprintf(os.Stderr, "Error: GUI launch failed: %v\n", err)
			fmt.Fprintln(os.Stderr, "[DSHX] Try 'dshx tui' for the terminal interface.")
			deps.Close()
			os.Exit(1)
		}

	case "tui":
		store, toolReg, adapter, err := deps.Full()
		if err != nil {
			fatal(err)
		}
		tui.RunTUI(store, toolReg, adapter, *model)

	case "server", "web":
		store, toolReg, adapter, err := deps.Full()
		if err != nil {
			fatal(err)
		}
		srv := gateway.NewServerWithVersion(store, toolReg, adapter, version)
		wireSettings(srv)
		wireServerExt(srv, deps)
		srv.AttachSubagentManager(deps.Subagents())
		addr := fmt.Sprintf("%s:%d", *host, *port)
		httpSrv := &http.Server{Addr: addr, Handler: srv.Routes()}
		fmt.Printf("DSHX Go API Gateway (%s) listening on http://%s\n", version, addr)

		// 优雅停机：SIGINT/SIGTERM 触发 srv.Shutdown，保证 defer deps.Close()
		// （插件子进程关闭/store Close）在退出前执行，而非被信号直接杀死。
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		serveErr := make(chan error, 1)
		go func() { serveErr <- httpSrv.ListenAndServe() }()
		select {
		case err := <-serveErr:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
				os.Exit(1)
			}
		case <-ctx.Done():
			fmt.Println("\n[DSHX] Shutdown signal received; draining connections...")
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = httpSrv.Shutdown(shutdownCtx)
		}

	case "acp":
		toolReg, adapter, err := deps.Tooling()
		if err != nil {
			fatal(err)
		}
		acpSrv := acp.NewServer(toolReg, adapter)
		if err := acpSrv.Serve(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "ACP server error: %v\n", err)
			os.Exit(1)
		}

	case "mcp":
		if *mcpConfig == "" {
			fmt.Fprintln(os.Stderr, "Error: mcp mode requires --mcp-config (JSON file)")
			os.Exit(1)
		}
		toolReg, _, err := deps.Tooling()
		if err != nil {
			fatal(err)
		}
		sups, err := mcp.MountConfigFile(context.Background(), *mcpConfig, toolReg, nil)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("DSHX MCP: %d server(s) mounted\n", len(sups))
		// 阻塞直至 Ctrl+C；退出前优雅关闭全部连接并注销工具
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt)
		<-sigCh
		for _, s := range sups {
			_ = s.Close()
		}
	case "sdk":
		// SDK JSON-RPC runtime over stdio: like ACP it never opens storage
		// unless the store is available; sessions are lazily created and
		// persisted through the configured backend.
		store, toolReg, adapter, err := deps.Full()
		if err != nil {
			fatal(err)
		}
		sdkSrv := gateway.NewSDKServer(store, toolReg, adapter, *model)
		// 复用进程级 subagent 管理器（其工具已注册在 toolReg 上），
		// 让 SDK 客户端收到 subagent.started / subagent.finished 通知。
		sdkSrv.AttachSubagentManager(deps.Subagents())
		if err := sdkSrv.Serve(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "SDK server error: %v\n", err)
			os.Exit(1)
		}

	case "headless":
		prompt := strings.Join(args, " ")
		if prompt == "" {
			fmt.Fprintln(os.Stderr, "Error: headless mode requires a prompt string.")
			os.Exit(1)
		}
		store, toolReg, adapter, err := deps.Full()
		if err != nil {
			fatal(err)
		}
		if code := runHeadless(store, toolReg, adapter, *model, *systemText, prompt); code != 0 {
			deps.Close()
			os.Exit(code)
		}

	default:
		// The whitelist check above makes this branch unreachable for unknown
		// modes; it exists only to keep the compiler happy.
		fatal(fmt.Errorf("unhandled mode %q", mode))
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}

// guiAvailable reports whether the current process plausibly has a display
// session for the desktop GUI. Windows is assumed to have one (the desktop
// session exists for any interactive logon); on Linux the DISPLAY/WAYLAND
// environment variables are the standard signal, with DSH_GUI=1 as an escape
// hatch to force GUI mode in unusual setups.
func guiAvailable() bool {
	if v := os.Getenv("DSH_GUI"); v != "" && v != "0" {
		return true
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}

// teamSpawner wires the Agent Teams runtime's teammate provider to a real DSH
// agent. Each spawned teammate is a full agent on the shared store/registry/
// adapter. The returned TeamChild satisfies tools.TeamChild via the agent.
// store may be nil (headless spawn of an in-memory child is unsupported, so
// the host must own a store for Team to be live); callers only invoke this
// after deps.Full() has opened the store.
func teamSpawner(store gateway.SessionStore, toolReg *tools.ToolRegistry, adapter llm.LlmAdapter, model string) tools.TeamSpawner {
	return func(name, description, prompt, context string) (tools.TeamChild, error) {
		childID := fmt.Sprintf("team-%d/%s", time.Now().UnixNano(), name)
		header := session.SessionHeader{
			ID:              childID,
			CreatedAt:       time.Now().UnixMilli(),
			Cwd:             ".",
			Origin:          "team",
			DelegationDepth: 1,
		}
		ringBuf := storage.NewRingBuffer(256)
		child := agent.NewAgent(header, ringBuf, nil, store, toolReg, adapter,
			fmt.Sprintf("You are teammate '%s'. %s", name, description), model)
		child.AutoTitle = true
		child.Start()
		return &teamChildAgent{ag: child}, nil
	}
}

// teamChildAgent adapts an *agent.Agent to the tools.TeamChild handle the
// Team runtime drives (PostUserMessage/Interrupt/Status/Model/Stop).
type teamChildAgent struct{ ag *agent.Agent }

func (t *teamChildAgent) ID() string { return t.ag.Header.ID }
func (t *teamChildAgent) PostUserMessage(msg session.UserMessagePayload) {
	t.ag.PostUserMessage(msg)
}
func (t *teamChildAgent) Interrupt() {
	t.ag.PostNextStep(session.ContentBlock{Type: "text", Text: "interrupt"})
}
func (t *teamChildAgent) Status() string {
	if t.ag.IsRunning() {
		return "running"
	}
	return "idle"
}
func (t *teamChildAgent) Model() string { return t.ag.ModelName }
func (t *teamChildAgent) Stop()         { t.ag.Stop() }

// generalSettingsSchema is the JSON Schema for the editable "general"
// namespace (language + composerEnter), the minimal pre-provisioned namespace
// proving the settings.describe / settings.mutate round-trip.
var generalSettingsSchema = []byte(`{
	"type": "object",
	"properties": {
		"language": { "type": "string", "default": "auto" },
		"composerEnter": { "type": "boolean", "default": true },
		"model": { "type": "string", "default": "deepseek-v4-flash" },
		"autoReviewModel": { "type": "string", "default": "" },
		"contextWindow": { "type": "string", "default": "" }
	}
}`)

// providerSchema is the JSON Schema for the "provider" namespace: a set of
// switchable provider profiles (each with protocol/baseUrl/model/apiKeyRef) and
// the id of the active one. Profiles are stored as an object map keyed by id so
// settings.mutate path ops can add/update a single profile without clobbering
// the rest.
var providerSchema = []byte(`{
	"type": "object",
	"properties": {
		"active": { "type": "string", "default": "" },
		"profiles": { "type": "object" }
	}
}`)

// wireServerExt attaches the Phase-4 backend seams (hooks runtime, real
// workspace backend, message-feedback sidecar) to a gateway server. It is
// called alongside wireSettings for every server-hosted profile.
func wireServerExt(srv *gateway.Server, deps *modeDeps) {
	// CC-style hooks runtime: load the first available hooks.json and attach the
	// shared registry's event bus + parsed hooks so every created session drives
	// its four dispatch intercept points. Loaded once at startup; best-effort.
	if h := deps.loadHooks(); h != nil {
		srv.Hooks = h
	}
	srv.HookBus = deps.hookBus

	// Real workspace backend: one manager rooted at the process working
	// directory, seeded with the configured roots (if any). Drives
	// workspace.list (real directory tree) + workspace.create.
	wm := workspace.NewManager("")
	for _, root := range deps.workspaceRoots {
		if root == "" {
			continue
		}
		if _, err := wm.Add(root); err != nil && !errors.Is(err, workspace.ErrAlreadyExists) {
			log.Printf("workspace: add root %s failed: %v", root, err)
		}
	}
	srv.WorkspaceMgr = wm

	// Message-feedback sidecar: an in-memory per-session rating+note store.
	if fb, err := feedback.NewStore(feedback.Config{MaxNoteBytes: 2048}); err == nil {
		srv.Feedback = fb
	} else {
		log.Printf("feedback: store init failed: %v", err)
	}

	// Active model: seeds llm.models' selected id and the token meter's context
	// window so session.context reports contextPressure against the right model.
	srv.Model = deps.model

	srv.PluginDir = deps.pluginDir
	srv.Plugins = deps.plugins
	srv.HydrateRuntime()
}

// loadHooks loads a CC-style hooks.json from $DSH_HOME/hooks.json, else
// <cwd>/.claude/hooks.json, when present. Variable substitution is applied for
// ${CLAUDE_PROJECT_DIR} (cwd) and ${CLAUDE_PLUGIN_ROOT}. Returns nil when no
// file exists (hooks runtime inert) or the file fails to parse.
func (m *modeDeps) loadHooks() *plugin.Hooks {
	home := os.Getenv("DSH_HOME")
	candidates := []string{}
	if home != "" {
		candidates = append(candidates, filepath.Join(home, "hooks.json"))
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, ".claude", "hooks.json"))
	}
	projectDir, _ := os.Getwd()
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		h, _, perr := plugin.ParseHooks(data, plugin.ParseOptions{
			ProjectDir: projectDir,
			PluginRoot: filepath.Dir(path),
		})
		if perr != nil {
			log.Printf("hooks: parse %s failed: %v", path, perr)
			continue
		}
		log.Printf("hooks: loaded %s", path)
		return h
	}
	return nil
}

// wireSettings attaches the settings and credential backends to a gateway
// server: it builds a $DSH_HOME/settings.yaml-backed Manager with the general
// namespace pre-registered, and a credential Manager whose set/unset fan to
// the server's host downlink as credential/updated events.
func wireSettings(srv *gateway.Server) {
	home := os.Getenv("DSH_HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
	}
	if home == "" {
		return
	}

	// Settings backend: single-file YAML under $DSH_HOME.
	settingsPath := filepath.Join(home, "settings.yaml")
	settingsMgr := settings.NewManager(settingsPath, func(ns string, rev int, next, prev any) {
		srv.Hub.BroadcastHostEvent("settings/updated", map[string]any{
			"ns":       ns,
			"revision": rev,
			"next":     next,
			"prev":     prev,
		})
	})
	if err := settingsMgr.Register("general", generalSettingsSchema, settings.Options{Writable: true}); err != nil {
		log.Printf("settings: register general failed: %v", err)
		return
	}
	if err := settingsMgr.Register("provider", providerSchema, settings.Options{Writable: true}); err != nil {
		log.Printf("settings: register provider failed: %v", err)
		return
	}
	store := settings.NewFileStore(settingsPath)
	if err := store.Load(settingsMgr); err != nil {
		log.Printf("settings: load failed: %v", err)
	}
	// Persist every committed write to disk so settings survive a restart.
	settingsMgr.SetPersist(func() error { return store.Save(settingsMgr) })
	srv.Settings = settingsMgr

	// Credential backend: process env > $DSH_HOME/.credentials.yaml > cwd/.env
	// > $DSH_HOME/.env. set/unset broadcast credential/updated.
	creds := credential.NewManager(credential.Options{
		DSHHome: home,
		OnChanged: func(ref string) {
			srv.Hub.BroadcastHostEvent("credential/updated", map[string]any{"ref": ref})
		},
	})
	srv.Credentials = creds
}

func runHeadless(store gateway.SessionStore, toolReg *tools.ToolRegistry, adapter llm.LlmAdapter, model, system, prompt string) int {
	header := session.SessionHeader{
		ID:        fmt.Sprintf("headless-%d", time.Now().UnixNano()),
		CreatedAt: time.Now().UnixMilli(),
		Cwd:       ".",
		Origin:    "headless",
	}

	ringBuf := storage.NewRingBuffer(256)
	ag := agent.NewAgent(header, ringBuf, nil, store, toolReg, adapter, system, model)
	// Headless 无交互用户：RequiresPerm 工具默认拒绝；设置
	// DSH_HEADLESS_ALLOW=all 可放行（确定性批处理策略，不弹窗）。
	ag.RequestUser = func(prompt string, options []string) (tools.ApprovalDecision, error) {
		if os.Getenv("DSH_HEADLESS_ALLOW") == "all" {
			return tools.ApprovalAllowOnce, nil
		}
		return tools.ApprovalDeny, nil
	}
	eventsChan := ag.Subscribe()
	ag.Start()
	defer ag.Stop()

	ag.PostUserMessage(session.UserMessagePayload{
		ID:   "headless-msg-1",
		Role: "user",
		Content: []session.ContentBlock{
			{Type: "text", Text: prompt},
		},
		Source: session.MessageSource{Kind: "user"},
	})

	exitCode := 0
	for env := range eventsChan {
		if env.Type == session.EventAssistantMessage {
			var msg session.AssistantMessagePayload
			_ = json.Unmarshal(env.Data, &msg)
			for _, b := range msg.Message.Content {
				if b.Type == "text" {
					fmt.Print(b.Text)
				}
			}
		}
		if env.Type == session.EventTurnEnd {
			fmt.Println()
			// 脚本化场景必须能以退出码判成败：turn/end reason.Kind==error
			// 时向 stderr 输出错误原因并以非零码结束（original-design.md:138
			// headless 验收前提），不再静默吞错返回 0。
			var end session.TurnEndPayload
			if err := json.Unmarshal(env.Data, &end); err == nil && end.Reason.Kind == "error" {
				msg := end.Reason.Message
				if msg == "" {
					msg = "turn failed (no detail reported)"
				}
				fmt.Fprintf(os.Stderr, "[DSHX] headless error: %s\n", msg)
				exitCode = 1
			}
			break
		}
	}
	return exitCode
}
