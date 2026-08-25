package mcp

// 产品会话挂载入口：把 MCP 服务器工具挂进常驻产品模式（gui/tui/server/acp/
// sdk/headless）共享的主 ToolRegistry。与专职 `dshx mcp` 校验模式的同步
// MountConfigFile 不同，产品路径必须零阻塞且可降级：
//   - 未配置（path 空）：零副作用，不触碰文件系统；
//   - 配置缺失/解析失败/挂载错误：记日志后静默降级为无 MCP，绝不阻断启动；
//   - 挂载在独立 goroutine 中执行，调用方不被任何子进程/网络 IO 阻塞。

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"dsh-go/pkg/tools"
)

// MountFunc 是一次配置挂载的可注入实现。生产路径传 MountConfigFile；
// 测试注入 mock 以断言"配置存在时挂载调用发生"。
type MountFunc func(ctx context.Context, path string, reg *tools.ToolRegistry, logger *log.Logger) ([]*Supervisor, error)

// ProductMount 是一次异步挂载的句柄。
type ProductMount struct {
	done      chan struct{}
	mu        sync.Mutex
	sups      []*Supervisor
	collected bool
}

// Done 返回完成信号：关闭时挂载流程已结束（成功、降级或空操作）。
func (m *ProductMount) Done() <-chan struct{} { return m.done }

// Collect 取回已挂载的 Supervisor（宿主退出时逐个 Close）。必须在 Done 之后
// 调用；降级/空操作时返回 nil。
func (m *ProductMount) Collect() []*Supervisor {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.collected {
		return nil
	}
	m.collected = true
	return m.sups
}

func (m *ProductMount) store(sups []*Supervisor) {
	m.mu.Lock()
	m.sups = sups
	m.mu.Unlock()
}

// MountAsync 异步把 path 指向的 MCP 配置挂载进 reg。
//
//   - path 为空或仅空白：立即完成，mount 不被调用（零副作用）；
//   - LoadConfigFile 失败（缺失/解析失败）：记日志降级，mount 不被调用；
//   - mount 执行错误：记日志降级（MountConfigFile 自身对部分失败有回滚语义，
//     返回 error 时已无残留注册）；initTimeout 界定整次顺序挂载的初始连接预算，
//     超时的服务器按各自 failOnStartupError 语义转后台重连或整体降级。
//
// 无论结果如何都不返回错误——产品启动路径不允许被 MCP 配置阻断。
func MountAsync(path string, reg *tools.ToolRegistry, logger *log.Logger, mount MountFunc, initTimeout time.Duration) *ProductMount {
	if logger == nil {
		logger = log.Default()
	}
	pm := &ProductMount{done: make(chan struct{})}
	go func() {
		defer close(pm.done)
		if strings.TrimSpace(path) == "" {
			return // 未配置：零副作用
		}
		if _, err := LoadConfigFile(path); err != nil {
			// 配置缺失/解析失败：静默降级为无 MCP（不得阻断无 MCP 用户启动）。
			logger.Printf("mcp: 配置不可用，已降级为无 MCP: %v", err)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), initTimeout)
		defer cancel()
		sups, err := mount(ctx, path, reg, logger)
		if err != nil {
			logger.Printf("mcp: 挂载失败，已降级为无 MCP: %v", err)
			return
		}
		pm.store(sups)
	}()
	return pm
}

// CloseSupervisors 依次关闭全部 Supervisor（忽略错误；日志可选）。
func CloseSupervisors(sups []*Supervisor, logger *log.Logger) {
	for i, s := range sups {
		if err := s.Close(); err != nil && logger != nil {
			logger.Printf("mcp: supervisor[%d] close: %v", i, err)
		}
	}
}
