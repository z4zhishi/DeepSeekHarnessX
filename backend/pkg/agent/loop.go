package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dsh-go/pkg/compaction"
	"dsh-go/pkg/llm"
	"dsh-go/pkg/plugin"
	"dsh-go/pkg/session"
	"dsh-go/pkg/storage"
	"dsh-go/pkg/tools"
)

// PersistSink is the durable append surface an agent writes every emitted
// event to. *storage.SqliteStore, *storage.SqliteGatewayStore and
// *storage.BboltStore all satisfy it; nil disables persistence (ACP stdio
// and in-memory modes).
type PersistSink interface {
	AppendEvents(meta *session.SessionHeader, events []*session.SessionEnvelope) error
	GetEvents(sessionID string, fromSeq int) ([]session.SessionEnvelope, error)
}

// seedSeqFrom resumes the agent counters at the stored tail so a re-opened
// session keeps appending contiguously (appendBatch contract). Beyond the
// next free seq it recovers the last turn number found among turn/start |
// turn/end payloads — the restart-monotonicity seed for turnNumber, since a
// rebuilt actor must number its first turn past everything already in the
// durable log — and returns the scanned events so NewAgent can warm the
// incremental log cache from this same single full read.
func seedSeqFrom(store PersistSink, id string) (int64, int32, []session.SessionEnvelope) {
	if store == nil {
		return 0, 0, nil
	}
	events, err := store.GetEvents(id, 0)
	if err != nil || len(events) == 0 {
		return 0, 0, nil
	}
	// Turn numbers are assigned monotonically, so the maximum seen equals the
	// last boundary marker's value; scanning both marker kinds also covers
	// logs whose tail ends mid-turn (crash-orphaned open turn).
	var lastTurn int32
	for i := range events {
		switch events[i].Type {
		case session.EventTurnStart, session.EventTurnEnd:
			var p struct {
				Turn int `json:"turn"`
			}
			if json.Unmarshal(events[i].Data, &p) == nil && p.Turn > int(lastTurn) {
				lastTurn = int32(p.Turn)
			}
		}
	}
	return int64(events[len(events)-1].Seq) + 1, lastTurn, events
}

// Agent runs a dedicated event-driven Actor loop for a single conversation session.
type Agent struct {
	Header       session.SessionHeader
	RingBuf      *storage.RingBuffer
	SegmentLog   *storage.SegmentLog
	Tools        *tools.ToolRegistry
	LlmAdapter   llm.LlmAdapter
	SystemPrompt string
	ModelName    string
	// Instructions carries the resolved workspace instruction files (the
	// community memory family: AGENTS.md / CLAUDE.md / ... from pkg/
	// instructions). It is appended to SystemPrompt when the ModelRequest is
	// built; empty keeps the system prompt byte-for-byte identical to the
	// pre-seam behavior. Hosts set it once at boot from
	// instructions.Resolve(workspace cwd).
	Instructions string
	// Effort overrides the reasoning_effort sent to the model for this agent
	// ("off" | "low" | "high" | "max"). Empty means the provider default. The
	// GUI/TUI set it per session (mirrors tunedAdapter's live override).
	Effort string

	nextTurnChan chan session.UserMessagePayload
	nextStepChan chan session.ContentBlock
	persist      PersistSink
	// RequestUser is an optional human/policy approval hook injected by the
	// hosting frontend (ACP approval waterfall, TUI modal, Godot modal).
	// When nil, permission-gated tools run without interactive approval.
	RequestUser func(prompt string, options []string) (tools.ApprovalDecision, error)
	// Compactor is an optional compaction engine; when set, the agent runs an
	// idle-session compaction pressure check after every completed turn.
	Compactor *compaction.CompactEngine
	// Schedule is the session-local reminder dispatcher (upstream
	// ScheduleRuntime): it wakes an idle agent with a due reminder and
	// appends the schedule/change dispatch events.
	Schedule *tools.ScheduleDispatcher
	// HookBus optionally wires the CC-style hooks runtime into the agent loop.
	// When both HookBus and a live *Hooks are set, the four dispatch intercept
	// points (UserPromptSubmit / PreToolUse / PostToolUse / Stop) run matching
	// command hooks and emit hook/invoked + hook/result events into the session
	// log. Dispatch is best-effort: a failing or missing hook never blocks the loop.
	HookBus *plugin.EventBus
	// Hooks is the parsed hooks.json configuration driven by HookBus. Fallback
	// for tests that set it directly; live hosts should set HooksProvider.
	Hooks *plugin.Hooks
	// HooksProvider is a live lookup for parsed hooks.json. When set, dispatchHook
	// re-reads it on every intercept so plugin Unload/SetEnabled("hooks", false)
	// stops hooks for already-running agents. Product hosts set this via
	// AttachPluginRuntime rather than assigning the field directly.
	HooksProvider HooksProvider
	// pluginRuntime is the live plugin host. Agents re-read it on every hook
	// dispatch (via HooksProvider) and every LLM Stream (via liveAdapter).
	pluginRuntime PluginRuntime
	// AutoTitle enables one-shot automatic session-title generation after the
	// first eligible human message (prompt first-eligible last-turn mode).
	// It defaults false so in-memory/test hosts stay title-free; the gateway,
	// TUI, and headless hosts opt in. Title generation is never load-bearing:
	// any failure silently degrades to the deterministic fallback.
	AutoTitle bool
	// teamTeardown stops the session's Team runtime (live children + registry)
	// on Stop(). It is set once by Start() via tools.RegisterTeamSession.
	teamTeardown func()
	ctx          context.Context
	cancelFunc   context.CancelFunc
	// turnCancel aborts only the in-flight turn (llmRetry/Stream) without
	// tearing down the actor. AbortTurn invokes it; a nil value means idle.
	turnMu     sync.Mutex
	turnCancel context.CancelFunc

	seqCounter atomic.Int64
	turnNumber atomic.Int32
	stepNumber atomic.Int32
	isRunning  atomic.Bool
	// titleDone latches the one-shot automatic session-title generation for the
	// first eligible user message (upstream session-title first-prompt mode).
	titleDone atomic.Bool

	// Incremental session-log cache: one full durable read at construction,
	// then fromSeq top-ups per access; invalidated after compaction rewrites
	// the log. evMu guards it because EmitEvent appends also arrive from
	// non-actor goroutines (schedule dispatcher, gateway/TUI command surfaces).
	evMu       sync.Mutex
	logCache   []session.SessionEnvelope // read-only mirror of the durable log tail
	cacheWarm  bool                      // false until first full load / after invalidation
	lastCached int                       // highest seq present in logCache (-1 when empty)

	// Turn/step lifecycle state machine (upstream loop contract):
	// turn/start -> step/start -> step/end -> ... -> turn/end. Illegal jumps
	// (step events outside a turn, turn/start inside a live turn, turn/end
	// with no turn) are rejected instead of corrupting the log.
	stateMu     sync.Mutex
	activeTurn  int
	activeStep  int
	subscribers []chan *session.SessionEnvelope
	subMu       sync.RWMutex
}

// NewAgent creates and initializes an agent session. The optional store is
// the durable persistence sink; pass nil to run purely in-memory (ACP stdio).
func NewAgent(
	header session.SessionHeader,
	ringBuf *storage.RingBuffer,
	segLog *storage.SegmentLog,
	store PersistSink,
	toolReg *tools.ToolRegistry,
	llmAdapter llm.LlmAdapter,
	systemPrompt string,
	modelName string,
) *Agent {
	ctx, cancel := context.WithCancel(context.Background())
	a := &Agent{
		Header:       header,
		RingBuf:      ringBuf,
		SegmentLog:   segLog,
		Tools:        toolReg,
		LlmAdapter:   llmAdapter,
		SystemPrompt: systemPrompt,
		ModelName:    modelName,
		persist:      store,
		nextTurnChan: make(chan session.UserMessagePayload, 16),
		nextStepChan: make(chan session.ContentBlock, 32),
		ctx:          ctx,
		cancelFunc:   cancel,
	}
	seqSeed, lastTurn, warmEvents := seedSeqFrom(store, header.ID)
	a.seqCounter.Store(seqSeed)
	// Restart monotonicity: the rebuilt actor continues past every turn the
	// durable log already contains. stepNumber intentionally restarts at 0 —
	// each turn re-zeroes it when the turn begins.
	a.turnNumber.Store(lastTurn)
	if len(warmEvents) > 0 {
		a.evMu.Lock()
		a.logCache = warmEvents
		a.lastCached = warmEvents[len(warmEvents)-1].Seq
		a.cacheWarm = true
		a.evMu.Unlock()
	}
	// Session-local reminder dispatcher: due schedules wake the agent with a
	// framed follow-up message and append the dispatch events through the
	// machine (upstream ScheduleRuntime composition).
	a.Schedule = tools.NewScheduleDispatcher(ctx, header.ID)
	a.Schedule.SetDispatchHooks(func(eventType string, payload any) {
		_, _ = a.EmitEvent(eventType, payload)
	}, func(text string) {
		a.PostUserMessage(session.UserMessagePayload{
			ID:   fmt.Sprintf("schedule-%d", time.Now().UnixNano()),
			Role: "user",
			Content: []session.ContentBlock{
				{Type: "text", Text: text},
			},
			Source: session.MessageSource{Kind: "plugin", Plugin: "schedule"},
		})
	})
	return a
}

