package plugin

import (
	"errors"
	"log"
	"os"
	"path/filepath"
)

// errHooksUnavailable 是宿主事件总线缺失时的错误。
var errHooksUnavailable = errors.New("plugin(hooks): event bus unavailable")

// HooksMountFunc 由宿主注入的 hooks.json 加载器（main.loadHooks 的 seam）。
// 返回 nil 表示没有可用 hooks（inert）。
type HooksMountFunc func() *Hooks

// hooksCapability 把 CC 兼容 hooks 运行时收敛为插件能力（Phase 2 迁移
// 第 3 项）。Mount 把解析后的 *Hooks 写入 Registry（cap.SetHooks），成为
// 进程内唯一活指针；Disposer 写回 nil，使 SetEnabled("hooks", false) /
// Unload 立刻让 dispatchHook 变成 no-op。
//
// 必须是指针类型：值接收者会让 c.hooks = c.mount() 写在副本上被丢弃。
// 本地 c.hooks 仅作能力侧观测；源真相是 Registry.Hooks()。
type hooksCapability struct {
	mount HooksMountFunc
	hooks *Hooks
}

// NewHooksCapability 构造 hooks 能力（无 hooks.json 时是零副作用空能力）。
func NewHooksCapability(mount HooksMountFunc) Capability {
	return &hooksCapability{mount: mount}
}

func (c *hooksCapability) Name() string { return "hooks" }

func (c *hooksCapability) Mount(cap *Capabilities) (Disposer, error) {
	if cap == nil || cap.Events == nil {
		return nil, errHooksUnavailable
	}
	if c.mount == nil {
		return func() {}, nil
	}
	h := c.mount()
	c.hooks = h
	if cap.SetHooks != nil {
		cap.SetHooks(h) // even if h is nil: inert live pointer
	}
	return func() {
		c.hooks = nil
		if cap.SetHooks != nil {
			cap.SetHooks(nil)
		}
	}, nil
}

// ErrHooksUnavailable 是宿主事件总线缺失时的错误。
var ErrHooksUnavailable = errHooksUnavailable

// LoadHooksDefault 是 main.loadHooks 的默认实现（默认搜索路径）。
func LoadHooksDefault(logger *log.Logger) *Hooks {
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
		h, _, perr := ParseHooks(data, ParseOptions{
			ProjectDir: projectDir,
			PluginRoot: filepath.Dir(path),
		})
		if perr != nil {
			if logger != nil {
				logger.Printf("hooks: parse %s failed: %v", path, perr)
			}
			continue
		}
		if logger != nil {
			logger.Printf("hooks: loaded %s", path)
		}
		return h
	}
	return nil
}

var _ Capability = (*hooksCapability)(nil)
