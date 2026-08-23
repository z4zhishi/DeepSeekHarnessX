package session

import (
	"encoding/json"
	"fmt"
)

// surface.go implements the canonical surface fold over a session event log,
// ported from `CK/packages/core/session/src/surface.ts`. The append-only log
// stays the source of truth; this layer produces the ordered model-visible
// view (append + positional replacement) that deriveMessages folds into LLM
// history.

// SurfaceFoldReplacement records one replacement operation observed while
// folding a session surface (upstream SurfaceFoldReplacement).
type SurfaceFoldReplacement struct {
	Seq          int   // Seq of the replacing event
	Start        int   // Declared inclusive start seq of the replaced range
	End          int   // Declared inclusive end seq of the replaced range
	ShadowedSeqs []int // Surface entries removed, in surface order
}

// SurfaceFoldResult is the complete result of replaying the surface
// operations in a session log (upstream SurfaceFoldResult).
type SurfaceFoldResult struct {
	Nodes             []int                    // Current surface event seqs, model-visible order
	Replacements      []SurfaceFoldReplacement // Replacement operations in event order
	ReplaceGeneration int                      // Monotonic count of folded positional replacements
}

type surfaceFoldState struct {
	nodes             []int
	replaceGeneration int
}

type surfacePlan struct {
	kind     string // "append" | "replace"
	seq      int
	start    int
	end      int
	startIdx int
	endIdx   int
	shadowed []int
}

// FoldSurface replays a complete session log through the canonical surface
// fold. Events must arrive in contiguous seq order starting at baseSeq.
func FoldSurface(events []*SessionEnvelope, baseSeq int) (*SurfaceFoldResult, error) {
	state := &surfaceFoldState{}
	result := &SurfaceFoldResult{}
	for index, event := range events {
		replacement, err := applySurfaceEvent(state, event, baseSeq+index, events, baseSeq)
		if err != nil {
			return nil, err
		}
		if replacement != nil {
			result.Replacements = append(result.Replacements, *replacement)
		}
	}
	result.Nodes = append([]int(nil), state.nodes...)
	result.ReplaceGeneration = state.replaceGeneration
	return result, nil
}

// SurfaceManager is an incremental surface view for live sessions: it folds
// log events as they arrive and validates candidates before admission.
type SurfaceManager struct {
	state         surfaceFoldState
	log           []*SessionEnvelope
	baseSeq       int
	lastProcessed int
	pendingEvent  *SessionEnvelope
	pendingPlan   *surfacePlan
}

// NewSurfaceManager creates a manager over a contiguous log window.
func NewSurfaceManager(log []*SessionEnvelope, baseSeq int) *SurfaceManager {
	return &SurfaceManager{log: log, baseSeq: baseSeq, lastProcessed: baseSeq - 1}
}

// Nodes returns the surface event sequences in model-visible order, folding
// any events appended since the previous access.
func (m *SurfaceManager) Nodes() ([]int, error) {
	if err := m.processDelta(); err != nil {
		return nil, err
	}
	return append([]int(nil), m.state.nodes...), nil
}

// ReplaceGeneration returns the monotonic count of folded replacements.
func (m *SurfaceManager) ReplaceGeneration() (int, error) {
	if err := m.processDelta(); err != nil {
		return 0, err
	}
	return m.state.replaceGeneration, nil
}

// ValidateNext validates the next candidate without mutating the committed
// surface. The event has not entered the log yet; its expected seq is
// baseSeq+len(log).
func (m *SurfaceManager) ValidateNext(event *SessionEnvelope) error {
	if m.lastProcessed < m.baseSeq+len(m.log)-1 {
		if err := m.processDelta(); err != nil {
			return err
		}
	}
	expectedSeq := m.baseSeq + len(m.log)
	plan, err := planSurfaceEvent(&m.state, event, expectedSeq, m.log, m.baseSeq)
	if err != nil {
		return err
	}
	m.pendingEvent = event
	m.pendingPlan = plan
	return nil
}