// Subscribe opens a live event stream channel for Web / TUI downlinks.
func (a *Agent) Subscribe() chan *session.SessionEnvelope {
	a.subMu.Lock()
	defer a.subMu.Unlock()
	ch := make(chan *session.SessionEnvelope, 256)
	a.subscribers = append(a.subscribers, ch)
	return ch
}

// Unsubscribe closes and removes a subscriber channel.
func (a *Agent) Unsubscribe(ch chan *session.SessionEnvelope) {
	a.subMu.Lock()
	defer a.subMu.Unlock()
	for i, sub := range a.subscribers {
		if sub == ch {
			a.subscribers = append(a.subscribers[:i], a.subscribers[i+1:]...)
			close(ch)
			break
		}
	}
}

// EmitEvent appends an event to RingBuffer, SegmentLog, the durable store,
// and broadcasts to subscribers. A non-nil surfaceOp attaches the marker to
// the envelope; surface-eligible types (user/message, assistant/message,
// tool/result) require it, mirroring upstream Session.append.
func (a *Agent) EmitEvent(eventType string, payload any, surfaceOp ...*session.SurfaceOp) (*session.SessionEnvelope, error) {
	// Snapshot, state machine transition, and boundary payload assignment run
	// under a single hold of stateMu. This closes the TOCTOU window: without
	// it, a concurrent EmitEvent (session.command, tool Emit) could transition
	// the machine between the snapshot and validateTransition, so the numbers
	// a boundary payload carries would no longer match the numbers the machine
	// assigned for it. validateTransition assumes the lock is already held.
	a.stateMu.Lock()
	preTurn, preStep := a.activeTurn, a.activeStep
	if err := a.validateTransition(eventType); err != nil {
		a.stateMu.Unlock()
		return nil, err
	}
	// Boundary events carry the state machine's assigned numbers; the caller
	// passes zero-valued placeholders so the machine stays the single source
	// of turn/step identity. Start events read POST-transition values (the
	// numbers validateTransition just assigned — pre-values are always 0 for
	// them); end events keep their pre-transition identity.
	switch eventType {
	case session.EventTurnStart:
		payload = session.TurnStartPayload{Turn: a.activeTurn}
	case session.EventStepStart:
		payload = session.StepStartPayload{Turn: a.activeTurn, Step: a.activeStep}
	case session.EventStepEnd:
		payload = session.StepEndPayload{Turn: preTurn, Step: preStep}
	case session.EventTurnEnd:
		if tp, ok := payload.(session.TurnEndPayload); ok {
			tp.Turn = preTurn
			payload = tp
		}
	}
	a.stateMu.Unlock()
	seq := int(a.seqCounter.Add(1)) - 1 // upstream seqs start at 0
	env, err := session.NewEnvelope(seq, eventType, payload)
	if err != nil {
		return nil, err
	}
	if len(surfaceOp) > 0 {
		op := surfaceOp[0]
		env.SurfaceOp = op
	}

	// 1. Durable session store (SQLite appendBatch / bbolt fallback). The
	// event is persisted before it enters any in-memory view, so a failed
	// append leaves no trace and the log stays the single source of truth.
	if a.persist != nil {
		if err := a.persist.AppendEvents(&a.Header, []*session.SessionEnvelope{env}); err != nil {
			return nil, fmt.Errorf("persist %s: %w", eventType, err)
		}
	}

	// 2. In-memory RingBuffer
	if a.RingBuf != nil {
		a.RingBuf.Push(env)
	}

	// 3. Persistent SegmentLog
	if a.SegmentLog != nil {
		_ = a.SegmentLog.Append(env)
	}

	// 4. Fan-out to subscribers
	a.subMu.RLock()
	for _, sub := range a.subscribers {
		select {
		case sub <- env:
		default:
			// Non-blocking drop if subscriber buffer is full to prevent pipeline stall
		}
	}
	a.subMu.RUnlock()

	// Mirror the persisted envelope into the incremental session-log cache
	// (best-effort continuity for derivation/compaction/title reads; a
	// late append behind a concurrent store top-up is dropped by the seq
	// guard in cacheAppend).
	a.cacheAppend(env)

	return env, nil
}

// validateTransition enforces the turn/step lifecycle contract before an
// event is admitted to the log. Boundary events mutate the machine only
// after admission succeeds; a rejected event leaves the state untouched.
//
// stateMu is expected to be HELD by the caller (EmitEvent holds it across the
// snapshot/transition/assign section) — this function does not lock itself,
// so EmitEvent's snapshot and the machine mutation share one critical section.
func (a *Agent) validateTransition(eventType string) error {
	switch eventType {
	case session.EventTurnStart:
		if a.activeTurn != 0 {
			return fmt.Errorf("%w: turn/start while turn %d is active", errInvalidTransition, a.activeTurn)
		}
		a.activeTurn = int(a.turnNumber.Add(1))
		a.activeStep = 0
	case session.EventTurnEnd:
		if a.activeTurn == 0 {
			return fmt.Errorf("%w: turn/end with no active turn", errInvalidTransition)
		}
		if a.activeStep != 0 {
			return fmt.Errorf("%w: turn/end while step %d is open", errInvalidTransition, a.activeStep)
		}
		a.activeTurn = 0
	case session.EventStepStart:
		if a.activeTurn == 0 {
			return fmt.Errorf("%w: step/start with no active turn", errInvalidTransition)
		}
		if a.activeStep != 0 {
			return fmt.Errorf("%w: step/start while step %d is open", errInvalidTransition, a.activeStep)
		}
		a.activeStep = int(a.stepNumber.Add(1))
	case session.EventStepEnd:
		if a.activeStep == 0 {
			return fmt.Errorf("%w: step/end with no active step", errInvalidTransition)
		}
		a.activeStep = 0
	}
	return nil
}

// PostUserMessage posts a user message to the next-turn inbox.
func (a *Agent) PostUserMessage(msg session.UserMessagePayload) {
	a.nextTurnChan <- msg
}

// PostNextStep posts a next-step priority interruption: the current step is
// aborted and a fresh step immediately restarts with the supplied prompt as
// the driving user message (upstream next-step inbox semantics).
func (a *Agent) PostNextStep(step session.ContentBlock) {
	a.nextStepChan <- step
}

// emitRaw appends a pre-built envelope (compaction transaction events) to the
// durable store, RingBuffer, SegmentLog, and subscribers without touching the
// turn/step state machine (compaction brackets are log-only).
func (a *Agent) emitRaw(env *session.SessionEnvelope) error {
	// Compaction transaction events carry placeholder seq 0; the agent's
	// global counter assigns the contiguous log seq here so the durable
	// log stays seamless (sourceEventSeqs resolve after assignment).
	env.Seq = int(a.seqCounter.Add(1)) - 1
	if a.persist != nil {
		if err := a.persist.AppendEvents(&a.Header, []*session.SessionEnvelope{env}); err != nil {
			return fmt.Errorf("persist %s: %w", env.Type, err)
		}
	}
	if a.RingBuf != nil {
		a.RingBuf.Push(env)
	}
	if a.SegmentLog != nil {
		_ = a.SegmentLog.Append(env)
	}
	a.subMu.RLock()
	for _, sub := range a.subscribers {
		select {
		case sub <- env:
		default:
		}
	}
	a.subMu.RUnlock()
	return nil
}

