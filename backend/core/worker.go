package core

import (
	"context"
	"sync"
	"time"
)

// SessionWorkerPool executes plugin tasks bound to an owner and a session.
//
// 并发模型（plan §Session-relative sync/async model —— session actor）：
//   - 每个 session 一条 FIFO 顺序队列 + 一个专属消费者 goroutine：
//     同 session 的任务严格按提交顺序串行执行（Sync 语义）；
//   - 不同 session 之间完全并行：一个 session 卡住不影响其它 session；
//   - 任务携带 ownerID + SessionContext；CancelOwner / CancelSession 主动
//     取消（未启动的从队列移除，进行中的通过 ctx 通知）；
//   - 全局并发配额 limit：同时执行的任务总数受限，防止插件任务压垮宿主；
//   - 生命周期：消费者 goroutine 常驻至 Close（每 session 一个，goroutine
//     泄漏上限 = 出现过的 session 数）；Close 幂等，通过 ctx 终止进行中的
//     任务并 settle 其 done 信号。
type SessionWorkerPool struct {
	mu        sync.Mutex
	seq       int64
	queues    map[string]*sessionQueue // sessionID -> FIFO 队列
	spawned   map[string]bool          // sessionID -> 消费者已拉起（防重复派生）
	limit     chan struct{}
	tasks     map[int64]*poolTask
	closed    bool
	closeCh   chan struct{}
	closeOnce sync.Once
}

// sessionQueue 是单个 session 的 FIFO 任务队列；wake 为缓冲 1 的唤醒令牌
// （消费者空闲时驻留在此，Submit 非阻塞投递）。
type sessionQueue struct {
	mu    sync.Mutex
	tasks []*poolTask
	wake  chan struct{}
}

type poolTask struct {
	id       int64
	ownerID  string
	session  SessionContext
	deadline time.Time // 零值表示无限期
	run      func(ctx context.Context)

	startOnce sync.Once
	started   chan struct{} // 消费者出队并持有取消柄后关闭
	settleOne sync.Once
	done      chan struct{} // 任务结束（执行完成 / 被取消 / pool 关闭）时关闭

	cancelMu   sync.Mutex
	cancelled  bool
	cancelFunc []context.CancelFunc
}

// NewSessionWorkerPool 创建受限并发的 session worker pool。limit <= 0 时
// 使用默认配额 8。
func NewSessionWorkerPool(limit int) *SessionWorkerPool {
	if limit <= 0 {
		limit = 8
	}
	return &SessionWorkerPool{
		queues:  map[string]*sessionQueue{},
		spawned: map[string]bool{},
		limit:   make(chan struct{}, limit),
		tasks:   map[int64]*poolTask{},
		closeCh: make(chan struct{}),
	}
}

// Submit 把 fn 排入 session 的顺序队列并立即返回 task id（Async 语义）。
// fn 为 nil 或 session.ID 为空时返回错误；pool 已关闭时任务被拒绝。
func (p *SessionWorkerPool) Submit(session SessionContext, ownerID string, deadline time.Duration, fn func(ctx context.Context)) (int64, error) {
	if fn == nil {
		return 0, ErrNilTask
	}
	if session.ID == "" {
		return 0, ErrNilSession
	}
	t := &poolTask{
		ownerID: ownerID,
		session: session,
		run:     fn,
		started: make(chan struct{}),
		done:    make(chan struct{}),
	}
	if deadline > 0 {
		t.deadline = time.Now().Add(deadline)
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return 0, ErrPoolClosed
	}
	p.seq++
	t.id = p.seq
	p.tasks[t.id] = t
	q := p.queues[session.ID]
	if q == nil {
		q = &sessionQueue{wake: make(chan struct{}, 1)}
		p.queues[session.ID] = q
	}
	first := !p.spawned[session.ID]
	if first {
		p.spawned[session.ID] = true
	}
	p.mu.Unlock()

	q.mu.Lock()
	q.tasks = append(q.tasks, t)
	q.mu.Unlock()
	if first {
		go p.consume(q, session.ID)
	}
	// 唤醒令牌缓冲 1：消费者正忙时令牌留给它收尾后的下一轮循环。
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return t.id, nil
}

// SubmitSync 同步等待任务 settle（执行完成 / 被取消 / pool 关闭）。等待只
// 阻塞当前调用方 goroutine（即当前 session 的执行流），绝不阻塞 worker
// pool、其它 session 或事件发布——这是 plan 的 Sync 语义边界。调用方 ctx
// 打断时任务已入队并由消费者执行，返回 ErrTaskPending。
func (p *SessionWorkerPool) SubmitSync(ctx context.Context, session SessionContext, ownerID string, deadline time.Duration, fn func(ctx context.Context)) error {
	id, err := p.Submit(session, ownerID, deadline, fn)
	if err != nil {
		return err
	}
	p.mu.Lock()
	t := p.tasks[id]
	p.mu.Unlock()
	if t == nil {
		return nil // 已完成
	}
	select {
	case <-t.done:
		t.cancelMu.Lock()
		cancelled := t.cancelled
		t.cancelMu.Unlock()
		if cancelled {
			return ErrTaskPending
		}
		return nil
	case <-p.closeCh:
		return ErrPoolClosed
	case <-ctx.Done():
		return ErrTaskPending
	}
}

