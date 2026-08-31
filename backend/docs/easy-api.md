# EasyAPI（dshx-easy-api）

## 定位

EasyAPI 是**独立可选**的开发者插件/lib（plan §EasyAPI as an optional
independent plugin/lib）：

- `backend/core/easyapi` 是独立包；Core 不依赖它，没有它 Core 与系统插件
  照常运行。
- 第三方插件 manifest 通过 `libs: [{"id":"dshx-easy-api","version":"^1.0"}]`
  声明依赖后获得本 facade。
- 每个方法都是对 Core API 的薄封装——下表给出与底层 Core API 的对应关系。

## Facade ↔ Core API 映射

| EasyAPI | 底层 Core API | 生命周期 / 线程语义 | 权限 |
|---|---|---|---|
| `New(safe, unsafe, events, tasks)` | 宿主在 Plugin Load 时注入四个服务 | 插件 load 后持有句柄；unload 后由 Core 回收 | — |
| `SafeContext()` / `helpers.SafeMessage` | `core.ContextService.AppendUserMessage` | 需 `ContextID.OwnerID`；session 隔离 | safe（默认受控开放） |
| `helpers.SafeChunk` | `AppendAssistantChunk` | 同上 | safe |
| `UnsafeContext()` / `helpers.UnsafeRewrite` | `RewriteHistory` / `DeleteMessage` | 会影响已发送上下文，建议先 preview/diff；事务与回滚由宿主审计链负责 | **必须显式 grant**，否则 `errUnsafeForbidden` |
| `Events()` | `core.EventBus.Subscribe/Publish` | 订阅绑定 ownerID；unload 时 `Unsubscribe(ownerID)` 自动清理 | 事件权限节点（带目录时） |
| `Tasks()` | `core.SessionWorkerPool`（见 scheduler 文档） | 同 session 串行、跨 session 并行；deadline/cancel 全托管 | scheduler 节点 |

## 最小示例（真实可运行）

`backend/core/easyapi/plugin_test.go: TestEasyAPIToCoreBridge` 演示了从真实
Core 服务创建 EasyAPI 并取回四个适配器。示例插件骨架见
`backend/plugins/_template/`（manifest + capability 说明）。

```go
// 第三方插件内的用法
ea := easyapi.New(ctx.Safe, ctx.Unsafe, ctx.Events, ctx.Tasks)
_ = easyapi.SafeMessage(ea.SafeContext(), core.ContextID{
    SessionID: "s-1", OwnerID: "my-plugin",
}, "hello")
ea.Events().Publish(ctxBg, core.Event{
    Topic: "my-plugin/event", Session: core.SessionContext{ID: "s-1"}, Payload: data,
})
```

## 禁止事项（Non-goals 契约）

- EasyAPI **永远不是** Core 的强制依赖；删除 core/easyapi 目录后核心必须编译运行。
- EasyAPI 不绕过权限：unsafe 直接调用必然失败，grant 必须来自宿主审批流。