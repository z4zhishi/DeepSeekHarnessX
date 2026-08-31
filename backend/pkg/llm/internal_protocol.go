// 内部协议（dsh-internal/v1）与协议注册表 —— pkg/llm 的中转层（relay）。
//
// 协议插件化架构 story：
//
//   - 内部协议即 LlmAdapter 接口本身（stream.go）：ModelRequest 进，
//     StreamChunk 流 + error 通道出。宿主 / Router / agent / 插件
//     LlmCapability 之间全部以内部协议对话，互不感知外部线格式。
//   - 线协议（wire protocol）是"线格式构造块"：openai-completions /
//     openai-responses / anthropic-messages 三个适配器把 ProviderProfile
//     翻译成一个实现内部协议的 LlmAdapter（内部协议 → 外部线的翻译，即中转）。
//     每条线协议在其自身文件中实现，行为零变更，只经 ProtocolFactory 注册。
//   - 三条线协议的播种即默认注册：包级 defaultProtocols 在初始化期按
//     SupportedProtocols() 顺序（most-reached first）注册；NewProtocolAdapter /
//     SupportedProtocols 都委托这张默认表（protocol.go）。需要自有协议集的
//     消费者（如插件 capability）用 NewProtocolRegistry + Register 自建注册
//     表，RegisterBuiltinProtocols 可复用同一条默认播种。
//   - DeepSeek 不是协议：deepseek 没有自有线格式（还是老三样）。它是
//     openai-completions 之上的默认 provider profile（profileFromDeepSeek 播种
//     api.deepseek.com / deepseek-v4-flash / DEEPSEEK_API_KEY 凭据引用）。
//   - InternalProtocol 只是内部协议的名字标签："dsh-internal/v1"。它不是线
//     格式、不注册进任何 registry——任何拥有 LlmAdapter 的组件都在说内部
//     协议；该常量供路由/展示层（如 provider profile 的 protocol 字段、插件
//     capability 协商）标记"此段直接走内部协议"。
package llm

import (
	"fmt"
	"strings"
	"sync"
)

// InternalProtocol is the name of DSHX's own canonical relay protocol
// （dsh-internal/v1）。内部协议本身即 LlmAdapter 接口：ModelRequest 进、
// StreamChunk 流 + error 通道出——宿主 / Router / 插件都以该协议对话，wire
// 协议适配器只是内部协议到外部线格式的翻译器。这个常量只在需要在配置面
// （路由表、profile 字段、cap 协商）上【命名】内部协议时使用；它永远不会
// 出现在 ProtocolRegistry 里（内部协议不需要线格式工厂——已经是内部协议了）。
const InternalProtocol = "dsh-internal/v1"

// ProtocolFactory builds one internal-protocol adapter (LlmAdapter) for a
// provider profile. 工厂把（可能带未知字段的）线协议专属配置折叠成内部协议
// 适配器；构造必须离线（无网络 I/O），Stream 时刻才解析凭据/发起请求。
type ProtocolFactory func(ProviderProfile) (LlmAdapter, error)

// ProtocolRegistry is the thread-safe name → factory table behind 协议
// 插件化 (wire protocols hosted by the host's own registry). The default
// registry seeds the three wire protocols in SupportedProtocols() order
// (most-reached first) and every consumer may build its own.
// 注册顺序（order）就是 List() / 未知协议错误文案的稳定顺序；重复注册总是
// 覆盖（explicit override）并保留原注册位置。
type ProtocolRegistry struct {
	mu      sync.RWMutex
	order   []string // 注册顺序 = List / 错误文案顺序（覆盖注册保持原位）
	factory map[string]ProtocolFactory
}

// NewProtocolRegistry returns an empty registry. 空表可直接 Register 消费者
// 自有协议，或先经 RegisterBuiltinProtocols 播种三条默认线协议。
func NewProtocolRegistry() *ProtocolRegistry {
	return &ProtocolRegistry{factory: make(map[string]ProtocolFactory)}
}

