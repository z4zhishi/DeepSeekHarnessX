package compaction

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"dsh-go/pkg/llm"
	"dsh-go/pkg/session"
)

// The compaction transaction, ported from
// `CK/packages/compaction/compaction-basic/src/region.ts` and
// `CK/packages/compaction/compaction/src/{checkpoint.ts,types.ts}`.
//
// Durable lifecycle: compaction/start (lock, log-only) -> compaction/summary
// (log-only, carries the shadow price) -> the immediately following
// user/message with `surfaceOp:{op:"replace"}` + `sourceEventSeqs` ->
// compaction/end (releases the lock; carries `error` on failure). A failed
// close deliberately leaves the unmatched start detectable, so the durable
// lock stays active for inspection.

// ManualCompactionErrorCode mirrors upstream `ManualCompactionErrorCode`.
type ManualCompactionErrorCode string

const (
	ManualErrBusy        ManualCompactionErrorCode = "busy"
	ManualErrCancelled   ManualCompactionErrorCode = "cancelled"
	ManualErrChanged     ManualCompactionErrorCode = "changed"
	ManualErrSummary     ManualCompactionErrorCode = "summary"
	ManualErrCommit      ManualCompactionErrorCode = "commit"
	ManualErrPersistence ManualCompactionErrorCode = "persistence"
)

// ManualCompactionError is a classified compaction failure.
type ManualCompactionError struct {
	Code    ManualCompactionErrorCode
	Message string
	Cause   error
}

func (e *ManualCompactionError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

func (e *ManualCompactionError) Unwrap() error { return e.Cause }

// SurfaceChangedError rejects a summary whose replacement boundaries are no
// longer the ones it was built from (upstream `SurfaceChangedError`).
type SurfaceChangedError struct{ message string }

func (e *SurfaceChangedError) Error() string { return e.message }

// SurfaceSpan is the inclusive surface-position pair shadowed by a replace.
type SurfaceSpan struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// CompactionResult mirrors upstream `CompactionResult`.
type CompactionResult struct {
	CompactionId       string
	SourceCommandId    string
	StartSeq           int
	SummarySeq         int
	EndSeq             int
	Summary            []session.ContentBlock
	ShadowedRange      SurfaceSpan
	ShadowedSeqs       []int
	ShadowedTokenCount int
}

// CompactionStartPayload mirrors the durable `compaction/start` data.
type CompactionStartPayload struct {
	CompactionId    string `json:"compactionId"`
	SourceCommandId string `json:"sourceCommandId,omitempty"`
	Turn            *int   `json:"turn"`
}

// CompactionSummaryPayload is the durable `compaction/summary` data.
type CompactionSummaryPayload struct {
	CompactionId       string                 `json:"compactionId"`
	SourceCommandId    string                 `json:"sourceCommandId,omitempty"`
	Summary            []session.ContentBlock `json:"summary"`
	ShadowedRange      SurfaceSpan            `json:"shadowedRange"`
	ShadowedSeqs       []int                  `json:"shadowedSeqs"`
	ShadowedTokenCount int                    `json:"shadowedTokenCount"`
	Provider           string                 `json:"provider"`
	Model              string                 `json:"model"`
	MaxTokens          int                    `json:"maxTokens,omitempty"`
	Usage              *session.TokenUsage    `json:"usage,omitempty"`
	LLMStreamCall      bool                   `json:"llmStreamCall,omitempty"`
	RawOutput          []session.ContentBlock `json:"rawOutput,omitempty"`
}

// CompactionEndPayload mirrors the durable `compaction/end` data.
type CompactionEndPayload struct {
	CompactionId    string `json:"compactionId"`
	SourceCommandId string `json:"sourceCommandId,omitempty"`
	Turn            *int   `json:"turn"`
	Error           string `json:"error,omitempty"`
}

// CompactCheckpointSource is the correlated provenance carried by the
// replacement user message (upstream `compactCheckpointSource`).
type CompactCheckpointSource struct {
	Kind            string `json:"kind"`
	Plugin          string `json:"plugin"`
	CompactionId    string `json:"compactionId"`
	SourceCommandId string `json:"sourceCommandId,omitempty"`
}

// IsCompactCheckpointSource reports whether a message source identifies a
// compaction checkpoint (upstream predicate).
func IsCompactCheckpointSource(src session.MessageSource) bool {
	return src.Kind == "plugin" && src.Plugin == "compact"
}

// Appender is the durable append surface a compaction transaction writes to.
// The appender assigns the event's Seq (0 placeholder) and persists it.
type Appender interface {
	AppendEvents(meta *session.SessionHeader, events []*session.SessionEnvelope) error
}

// TokenMeter estimates per-node token prices for a session event stream.
type TokenMeter interface {
	MeasureNodes(events []session.SessionEnvelope, nodes []int) ([]int, error)
}

// SummarizationInput is the replayed conversation prefix the summarizer condenses.
type SummarizationInput struct {
	System   string                 `json:"system,omitempty"`
	Tools    []llm.ToolDeclaration  `json:"tools,omitempty"`
	Messages []session.ModelMessage `json:"messages"`
}

// Summarizer turns one replayed region into the compact checkpoint summary.
type Summarizer interface {
	Summarize(ctx context.Context, input SummarizationInput) ([]session.ContentBlock, *session.TokenUsage, error)
}

// TemplateSummarizer frames the standard checkpoint using a fixed text
// summary without a model call (tests and offline runs).
type TemplateSummarizer struct{}

func (TemplateSummarizer) Summarize(_ context.Context, input SummarizationInput) ([]session.ContentBlock, *session.TokenUsage, error) {
	var lines []string
	lines = append(lines, "## Primary Request and Intent", "- "+firstUserText(input.Messages))
	lines = append(lines, "", "## Key Technical Concepts", "- (none)")
	lines = append(lines, "", "## Files and Code", "- (none)")
	lines = append(lines, "", "## Errors and Fixes", "- (none)")
	lines = append(lines, "", "## Pending Jobs", "- (none)")
	lines = append(lines, "", "## Current Work", "- (none)")
	lines = append(lines, "", "## Next Step", "- (none)")
	lines = append(lines, "", "## Critical Context", "- (none)")
	return []session.ContentBlock{{Type: "text", Text: strings.Join(lines, "\n")}}, nil, nil
}

func firstUserText(messages []session.ModelMessage) string {
	for _, m := range messages {
		if m.Role != "user" {
			continue
		}
		for _, b := range m.Content {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				return strings.TrimSpace(b.Text)
			}
		}
	}
	return "(none)"
}

