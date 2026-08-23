package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"dsh-go/pkg/acp"
	"dsh-go/pkg/agent"
	"dsh-go/pkg/embedgui"
	"dsh-go/pkg/gateway"
	"dsh-go/pkg/llm"
	"dsh-go/pkg/mcp"
	"dsh-go/pkg/plugin"
	"dsh-go/pkg/session"
	"dsh-go/pkg/storage"
	"dsh-go/pkg/subagent"
	"dsh-go/pkg/tools"
	"dsh-go/pkg/tui"
)

// version is injected at build time via `-ldflags "-X main.version=..."`.
var version = "dev"

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
	pluginDir string
	initErr   error
}

func newModeDeps(needStore, mock bool, dataDir, model, storeKind, pluginDir string) *modeDeps {
	return &modeDeps{needStore: needStore, mock: mock, dataDir: dataDir, model: model, useBbolt: storeKind == "bbolt", pluginDir: pluginDir}
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
		// whose Stream() reports MISSING_CREDENTIAL at call time.
		var adapter llm.LlmAdapter
		if m.mock {
			adapter = &llm.MockLlmAdapter{}
		} else {
			adapter = llm.NewDeepSeekAdapter(llm.DeepSeekConfig{
				APIKey: os.Getenv("DEEPSEEK_API_KEY"),
				Model:  m.model,
			})
		}
		m.adapter = adapter

		m.subagents = subagent.NewManager(m.toolReg, m.adapter)
		m.subagents.RegisterSubagentTools(m.toolReg)

		// 插件边界：把"扁平单例"收敛为"能力 + 注册表 + 事件总线"。
		// 内置能力经 Registry.Register 编译期注册；外部子进程插件经
		// Registry.ScanDir 扫描插件目录（*.json manifest）后由 Reconcile 拉起。
		// 两者共享同一 ToolRegistry/CommandRegistry/EventBus。
		m.plugins = plugin.NewRegistry(m.toolReg, m.toolReg.Commands, plugin.NewEventBus(), log.Default())
		if m.pluginDir != "" {
			if err := m.plugins.ScanDir(context.Background(), m.pluginDir); err != nil {
				m.initErr = fmt.Errorf("扫描插件目录失败: %w", err)
				return
			}
		}
		m.plugins.Reconcile(context.Background())
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

// Close releases the store if it was opened. Safe to call unconditionally.
func (m *modeDeps) Close() {
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
		profile    = flag.String("profile", "", "Profile to launch: gui | tui | web | headless | server | acp")
		model      = flag.String("model", "deepseek-chat", "Default model name")
		storeKind  = flag.String("store", "sqlite", "Storage engine: sqlite (default) | bbolt")
		dataDir    = flag.String("data-dir", ".dsh-data", "Storage data directory")
		systemText = flag.String("system", "You are DeepSeek Harness (DSH) Assistant.", "System prompt")
		showVer    = flag.Bool("version", false, "Print version and exit")
		mockLlm    = flag.Bool("mock", false, "Use the mock LLM adapter (test/demo only; default behavior reports MISSING_CREDENTIAL when DEEPSEEK_API_KEY is unset)")
		mcpConfig  = flag.String("mcp-config", "", "MCP servers JSON config file (mcp mode)")
		pluginDir  = flag.String("plugin-dir", "", "External plugin directory (JSON-RPC subprocess plugins; *.json manifests)")
	)
	flag.Parse()

	// Fast paths: --help (handled by flag package) and --version return before
	// any storage/tools/adapter work is done.
	if *showVer {
		fmt.Printf("DSH %s\n", version)
		os.Exit(0)
	}

	args := flag.Args()

	// Default to "gui" for desktop double-click execution.
	mode := "gui"
	if *profile != "" {
		mode = *profile
	} else if len(args) > 0 {
		mode = args[0]
		args = args[1:]
	}

	// ACP is automation-only over stdio: it never opens storage. Every other
	// mode persists, so it is the only mode that constructs deps without a store.
	needStore := mode != "acp" && mode != "mcp"
	deps := newModeDeps(needStore, *mockLlm, *dataDir, *model, *storeKind, *pluginDir)
	defer deps.Close()

	switch mode {
	case "gui":
		store, toolReg, adapter, err := deps.Full()
		if err != nil {
			fatal(err)
		}
		// server/gui 共用进程级 subagent 管理器：host 下行广播
		// host/subagent-started|finished，Godot 谱系树据此渲染。
		srv := gateway.NewServer(store, toolReg, adapter)
		srv.AttachSubagentManager(deps.Subagents())
		_ = embedgui.LaunchAllInOneGUIWithServer(*port, srv)

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
		srv := gateway.NewServer(store, toolReg, adapter)
		srv.AttachSubagentManager(deps.Subagents())
		addr := fmt.Sprintf("127.0.0.1:%d", *port)
		fmt.Printf("DSH Go API Gateway listening on http://%s\n", addr)
		if err := http.ListenAndServe(addr, srv.Routes()); err != nil {
			fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
			os.Exit(1)
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
		fmt.Printf("DSH MCP: %d server(s) mounted\n", len(sups))
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
		runHeadless(store, toolReg, adapter, *model, *systemText, prompt)

	default:
		store, toolReg, adapter, err := deps.Full()
		if err != nil {
			fatal(err)
		}
		// server/gui 共用进程级 subagent 管理器：host 下行广播
		// host/subagent-started|finished，Godot 谱系树据此渲染。
		srv := gateway.NewServer(store, toolReg, adapter)
		srv.AttachSubagentManager(deps.Subagents())
		_ = embedgui.LaunchAllInOneGUIWithServer(*port, srv)
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}

func runHeadless(store gateway.SessionStore, toolReg *tools.ToolRegistry, adapter llm.LlmAdapter, model, system, prompt string) {
	header := session.SessionHeader{
		ID:        fmt.Sprintf("headless-%d", time.Now().UnixNano()),
		CreatedAt: time.Now().UnixMilli(),
		Cwd:       ".",
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
			break
		}
	}
}
