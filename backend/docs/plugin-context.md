# core: Plugin Context（Safe / Unsafe）

## 定位

`core.Context` 是插件拿到的宿主服务面（plan §API contracts）。本文说明
SafeContext / UnsafeContext 的权限边界、字段语义与可调用时机。

## ContextID

```go
type ContextID struct {
    SessionID, TurnID, StepID, CallID string
    OwnerID  string // 发起操作的插件 owner（必填）
    PluginID string // 声明身份；与 OwnerID 不一致时 unsafe 直接拒绝
}
```

## SafeContext（默认受控开放）

`core.ContextService` 的真实门禁：session、owner 必须非空；unsafe 额外要求
显式 grant。

| 方法 | 校验 | 说明 |
|---|---|---|
| `AppendUserMessage` | text 非空 + owner | 追加用户消息 |
| `AppendAssistantChunk` | chunk 非空 | 流式追加 |
| `UpdateToolOutput` | tool 名非空 | 修改插件自有工具输出 |
| `AddAttachment` | name+path 非空 | 附加文件 |

## UnsafeContext（必须显式授权）

| 方法 | 权限节点 | 失败错误 |
|---|---|---|
| `RewriteHistory` | `context.unsafe.rewriteHistory` | `errUnsafeForbidden` |
| `DeleteMessage` | `context.unsafe.deleteMessage` | `errUnsafeForbidden` |

`allowed()` 三重门禁：`SessionID != ""`、`OwnerID != ""`、
`PluginID == "" || PluginID == OwnerID`（防跨插件代持），最后过
`PermissionService.Resolve(session, node)`。

## 授权流程（UI 契约）

1. 插件 manifest 声明 unsafe 权限节点；
2. 用户在插件面板看到危险提示（会修改已发送上下文，影响后续模型行为）；
3. 宿主调用 `PermissionService.Grant(Target{Kind:"session", ID}, Grant{
   Node, Duration/ExpiresAt})`；
4. 过期后 `Resolve` 拒绝（惰性判断），`Tick()` 物理清理；
5. `ExpireGrant` 可立即吊销单条 grant。

## 测试基线

- `core/core_test.go: TestContextSafetyGate` —— 无 grant 拒绝、grant 后放行
- `core/core_test.go: TestRegisterValidation` —— nil 插件/空 ID 拒绝
- `core/integration_test.go: TestBuiltinPluginEventAndLifecycleIntegration`
  —— 订阅经 owner 绑定，Unload 后不再投递