// CompactEngine holds the configuration and services for compaction.
type CompactEngine struct {
	RetainTokens int // minimum recent tail budget retained verbatim
	Meter        TokenMeter
	Summarizer   Summarizer
	Provider     string
	Model        string
	MaxTokens    int
}

// NewCompactEngine builds the engine with defaults (TemplateSummarizer when
// no summarizer is provided).
func NewCompactEngine(retainTokens int, meter TokenMeter, summarizer Summarizer, provider, model string) *CompactEngine {
	if summarizer == nil {
		summarizer = TemplateSummarizer{}
	}
	return &CompactEngine{
		RetainTokens: retainTokens,
		Meter:        meter,
		Summarizer:   summarizer,
		Provider:     provider,
		Model:        model,
	}
}

// Measure returns per-node prices for a surface (used by Caller and tests).
func (e *CompactEngine) Measure(events []session.SessionEnvelope, nodes []int) ([]int, error) {
	if e.Meter != nil {
		return e.Meter.MeasureNodes(events, nodes)
	}
	priced := make([]int, len(nodes))
	for i := range priced {
		priced[i] = 1
	}
	return priced, nil
}

// SelectCompactableRange resolves the next head-anchored range while retaining
// a priced recent tail and never splitting an assistant tool-call/result
// pair. Returns nil when no safe range can be compacted.
func (e *CompactEngine) SelectCompactableRange(events []session.SessionEnvelope, nodes []int) (*SurfaceSpan, error) {
	if len(nodes) == 0 {
		return nil, nil
	}
	priced, err := e.Measure(events, nodes)
	if err != nil {
		return nil, err
	}
	if len(priced) != len(nodes) {
		return nil, fmt.Errorf("compaction: token-meter surface does not match the current session surface")
	}

	accumulated := 0
	keepFromIdx := len(nodes)
	for index := len(nodes) - 1; index >= 0; index-- {
		accumulated += priced[index]
		keepFromIdx = index
		if accumulated >= e.RetainTokens {
			break
		}
	}
	if keepFromIdx == 0 {
		return nil, nil
	}

	for keepFromIdx > 0 {
		balanced, err := ToolPairingBalancedBefore(events, nodes, keepFromIdx)
		if err != nil {
			return nil, err
		}
		if balanced {
			break
		}
		keepFromIdx--
	}
	if keepFromIdx == 0 {
		return nil, nil
	}
	return &SurfaceSpan{Start: nodes[0], End: nodes[keepFromIdx-1]}, nil
}

