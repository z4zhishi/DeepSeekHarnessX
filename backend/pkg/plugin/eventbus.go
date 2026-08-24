package plugin

import (
	"context"
	"reflect"
	"sync"
)

// EventBus 是轻量类型化事件总线（目标架构 §eventbus.go），镜像 Cordis 的
// ctx.on / ctx.emit。它把"域事件"（internal/status、fs/observed 等）从
// 硬编码单例调用解耦为 topic 驱动的发布/订阅，让插件既能订阅宿主域事件，
// 也能发布自己的事件给其它插件订阅。
//
// 线程安全：可并发 Emit 与 On。事件按 handler 注册顺序同步派发。
type EventBus struct {
	mu       sync.RWMutex
	handlers map[string][]EventHandler
}

// Event 是总线上的一个事件载体。
type Event struct {
	// Topic 是事件主题，如 "fs/observed"、"<pluginName>.<custom>"。
	Topic string
	// Payload 是类型化的负载；订阅者用类型断言取回具体类型。
	Payload any
}

// EventHandler 处理一个事件。handler 内的 panic 被捕获并忽略（保持总线
// 健壮，单个坏 handler 不影响其它订阅者）。
type EventHandler func(ev Event)

// NewEventBus 构造空总线。
func NewEventBus() *EventBus {
	return &EventBus{handlers: map[string][]EventHandler{}}
}

// On 订阅一个主题；返回一个取消函数（Cordis ctx.on 的 off 语义）。
// 主题支持精确匹配。同一 handler 重复订阅会触发多次（上游语义）。
func (b *EventBus) On(topic string, h EventHandler) func() {
	if topic == "" || h == nil {
		return func() {}
	}
	b.mu.Lock()
	b.handlers[topic] = append(b.handlers[topic], h)
	b.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() { b.off(topic, h) })
	}
}

func (b *EventBus) off(topic string, h EventHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	hs := b.handlers[topic]
	for i := range hs {
		if hs[i] != nil && sameHandler(hs[i], h) {
			hs = append(hs[:i], hs[i+1:]...)
			if len(hs) == 0 {
				delete(b.handlers, topic)
			} else {
				b.handlers[topic] = hs
			}
			return
		}
	}
}

// Emit 发布一个事件；对所有匹配 handler 同步派发。阻断由 handler 自身控制
// （总线不引入异步缓冲，保证订阅者按发布顺序观测事件）。
func (b *EventBus) Emit(topic string, payload any) {
	if b == nil {
		return
	}
	b.mu.RLock()
	hs := b.handlers[topic]
	if len(hs) == 0 {
		b.mu.RUnlock()
		return
	}
	// 复制切片：handler 可能在回调里取消/新增订阅，避免迭代期写 map。
	cp := make([]EventHandler, len(hs))
	copy(cp, hs)
	b.mu.RUnlock()
	ev := Event{Topic: topic, Payload: payload}
	for _, h := range cp {
		if h == nil {
			continue
		}
		func() {
			defer func() { _ = recover() }()
			h(ev)
		}()
	}
}

// Has 报告是否有订阅者；测试/诊断用。
func (b *EventBus) Has(topic string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.handlers[topic]) > 0
}

// EventHookInvoked / EventHookResult 是钩子运行时真实发布的事件主题（常量已预埋在
// session/events.go:32-33；plugin 不反向依赖 session，故用同值字面量，保证事件类型
// 与既有持久化/回放路径完全一致）。
const (
	EventHookInvoked = "hook/invoked"
	EventHookResult  = "hook/result"
)

// DispatchHook 是 hooks 运行时与事件总线的拦截点接线（生态兼容 P1）。它在给定
// 拦截点对 subject 运行匹配的 command 钩子，并在每次调用时先发 hook/invoked、运行
// 后发 hook/result（按 handlerId 配对，对齐 appendHookInvoked/appendHookResult）。
//
// 单个钩子失败不阻断主流程：runOne 内部隔离错误，结果载荷照常发出。point 不受
// 支持（非 7 个 CC 拦截点）时为空操作。返回所有钩子结果（保持顺序），供调用方
// 决定是否读取其决策。
func (b *EventBus) DispatchHook(ctx context.Context, hooks *Hooks, point, subject string, turn int) []HookOutcome {
	if hooks == nil {
		return nil
	}
	onInvoked := func(inv HookInvokedPayload) { b.Emit(EventHookInvoked, inv) }
	onResult := func(res HookResultPayload) { b.Emit(EventHookResult, res) }
	return hooks.Dispatch(ctx, point, subject, turn, onInvoked, onResult)
}

// sameHandler 判定两个 handler 是否同一闭包实例（用于精确取消）。
func sameHandler(a, b EventHandler) bool {
	return &a == &b || reflectPointer(a) == reflectPointer(b)
}

func reflectPointer(f EventHandler) uintptr {
	return reflect.ValueOf(f).Pointer()
}