// Register adds factory under name.
//
// 语义（显式声明）：
//   - 空 name 或 nil factory → 报错，表不变；
//   - 新 name → 追加到注册顺序末尾；
//   - 重复 name → 显式覆盖：新工厂替换旧工厂，原注册位置不变（对无捕获的
//     顶层工厂，覆盖与幂等 no-op 观测等价；Go 函数值不可比较——仅可与 nil
//     比较——故不做"同一工厂"识别，统一走覆盖路径，错误与 no-op 均返回成功）。
func (r *ProtocolRegistry) Register(name string, factory ProtocolFactory) error {
	if name == "" {
		return fmt.Errorf("llm: protocol name must not be empty")
	}
	if factory == nil {
		return fmt.Errorf("llm: protocol %q factory must not be nil", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.factory[name]; !exists {
		r.order = append(r.order, name)
	}
	r.factory[name] = factory
	return nil
}

// Unregister removes name. Unknown → error（显式失败，不静默）。
func (r *ProtocolRegistry) Unregister(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.factory[name]; !exists {
		return fmt.Errorf("llm: protocol %q is not registered", name)
	}
	delete(r.factory, name)
	for i, n := range r.order {
		if n == name {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	return nil
}

// List returns the registered protocol names in registration (seed) order —
// a fresh copy safe for callers to mutate.
func (r *ProtocolRegistry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}

// Build resolves name and invokes its factory for the profile. Build 不做
// 空名回退——NewProtocolAdapter 拥有"空 Protocol → openai-completions"的
// 便捷默认；未知 name → 错误并列出已注册名（与 NewProtocolAdapter 的旧
// switch 语义逐字一致）。
func (r *ProtocolRegistry) Build(name string, p ProviderProfile) (LlmAdapter, error) {
	r.mu.RLock()
	factory, ok := r.factory[name]
	names := append([]string(nil), r.order...)
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("llm: unknown protocol %q (supported: %s)", name, strings.Join(names, ", "))
	}
	return factory(p)
}

// wire-protocol factories：三条线协议适配器的注册入口（内部协议 → 外部线格式）
func completionsFactory(p ProviderProfile) (LlmAdapter, error) { return newCompletionsAdapter(p), nil }

func responsesFactory(p ProviderProfile) (LlmAdapter, error) { return newResponsesAdapter(p), nil }

func anthropicFactory(p ProviderProfile) (LlmAdapter, error) { return newAnthropicAdapter(p), nil }

// defaultProtocols is the package-default registry, seeded once at init with
// the three hand-declared wire protocols in SupportedProtocols() order
// (most-reached first). Consumers never mutate it: NewProtocolAdapter /
// SupportedProtocols read it, and consumers wanting a custom set build their
// own via NewProtocolRegistry + Register.
var defaultProtocols = newDefaultProtocolRegistry()

func newDefaultProtocolRegistry() *ProtocolRegistry {
	r := NewProtocolRegistry()
	if err := RegisterBuiltinProtocols(r); err != nil {
		// Unreachable: the three builtin seeds are compile-time constants with
		// non-empty names and non-nil factories.
		panic(fmt.Sprintf("llm: seeding default protocol registry: %v", err))
	}
	return r
}

// RegisterBuiltinProtocols seeds the three wire protocol factories into any
// registry in SupportedProtocols() (seed) order — the same registration the
// package-default registry gets, exposed so consumers (插件 capability) that
// build their own ProtocolRegistry can start from the built-in wire set.
// 重复调用安全：同 (name, factory) 的重复注册按覆盖语义幂等收敛。
func RegisterBuiltinProtocols(r *ProtocolRegistry) error {
	for _, seed := range []struct {
		name    string
		factory ProtocolFactory
	}{
		{ProtocolOpenAICompletions, completionsFactory},
		{ProtocolOpenAIResponses, responsesFactory},
		{ProtocolAnthropicMessages, anthropicFactory},
	} {
		if err := r.Register(seed.name, seed.factory); err != nil {
			return err
		}
	}
	return nil
}