// SurfaceSelection is one validated inclusive span of current surface positions.
type SurfaceSelection struct {
	Span         SurfaceSpan
	StartIdx     int
	EndIdx       int
	ShadowedSeqs []int
}

// ValidateSurfaceRegion validates one requested surface-position span before
// asynchronous work begins (exported for lock/policy checks).
func (e *CompactEngine) ValidateSurfaceRegion(events []session.SessionEnvelope, nodes []int, start, end int) (*SurfaceSelection, error) {
	startIdx := -1
	for i, n := range nodes {
		if n == start {
			startIdx = i
			break
		}
	}
	if startIdx == -1 {
		return nil, fmt.Errorf("compactRegion: start seq %d not found in surface", start)
	}
	endIdx := -1
	for i, n := range nodes {
		if n == end {
			endIdx = i
			break
		}
	}
	if endIdx == -1 {
		return nil, fmt.Errorf("compactRegion: end seq %d not found in surface", end)
	}
	if startIdx > endIdx {
		return nil, fmt.Errorf("compactRegion: start seq %d (position %d) is after end seq %d (position %d)", start, startIdx, end, endIdx)
	}
	before, err := ToolPairingBalancedBefore(events, nodes, startIdx)
	if err != nil {
		return nil, err
	}
	if !before {
		return nil, fmt.Errorf("compactRegion: start seq %d is not a balanced boundary (would split a step's tool-call/result pair)", start)
	}
	after, err := ToolPairingBalancedAfter(events, nodes, endIdx)
	if err != nil {
		return nil, err
	}
	if !after {
		return nil, fmt.Errorf("compactRegion: end seq %d is not a balanced boundary (would split a step, or the step is still open)", end)
	}
	return &SurfaceSelection{
		Span:         SurfaceSpan{Start: start, End: end},
		StartIdx:     startIdx,
		EndIdx:       endIdx,
		ShadowedSeqs: append([]int(nil), nodes[startIdx:endIdx+1]...),
	}, nil
}

// CompactTransactionOptions carries the transaction bracket options.
type CompactTransactionOptions struct {
	// Owner is the numbered turn that encloses an automatic compaction; nil
	// writes a standalone bracket (manual, idle).
	Owner *int
	// Manual marks a direct human-command compaction (cancel-sensitive).
	Manual bool
	// SourceCommandId records the initiating manual command when present.
	SourceCommandId string
	// Start/End select the surface-position span to compact.
	Start int
	End   int
}

