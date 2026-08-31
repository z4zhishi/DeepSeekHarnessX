package plugin

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"dsh-go/pkg/mcp"
)

// contextWithTimeout 包装 context.WithTimeout，让本文件保持单一 context 导入。
func contextWithTimeout(d time.Duration) (context.Context, func()) {
	return context.WithTimeout(context.Background(), d)
}

// mcpCapability 把 MCP 服务器连接收敛为插件能力（Phase 2 迁移第 4 项）。
//
// 产品路径经 Registry Mount/Unload 管理工具认领与回收。mcp.MountAsync 是
// 非产品辅助（测试/遗留异步挂载），不得作为生产会话挂载入口。
//
// 与旧路径（MountAsync 静默降级）不同，挂在插件生命周期下后失败路径是可
// 观察的：配置缺失/挂载失败记录到 lastErr，插件面板显示 error 状态，不再
// 让"挂了但看起来没挂"混过去。挂载的所有工具经 owner 认领，Disposer 关闭
// 全部 Supervisor 并按 owner 注销工具——与其它 capability 同一套回收语义。

var errMcpUnavailable = errors.New("plugin(mcp): tool registry unavailable")

type mcpCapability struct {
	path        string
	mount       mcp.MountFunc
	initTimeout time.Duration
	logger      *log.Logger
}

// NewMcpCapability 构造 MCP 连接能力。path 为空时 Mount 返回空 disposer
// （零副作用）；宿主的 Close 不再需要单独跟踪 mcpMount。
func NewMcpCapability(path string, mount mcp.MountFunc, initTimeout time.Duration, logger *log.Logger) Capability {
	if mount == nil {
		mount = mcp.MountConfigFile
	}
	return mcpCapability{path: path, mount: mount, initTimeout: initTimeout, logger: logger}
}

func (c mcpCapability) Name() string { return "mcp" }

func (c mcpCapability) Mount(cap *Capabilities) (Disposer, error) {
	if cap == nil || cap.Tools == nil {
		return nil, errMcpUnavailable
	}
	if strings.TrimSpace(c.path) == "" {
		return func() {}, nil // 未配置：零副作用（保持旧的空路径语义）
	}
	logger := c.logger
	if logger == nil {
		logger = log.Default()
	}
	mount := c.mount
	if mount == nil {
		mount = mcp.MountConfigFile
	}
	ctx, cancel := contextWithTimeout(c.initTimeout)
	defer cancel()
	before := cap.Tools.Names()
	sups, err := mount(ctx, c.path, cap.Tools, logger)
	if err != nil {
		// 回滚 mount 内部已完成，这里把可观察状态交给插件生命周期。
		return nil, err
	}
	after := cap.Tools.Names()
	beforeSet := make(map[string]bool, len(before))
	for _, name := range before {
		beforeSet[name] = true
	}
	owned := make([]string, 0, len(after))
	for _, name := range after {
		if !beforeSet[name] {
			owned = append(owned, name)
		}
	}
	cap.Tools.ClaimOwner(c.Name(), owned...)
	return func() {
		for i, s := range sups {
			if cerr := s.Close(); cerr != nil && logger != nil {
				logger.Printf("mcp: supervisor[%d] close: %v", i, cerr)
			}
		}
		for _, name := range owned {
			cap.Tools.UnregisterOwned(c.Name(), name)
		}
	}, nil
}

var _ Capability = mcpCapability{}
