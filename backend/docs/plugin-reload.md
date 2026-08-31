# core: Reload（热重载 / 回滚）

## 定位

`core.ReloadManager` 是 plan §Loading priority and reload 中"快照 → 构建 →
提交 → 回滚"的最小真实实现。`pkg/plugin.Registry.Reload` 承担宿主级插件
reload（dispose 旧 mount → 重挂载 → 失败记录 lastErr）。

## ReloadManager 语义

```go
Reload(config) (any, error)
```

1. 序列化：`reloadMu` 保证多个 reload 串行；
2. 快照隔离：传入 config 与存储 config 双向 clone（调用方/工厂都改不到内部态）；
3. 构建：factory(克隆快照)；失败 → 返回 `(previous, "reload: build candidate: …" )`，
   原 owner/config 不变（`errors.Is` 可识别 factory 原始错误）；
4. 提交：写锁内一次性替换 owner+config（读者永远见一致对）；
5. teardown：锁外关闭旧 owner（`io.Closer`）；失败返回
   `(candidate, "reload: close previous owner: …")` —— 新 owner 已生效，错误
   表达 teardown 不完整。

`ReloadCurrent()`：以当前配置重建，语义同上。

## pkg/plugin.Registry.Reload（宿主侧）

1. `Unload(name)`：disposer 回收工具/命令/事件/Host；
2. builtin → `mountBuiltin` 重新挂载（owner 认领，重挂前清理旧 disposer）；
   失败 → `lastErr[name]` 记录（插件面板显示 error）；
3. external → 重新拉起 Host 子进程（ABI 校验、整代同步）。

## reloadPolicy 映射

| policy | 行为 |
|---|---|
| safe | 只重载自身（Unload+mount，不动依赖方） |
| isolated | 依赖接口无法维持时拒绝 —— 宿主通过 `Describe()` 的依赖信息在上层执行 |
| cascade | 按依赖顺序级联 —— 上层显式确认后逐个 Reload |

（cascade/isolated 的依赖图编排位于宿主装配层；core 提供"单插件可靠重载 +
失败可回滚 + 错误可观察"两个原语。）

## 测试基线

`core/reload_test.go`：快照别名、失败回滚、并发 reload、factory 隔离、
missing factory —— 全绿。