func (m *SurfaceManager) processDelta() error {
	tailSeq := m.baseSeq + len(m.log) - 1
	for seq := m.lastProcessed + 1; seq <= tailSeq; seq++ {
		index := seq - m.baseSeq
		event := m.log[index]
		pending := m.pendingPlan
		if pending != nil && m.pendingEvent == event && pending.seq == seq {
			applySurfacePlan(&m.state, pending)
		} else {
			if _, err := applySurfaceEvent(&m.state, event, seq, m.log, m.baseSeq); err != nil {
				return err
			}
		}
		if pending != nil && pending.seq <= seq {
			m.pendingEvent = nil
			m.pendingPlan = nil
		}
		m.lastProcessed = seq
	}
	return nil
}

func applySurfacePlan(state *surfaceFoldState, plan *surfacePlan) *SurfaceFoldReplacement {
	if plan == nil {
		return nil
	}
	if plan.kind == "append" {
		state.nodes = append(state.nodes, plan.seq)
		return nil
	}
	if plan.kind == "replace" {
		shadowed := append([]int(nil), state.nodes[plan.startIdx:plan.endIdx+1]...)
		state.nodes = append(state.nodes[:plan.startIdx], append([]int{plan.seq}, state.nodes[plan.endIdx+1:]...)...)
		state.replaceGeneration++
		return &SurfaceFoldReplacement{
			Seq:          plan.seq,
			Start:        plan.start,
			End:          plan.end,
			ShadowedSeqs: shadowed,
		}
	}
	return nil
}

func applySurfaceEvent(state *surfaceFoldState, event *SessionEnvelope, expectedSeq int, events []*SessionEnvelope, baseSeq int) (*SurfaceFoldReplacement, error) {
	plan, err := planSurfaceEvent(state, event, expectedSeq, events, baseSeq)
	if err != nil {
		return nil, err
	}
	return applySurfacePlan(state, plan), nil
}

// planSurfaceEvent validates one event at its replay boundary and prepares
// its atomic fold transition. Non-surface events yield a nil plan.
func planSurfaceEvent(state *surfaceFoldState, event *SessionEnvelope, expectedSeq int, events []*SessionEnvelope, baseSeq int) (*surfacePlan, error) {
	if event.Seq != expectedSeq {
		return nil, fmt.Errorf("session event seq %d is not contiguous; expected %d", event.Seq, expectedSeq)
	}
	op, err := surfaceOpOf(event)
	if err != nil {
		return nil, err
	}
	if op == nil {
		return nil, nil
	}
	if op.IsAppend() {
		if err := assertProvenance(event, nil); err != nil {
			return nil, err
		}
		return &surfacePlan{kind: "append", seq: event.Seq}, nil
	}
	rangeInfo, err := replacementRange(state, op)
	if err != nil {
		return nil, err
	}
	if err := assertProvenance(event, rangeInfo.shadowed); err != nil {
		return nil, err
	}
	if err := assertToolResultRewrite(event, rangeInfo.shadowed, events, baseSeq); err != nil {
		return nil, err
	}
	return &surfacePlan{
		kind:     "replace",
		seq:      event.Seq,
		start:    op.Start,
		end:      op.End,
		startIdx: rangeInfo.startIdx,
		endIdx:   rangeInfo.endIdx,
		shadowed: rangeInfo.shadowed,
	}, nil
}

// surfaceOpOf validates event-local surface eligibility and returns its
// operation (upstream `surfaceOpOf`):
//   - non-surface events must NOT carry surfaceOp / sourceEventSeqs (error);
//   - surface-eligible events MUST carry a surfaceOp marker (error when absent);
//   - the marker must be the append string or an exact positional-replacement
//     shape `{op:"replace",start,end}`.
func surfaceOpOf(event *SessionEnvelope) (*SurfaceOp, error) {
	if !IsSurfaceEligibleType(event.Type) {
		if event.SurfaceOp != nil {
			return nil, fmt.Errorf("session event %q is not surface-eligible and cannot carry surfaceOp", event.Type)
		}
		if event.SourceEventSeqs != nil {
			return nil, fmt.Errorf("session event %q is not surface-eligible and cannot carry sourceEventSeqs", event.Type)
		}
		return nil, nil
	}
	op := event.SurfaceOp
	if op == nil {
		return nil, fmt.Errorf("session event %q is surface-eligible and requires a surfaceOp marker", event.Type)
	}
	if op.IsAppend() {
		return op, nil
	}
	if op.Op != "replace" || op.Start < 0 || op.End < 0 {
		return nil, fmt.Errorf("session event %q carries an invalid replace surfaceOp", event.Type)
	}
	return op, nil
}

