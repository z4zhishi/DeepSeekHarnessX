package compaction

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
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
// rawOutput is the complete provider output before the text-only projection
// and llmStreamCall marks a call made through the harness LLM seam — both are
// recorded verbatim on compaction/summary so the auxiliary call stays
// reconstructible from the log alone (upstream SummaryResult).
type Summarizer interface {
	Summarize(ctx context.Context, input SummarizationInput) (summary []session.ContentBlock, usage *session.TokenUsage, rawOutput []session.ContentBlock, llmStreamCall bool, err error)
}

// TemplateSummarizer frames the standard checkpoint using a fixed text
// summary without a model call (tests and offline runs).
type TemplateSummarizer struct{}

func (TemplateSummarizer) Summarize(_ context.Context, input SummarizationInput) ([]session.ContentBlock, *session.TokenUsage, []session.ContentBlock, bool, error) {
	var lines []string
	lines = append(lines, "## Primary Request and Intent", "- "+firstUserText(input.Messages))
	lines = append(lines, "", "## Key Technical Concepts", "- (none)")
	lines = append(lines, "", "## Files and Code", "- (none)")
	lines = append(lines, "", "## Errors and Fixes", "- (none)")
	lines = append(lines, "", "## Pending Jobs", "- (none)")
	lines = append(lines, "", "## Current Work", "- (none)")
	lines = append(lines, "", "## Next Step", "- (none)")
	lines = append(lines, "", "## Critical Context", "- (none)")
	text := strings.Join(lines, "\n")
	blocks := []session.ContentBlock{{Type: "text", Text: text}}
	return blocks, nil, blocks, false, nil
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
	// ContextLimit is the routed model's context window in tokens. 0 (the
	// zero value) keeps the legacy always-compact-when-selectable behavior;
	// when set, PressureQualified gates automatic compaction on projected
	// transcript size against ThresholdTokens.
	ContextLimit int
	// ThresholdRatio is the pressure fraction of ContextLimit that qualifies
	// a session for compaction (upstream thresholdRatio; default 0.8).
	ThresholdRatio float64
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

// DefaultThresholdRatio mirrors upstream DEFAULT_THRESHOLD_RATIO: the default
// pressure fraction of the context window that qualifies for compaction.
const DefaultThresholdRatio = 0.8

// ThresholdTokens resolves the absolute token pressure target this engine
// compacts at (upstream resolveCompactSpec's floor(contextWindow*ratio)).
func (e *CompactEngine) ThresholdTokens() int {
	ratio := e.ThresholdRatio
	if ratio <= 0 || ratio > 1 {
		ratio = DefaultThresholdRatio
	}
	return int(float64(e.ContextLimit) * ratio)
}

// PressureQualified reports whether projected transcript tokens have reached
// the compaction threshold (upstream compactIfNeeded's pressure gate:
// `measurement.totalTokens < spec.thresholdTokens → return null`). With no
// ContextLimit configured the gate is always open, preserving the legacy
// select-then-compact behavior; loop.go wires this before CompactTransaction:
//
//	qualified, err := a.Compactor.PressureQualified(log)
//	if err != nil || !qualified { return }
func (e *CompactEngine) PressureQualified(events []session.SessionEnvelope) (bool, error) {
	if e.ContextLimit <= 0 {
		return true, nil
	}
	meter := llm.Meter{ContextLimit: e.ContextLimit}
	metrics, err := meter.Measure(events)
	if err != nil {
		return false, err
	}
	return metrics.ProjectedTokens >= e.ThresholdTokens(), nil
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
	// writes a standalone bracket (manual, idle). A non-manual transaction
	// with a nil Owner additionally asserts an open turn exists (upstream
	// 'current-turn' owner semantics).
	Owner *int
	// Manual marks a direct human-command compaction (cancel-sensitive).
	Manual bool
	// SourceCommandId records the initiating manual command when present.
	SourceCommandId string
	// Start/End select the surface-position span to compact.
	Start int
	End   int
	// Flush runs once after the bracket closed successfully (upstream
	// durability checkpoint); its failure classifies as persistence.
	Flush func() error
	// Reload observes the live log at the stability checkpoint (upstream
	// reads session.events directly). When set, it must return the freshest
	// durable log plus its folded surface nodes; the transaction replays the
	// selected span against them after summarization and refuses to commit
	// over a changed surface (SurfaceChangedError). Nil disables the recheck
	// (single-writer embedded runs where nothing can interleave).
	Reload func() ([]session.SessionEnvelope, []int, error)
	// Automatic marks a current-turn-owner compaction (upstream
	// owner:'current-turn'): the bracket must lie inside the session's open
	// turn — a missing open turn is rejected ('no open turn') instead of
	// silently writing a standalone bracket across a turn boundary. The
	// owner turn number is derived from the log; Start/End still select the
	// span. Manual transactions ignore this flag.
	Automatic bool
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
	// Automatic (current-turn owner) path: the bracket must be enclosed in an
	// open turn (upstream throws 'no open turn'); otherwise the standalone
	// bracket would straddle a turn boundary, which upstream invariants
	// forbid. Manual and idle transactions skip this assertion.
	if options.Automatic && state.openTurn == nil {
		return nil, fmt.Errorf("compactRegion: no open turn — automatic compaction events must be enclosed in a turn")
	}

	compactionId := newCompactionID()
	ownerTurn := options.Owner
	if ownerTurn == nil {
		ownerTurn = state.openTurn
	}
	startEnv, err := session.NewEnvelope(0, session.EventCompactionStart, CompactionStartPayload{
		CompactionId:    compactionId,
		SourceCommandId: options.SourceCommandId,
		Turn:            ownerTurn,
	})
	if err != nil {
		return nil, err
	}
	if err := appendFn(startEnv); err != nil {
		return nil, &ManualCompactionError{Code: ManualErrPersistence, Message: "compaction/start could not be persisted", Cause: err}
	}

	// Summarize, recheck surface stability against the live log, then commit.
	//
	// Prepare phase (upstream prepareCompaction): snapshot the selected span's
	// per-node prices BEFORE the asynchronous summarization so the stability
	// recheck can compare against them afterwards.
	preparedPrices, err := e.Measure(events, nodes)
	if err != nil || len(preparedPrices) != len(nodes) {
		err := fmt.Errorf("compaction: token-meter surface does not match the current session surface")
		e.closeFailure(startEnv, compactionId, options, err, appendFn)
		return nil, e.classifyFailure(options, false, err)
	}
	preparedSpan := append([]int(nil), preparedPrices[selection.StartIdx:selection.EndIdx+1]...)

	input, err := regionMessages(events, selection.ShadowedSeqs)
	if err != nil {
		e.closeFailure(startEnv, compactionId, options, err, appendFn)
		return nil, e.classifyFailure(options, false, err)
	}
	summaryBlocks, usage, rawOutput, llmStreamCall, err := e.Summarizer.Summarize(ctx, input)
	if err != nil {
		e.closeFailure(startEnv, compactionId, options, err, appendFn)
		return nil, e.classifyFailure(options, false, err)
	}
	if options.Manual && ctx.Err() != nil {
		err := ctx.Err()
		e.closeFailure(startEnv, compactionId, options, err, appendFn)
		return nil, &ManualCompactionError{Code: ManualErrCancelled, Message: "compaction cancelled during summarization", Cause: err}
	}

	// Stability recheck: replay the live log and require the selected span to
	// still be present, contiguous, equally priced, and balanced. Nodes added
	// outside the span do not invalidate the summary; anything inside does.
	if err := e.assertSelectedSpanStable(selection, preparedSpan, options); err != nil {
		e.closeFailure(startEnv, compactionId, options, err, appendFn)
		return nil, e.classifyFailure(options, false, err)
	}

	shadowedTokens, err := e.shadowedTokenCount(events, nodes, selection)
	if err != nil {
		e.closeFailure(startEnv, compactionId, options, err, appendFn)
		return nil, e.classifyFailure(options, false, err)
	}
	framed := frameSummary(summaryBlocks)
	framedTokens := countTokens(framed)
	if framedTokens >= shadowedTokens {
		err = fmt.Errorf("summary is not smaller than the shadowed content (%d estimated framed tokens >= %d)", framedTokens, shadowedTokens)
		e.closeFailure(startEnv, compactionId, options, err, appendFn)
		return nil, e.classifyFailure(options, false, err)
	}

	summaryEvent, err := e.commitBody(header, startEnv, compactionId, options, selection, shadowedTokens, summaryBlocks, framed, usage, rawOutput, llmStreamCall, appendFn)
	if err != nil {
		e.closeFailure(startEnv, compactionId, options, err, appendFn)
		return nil, e.classifyFailure(options, true, err)
	}
	endEvent, err := e.commitEnd(header, compactionId, options, ownerTurn, appendFn)
	if err != nil {
		wrapped := &ManualCompactionError{Code: ManualErrCommit, Message: "compaction/end could not be persisted", Cause: err}
		e.closeFailure(startEnv, compactionId, options, wrapped, appendFn)
		return nil, wrapped
	}

	result := &CompactionResult{
		CompactionId:       compactionId,
		SourceCommandId:    options.SourceCommandId,
		StartSeq:           startEnv.Seq,
		SummarySeq:         summaryEvent.Seq,
		EndSeq:             endEvent.Seq,
		Summary:            framed,
		ShadowedRange:      selection.Span,
		ShadowedSeqs:       append([]int(nil), selection.ShadowedSeqs...),
		ShadowedTokenCount: shadowedTokens,
	}
	// Post-success durability checkpoint (upstream options.flush): classified
	// as persistence on failure, never silently swallowed.
	if options.Flush != nil {
		if err := options.Flush(); err != nil {
			return result, &ManualCompactionError{Code: ManualErrPersistence, Message: "compaction durability checkpoint failed", Cause: err}
		}
	}
	return result, nil
}

// classifyFailure wraps a transaction failure for manual callers: commit-stage
// errors become commit, a stability violation becomes changed, everything else
// after start is summary. Non-manual callers get the bare cause (upstream only
// wraps for `owner === null`).
func (e *CompactEngine) classifyFailure(options CompactTransactionOptions, commitStage bool, cause error) error {
	if !options.Manual {
		return cause
	}
	if commitStage {
		return &ManualCompactionError{Code: ManualErrCommit, Message: "manual compaction did not commit cleanly", Cause: cause}
	}
	var changed *SurfaceChangedError
	if errors.As(cause, &changed) {
		return &ManualCompactionError{Code: ManualErrChanged, Message: "the compacted history changed during manual compaction", Cause: cause}
	}
	var manual *ManualCompactionError
	if errors.As(cause, &manual) {
		return manual
	}
	return &ManualCompactionError{Code: ManualErrSummary, Message: "manual compaction could not produce a smaller summary", Cause: cause}
}

// assertSelectedSpanStable replays the current log through the Reload seam and
// verifies the selected span survived summarization unchanged (upstream
// assertSelectedSpanStable): same shadowed seqs in order, equal per-node
// prices, boundaries still balanced. A nil Reload skips the check — the
// embedded single-writer loop cannot interleave appends between Summarize and
// commit because both run synchronously on the actor goroutine.
func (e *CompactEngine) assertSelectedSpanStable(selection *SurfaceSelection, preparedSpan []int, options CompactTransactionOptions) error {
	if options.Reload == nil {
		return nil
	}
	events, nodes, err := options.Reload()
	if err != nil {
		return &SurfaceChangedError{message: fmt.Sprintf("compaction: stability recheck could not read the session log: %v", err)}
	}
	current, err := e.ValidateSurfaceRegion(events, nodes, selection.Span.Start, selection.Span.End)
	if err != nil {
		return &SurfaceChangedError{message: fmt.Sprintf("compaction: the selected span is no longer a valid replacement target: %v", err)}
	}
	if len(current.ShadowedSeqs) != len(selection.ShadowedSeqs) {
		return &SurfaceChangedError{message: "compaction: the selected span changed during summarization"}
	}
	for i := range current.ShadowedSeqs {
		if current.ShadowedSeqs[i] != selection.ShadowedSeqs[i] {
			return &SurfaceChangedError{message: "compaction: the selected span changed during summarization"}
		}
	}
	pricedNow, err := e.Measure(events, nodes)
	if err != nil || len(pricedNow) != len(nodes) || current.EndIdx >= len(pricedNow) {
		return &SurfaceChangedError{message: "compaction: the selected span was rewritten during summarization"}
	}
	for i := current.StartIdx; i <= current.EndIdx; i++ {
		if i-current.StartIdx >= len(preparedSpan) || pricedNow[i] != preparedSpan[i-current.StartIdx] {
			return &SurfaceChangedError{message: "compaction: the selected span was rewritten during summarization"}
		}
	}
	return nil
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
	framed []session.ContentBlock,
	usage *session.TokenUsage,
	rawOutput []session.ContentBlock,
	llmStreamCall bool,
	appendFn func(env *session.SessionEnvelope) error,
) (*session.SessionEnvelope, error) {
	// compaction/summary records the summarizer's own output (upstream writes
	// summaryResult.summary verbatim); the framing lives on the checkpoint.
	summaryPayload := CompactionSummaryPayload{
		CompactionId:       compactionId,
		SourceCommandId:    options.SourceCommandId,
		Summary:            append([]session.ContentBlock(nil), summary...),
		ShadowedRange:      selection.Span,
		ShadowedSeqs:       append([]int(nil), selection.ShadowedSeqs...),
		ShadowedTokenCount: shadowedTokens,
		Provider:           e.Provider,
		Model:              e.Model,
		MaxTokens:          e.MaxTokens,
		Usage:              usage,
		LLMStreamCall:      llmStreamCall,
		RawOutput:          rawOutput,
	}
	summaryEnv, err := session.NewEnvelope(0, session.EventCompactionSummary, summaryPayload)
	if err != nil {
		return nil, err
	}
	if err := appendFn(summaryEnv); err != nil {
		return nil, err
	}

	// The replacement user/message shadows the range; its source seqs are
	// [startSeq, summarySeq, ...shadowedSeqs] per the upstream protocol. The
	// framed block array lands verbatim as the content (upstream
	// createUserMessage({content: frameSummary(...)})) and the source carries
	// the correlated checkpoint provenance (compactionId + optional command).
	replacement := session.UserMessagePayload{
		ID:      newCheckpointMessageID(),
		Role:    "user",
		Content: append([]session.ContentBlock(nil), framed...),
		Source: session.MessageSource{
			Kind:            "plugin",
			Plugin:          "compact",
			CompactionId:    compactionId,
			SourceCommandId: options.SourceCommandId,
		},
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
	ownerTurn *int,
	appendFn func(env *session.SessionEnvelope) error,
) (*session.SessionEnvelope, error) {
	payload := CompactionEndPayload{
		CompactionId:    compactionId,
		SourceCommandId: options.SourceCommandId,
		Turn:            ownerTurn,
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

// projectEvent mirrors `session.ProjectEventMessage`: projects one surface
// event into its model message, or nil when it produces none — verbatim
// pass-through of the nested message (upstream deriveEventMessage).
func projectEvent(env *session.SessionEnvelope) (*session.ModelMessage, error) {
	return session.ProjectEventMessage(env)
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
	// Upstream CompactionId(randomUUID()) — a random v4 UUID.
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]), hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]), hex.EncodeToString(b[8:10]), hex.EncodeToString(b[10:16]))
}

// newCheckpointMessageID is the replacement checkpoint message id (upstream
// createUserMessage assigns a UUID MessageId).
func newCheckpointMessageID() string { return newCompactionID() }

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

// countTokens prices framed checkpoint content with the shared fixed-density
// heuristic (delegating to the llm package's single-source arithmetic, which
// recurses into nested blocks).
func countTokens(blocks []session.ContentBlock) int {
	return llm.EstimateContentTokens(blocks)
}