// consume 是单个 session 的专属消费者（session actor）：被 Submit 拉起后
// 循环"等令牌 → 清空队列"，直到 pool Close。任务执行顺序 = 入队顺序。
func (p *SessionWorkerPool) consume(q *sessionQueue, sessionID string) {
	defer func() {
		p.mu.Lock()
		// Close 之后 spawned 不再复用；Close 前消费者不会退出。
		if p.closed {
			delete(p.spawned, sessionID)
		}
		p.mu.Unlock()
	}()
	for {
		select {
		case <-p.closeCh:
			return
		case <-q.wake:
		}
		for {
			q.mu.Lock()
			if len(q.tasks) == 0 {
				q.mu.Unlock()
				break
			}
			t := q.tasks[0]
			q.tasks = q.tasks[1:]
			q.mu.Unlock()

			p.runTask(t, sessionID)
			select {
			case <-p.closeCh:
				return
			default:
			}
		}
	}
}

// runTask 执行单个任务：全局并发配额、deadline、取消通知、panic 隔离，
// 结束时 settle（关 done）并从跟踪表移除。
func (p *SessionWorkerPool) runTask(t *poolTask, sessionID string) {
	defer func() {
		p.mu.Lock()
		delete(p.tasks, t.id)
		p.mu.Unlock()
		t.settleOne.Do(func() { close(t.done) })
	}()
	// 被取消的入队任务：跳过执行，但仍需 settle（Sync 等待方不能悬挂）。
	t.cancelMu.Lock()
	if t.cancelled {
		t.cancelMu.Unlock()
		return
	}
	t.cancelMu.Unlock()

	select {
	case <-p.closeCh:
		return
	default:
	}
	// 全局并发配额：所有 session 共享 limit 个槽位。
	p.limit <- struct{}{}
	defer func() { <-p.limit }()

	ctx := context.Background()
	var cancel context.CancelFunc
	if !t.deadline.IsZero() {
		ctx, cancel = context.WithDeadline(ctx, t.deadline)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	// 注册取消柄后任务才视为 started（取消 API 从此刻起能终止它）。
	t.cancelMu.Lock()
	if t.cancelled {
		t.cancelMu.Unlock()
		cancel()
		return
	}
	t.cancelFunc = append(t.cancelFunc, cancel)
	t.cancelMu.Unlock()
	t.startOnce.Do(func() { close(t.started) })

	// 关闭时通过 ctx 通知进行中的任务。
	go func() {
		select {
		case <-p.closeCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	func() {
		defer func() { _ = recover() }() // 插件任务 panic 不拖垮消费者
		t.run(ctx)
	}()

	t.cancelMu.Lock()
	t.cancelFunc = nil
	t.cancelMu.Unlock()
	cancel() // 释放 watcher goroutine，防泄漏
}

// CancelOwner 主动取消一个 owner 的全部任务：未启动的从队列移除并 settle，
// 进行中的通过 ctx 通知。
func (p *SessionWorkerPool) CancelOwner(ownerID string) {
	p.cancelWhere(func(t *poolTask) bool { return t.ownerID == ownerID })
}

// CancelSession 主动取消一个 session 的全部任务，语义同 CancelOwner。
func (p *SessionWorkerPool) CancelSession(session SessionContext) {
	if session.ID == "" {
		return
	}
	p.cancelWhere(func(t *poolTask) bool { return t.session.ID == session.ID })
}

func (p *SessionWorkerPool) cancelWhere(match func(*poolTask) bool) {
	p.mu.Lock()
	matched := make([]*poolTask, 0)
	for id, t := range p.tasks {
		if match(t) {
			delete(p.tasks, id)
			matched = append(matched, t)
		}
	}
	bySession := map[string][]*poolTask{}
	for _, t := range matched {
		bySession[t.session.ID] = append(bySession[t.session.ID], t)
	}
	queues := make([]*sessionQueue, 0, len(bySession))
	for sid := range bySession {
		if q, ok := p.queues[sid]; ok {
			queues = append(queues, q)
		}
	}
	tasks := matched
	p.mu.Unlock()

	// 先从队列移除（防止消费者随后弹出执行），再标记取消并通知进行中任务。
	for _, q := range queues {
		q.mu.Lock()
		kept := q.tasks[:0]
		for _, t := range q.tasks {
			dropped := false
			for _, m := range tasks {
				if m == t {
					dropped = true
					break
				}
			}
			if !dropped {
				kept = append(kept, t)
			}
		}
		q.tasks = kept
		q.mu.Unlock()
	}
	for _, t := range matched {
		t.cancelMu.Lock()
		t.cancelled = true
		for _, cancel := range t.cancelFunc {
			cancel()
		}
		t.cancelFunc = nil
		t.cancelMu.Unlock()
		t.startOnce.Do(func() { close(t.started) })
		t.settleOne.Do(func() { close(t.done) })
	}
}

// Active 返回当前未完成的任务数（已入队 + 进行中）。
func (p *SessionWorkerPool) Active() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.tasks)
}

// Tick 满足 core.TaskAPI 形状；worker pool 无需周期清理（取消/关闭即时生效）。
func (p *SessionWorkerPool) Tick() {}

// Close 关闭 pool：拒绝新任务，通过 ctx 取消进行中的任务，唤醒空闲消费者
// 令其退出。幂等。所有 settle 均已由消费者/取消路径兜底，等待方不悬挂。
func (p *SessionWorkerPool) Close() {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		close(p.closeCh)
	})
}