func assertProvenance(event *SessionEnvelope, shadowed []int) error {
	sources := map[int]bool{}
	if len(event.SourceEventSeqs) > 0 {
		for _, source := range event.SourceEventSeqs {
			if source < 0 {
				return fmt.Errorf("session event %q sourceEventSeqs must densely contain non-negative safe integers", event.Type)
			}
			if sources[source] {
				return fmt.Errorf("sourceEventSeqs must not contain duplicates")
			}
			sources[source] = true
			if source >= event.Seq {
				return fmt.Errorf("sourceEventSeqs must reference earlier events: %d >= current seq %d", source, event.Seq)
			}
		}
	}
	for _, seq := range shadowed {
		if !sources[seq] {
			return fmt.Errorf("surface replace: sourceEventSeqs must include every shadowed surface node; missing %d", seq)
		}
	}
	return nil
}

type replacementRangeInfo struct {
	startIdx int
	endIdx   int
	shadowed []int
}

func replacementRange(state *surfaceFoldState, op *SurfaceOp) (*replacementRangeInfo, error) {
	startIdx := -1
	for i, n := range state.nodes {
		if n == op.Start {
			startIdx = i
			break
		}
	}
	if startIdx == -1 {
		return nil, fmt.Errorf("surface replace: start seq %d not found in surface", op.Start)
	}
	endIdx := -1
	for i, n := range state.nodes {
		if n == op.End {
			endIdx = i
			break
		}
	}
	if endIdx == -1 {
		return nil, fmt.Errorf("surface replace: end seq %d not found in surface", op.End)
	}
	if startIdx > endIdx {
		return nil, fmt.Errorf("surface replace: start seq %d (index %d) is after end seq %d (index %d)", op.Start, startIdx, op.End, endIdx)
	}
	return &replacementRangeInfo{
		startIdx: startIdx,
		endIdx:   endIdx,
		shadowed: append([]int(nil), state.nodes[startIdx:endIdx+1]...),
	}, nil
}

// assertToolResultRewrite restricts a tool/result replacement to one current
// result's content (upstream rule).
func assertToolResultRewrite(event *SessionEnvelope, shadowed []int, events []*SessionEnvelope, baseSeq int) error {
	if event.Type != EventToolResult {
		return nil
	}
	if len(shadowed) != 1 {
		return fmt.Errorf("tool/result surface replacement must rewrite exactly one current node")
	}
	originalSeq := shadowed[0]
	original := events[originalSeq-baseSeq]
	if original == nil || original.Type != EventToolResult {
		return fmt.Errorf("tool/result surface replacement must rewrite a current tool/result node")
	}
	if !toolResultRewriteEqual(original, event) {
		return fmt.Errorf("tool/result surface replacement may change only content")
	}
	return nil
}

// toolResultRewriteEqual implements upstream assertToolResultRewrite's
// structural check: the replacement may change only the message content of a
// tool/result; every other field must be deep-equal (content normalized to
// the null marker before comparison).
func toolResultRewriteEqual(original, replacement *SessionEnvelope) bool {
	origNormalized, ok1 := normalizeToolResultForRewrite(original)
	replNormalized, ok2 := normalizeToolResultForRewrite(replacement)
	if !ok1 || !ok2 {
		return false
	}
	return deepEqualJSON(origNormalized, replNormalized)
}

// normalizeToolResultForRewrite rewrites the message content to the empty
// string before comparison so only non-content fields participate in
// equality (upstream replaces content with null).
func normalizeToolResultForRewrite(env *SessionEnvelope) (json.RawMessage, bool) {
	var data map[string]any
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, false
	}
	message, ok := data["message"].(map[string]any)
	if !ok {
		return nil, false
	}
	content, ok := message["content"].([]any)
	if !ok {
		return nil, false
	}
	for i := range content {
		block, ok := content[i].(map[string]any)
		if !ok {
			return nil, false
		}
		block["content"] = ""
	}
	message["content"] = content
	data["message"] = message
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, false
	}
	return raw, true
}

func deepEqualJSON(a, b json.RawMessage) bool {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return deepEqualAny(av, bv)
}

func deepEqualAny(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, va := range av {
			vb, ok := bv[k]
			if !ok || !deepEqualAny(va, vb) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !deepEqualAny(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}
