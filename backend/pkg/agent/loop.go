package agent

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"dsh-go/pkg/compaction"
	"dsh-go/pkg/llm"
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

// seedSeqFrom resumes the agent sequence counter at the stored next seq so a
// re-opened session keeps appending contiguously (appendBatch contract).
func seedSeqFrom(store PersistSink, id string) int64 {
	if store == nil {
		return 0
	}
	events, err := store.GetEvents(id, 0)
	if err != nil || len(events) == 0 {
		return 0
	}
	return int64(events[len(events)-1].Seq) + 1
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
	Schedule   *tools.ScheduleDispatcher
	ctx        context.Context
	cancelFunc context.CancelFunc

	seqCounter atomic.Int64
	turnNumber atomic.Int32
	stepNumber atomic.Int32
	isRunning  atomic.Bool

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
	a.seqCounter.Store(seedSeqFrom(store, header.ID))
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
	// Snapshot the machine's current numbers BEFORE the transition mutates
	// them: a step/end payload must carry the step it closes, and turn/end
	// the turn it ends.
	a.stateMu.Lock()
	preTurn, preStep := a.activeTurn, a.activeStep
	a.stateMu.Unlock()
	if err := a.validateTransition(eventType); err != nil {
		return nil, err
	}
	// Boundary events carry the state machine's assigned numbers; the caller
	// passes zero-valued placeholders so the machine stays the single source
	// of turn/step identity.
	switch eventType {
	case session.EventTurnStart:
		payload = session.TurnStartPayload{Turn: preTurn}
	case session.EventStepStart:
		payload = session.StepStartPayload{Turn: preTurn, Step: preStep}
	case session.EventStepEnd:
		payload = session.StepEndPayload{Turn: preTurn, Step: preStep}
	case session.EventTurnEnd:
		if tp, ok := payload.(session.TurnEndPayload); ok {
			tp.Turn = preTurn
			payload = tp
		}
	}
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

	return env, nil
}

