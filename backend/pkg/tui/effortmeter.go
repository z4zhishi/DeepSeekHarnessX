package tui

import (
	"context"
	"sync/atomic"

	"dsh-go/pkg/llm"
)

// tunedAdapter wraps the real llm.LlmAdapter inside the TUI process and is
// the single injection point for three status-bar features, without touching
// pkg/agent (whose ModelRequest construction is read-only for this change):
//
//  3. reasoning effort — overrides ModelRequest.ReasoningEffort on ordinary
//     conversation requests;
//  4. model override — swaps in a user-chosen model id (/model);
//  5. cache-rate metering — tallies TokenUsage from usage chunks.
//
// Auxiliary requests (Purpose "session-title" / "compaction") are passed
// through untouched: their effort/model behavior is deliberately tuned
// downstream (adapters force title generation to non-thinking), and their
// tokens are excluded from the conversation-meter口径.
type tunedAdapter struct {
	inner llm.LlmAdapter

	effort      atomic.Value // string
	model       atomic.Value // string; "" = pass agent's ModelName through
	inputTok    atomic.Int64
	outputTok   atomic.Int64
	cacheRead   atomic.Int64
	cacheWrite  atomic.Int64
	onUsageOnce atomic.Bool // set once a first usage arrives (drives UI refresh)

	onUsage func() // optional callback invoked after each usage chunk (UI refresh)
}

func newTunedAdapter(inner llm.LlmAdapter, initialModel string) *tunedAdapter {
	t := &tunedAdapter{inner: inner}
	t.effort.Store(llm.DefaultReasoningEffort)
	t.model.Store(initialModel)
	return t
}

// Stream implements llm.LlmAdapter. The request struct is a value copy, so
// overriding fields here never leaks back into the agent loop.
func (t *tunedAdapter) Stream(ctx context.Context, req llm.ModelRequest) (<-chan llm.StreamChunk, <-chan error) {
	auxiliary := req.Purpose != ""
	if !auxiliary {
		if e, _ := t.effort.Load().(string); e != "" {
			req.ReasoningEffort = e
		}
		if m, _ := t.model.Load().(string); m != "" {
			req.Model = m
		}
	}
	chunks, errs := t.inner.Stream(ctx, req)
	if auxiliary {
		return chunks, errs
	}
	out := make(chan llm.StreamChunk, 64)
	go func() {
		defer close(out)
		for c := range chunks {
			if c.Type == llm.ChunkUsage && c.Usage != nil {
				t.inputTok.Add(int64(c.Usage.InputTokens))
				t.outputTok.Add(int64(c.Usage.OutputTokens))
				t.cacheRead.Add(int64(c.Usage.CacheReadTokens))
				t.cacheWrite.Add(int64(c.Usage.CacheWriteTokens))
				t.onUsageOnce.Store(true)
				if t.onUsage != nil {
					t.onUsage()
				}
			}
			out <- c
		}
	}()
	return out, errs
}

// SetEffort updates the outbound reasoning effort ("off"|"low"|"high"|"max").
func (t *tunedAdapter) SetEffort(e string) { t.effort.Store(e) }

// Effort returns the current effort setting.
func (t *tunedAdapter) Effort() string {
	e, _ := t.effort.Load().(string)
	return e
}

// CycleEffort rotates low → high → max → off → low and returns the new value.
// Order matches ascending capability so one keypress-free command walks the
// full range predictably.
func (t *tunedAdapter) CycleEffort() string {
	order := []string{"low", "high", "max", "off"}
	cur := t.Effort()
	for i, v := range order {
		if v == cur {
			next := order[(i+1)%len(order)]
			t.SetEffort(next)
			return next
		}
	}
	t.SetEffort(order[1])
	return order[1]
}

// ValidEffort reports whether s is an accepted /thinking argument.
func ValidEffort(s string) bool {
	switch s {
	case "", "off", "low", "high", "max":
		return true
	}
	return false
}

// SetModel overrides the outbound model id ("" restores the configured one).
func (t *tunedAdapter) SetModel(m string) { t.model.Store(m) }

// Model returns the effective display/override model name.
func (t *tunedAdapter) Model() string {
	m, _ := t.model.Load().(string)
	return m
}

// UsageTotals is a cumulative snapshot of conversation token metering.
type UsageTotals struct {
	Input      int
	Output     int
	CacheRead  int
	CacheWrite int
}

// Total sums every bucket — the displayed "… tok" figure.
func (u UsageTotals) Total() int {
	return u.Input + u.Output + u.CacheRead + u.CacheWrite
}

// CacheRate implements the agreed口径: cached-prompt share of all prompt-side
// tokens, cacheRead/(input+cacheRead). Returns -1 when nothing has been
// measured yet (display shows "–"). Cache-write is excluded from the
// denominator: on providers that only report writes (first turn of a fresh
// session) there was no opportunity for a hit, and counting writes as misses
// would drag the number down misleadingly.
func (u UsageTotals) CacheRate() float64 {
	denom := u.Input + u.CacheRead
	if denom <= 0 {
		return -1
	}
	return float64(u.CacheRead) / float64(denom) * 100
}

// Totals snapshots the meters under one lock-free consistent read (each
// counter is independent; drift across buckets within one read is bounded by
// a single in-flight usage chunk and self-corrects on the next).
func (t *tunedAdapter) Totals() UsageTotals {
	return UsageTotals{
		Input:      int(t.inputTok.Load()),
		Output:     int(t.outputTok.Load()),
		CacheRead:  int(t.cacheRead.Load()),
		CacheWrite: int(t.cacheWrite.Load()),
	}
}
