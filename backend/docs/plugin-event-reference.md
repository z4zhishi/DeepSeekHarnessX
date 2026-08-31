# core: EventBus & Event Catalog

## 定位

`core.EventBus` + `core.EventRegistry` 是 plan §Event API and discovery 的真实
实现：事件订阅 owner-scoped，危险 topic 被目录强制约束。

## API

### EventRegistry（目录）

| 方法 | 语义 |
|---|---|
| `RegisterTopic(EventDescriptor)` | 登记 topic；空 topic 忽略 |
| `ListTopics()` | 全部已登记描述符 |
| `Describe(topic)` | 单条描述 |

### EventBus（总线）

| 方法 | 语义 |
|---|---|
| `Subscribe(topic, ownerID, handler)` | owner-scoped 订阅；返回精确退订函数 |
| `Publish(ctx, Event)` | 同步投递；ctx 打断即停（不阻塞发布者于后续 handler） |
| `Unsubscribe(ownerID)` | 移除该 owner 全部订阅（插件 unload 时调用） |

### 带目录的总线（`NewEventBusWithRegistry`）

| 约束 | 行为 |
|---|---|
| topic 声明 `RequiresApproval` | Publish/Subscribe 均被阻断（返回 no-op 订阅） |
| 其余 topic | 正常投递 |
| 空 topic / 空 owner / nil handler | 返回 no-op，不注册 |

## Event

```go
type Event struct {
    Topic, Version string
    Session SessionContext   // session-scoped 事件边界
    Payload any
}
```

兼容映射：`pkg/plugin.EventBus`（host 总线）持有订阅 ID→owner 索引；
`CoreBridge.ContextOwned` 把 core 订阅落到 owner 精确退订，插件 unload/
reload 不残留旧订阅（`OffOwner` 语义在 pkg 侧由 `Registry.Unload` 触发）。

## 测试基线

- `core/core_test.go: TestPluginLifecycle` —— Unload 后句柄关闭
- `core/integration_test.go` —— 订阅投递 / Unload 后隔离
- `pkg/plugin/describe_test.go` —— Describe 排序与订阅计数
- `pkg/plugin/core_bridge_test.go` —— 退订后不再收到调用