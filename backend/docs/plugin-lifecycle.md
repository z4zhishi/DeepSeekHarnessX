# core: Plugin Lifecycle & Registry

## 定位

`core.Registry` 是 plan §Plugin API / lifecycle 的真实实现。它管理插件发现、
加载、句柄与回收；`pkg/plugin.Registry`（兼容层）通过 `CoreBridge` 接入同一契约。

## 生命周期

```text
Register → Load → active ─→ Unload → closed
                 └→ Load(重复) → 旧 handle 关闭后替换
Load 失败     → 已成功句柄逆序回滚关闭
Registry.Unload → 全部句柄锁外逆序关闭（幂等）
```

## API

| 方法 | 语义 | 失败行为 |
|---|---|---|
| `Register(p)` | 登记插件；nil/空 ID 返回错误 | 不修改状态 |
| `Load(ctx, svc)` | 按依赖排序加载全部；**不持锁执行插件代码**（防回调死锁） | 失败逆序回滚已成功句柄 |
| `LoadContext(ctx, svc)` | 同上，显式 ctx | 同上 |
| `Unload()` | 关闭全部句柄 | 幂等；锁外执行 Close |
| `Describe()` | 输出元数据快照 | — |
| `Has/HandleOf/FindPlugin` | 查询 | — |

## ReloadManager

候选构建失败 → 保留原 owner 与错误链（`errors.Is` 可识别原始错误）；成功提交
后**锁外**关闭旧 owner（`io.Closer`），关闭失败返回 `reload: close previous
owner: …`。回滚语义：任何失败都会让 active 状态保持不变。

## 关键不变量（测试断言）

1. 插件 Load 不持 registry 锁 —— 插件回调可以安全调用其它服务。
2. Load 中途失败 → 已加载句柄按逆序全部 Close。
3. Unload 后事件不再投递（integration test）。
4. Reload 期间读者永远看到一致的 owner/config 对（`cloneMap` 快照隔离）。

## 测试基线

`core/core_test.go`、`core/integration_test.go`、`core/reload_test.go`
（并发 reload / rollback / snapshot 别名 / factory 隔离 全绿）。