// errInvalidTransition marks a lifecycle state-machine rejection raised by
// validateTransition: the event was refused BEFORE any machine mutation or
// durable append, so tolerating it carries no ghost-state risk (the pre-step
// interrupt drain relies on this when no step is open). Persistence failures
// carry no such marker and drive boundary-event escalation instead.
var errInvalidTransition = errors.New("invalid transition")

// errStreamClosedAfterCancel is the sentinel routed when the step-loop stream
// read observes a closed chunkChan after the turn context was already
// cancelled: llmRetry exits silently on ctx.Done and closes both channels
// without an error, so the close itself is an abort artifact, never a clean
// EOF. routeStepFatal consumes only the ctx state for this shape — aborted
// beats failed — so the message is purely diagnostic.
var errStreamClosedAfterCancel = errors.New("stream closed after turn context cancellation")

// emitBoundary appends a turn/step lifecycle boundary event (turn/start,
// step/end, turn/end). The transition machine mutates before AppendEvents, so
// a persistence failure would leave later events referencing a turn/step the
// log never admitted; on such a failure this converges the open turn with an
// error reason (finishTurnReason) and reports false so the caller stops
// deriving follow-up events for the round. State-machine rejections are
// tolerated (reports true) to preserve the historical swallow semantics —
// nothing was mutated or written when they fire.
func (a *Agent) emitBoundary(eventType string, payload any) bool {
	_, err := a.EmitEvent(eventType, payload)
	if err == nil {
		return true
	}
	if errors.Is(err, errInvalidTransition) {
		return true
	}
	a.finishTurnReason("error", fmt.Sprintf("boundary %s persist failed: %v", eventType, err))
	return false
}

// cacheAppend mirrors one successfully persisted envelope into the
// incremental session-log cache. A cold cache skips the append (its next
// reader full-loads anyway); an append that arrives behind a concurrent
// reader which already topped up past this seq from the store is dropped by
// the seq guard.
func (a *Agent) cacheAppend(env *session.SessionEnvelope) {
	if env == nil {
		return
	}
	a.evMu.Lock()
	defer a.evMu.Unlock()
	if !a.cacheWarm || env.Seq <= a.lastCached {
		return
	}
	a.logCache = append(a.logCache, *env)
	a.lastCached = env.Seq
}

// invalidateLogCache drops the incremental session-log cache: compaction
// rewrites (shadowed source rows + replacement events) make every mirrored
// entry suspect the moment the transaction attempts its first bracket, so
// the next reader pays one full reload of the rewritten log.
func (a *Agent) invalidateLogCache() {
	a.evMu.Lock()
	a.logCache = nil
	a.lastCached = -1
	a.cacheWarm = false
	a.evMu.Unlock()
}

// cachedSessionEvents returns the durable session log through the incremental
// cache: one full load when cold, otherwise a fromSeq top-up past the last
// mirrored seq (PersistSink.GetEvents is fromSeq-inclusive). A failed top-up
// falls back to one full reload before serving the possibly-stale snapshot.
// Callers must treat the returned slice as read-only.
func (a *Agent) cachedSessionEvents() []session.SessionEnvelope {
	a.evMu.Lock()
	defer a.evMu.Unlock()
	if !a.cacheWarm {
		events, err := a.persist.GetEvents(a.Header.ID, 0)
		if err != nil {
			return nil
		}
		a.logCache = events
		a.lastCached = -1
		if len(events) > 0 {
			a.lastCached = events[len(events)-1].Seq
		}
		a.cacheWarm = true
		return a.logCache
	}
	events, err := a.persist.GetEvents(a.Header.ID, a.lastCached+1)
	if err != nil {
		if events, err = a.persist.GetEvents(a.Header.ID, 0); err != nil {
			return a.logCache
		}
		a.logCache = events
		a.lastCached = -1
		if len(events) > 0 {
			a.lastCached = events[len(events)-1].Seq
		}
		return a.logCache
	}
	for i := range events {
		if events[i].Seq <= a.lastCached {
			continue // late/duplicated EmitEvent append already covered by the store read
		}
		a.logCache = append(a.logCache, events[i])
		a.lastCached = events[i].Seq
	}
	return a.logCache
}

// loadSessionLog returns the full session log for derivation-style consumers,
// preserving the historical source precedence: the durable store (via the
// incremental cache) when a persist sink exists, else the ring window, else
// the legacy segment log. Callers must treat the result as read-only.
func (a *Agent) loadSessionLog() []session.SessionEnvelope {
	if a.persist != nil {
		if evs := a.cachedSessionEvents(); len(evs) > 0 {
			return evs
		}
	}
	if a.RingBuf != nil {
		if ptrs := a.RingBuf.GetSince(0); len(ptrs) > 0 {
			out := make([]session.SessionEnvelope, len(ptrs))
			for i, p := range ptrs {
				out[i] = *p
			}
			return out
		}
	}
	if a.SegmentLog != nil {
		_, evs, _ := a.SegmentLog.ReadAll()
		return evs
	}
	return nil
}

// maybeCompact runs the idle-session compaction pressure check after a
// completed turn: it selects a head-anchored balanced range and runs the
// durable compaction transaction through emitRaw (brackets are log-only).
func (a *Agent) maybeCompact() {
	if a.Compactor == nil {
		return
	}
	var log []session.SessionEnvelope
	if a.persist != nil {
		log = a.cachedSessionEvents()
	} else if a.SegmentLog != nil {
		_, log, _ = a.SegmentLog.ReadAll()
	}
	if len(log) == 0 {
		return
	}
	fold, err := session.FoldSurface(eventPointers(log), 0)
	if err != nil {
		return
	}
	span, err := a.Compactor.SelectCompactableRange(log, fold.Nodes)
	if err != nil || span == nil {
		return
	}
	_, _ = a.Compactor.CompactTransaction(
		context.Background(),
		&a.Header,
		log,
		fold.Nodes,
		func(env *session.SessionEnvelope) error { return a.emitRaw(env) },
		compaction.CompactTransactionOptions{Start: span.Start, End: span.End},
	)
	// The transaction rewrites/shadows the durable log, so the incremental
	// cache is stale regardless of the outcome — invalidate unconditionally;
	// the next reader full-reloads the rewritten log.
	a.invalidateLogCache()
}

// eventPointers converts a value slice to pointers for FoldSurface.
func eventPointers(events []session.SessionEnvelope) []*session.SessionEnvelope {
	out := make([]*session.SessionEnvelope, len(events))
	for i := range events {
		out[i] = &events[i]
	}
	return out
}

// Start runs the agent execution Actor loop.
func (a *Agent) Start() {
	tools.RegisterScheduleEvents(a.Header.ID, func() []*session.SessionEnvelope {
		// The durable store is the authoritative full log; the ring only
		// covers its bounded window and would drop early schedule events.
		if a.persist != nil {
			if evs, err := a.persist.GetEvents(a.Header.ID, 0); err == nil && len(evs) > 0 {
				ptrs := make([]*session.SessionEnvelope, len(evs))
				for i := range evs {
					ptrs[i] = &evs[i]
				}
				return ptrs
			}
		}
		if a.RingBuf != nil {
			return a.RingBuf.GetSince(0)
		}
		return nil
	})
	if a.Schedule != nil {
		a.Schedule.Start()
	}
	// 注册本 lead 会话的 Team 运行时：spawn_teammate/wait/interrupt/send 需要
	// 一个进程级 child 注册表。返回的 teardown 在 Stop() 里关闭 live children
	// 并注销（重复调用幂等）。
	a.teamTeardown = tools.RegisterTeamSession(a.Header.ID)
	go a.actorLoop()
}

// Stop cancels ongoing execution.
func (a *Agent) Stop() {
	// CC Stop hook: runs once on agent teardown (best-effort; a missing runtime
	// or a hook failure is a no-op and never blocks shutdown). turn 0 because a
	// Stop may occur outside any turn.
	a.dispatchHook(plugin.HookPointStop, "", 0)
	if a.teamTeardown != nil {
		td := a.teamTeardown
		a.teamTeardown = nil
		td()
	}
	a.cancelFunc()
	if a.Schedule != nil {
		a.Schedule.Stop()
	}
	tools.UnregisterScheduleEvents(a.Header.ID)
}

