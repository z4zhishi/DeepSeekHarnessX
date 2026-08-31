# core: Session Worker Pool / Scheduler

## 定位

`core.SessionWorkerPool` 是 plan §Session-relative sync/async model 的真实实现：
插件不管理裸 goroutine，任务按 session 隔离执行，宿主统一持有并发配额、取消
与 deadline。它满足 `core.TaskAPI` 接口（`CancelOwner` / `CancelSession` /
`Active` / `Tick`），是 `PluginContext.Tasks` 的生产实现。

## 并发模型（session actor）

```text
SessionWorkerPool
├── queues:  map[sessionID]*sessionQueue   # 每 session 一条 FIFO 队列
├── spawned: map[sessionID]bool            # 每 session 至多一个消费者 goroutine
├── limit:   chan struct{}                 # 全局并发配额（默认 8）
└── tasks:   map[taskID]*poolTask          # 未完成任务跟踪（Active/Cancel 依据）
```

- **同 session 串行**：session 的任务按入队顺序执行（Sync 语义）。
- **跨 session 并行**：一个 session 卡住不影响任何其它 session、子代理或
  事件发布（`TestWorkerPoolSessionIsolation` 断言）。
- **全局配额**：所有 session 共享 `limit` 个执行槽位，防止插件任务压垮宿主。

## API

| 方法 | 语义 | 阻塞行为 |
|---|---|---|
| `Submit(session, ownerID, deadline, fn)` | 异步入队，返回 task id | 立即返回 |
| `SubmitSync(ctx, ...)` | 入队并等待 settle | 只阻塞调用方 goroutine（当前 session 执行流），绝不阻塞 pool/其它 session |
| `CancelOwner(ownerID)` | 取消该 owner 全部任务 | 未启动：从队列移除并 settle；进行中：通过 ctx 通知 |
| `CancelSession(session)` | 取消该 session 全部任务 | 同上 |
| `Active()` | 未完成任务数（已入队 + 进行中） | 立即返回 |
| `Close()` | 拒绝新任务、ctx 取消进行中任务、唤醒空闲消费者退出 | 幂等 |

## 任务生命周期

```text
Submit → queued → started(出队+注册取消柄) → running → settled(done 关闭)
              └→ cancelled ──────────────→ settled（跳过执行，等待方得到 ErrTaskPending）
deadline 到期 → ctx 取消 → run 函数自行返回 → settled
pool Close  → ctx 取消 + 消费者退出 → queued 任务不再执行，等待方得 ErrPoolClosed
```

- `done` 永远会被关闭：执行完成、panic（隔离 recover）、被取消、pool 关闭
  四条路径都会 settle，`SubmitSync` 等待方不悬挂。
- panic 隔离：插件任务 panic 不拖垮消费者 goroutine。

## 可调用时机（Event Reference 风格）

| 调用点 | 允许的 API | 说明 |
|---|---|---|
| 插件运行期（任意时刻） | Submit / SubmitSync / Active | 异步结果经订阅的 session 事件回传 |
| 插件 disable/unload 前 | CancelOwner(自身) | 释放本插件全部 in-flight 任务 |
| 会话 teardown | CancelSession | 逾期任务策略：默认取消并 settle |
| 宿主关闭 | Close | 幂等；后续 Submit 返回 ErrPoolClosed |

## 测试基线

`core/worker_test.go`（`-race -count=2` 全绿）：

- `TestWorkerPoolSessionOrdering` —— 同 session FIFO 顺序
- `TestWorkerPoolSessionIsolation` —— 卡住的 session 不阻塞其它 session
- `TestWorkerPoolDeadlineCancelsRunningTask` —— deadline ctx 取消
- `TestWorkerPoolSubmitSyncReturnsAfterRun` —— Sync 等待 settle
- `TestWorkerPoolConcurrentSessionsScale` —— 8 session 并行不串行化
- `TestWorkerPoolCloseIdempotentAndRejects` —— Close 幂等 + 拒绝新任务
- `TestWorkerPoolCloseSettlesWaitingTasks` —— Close 后 SubmitSync 返回 ErrPoolClosed 不悬挂