// CompactTransaction runs the single durable compaction transaction over the
// selected positional span, mirroring upstream `compactSurfaceRegion`.
func (e *CompactEngine) CompactTransaction(
	ctx context.Context,
	header *session.SessionHeader,
	events []session.SessionEnvelope,
	nodes []int,
	appendFn func(env *session.SessionEnvelope) error,
	options CompactTransactionOptions,
) (*CompactionResult, error) {
	if options.Manual && ctx.Err() != nil {
		return nil, &ManualCompactionError{Code: ManualErrCancelled, Message: "compaction cancelled before start"}
	}
	selection, err := e.ValidateSurfaceRegion(events, nodes, options.Start, options.End)
	if err != nil {
		return nil, err
	}
	state, err := inspectCompactionEntryState(events)
	if err != nil {
		return nil, err
	}
	if err := assertCompactionInactive(state, "compaction"); err != nil {
		return nil, err
	}
	if options.Manual && state.openTurn != nil {
		return nil, &ManualCompactionError{Code: ManualErrBusy, Message: "manual compaction: the session already has an open turn"}
	}

	compactionId := newCompactionID()
	startEnv, err := session.NewEnvelope(0, session.EventCompactionStart, CompactionStartPayload{
		CompactionId:    compactionId,
		SourceCommandId: options.SourceCommandId,
		Turn:            options.Owner,
	})
	if err != nil {
		return nil, err
	}
	if err := appendFn(startEnv); err != nil {
		return nil, &ManualCompactionError{Code: ManualErrPersistence, Message: "compaction/start could not be persisted", Cause: err}
	}

	// Summarize with stability recheck before commit.
	input, err := regionMessages(events, selection.ShadowedSeqs)
	if err != nil {
		e.closeFailure(startEnv, compactionId, options, err, appendFn)
		return nil, err
	}
	summary, usage, err := e.Summarizer.Summarize(ctx, input)
	if err != nil {
		e.closeFailure(startEnv, compactionId, options, err, appendFn)
		return nil, err
	}
	if options.Manual && ctx.Err() != nil {
		e.closeFailure(startEnv, compactionId, options, ctx.Err(), appendFn)
		return nil, &ManualCompactionError{Code: ManualErrCancelled, Message: "compaction cancelled during summarization"}
	}
	shadowedTokens, err := e.shadowedTokenCount(events, nodes, selection)
	if err != nil {
		e.closeFailure(startEnv, compactionId, options, err, appendFn)
		return nil, err
	}
	framed := frameSummary(summary)
	framedTokens := countTokens(framed)
	if framedTokens >= shadowedTokens {
		err = fmt.Errorf("summary is not smaller than the shadowed content (%d estimated framed tokens >= %d)", framedTokens, shadowedTokens)
		e.closeFailure(startEnv, compactionId, options, err, appendFn)
		return nil, err
	}

	summaryEvent, err := e.commitBody(header, startEnv, compactionId, options, selection, shadowedTokens, framed, usage, appendFn)
	if err != nil {
		e.closeFailure(startEnv, compactionId, options, err, appendFn)
		return nil, err
	}
	endEvent, err := e.commitEnd(header, compactionId, options, appendFn)
	if err != nil {
		return nil, &ManualCompactionError{Code: ManualErrCommit, Message: "compaction/end could not be persisted", Cause: err}
	}
	return &CompactionResult{
		CompactionId:       compactionId,
		SourceCommandId:    options.SourceCommandId,
		StartSeq:           startEnv.Seq,
		SummarySeq:         summaryEvent.Seq,
		EndSeq:             endEvent.Seq,
		Summary:            framed,
		ShadowedRange:      selection.Span,
		ShadowedSeqs:       append([]int(nil), selection.ShadowedSeqs...),
		ShadowedTokenCount: shadowedTokens,
	}, nil
}

// commitBody appends the compaction/summary record and the replacement
// user/message with its replace surfaceOp and sourceEventSeqs.
func (e *CompactEngine) commitBody(
	header *session.SessionHeader,
	startEnv *session.SessionEnvelope,
	compactionId string,
	options CompactTransactionOptions,
	selection *SurfaceSelection,
	shadowedTokens int,
	summary []session.ContentBlock,
	usage *session.TokenUsage,
	appendFn func(env *session.SessionEnvelope) error,
) (*session.SessionEnvelope, error) {
	summaryPayload := CompactionSummaryPayload{
		CompactionId:       compactionId,
		SourceCommandId:    options.SourceCommandId,
		Summary:            summary,
		ShadowedRange:      selection.Span,
		ShadowedSeqs:       append([]int(nil), selection.ShadowedSeqs...),
		ShadowedTokenCount: shadowedTokens,
		Provider:           e.Provider,
		Model:              e.Model,
		MaxTokens:          e.MaxTokens,
		Usage:              usage,
	}
	summaryEnv, err := session.NewEnvelope(0, session.EventCompactionSummary, summaryPayload)
	if err != nil {
		return nil, err
	}
	if err := appendFn(summaryEnv); err != nil {
		return nil, err
	}

	// The replacement user/message shadows the range; its source seqs are
	// [startSeq, summarySeq, ...shadowedSeqs] per the upstream protocol.
	replacement := session.UserMessagePayload{
		ID:   fmt.Sprintf("cp-%s", compactionId),
		Role: "user",
		Content: []session.ContentBlock{
			{Type: "text", Text: blockText(summary)},
		},
		Source: session.MessageSource{Kind: "plugin", Plugin: "compact"},
	}
	replacementEnv, err := session.NewEnvelope(0, session.EventUserMessage, replacement)
	if err != nil {
		return nil, err
	}
	op := session.SurfaceOp{Op: "replace", Start: selection.Span.Start, End: selection.Span.End}
	replacementEnv.SurfaceOp = &op
	sourceSeqs := []int{startEnv.Seq, summaryEnv.Seq}
	sourceSeqs = append(sourceSeqs, selection.ShadowedSeqs...)
	replacementEnv.SourceEventSeqs = sourceSeqs
	if err := appendFn(replacementEnv); err != nil {
		return nil, err
	}
	return summaryEnv, nil
}