// IsRunning reports whether the actor is mid-turn (TeamChild.Status uses it).
func (a *Agent) IsRunning() bool { return a.isRunning.Load() }

// Alive reports whether the actor loop is still serving (context not cancelled).
// Distinct from IsRunning, which is only true during a turn. session.stop
// cancels the context immediately; a stopped agent may remain in the gateway
// map and must be replaced on resume.
func (a *Agent) Alive() bool {
	return a != nil && a.ctx != nil && a.ctx.Err() == nil
}

// Done exposes the agent's app-level termination signal — the context Stop()
// cancels — so long-lived downlink consumers can stop ranging a dead agent's
// subscriber channel (and then Unsubscribe it) instead of parking forever.
// No new lifecycle state: Start/Stop already own this context.
func (a *Agent) Done() <-chan struct{} {
	return a.ctx.Done()
}

// AbortTurn cancels the per-turn context used by llmRetry/Stream without
// cancelling the actor. The loop emits step/end + turn/end reason=aborted
// and then waits for the next PostUserMessage. A no-op when idle.
func (a *Agent) AbortTurn() {
	a.turnMu.Lock()
	cancel := a.turnCancel
	a.turnMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// deriveTurnCtx starts a child of a.ctx for one turn. The previous turn's
// cancel (if any) is invoked first so a stale abort cannot leak.
func (a *Agent) deriveTurnCtx() context.Context {
	a.turnMu.Lock()
	defer a.turnMu.Unlock()
	if a.turnCancel != nil {
		a.turnCancel()
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.turnCancel = cancel
	return ctx
}

// clearTurnCtx drops the current turn cancel so a later AbortTurn is a no-op.
func (a *Agent) clearTurnCtx() {
	a.turnMu.Lock()
	defer a.turnMu.Unlock()
	if a.turnCancel != nil {
		a.turnCancel()
		a.turnCancel = nil
	}
}

// finishTurnReason closes an open step (if any) and the active turn with kind.
func (a *Agent) finishTurnReason(kind, message string) {
	a.stateMu.Lock()
	stepOpen := a.activeStep != 0
	turnOpen := a.activeTurn != 0
	a.stateMu.Unlock()
	if stepOpen {
		_, _ = a.EmitEvent(session.EventStepEnd, session.StepEndPayload{})
	}
	if turnOpen {
		_, _ = a.EmitEvent(session.EventTurnEnd, session.TurnEndPayload{
			Reason: session.TurnEndReason{Kind: kind, Message: message},
		})
	}
}

// HooksProvider is a live lookup for parsed hooks.json (typically *plugin.Registry).
type HooksProvider interface {
	Hooks() *plugin.Hooks
}

// dispatchHook runs the CC-style hooks runtime at one interception point. It is
// a no-op when the runtime is not wired (HookBus or live hooks nil). Each matching
// command hook first emits hook/invoked, then (after running) hook/result, so
// the durable log carries the paired lifecycle audit; a failure is isolated and
// never panics. Returns true when any hook decided to BLOCK (exit code 2), so a
// PreToolUse hook can actually gate the tool call.
func (a *Agent) dispatchHook(point, subject string, turn int) bool {
	hooks := a.Hooks
	if a.HooksProvider != nil {
		hooks = a.HooksProvider.Hooks()
	}
	if a.HookBus == nil || hooks == nil {
		return false
	}
	os := a.HookBus.DispatchHook(a.ctx, hooks, point, subject, turn)
	for _, o := range os {
		if o.Decision == "block" {
			return true
		}
	}
	return false
}

func (a *Agent) actorLoop() {
turns:
	for {
		select {
		case <-a.ctx.Done():
			return

		case userMsg := <-a.nextTurnChan:
			a.isRunning.Store(true)
			a.stepNumber.Store(0)
			turnCtx := a.deriveTurnCtx()

			// 1. turn/start — the transition machine assigns the turn number.
			// A persistence failure here leaves the machine advanced past a
			// start that never reached the log; converge the turn instead of
			// deriving user/message and steps against a ghost turn.
			if !a.emitBoundary(session.EventTurnStart, session.TurnStartPayload{}) {
				a.isRunning.Store(false)
				a.clearTurnCtx()
				continue turns
			}
			a.stateMu.Lock()
			turn := a.activeTurn
			a.stateMu.Unlock()

			// 2. user/message
			_, _ = a.EmitEvent(session.EventUserMessage, userMsg, &session.AppendSurfaceOp)
			// CC UserPromptSubmit hook: runs after the human message enters the log.
			// The turn number is already assigned by the transition machine.
			a.dispatchHook(plugin.HookPointUserPromptSubmit, "", turn)

			// Step loop
			turnFinished := false
			turnClosed := false
			// stickyMaxTokens latches a max-tokens finish seen in any step of
			// this turn: later steps still run normally, but the turn closes
			// as max-tokens instead of a misleading completed.
			stickyMaxTokens := false
			for !turnFinished {
				// next-step priority interruption: a queued user block aborts
				// the current step (closing it) and immediately starts a new
				// step driven by that prompt.
				select {
				case nextBlock := <-a.nextStepChan:
					// Boundary check: escalate only persistence failures — a
					// transition rejection here (interrupt drained between
					// steps, no open step) is tolerated as before.
					if !a.emitBoundary(session.EventStepEnd, session.StepEndPayload{}) {
						a.isRunning.Store(false)
						a.clearTurnCtx()
						continue turns
					}
					_, _ = a.EmitEvent(session.EventUserMessage, session.UserMessagePayload{
						ID:      fmt.Sprintf("step-%d", time.Now().UnixNano()),
						Role:    "user",
						Content: []session.ContentBlock{nextBlock},
						Source:  session.MessageSource{Kind: "user"},
					}, &session.AppendSurfaceOp)
					continue
				default:
				}
				select {
				case <-a.ctx.Done():
					a.finishTurnReason("aborted", "User aborted")
					a.isRunning.Store(false)
					a.clearTurnCtx()
					return
				case <-turnCtx.Done():
					a.finishTurnReason("aborted", "User aborted")
					a.isRunning.Store(false)
					a.clearTurnCtx()
					continue turns
				default:
				}

				_, _ = a.EmitEvent(session.EventStepStart, session.StepStartPayload{})
				a.stateMu.Lock()
				step := a.activeStep
				a.stateMu.Unlock()

				// Derive messages from the full session history: the durable
				// store is authoritative (all hosts persist), the ring is the
				// fallback for in-memory sessions (ACP), and the segment log
				// remains the legacy path. The store leg goes through the
				// incremental cache (one construction-time full load, then
				// fromSeq top-ups) instead of a full scan per step. Without
				// this, real sessions would send an empty history to the
				// model on every step.
				events := a.loadSessionLog()
				derivedMsgs, _ := session.DeriveMessages(events)

				// Tool declarations
				var toolDecls []llm.ToolDeclaration
				if a.Tools != nil {
					for _, t := range a.Tools.ListDeclarations() {
						toolDecls = append(toolDecls, llm.ToolDeclaration{
							Name:        t.Name,
							Description: t.Description,
							Parameters:  t.ParametersJSON,
						})
					}
				}

				// Effective system prompt: host base prompt + resolved
				// workspace instructions (community memory family). Empty
				// Instructions leaves SystemPrompt untouched.
				system := a.SystemPrompt
				if a.Instructions != "" {
					if system == "" {
						system = a.Instructions
					} else {
						system = system + "\n\n" + a.Instructions
					}
				}

				modelReq := llm.ModelRequest{
					Model:           a.ModelName,
					Messages:        derivedMsgs,
					System:          system,
					Tools:           toolDecls,
					ReasoningEffort: a.Effort, // "" => provider default
				}

				// Log-only request/header snapshot (upstream EpochHeader):
				// the latest snapshot reconstructs the request header.
				var schemaTools []session.ToolSchema
				for _, td := range toolDecls {
					schemaTools = append(schemaTools, session.ToolSchema{
						Name:        td.Name,
						Description: td.Description,
						Parameters:  td.Parameters,
					})
				}
				_, _ = a.EmitEvent(session.EventRequestHeader, session.RequestHeaderPayload{
					Header: session.EpochHeader{
						Config: session.LlmCallConfig{
							Provider: "deepseek-official",
							Model:    a.ModelName,
						},
						System: system,
						Tools:  schemaTools,
					},
					Reason: session.HeaderReasonInitial,
				})

				// Stream from LLM adapter, with provider-routed retry on
				// retryable transient failures (RATE_LIMIT/SERVER/TIMEOUT/
				// TRANSPORT). The default normal policy retries up to 5 times
				// with exponential backoff + jitter (upstream llm-retry).
				chunkChan, errChan := a.llmRetry(turnCtx, turn, step, modelReq, defaultRetryMax)
				assembler := llm.NewBlockAssembler()

				var pendingNextStep *session.ContentBlock
				streamDone := false
				interruptedByNextStep := false
				llmFailed := false
				actorStopped := false
				turnAborted := false
				// routeStepFatal routes one fatal stream error through the same
				// turn-closing path regardless of which select case delivered
				// it: context cancellation wins first (aborted beats failed),
				// then the step:end boundary, then turn:end carrying the
				// structured provider code. Outcome flags feed the post-stream
				// switch below.
				routeStepFatal := func(err error) {
					if a.ctx.Err() != nil {
						actorStopped = true
						return
					}
					if turnCtx.Err() != nil {
						turnAborted = true
						return
					}
					if !a.emitBoundary(session.EventStepEnd, session.StepEndPayload{Turn: turn, Step: step}) {
						llmFailed = true
						return
					}
					if !a.turnEndError(turn, err) {
						llmFailed = true
						return
					}
					llmFailed = true
				}
				for !streamDone {
					// Priority check: the next-step inbox wins over stream
					// chunks at every iteration, so an interrupt can never be
					// starved by a fast provider.
					select {
					case nextBlock := <-a.nextStepChan:
						blockCopy := nextBlock
						pendingNextStep = &blockCopy
						streamDone = true
						interruptedByNextStep = true
					default:
					}
					if streamDone {
						break
					}
					select {
					case <-a.ctx.Done():
						actorStopped = true
						streamDone = true
					case <-turnCtx.Done():
						if a.ctx.Err() != nil {
							actorStopped = true
						} else {
							turnAborted = true
						}
						streamDone = true
					case err, ok := <-errChan:
						if !ok {
							errChan = nil
							break
						}
						if err != nil {
							routeStepFatal(err)
							streamDone = true
						}
					case chunk, ok := <-chunkChan:
						if !ok {
							// A fatal buffered error must not be lost to
							// select randomness: llmRetry buffers outErr
							// before closing both channels (same shape as
							// llm/sse.go failStream), so closed-chunkChan
							// alone is not proof of clean EOF — draining
							// finds the buffered failure, closed-empty means
							// the stream really ended.
							select {
							case err, errOk := <-errChan:
								if errOk && err != nil {
									routeStepFatal(err)
								}
							default:
							}
							// Cancellation wins over close: on ctx.Done
							// llmRetry exits silently (both channels closed,
							// no error delivered), so a closed chunkChan races
							// the turnCtx.Done case above with no intent data
							// at this select — and the close would classify
							// the cancelled turn as a clean completion with
							// an empty assistant message. Before accepting
							// EOF, check the turn context (a child of a.ctx,
							// so AbortTurn and Stop both land here): when
							// cancelled, route through the same fatal path
							// (aborted beats failed) instead of the clean-EOF
							// fallthrough.
							if turnCtx.Err() != nil {
								routeStepFatal(errStreamClosedAfterCancel)
							}
							streamDone = true
							break
						}
						assembler.IngestChunk(chunk)
						_, _ = a.EmitEvent(session.EventAssistantChunk, map[string]any{
							"turn":  turn,
							"step":  step,
							"chunk": chunk,
						})
					}
				}

				if actorStopped {
					a.finishTurnReason("aborted", "User aborted")
					a.isRunning.Store(false)
					a.clearTurnCtx()
					return
				}
				if turnAborted {
					a.finishTurnReason("aborted", "User aborted")
					a.isRunning.Store(false)
					a.clearTurnCtx()
					continue turns
				}
				if llmFailed {
					a.isRunning.Store(false)
					a.clearTurnCtx()
					turnClosed = true
					break
				}

				// Assembled assistant message
				blocks, usage, finishReason := assembler.Result()
				if interruptedByNextStep {
					// Persist the truncated reply before closing the step so
					// the log carries a terminal assistant/message for the
					// abandoned chunk sequence instead of orphan chunks and a
					// lost token-usage record. An interruption during an
					// uncommitted (still-open) block leaves no committed
					// blocks; then nothing is emitted, as before.
					if len(blocks) > 0 {
						_, _ = a.EmitEvent(session.EventAssistantMessage, session.AssistantMessagePayload{
							Turn: turn,
							Step: step,
							Message: session.WireMessage{
								ID:      fmt.Sprintf("asst-%d-%d", turn, step),
								Role:    "assistant",
								Content: blocks,
								Source:  session.MessageSource{Kind: "model", Provider: a.ModelName, Model: a.ModelName},
							},
							Usage:       usage,
							Interrupted: true,
						}, &session.AppendSurfaceOp)
					}
					if !a.emitBoundary(session.EventStepEnd, session.StepEndPayload{Turn: turn, Step: step}) {
						a.isRunning.Store(false)
						a.clearTurnCtx()
						turnClosed = true
						break
					}
					if pendingNextStep != nil {
						_, _ = a.EmitEvent(session.EventUserMessage, session.UserMessagePayload{
							ID:      fmt.Sprintf("step-%d", time.Now().UnixNano()),
							Role:    "user",
							Content: []session.ContentBlock{*pendingNextStep},
							Source:  session.MessageSource{Kind: "user"},
						}, &session.AppendSurfaceOp)
					}
					continue
				}
				// Provider finish routing (upstream maps wire finish reasons
				// onto this vocabulary):
				//   max-tokens — sticky: this round still executes normally
				//     (assistant message, tools), but the turn ends as
				//     max-tokens instead of completed;
				//   error — the provider terminated abnormally (empty
				//     response, content filter): converge the turn now rather
				//     than reporting completed.
				if finishReason == "max-tokens" {
					stickyMaxTokens = true
				}
				if finishReason == "error" {
					if len(blocks) > 0 {
						_, _ = a.EmitEvent(session.EventAssistantMessage, session.AssistantMessagePayload{
							Turn: turn,
							Step: step,
							Message: session.WireMessage{
								ID:      fmt.Sprintf("asst-%d-%d", turn, step),
								Role:    "assistant",
								Content: blocks,
								Source:  session.MessageSource{Kind: "model", Provider: a.ModelName, Model: a.ModelName},
							},
							Usage: usage,
						}, &session.AppendSurfaceOp)
					}
					a.finishTurnReason("error", fmt.Sprintf("model finished step %d with reason %q", step, finishReason))
					a.isRunning.Store(false)
					a.clearTurnCtx()
					turnClosed = true
					break
				}
				asstPayload := session.AssistantMessagePayload{
					Turn: turn,
					Step: step,
					Message: session.WireMessage{
						ID:      fmt.Sprintf("asst-%d-%d", turn, step),
						Role:    "assistant",
						Content: blocks,
						Source:  session.MessageSource{Kind: "model", Provider: a.ModelName, Model: a.ModelName},
					},
					Usage: usage,
				}
				_, _ = a.EmitEvent(session.EventAssistantMessage, asstPayload, &session.AppendSurfaceOp)

				// Check for tool calls
				var toolCalls []session.ContentBlock
				for _, b := range blocks {
					if b.Type == "tool-call" {
						toolCalls = append(toolCalls, b)
					}
				}

				if len(toolCalls) > 0 && a.Tools != nil {
					// Execute each tool call
					for _, tc := range toolCalls {
						_, _ = a.EmitEvent(session.EventToolCall, session.ToolCallPayload{
							Turn:      turn,
							Step:      step,
							CallID:    tc.ID,
							Name:      tc.Name,
							Arguments: tc.Arguments,
							// Running-card rendering intent inferred from the tool
							// name + args so the client can draw a live
							// terminal/diff card while the call is in flight.
							View: buildToolCallView(tc.Name, tc.Arguments),
						})

						execCtx := tools.ToolExecutionContext{
							Context:     turnCtx,
							SessionID:   a.Header.ID,
							Cwd:         a.Header.Cwd,
							Turn:        turn,
							Step:        step,
							CallID:      tc.ID,
							RequestUser: a.RequestUser,
							// Tool-emitted domain events (todo/plan/goal
							// snapshots, approval audits, plan-mode flips)
							// land in the durable session log through the
							// agent machine.
							Emit: func(eventType string, payload any) {
								_, _ = a.EmitEvent(eventType, payload)
							},
						}

						// CC PreToolUse hook: runs with the tool name as subject
						// before the call executes. A hook that exits 2 (block)
						// now actually gates the tool: the call is skipped and a
						// tool-result error records the deny reason (the loop's
						// own permission policy still applies separately).
						resStr := ""
						isErr := false
						hookBlocked := a.dispatchHook(plugin.HookPointPreToolUse, tc.Name, turn)
						if hookBlocked {
							isErr = true
							resStr = "[blocked by PreToolUse hook]"
						} else {
							var pipeErr error
							resStr, isErr, pipeErr = a.Tools.ExecutePipeline(execCtx, tc.Name, tc.Arguments)
							_ = pipeErr
						}

						// Spill oversized plain-text results out of the model
						// context: results over the threshold are persisted to a
						// private session file and replaced with a head/tail
						// preview + locator + retrieval hint. `read`/terminal
						// tools skip the model-facing preview so a spill can
						// never loop back into read (upstream spill-policy
						// skips `read`). Best-effort: on any failure the inline
						// result is kept unchanged.
						resultText := resStr
						if !skipSpillFor(tc.Name) && !isErr {
							if preview, serr := tools.Save(a.Header.ID, tc.Name, resStr); serr == nil {
								resultText = preview
							}
						}

						view := buildToolResultView(tc.Name, resultText, isErr)
						_, _ = a.EmitEvent(session.EventToolResult, session.ToolResultPayload{
							Turn: turn,
							Step: step,
							Message: session.WireMessage{
								ID:   fmt.Sprintf("tool-%d-%d-%s", turn, step, tc.ID),
								Role: "user",
								Content: []session.ContentBlock{
									{Type: "tool-result", ToolCallID: tc.ID, Content: []session.ContentBlock{{Type: "text", Text: resultText}}, IsError: isErr},
								},
								Source: session.MessageSource{Kind: "tool", CallID: tc.ID},
							},
							View: view,
						}, &session.AppendSurfaceOp)

						// CC PostToolUse hook: runs after the tool result entered
						// the log, with the tool name as subject (best-effort).
						a.dispatchHook(plugin.HookPointPostToolUse, tc.Name, turn)

						if turnCtx.Err() != nil {
							if a.ctx.Err() != nil {
								a.finishTurnReason("aborted", "User aborted")
								a.isRunning.Store(false)
								a.clearTurnCtx()
								return
							}
							a.finishTurnReason("aborted", "User aborted")
							a.isRunning.Store(false)
							a.clearTurnCtx()
							turnClosed = true
							turnFinished = true
							break
						}
					}
					if turnClosed {
						break
					}
				} else {
					// No more tools to run; turn complete
					turnFinished = true
				}

				if !a.emitBoundary(session.EventStepEnd, session.StepEndPayload{Turn: turn, Step: step}) {
					a.isRunning.Store(false)
					a.clearTurnCtx()
					turnClosed = true
				}
			}

			if !turnClosed {
				// turn/end — the reason reflects a sticky max-tokens ceiling
				// hit in any step of this turn (merge-extensible kind).
				endKind := "completed"
				if stickyMaxTokens {
					endKind = "max-tokens"
				}
				turnEndOK := a.emitBoundary(session.EventTurnEnd, session.TurnEndPayload{
					Turn:   turn,
					Reason: session.TurnEndReason{Kind: endKind},
				})
				a.isRunning.Store(false)
				if turnEndOK {
					// One-shot automatic session title on the first completed turn.
					// Best-effort and never load-bearing: GenerateTitle already degrades
					// any LLM failure to the deterministic fallback (or "" when there is
					// no eligible user text), so a failed title never surfaces as an
					// error here. nil adapter / empty log both emit a no-op title event.
					if a.AutoTitle && a.titleDone.CompareAndSwap(false, true) {
						a.generateSessionTitle()
					}
					// Idle-session compaction pressure check (log-only brackets).
					a.maybeCompact()
				}
			}
			a.clearTurnCtx()
		}
	}
}

// generateSessionTitle emits a log-only `session/title` snapshot from the full
// session log (durable store, else ring, else segment log). It always succeeds
// from the caller's perspective; a nil adapter or empty log simply yields an
// empty-title no-op event, matching upstream's silent title service.
func (a *Agent) generateSessionTitle() {
	events := a.loadSessionLog()
	// Eligible human messages that are durable at this point.
	messages := CollectTitleMessages(events, -1)
	seqs := make([]int, 0, len(messages))
	for _, m := range messages {
		seqs = append(seqs, m.Seq)
	}
	title, _ := GenerateTitle(context.Background(), a.liveAdapter(), events)
	// Deterministic fallback / empty: single provider event, never a mock LLM
	// dispatch (generation is off the main path).
	_, _ = a.EmitEvent(session.EventSessionTitle, titleEventData{
		Title:       title,
		MessageSeqs: seqs,
		Source:      titleSource{Kind: "fallback"},
	})
}

// titleEventData is the log-only `session/title` snapshot payload (upstream
// SessionTitleEventData). It never enters the model surface or derived history.
type titleEventData struct {
	Title       string      `json:"title"`
	MessageSeqs []int       `json:"messageSeqs"`
	Source      titleSource `json:"source"`
}

// titleSource records how an accepted session title was produced (upstream
// SessionTitleSource). Only the deterministic fallback is wired here.
type titleSource struct {
	Kind string `json:"kind"`
}

// ---------------------------------------------------------------------------
// llm-retry (upstream CK/packages/llm/llm-retry)
// ---------------------------------------------------------------------------

// defaultRetryMax is the default maximum number of retries after the first
// attempt (upstream DEFAULT_MAX_RETRIES = 5).
const defaultRetryMax = 5

// defaultRetryInitialMs is the initial exponential-backoff delay (upstream
// DEFAULT_INITIAL_DELAY_MS = 500).
const defaultRetryInitialMs = 500

// defaultRetryMaxMs caps the exponential backoff (upstream
// DEFAULT_MAX_DELAY_MS = 10000).
const defaultRetryMaxMs = 10_000

// defaultRetryJitter is the symmetric jitter ratio (upstream
// DEFAULT_JITTER_RATIO = 0.1).
const defaultRetryJitter = 0.1

// retryableCodes are the provider-neutral failure codes eligible for a normal
// retry policy (upstream DEFAULT_RETRYABLE_CODES).
var retryableCodes = map[string]bool{
	"RATE_LIMIT": true,
	"SERVER":     true,
	"TIMEOUT":    true,
	"TRANSPORT":  true,
	// EMPTY_RESPONSE is admitted by policy for upstream parity, but no llm
	// adapter currently produces an empty-response ERROR SENTINEL: the
	// deepseek adapter maps stop-with-no-blocks onto a ChunkFinish reason
	// "error" on the chunk channel, which the loop's finish routing above
	// converges as an error turn — so this code stays unreachable until
	// pkg/llm grows the sentinel (classifyLlmError can then map it).
	"EMPTY_RESPONSE": true,
}

// isRetryableCode reports whether a failure code qualifies for a normal retry.
func isRetryableCode(code string) bool { return retryableCodes[code] }

// llmRetryError is the provider-neutral failure fact the agent loop routes on.
// It is a strict subset of the ProviderError / transport errors the
// deepseek adapter can produce.
type llmRetryError struct {
	message string
	code    string
	// providerRetryAfterMs is the upstream-requested delay in milliseconds;
	// <=0 means absent.
	providerRetryAfterMs int64
}

// llmRetry implements the provider-routed exponential-backoff retry policy on
// the agent loop's model request (upstream llm-retry). It runs a private retry
// loop in a goroutine that repeatedly calls Stream(); whenever a failed attempt
// carries a retryable code it schedules a cancellable backoff wait — appending
// an `llm/retry` event before the wait and an `llm/retry-started` event after it
// — before retrying. Retries are transparent to the caller: healthy chunks and
// the final finish stream through the returned chunk channel, and only the
// terminal outcome (success, an exhausted budget, or a non-retryable failure)
// is surfaced on the returned error channel. Context cancellation aborts a
// pending wait without appending retry-started.
//
// The initial delay is the upstream's providerRetryAfterMs when present and
// within the max cap, otherwise the local exponential backoff. Each scheduled
// retry appends llm/retry (upstream LlmRetryEventData) and llm/retry-started
// (LlmRetryStartedEventData); a terminal failure appends llm/retry with
// outcome "gave-up".
func (a *Agent) llmRetry(ctx context.Context, turn, step int, modelReq llm.ModelRequest, maxRetries int) (<-chan llm.StreamChunk, <-chan error) {
	initial := time.Duration(defaultRetryInitialMs) * time.Millisecond
	cap := time.Duration(defaultRetryMaxMs) * time.Millisecond
	jitter := defaultRetryJitter

	outChunk := make(chan llm.StreamChunk, 64)
	outErr := make(chan error, 1)

	go func() {
		defer close(outChunk)
		defer close(outErr)

		// attempt counts completed retries (0 before any).
		attempt := 0
		for {
			adapter := a.liveAdapter()
			if adapter == nil {
				outErr <- fmt.Errorf("llm adapter unavailable (llm-provider disabled or unloaded)")
				return
			}
			chunkChan, errChan := adapter.Stream(ctx, modelReq)

			// Forward chunks and watch for the terminal error or clean finish.
			// chunkChan is the authoritative stream (its close is the candidate
			// clean EOF); errChan only signals a fatal error when it delivers a
			// non-nil value — a closed or nil errChan alongside a healthy chunk
			// stream is normal adapter behavior and must not end the stream early.
			//
			// Closed chunkChan alone is NOT proof of clean EOF. Every adapter
			// close path (llm/sse.go failStream and startStream) buffers the
			// fatal error BEFORE closing the channels, so at close time a fatal
			// error is already buffered on errChan whenever one exists — and
			// Go's select picks pseudo-randomly between the two ready channels.
			// Letting closed-chunkChan win would classify fatal transport/auth
			// failures as clean successes and complete the turn with an
			// empty/partial assistant message (W8-c evidence: ~50% of real
			// failures surfaced as silent empty successes). The !ok branch
			// therefore drains errChan non-blockingly once before accepting the
			// close: the buffered error is found deterministically; closed-empty
			// or not-yet-ready means the stream truly ended. This keeps the
			// documented contract intact — errChan still never ends a healthy
			// stream early, and an error that arrives while chunkChan is open is
			// taken by the errChan case exactly as before.
			var attemptErr error
			streamDone := false
			for !streamDone {
				select {
				case <-ctx.Done():
					// Abort: cancellation wins over any pending retry.
					return
				case chunk, ok := <-chunkChan:
					if !ok {
						// Cancellation wins over close: when ctx is already
						// done this close is the silent-exit shape (both
						// channels closed, no error) and must not be classified
						// as a clean success — nor may a buffered adapter
						// failure from before the cancel schedule a retry.
						// Return without classifying; the step loop routes the
						// cancellation on its side.
						if ctx.Err() != nil {
							return
						}
						// A fatal buffered error must not be lost to select
						// randomness: failStream closes both channels with the
						// error already buffered, so closed-chunkChan alone is
						// not proof of clean EOF.
						select {
						case err, errOk := <-errChan:
							if errOk && err != nil {
								attemptErr = err
							}
						default:
						}
						streamDone = true
						continue
					}
					// Forward a healthy chunk (finish included) to the caller.
					select {
					case outChunk <- chunk:
					case <-ctx.Done():
						return
					}
				case err, ok := <-errChan:
					if ok && err != nil {
						attemptErr = err
						streamDone = true
					}
					// ok=false (closed) or nil err: chunkChan still drives.
				}
			}
			if attemptErr == nil {
				// Clean success: the full stream (finish included) was forwarded.
				return
			}
			failure := classifyLlmError(attemptErr)
			if failure == nil || !isRetryableCode(failure.code) {
				// Non-retryable terminal failure.
				_, _ = a.EmitEvent(session.EventLlmRetry, map[string]any{
					"turn": turn, "step": step, "retry": attempt + 1, "maxRetries": maxRetries,
					"code": failureCode(failure), "message": attemptErr.Error(), "outcome": "gave-up",
				})
				outErr <- attemptErr
				return
			}
			if attempt >= maxRetries {
				// Budget exhausted: surface the last retryable error.
				_, _ = a.EmitEvent(session.EventLlmRetry, map[string]any{
					"turn": turn, "step": step, "retry": attempt, "maxRetries": maxRetries,
					"code": failure.code, "message": "retry budget exhausted", "outcome": "gave-up",
				})
				outErr <- attemptErr
				return
			}
			// Retryable with budget remaining. Respect the provider's retry-after
			// when present and within the cap, else exponential backoff + jitter.
			var delay time.Duration
			if failure.providerRetryAfterMs > 0 {
				p := time.Duration(failure.providerRetryAfterMs) * time.Millisecond
				if p > cap {
					delay = cap
				} else {
					delay = p
				}
			} else {
				delay = backoffDelay(initial, cap, jitter, attempt+1)
			}
			// Durable audit of the scheduled retry (upstream llm/retry).
			_, _ = a.EmitEvent(session.EventLlmRetry, map[string]any{
				"turn": turn, "step": step, "retry": attempt + 1, "maxRetries": maxRetries,
				"delayMs": int64(delay / time.Millisecond), "code": failure.code, "message": failure.message,
				"outcome": "retried",
			})
			if !waitAbortable(ctx, delay) {
				// Aborted while waiting: no retry-started; surface cancellation.
				return
			}
			_, _ = a.EmitEvent(session.EventLlmRetryStarted, map[string]any{
				"turn": turn, "step": step, "retry": attempt + 1,
			})
			attempt++
		}
	}()

	return outChunk, outErr
}

// failureCode returns the classifier's code, or a fallback when it is nil.
func failureCode(f *llmRetryError) string {
	if f == nil {
		return "UNKNOWN"
	}
	return f.code
}

// classifyLlmError maps an adapter error onto a provider-neutral retryable
// classification, or nil when it is not a retryable failure. It understands the
// provider adapter's typed ProviderError (code field) and the transport
// sentinels (ErrDeepSeekStream/Watchdog).
func classifyLlmError(err error) *llmRetryError {
	if err == nil {
		return nil
	}
	var dpe *llm.ProviderError
	if errors.As(err, &dpe) {
		switch dpe.Code {
		case "RATE_LIMIT", "SERVER", "TIMEOUT", "TRANSPORT":
			return &llmRetryError{
				message:              dpe.Message,
				code:                 dpe.Code,
				providerRetryAfterMs: int64(dpe.ProviderRetryAfter / time.Millisecond),
			}
		}
		return nil
	}
	// Unwrapped transport failures: the deepseek adapter surfaces raw net errors
	// (DNS, connection refused, reset) and its watchdog/stream sentinels.
	if errors.Is(err, llm.ErrDeepSeekWatchdog) || errors.Is(err, llm.ErrDeepSeekStream) {
		return &llmRetryError{message: err.Error(), code: "TRANSPORT"}
	}
	return nil
}

// classifyProviderCode extracts the structured provider failure code for an
// LLM error so the host (GUI modal vs inline hint) can react without re-parsing
// the message string. AUTH → "AUTH"; an INVALID_REQUEST with a context-window
// signature → "CONTEXT_WINDOW_EXCEEDED"; otherwise the raw provider code or
// "UNKNOWN". The zero value ("") means "no structured code" and is treated as a
// non-blocking inline failure by the host.
func classifyProviderCode(err error) string {
	var dpe *llm.ProviderError
	if errors.As(err, &dpe) && dpe != nil {
		if dpe.Code != "" {
			return dpe.Code
		}
	}
	return "UNKNOWN"
}

// turnEndError emits a turn/end error reason carrying the structured provider
// code, so the host can route AUTH (401/403) to a modal and param/config
// failures (400/INVALID_REQUEST) to an inline, non-blocking hint.
func (a *Agent) turnEndError(turn int, err error) bool {
	code := classifyProviderCode(err)
	if code == "" {
		code = "UNKNOWN"
	}
	return a.emitBoundary(session.EventTurnEnd, session.TurnEndPayload{
		Turn: turn,
		Reason: session.TurnEndReason{
			Kind:    "error",
			Message: err.Error(),
			Code:    code,
		},
	})
}

// backoffDelay computes the jittered exponential backoff for retry attempt n
// (1-based): min(initial * 2^(n-1), max) scaled by a symmetric random factor
// in [1-jitter, 1+jitter], never above max (upstream localDelay).
func backoffDelay(initial, max time.Duration, jitter float64, retry int) time.Duration {
	if retry < 1 {
		retry = 1
	}
	exp := retry - 1
	if exp > 10 {
		exp = 10 // bounded to keep the doubling from overflowing duration
	}
	base := initial
	for i := 0; i < exp; i++ {
		base *= 2
	}
	if base > max {
		base = max
	}
	factor := 1 - jitter + 2*jitter*rand.Float64()
	d := time.Duration(float64(base) * factor)
	if d > max {
		d = max
	}
	return d
}

// waitAbortable sleeps delay unless ctx is canceled; returns false when aborted.
func waitAbortable(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// buildToolResultView derives the rendering intent (real card view) for a tool
// result. Best-effort: the text diff is parsed into hunks, shell/terminal tools
// become an ANSI-friendly terminal card, everything else falls back to text.
func buildToolResultView(name, out string, isErr bool) *session.ToolResultView {
	if out == "" {
		return &session.ToolResultView{Kind: "text", Text: ""}
	}
	if looksLikeDiff(out) {
		return &session.ToolResultView{Kind: "diff", Diffs: parseDiff(out)}
	}
	if isCommandTool(name) {
		return &session.ToolResultView{
			Kind:     "terminal",
			Terminal: &session.TerminalView{Lines: capLines(splitLines(out), 240), ExitCode: boolToExit(isErr)},
		}
	}
	return &session.ToolResultView{Kind: "text", Text: capText(out, 32*1024)}
}

// buildToolCallView derives the running-card rendering intent for an in-flight
// tool call, inferred from the tool name + raw arguments. It mirrors the kind
// decision of buildToolResultView but carries no content yet: the client draws
// a live skeleton (terminal/diff/text) and replaces it with the settled card
// when the matching tool/result event arrives.
func buildToolCallView(name, argsJSON string) *session.ToolResultView {
	switch {
	case isCommandTool(name):
		return &session.ToolResultView{Kind: "terminal", Terminal: &session.TerminalView{Lines: nil, ExitCode: 0}}
	case looksLikeDiffCall(name, argsJSON):
		return &session.ToolResultView{Kind: "diff"}
	default:
		return &session.ToolResultView{Kind: "text", Text: ""}
	}
}

// looksLikeDiffCall reports whether a tool call is expected to render as a
// unified-diff card (edit-family tools acting on text). Best-effort from the
// tool name; the result parse remains authoritative.
func looksLikeDiffCall(name, argsJSON string) bool {
	switch name {
	case "write_file", "replace_file_content":
		var args struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return false
		}
		return args.Content != ""
	}
	return false
}

// looksLikeDiff reports whether a tool output is a unified diff (its own file
// headers and/or `@@` hunk markers).
func looksLikeDiff(out string) bool {
	hasDiffHeader := false
	hasHunk := false
	for _, line := range splitLines(out) {
		if strings.HasPrefix(line, "diff --git ") {
			hasDiffHeader = true
			continue
		}
		if strings.HasPrefix(line, "@@ ") {
			hasHunk = true
		}
	}
	return hasHunk || (hasDiffHeader && strings.Contains(out, "\n--- "))
}

// isCommandTool reports whether a tool name maps to a command/terminal card.
func isCommandTool(name string) bool {
	switch name {
	case "run_command", "bash_persistent", "bash_reset", "terminal_open",
		"terminal_send", "terminal_read", "terminal_list", "terminal_signal",
		"terminal_close", "job_output", "job_list", "job_kill":
		return true
	}
	return false
}

// skipSpillFor reports whether a tool's final result must NOT be spilled into a
// preview. `read`/terminal-family tools are the tools that consume a spilled
// file's locator and the ones that produce the largest outputs; spilling their
// results would both drop the exact bytes a later read would fetch and risk a
// read → spill → read loop (upstream spill-policy skips `read` on the
// model-facing arm).
func skipSpillFor(name string) bool {
	switch name {
	case "read_file", "read_image", "read", "terminal_open", "terminal_send",
		"terminal_read", "terminal_list", "terminal_signal", "terminal_close",
		"job_output":
		return true
	}
	return false
}

// splitLines splits a string on '\n', dropping a trailing empty final line.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// capLines keeps a head+tail of lines so a huge shell dump cannot freeze the
// GUI/TUI renderer. The model-facing tool-result text is unchanged.
func capLines(lines []string, max int) []string {
	if max <= 0 || len(lines) <= max {
		return lines
	}
	head := max / 2
	if head < 1 {
		head = 1
	}
	tail := max - head
	out := make([]string, 0, max+1)
	out = append(out, lines[:head]...)
	out = append(out, fmt.Sprintf("… %d lines omitted …", len(lines)-max))
	out = append(out, lines[len(lines)-tail:]...)
	return out
}

// capText keeps a head+tail of a view payload. Rune-counted so CJK is not
// sliced mid-character.
func capText(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	head := max / 2
	return string(r[:head]) + "\n…\n" + string(r[len(r)-head:])
}

func boolToExit(isErr bool) int {
	if isErr {
		return 1
	}
	return 0
}

// parseDiff splits a unified diff stream into per-file hunks (best-effort).
// It tracks the current file header (`diff --git` / `---`/`+++`), then walks
// `-`/`+`/context lines splitting old (removed) vs new (added) text, resetting
// old/new buffers at each `@@` hunk boundary.
func parseDiff(out string) []session.DiffHunk {
	var hunks []session.DiffHunk
	current := session.DiffHunk{}
	var oldLines, newLines []string

	flush := func() {
		if current.Path != "" && len(oldLines)+len(newLines) > 0 {
			current.Old = strings.Join(oldLines, "\n")
			current.New = strings.Join(newLines, "\n")
			hunks = append(hunks, current)
		}
		current = session.DiffHunk{}
		oldLines = nil
		newLines = nil
	}

	for _, line := range splitLines(out) {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			// New file section — reset buffers and try to pull the b/ path.
			flush()
			if idx := strings.Index(line, " b/"); idx > 0 {
				current.Path = strings.TrimSpace(line[idx+3:])
			}
		case strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ "):
			// Path fallback when no diff --git header was present.
			if strings.HasPrefix(line, "+++ ") {
				p := strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
				p = strings.TrimPrefix(p, "b/")
				if current.Path == "" {
					current.Path = p
				}
			}
			if strings.HasPrefix(line, "--- ") {
				p := strings.TrimSpace(strings.TrimPrefix(line, "--- "))
				if strings.HasPrefix(p, "a/") {
					p = p[2:]
				}
				if current.Path == "" {
					current.Path = p
				}
			}
		case strings.HasPrefix(line, "@@ "):
			// Hunk boundary: commit the accumulated old/new for this hunk.
			if current.Path != "" && len(oldLines)+len(newLines) > 0 {
				h := session.DiffHunk{
					Path: current.Path,
					Old:  strings.Join(oldLines, "\n"),
					New:  strings.Join(newLines, "\n"),
				}
				hunks = append(hunks, h)
				oldLines = nil
				newLines = nil
			}
		case strings.HasPrefix(line, "+"):
			if len(line) > 1 {
				newLines = append(newLines, line[1:])
			} else {
				newLines = append(newLines, "")
			}
		case strings.HasPrefix(line, "-"):
			if len(line) > 1 {
				oldLines = append(oldLines, line[1:])
			} else {
				oldLines = append(oldLines, "")
			}
		case strings.HasPrefix(line, "\\"):
			// "\ No newline at end of file" marker — ignore.
		default:
			// Context lines appear in both old and new.
			oldLines = append(oldLines, line)
			newLines = append(newLines, line)
		}
	}
	flush()
	return hunks
}