// validateTransition enforces the turn/step lifecycle contract before an
// event is admitted to the log. Boundary events mutate the machine only
// after admission succeeds; a rejected event leaves the state untouched.
func (a *Agent) validateTransition(eventType string) error {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	switch eventType {
	case session.EventTurnStart:
		if a.activeTurn != 0 {
			return fmt.Errorf("invalid transition: turn/start while turn %d is active", a.activeTurn)
		}
		a.activeTurn = int(a.turnNumber.Add(1))
		a.activeStep = 0
	case session.EventTurnEnd:
		if a.activeTurn == 0 {
			return fmt.Errorf("invalid transition: turn/end with no active turn")
		}
		if a.activeStep != 0 {
			return fmt.Errorf("invalid transition: turn/end while step %d is open", a.activeStep)
		}
		a.activeTurn = 0
	case session.EventStepStart:
		if a.activeTurn == 0 {
			return fmt.Errorf("invalid transition: step/start with no active turn")
		}
		if a.activeStep != 0 {
			return fmt.Errorf("invalid transition: step/start while step %d is open", a.activeStep)
		}
		a.activeStep = int(a.stepNumber.Add(1))
	case session.EventStepEnd:
		if a.activeStep == 0 {
			return fmt.Errorf("invalid transition: step/end with no active step")
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

// maybeCompact runs the idle-session compaction pressure check after a
// completed turn: it selects a head-anchored balanced range and runs the
// durable compaction transaction through emitRaw (brackets are log-only).
func (a *Agent) maybeCompact() {
	if a.Compactor == nil {
		return
	}
	var log []session.SessionEnvelope
	var err error
	if a.persist != nil {
		log, err = a.persist.GetEvents(a.Header.ID, 0)
	} else if a.SegmentLog != nil {
		_, log, _ = a.SegmentLog.ReadAll()
	}
	if err != nil || len(log) == 0 {
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
	go a.actorLoop()
}

// Stop cancels ongoing execution.
func (a *Agent) Stop() {
	a.cancelFunc()
	if a.Schedule != nil {
		a.Schedule.Stop()
	}
	tools.UnregisterScheduleEvents(a.Header.ID)
}

func (a *Agent) actorLoop() {
	for {
		select {
		case <-a.ctx.Done():
			return

		case userMsg := <-a.nextTurnChan:
			a.isRunning.Store(true)
			a.stepNumber.Store(0)

			// 1. turn/start 閳?the transition machine assigns the turn number
			_, _ = a.EmitEvent(session.EventTurnStart, session.TurnStartPayload{})
			a.stateMu.Lock()
			turn := a.activeTurn
			a.stateMu.Unlock()

			// 2. user/message
			_, _ = a.EmitEvent(session.EventUserMessage, userMsg, &session.AppendSurfaceOp)

			// Step loop
			turnFinished := false
			for !turnFinished {
				// next-step priority interruption: a queued user block aborts
				// the current step (closing it) and immediately starts a new
				// step driven by that prompt.
				select {
				case nextBlock := <-a.nextStepChan:
					_, _ = a.EmitEvent(session.EventStepEnd, session.StepEndPayload{})
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
					// Close the open step before ending the turn so the
					// lifecycle machine accepts the transition.
					_, _ = a.EmitEvent(session.EventStepEnd, session.StepEndPayload{})
					_, _ = a.EmitEvent(session.EventTurnEnd, session.TurnEndPayload{
						Turn:   turn,
						Reason: session.TurnEndReason{Kind: "aborted", Message: "User aborted"},
					})
					a.isRunning.Store(false)
					return
				default:
				}

				_, _ = a.EmitEvent(session.EventStepStart, session.StepStartPayload{})
				a.stateMu.Lock()
				step := a.activeStep
				a.stateMu.Unlock()

				// Derive messages from the full session history: the durable
				// store is authoritative (all hosts persist), the ring is the
				// fallback for in-memory sessions (ACP), and the segment log
				// remains the legacy path. Without this, real sessions would
				// send an empty history to the model on every step.
				var events []session.SessionEnvelope
				if a.persist != nil {
					if evs, err := a.persist.GetEvents(a.Header.ID, 0); err == nil && len(evs) > 0 {
						events = evs
					}
				}
				if len(events) == 0 && a.RingBuf != nil {
					if ptrs := a.RingBuf.GetSince(0); len(ptrs) > 0 {
						events = make([]session.SessionEnvelope, len(ptrs))
						for i, p := range ptrs {
							events[i] = *p
						}
					}
				}
				if len(events) == 0 && a.SegmentLog != nil {
					_, events, _ = a.SegmentLog.ReadAll()
				}
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

				modelReq := llm.ModelRequest{
					Model:    a.ModelName,
					Messages: derivedMsgs,
					System:   a.SystemPrompt,
					Tools:    toolDecls,
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
						System: a.SystemPrompt,
						Tools:  schemaTools,
					},
					Reason: session.HeaderReasonInitial,
				})

				// Stream from LLM adapter
				chunkChan, errChan := a.LlmAdapter.Stream(a.ctx, modelReq)
				assembler := llm.NewBlockAssembler()

				var pendingNextStep *session.ContentBlock
				streamDone := false
				interruptedByNextStep := false
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
						streamDone = true
					case err, ok := <-errChan:
						if ok && err != nil {
							_, _ = a.EmitEvent(session.EventStepEnd, session.StepEndPayload{Turn: turn, Step: step})
							_, _ = a.EmitEvent(session.EventTurnEnd, session.TurnEndPayload{
								Turn:   turn,
								Reason: session.TurnEndReason{Kind: "error", Message: err.Error()},
							})
							a.isRunning.Store(false)
							return
						}
					case chunk, ok := <-chunkChan:
						if !ok {
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

				// Assembled assistant message
				blocks, usage, _ := assembler.Result()
				if interruptedByNextStep {
					_, _ = a.EmitEvent(session.EventStepEnd, session.StepEndPayload{Turn: turn, Step: step})
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
						})

						execCtx := tools.ToolExecutionContext{
							Context:     a.ctx,
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

						resStr, isErr, _ := a.Tools.ExecutePipeline(execCtx, tc.Name, tc.Arguments)

						_, _ = a.EmitEvent(session.EventToolResult, session.ToolResultPayload{
							Turn: turn,
							Step: step,
							Message: session.WireMessage{
								ID:   fmt.Sprintf("tool-%d-%d-%s", turn, step, tc.ID),
								Role: "user",
								Content: []session.ContentBlock{
									{Type: "tool-result", ToolCallID: tc.ID, Content: []session.ContentBlock{{Type: "text", Text: resStr}}, IsError: isErr},
								},
								Source: session.MessageSource{Kind: "tool", CallID: tc.ID},
							},
						}, &session.AppendSurfaceOp)
					}
				} else {
					// No more tools to run; turn complete
					turnFinished = true
				}

				_, _ = a.EmitEvent(session.EventStepEnd, session.StepEndPayload{Turn: turn, Step: step})
			}

			// turn/end
			_, _ = a.EmitEvent(session.EventTurnEnd, session.TurnEndPayload{
				Turn:   turn,
				Reason: session.TurnEndReason{Kind: "completed"},
			})
			a.isRunning.Store(false)
			// Idle-session compaction pressure check (log-only brackets).
			a.maybeCompact()
		}
	}
}