// commitEnd appends the closing compaction/end event.
func (e *CompactEngine) commitEnd(
	header *session.SessionHeader,
	compactionId string,
	options CompactTransactionOptions,
	appendFn func(env *session.SessionEnvelope) error,
) (*session.SessionEnvelope, error) {
	payload := CompactionEndPayload{
		CompactionId:    compactionId,
		SourceCommandId: options.SourceCommandId,
		Turn:            options.Owner,
	}
	endEvent, err := session.NewEnvelope(0, session.EventCompactionEnd, payload)
	if err != nil {
		return nil, err
	}
	if err := appendFn(endEvent); err != nil {
		return nil, err
	}
	return endEvent, nil
}

// closeFailure appends a failing compaction/end, leaving the unmatched start
// detectable (upstream close behavior).
func (e *CompactEngine) closeFailure(
	startEnv *session.SessionEnvelope,
	compactionId string,
	options CompactTransactionOptions,
	cause error,
	appendFn func(env *session.SessionEnvelope) error,
) {
	payload := CompactionEndPayload{
		CompactionId:    compactionId,
		SourceCommandId: options.SourceCommandId,
		Turn:            options.Owner,
		Error:           cause.Error(),
	}
	endEvent, err := session.NewEnvelope(0, session.EventCompactionEnd, payload)
	if err == nil {
		_ = appendFn(endEvent)
	}
}

// shadowedTokenCount prices the selected span under the configured meter.
func (e *CompactEngine) shadowedTokenCount(events []session.SessionEnvelope, nodes []int, selection *SurfaceSelection) (int, error) {
	priced, err := e.Measure(events, nodes)
	if err != nil {
		return 0, err
	}
	if selection.EndIdx >= len(priced) {
		return 0, fmt.Errorf("compaction: token-meter surface does not match the current session surface")
	}
	total := 0
	for _, t := range priced[selection.StartIdx : selection.EndIdx+1] {
		total += t
	}
	return total, nil
}

// regionMessages projects the shadowed surface nodes to model messages in
// surface order (upstream buildSummarizationInput).
func regionMessages(events []session.SessionEnvelope, shadowedSeqs []int) (SummarizationInput, error) {
	bySeq := make(map[int]*session.SessionEnvelope, len(events))
	for i := range events {
		bySeq[events[i].Seq] = &events[i]
	}
	var out []session.ModelMessage
	for _, seq := range shadowedSeqs {
		env, ok := bySeq[seq]
		if !ok {
			return SummarizationInput{}, fmt.Errorf("compaction: shadowed seq %d not found in log", seq)
		}
		message, err := projectEvent(env)
		if err != nil {
			return SummarizationInput{}, err
		}
		if message != nil {
			out = append(out, *message)
		}
	}
	return SummarizationInput{Messages: out}, nil
}

// projectEvent mirrors `session.deriveEventMessage`: projects one surface
// event into its model message, or nil when it produces none.
func projectEvent(env *session.SessionEnvelope) (*session.ModelMessage, error) {
	switch env.Type {
	case session.EventUserMessage:
		var userMsg session.UserMessagePayload
		if err := json.Unmarshal(env.Data, &userMsg); err != nil {
			return nil, fmt.Errorf("invalid user/message event data at seq %d: %w", env.Seq, err)
		}
		return &session.ModelMessage{Role: "user", Content: userMsg.Content}, nil
	case session.EventAssistantMessage:
		var asstMsg session.AssistantMessagePayload
		if err := json.Unmarshal(env.Data, &asstMsg); err != nil {
			return nil, fmt.Errorf("invalid assistant/message event data at seq %d: %w", env.Seq, err)
		}
		if len(asstMsg.Message.Content) == 0 {
			return nil, nil
		}
		return &session.ModelMessage{Role: "assistant", Content: asstMsg.Message.Content}, nil
	case session.EventToolResult:
		var toolRes session.ToolResultPayload
		if err := json.Unmarshal(env.Data, &toolRes); err != nil {
			return nil, fmt.Errorf("invalid tool/result event data at seq %d: %w", env.Seq, err)
		}
		if len(toolRes.Message.Content) != 1 || toolRes.Message.Content[0].Type != "tool-result" {
			return nil, fmt.Errorf("invalid tool/result event data at seq %d: message.content must be one tool-result block", env.Seq)
		}
		return &session.ModelMessage{Role: "tool", Content: []session.ContentBlock{toolRes.Message.Content[0]}}, nil
	}
	return nil, nil
}

// entryState captures the scan result of inspectCompactionEntryState.
type entryState struct {
	openTurn                  *int
	unmatchedCompactionStart  *compactionStart
	latestEndSeedSeq          *int
	compactionEntryStateKnown bool
	openTurnStateKnown        bool
}

// compactionStart is a located unmatched compaction/start event.
type compactionStart struct {
	Seq     int
	Payload CompactionStartPayload
}

// inspectCompactionEntryState scans the tail for open-turn, unmatched
// compaction-start, and latest constructor-seed boundary state.
func inspectCompactionEntryState(events []session.SessionEnvelope) (entryState, error) {
	state := entryState{}
	for index := len(events) - 1; index >= 0; index-- {
		event := &events[index]
		if state.latestEndSeedSeq == nil && event.Type == session.EventSessionEndSeed {
			seq := event.Seq
			state.latestEndSeedSeq = &seq
		}
		if !state.compactionEntryStateKnown {
			if event.Type == session.EventCompactionStart {
				var payload CompactionStartPayload
				_ = json.Unmarshal(event.Data, &payload)
				state.unmatchedCompactionStart = &compactionStart{Seq: event.Seq, Payload: payload}
				state.compactionEntryStateKnown = true
			} else if event.Type == session.EventCompactionEnd {
				state.compactionEntryStateKnown = true
			}
		}
		if !state.openTurnStateKnown {
			if event.Type == session.EventTurnStart {
				var payload session.TurnStartPayload
				_ = json.Unmarshal(event.Data, &payload)
				state.openTurn = &payload.Turn
				state.openTurnStateKnown = true
			} else if event.Type == session.EventTurnEnd {
				state.openTurnStateKnown = true
			}
		}
		if state.openTurnStateKnown && state.compactionEntryStateKnown && state.latestEndSeedSeq != nil {
			break
		}
	}
	return state, nil
}

// assertCompactionInactive rejects a durable unmatched compaction marker
// unless a later constructor-seed boundary proves its owner belongs to an
// earlier session lifecycle.
func assertCompactionInactive(state entryState, stage string) error {
	if state.unmatchedCompactionStart == nil {
		return nil
	}
	if state.latestEndSeedSeq != nil && *state.latestEndSeedSeq > state.unmatchedCompactionStart.Seq {
		return nil
	}
	return &ManualCompactionError{
		Code:    ManualErrBusy,
		Message: fmt.Sprintf("%s: compaction already in progress; the session compaction lock is already active", stage),
	}
}

// ActiveCompaction inspects the durable lock; nil when no active transaction.
func ActiveCompaction(events []session.SessionEnvelope) (*CompactionEntry, error) {
	state, err := inspectCompactionEntryState(events)
	if err != nil {
		return nil, err
	}
	if state.unmatchedCompactionStart == nil {
		return nil, nil
	}
	if state.latestEndSeedSeq != nil && *state.latestEndSeedSeq > state.unmatchedCompactionStart.Seq {
		return nil, nil
	}
	return &CompactionEntry{
		CompactionId: state.unmatchedCompactionStart.Payload.CompactionId,
		StartSeq:     state.unmatchedCompactionStart.Seq,
	}, nil
}

// CompactionEntry exposes a detected active compaction lock.
type CompactionEntry struct {
	CompactionId string
	StartSeq     int
}

func newCompactionID() string {
	var raw [12]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

func blockText(blocks []session.ContentBlock) string {
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" {
			sb.WriteString(b.Text)
			sb.WriteString("\n")
		}
	}
	return strings.TrimSpace(sb.String())
}

// frameSummary wraps the summary content in the checkpoint preamble/tag
// framing (upstream frameSummary in compaction-basic/src/summarizer.ts).
func frameSummary(summary []session.ContentBlock) []session.ContentBlock {
	const preamble = "This is an automatically generated checkpoint condensing an earlier span of the conversation to free up context. Treat the captured context as established background and build on it without restating it. Continue the task directly from the messages that follow, without acknowledging this checkpoint."
	const openTag = "<compacted-summary>"
	const closeTag = "</compacted-summary>"
	framed := []session.ContentBlock{
		{Type: "text", Text: preamble + "\n\n" + openTag},
	}
	framed = append(framed, summary...)
	framed = append(framed, session.ContentBlock{Type: "text", Text: closeTag})
	return framed
}
func countTokens(blocks []session.ContentBlock) int {
	total := 0
	for _, b := range blocks {
		total += (len(b.Text) + 3) / 4
	}
	return total
}
