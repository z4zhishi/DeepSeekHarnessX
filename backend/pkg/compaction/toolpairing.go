package compaction

import (
	"encoding/json"
	"fmt"

	"dsh-go/pkg/session"
)

// Tool-pairing balance, ported from `CK/packages/compaction/compaction/src/tool-pairing.ts`.
// A surface cut is balanced when no in-progress assistant tool-call crosses
// it; cutting at an unbalanced boundary would split a step's call/result pair.

// eventToolDelta returns how one surface event changes the in-progress
// tool-call count: an assistant/message contributes its tool-call blocks and
// a tool/result closes exactly one call (upstream `eventDelta`).
func eventToolDelta(env *session.SessionEnvelope) (int, error) {
	switch env.Type {
	case session.EventAssistantMessage:
		var payload session.AssistantMessagePayload
		if err := json.Unmarshal(env.Data, &payload); err != nil {
			return 0, fmt.Errorf("assistant/message data at seq %d: %w", env.Seq, err)
		}
		count := 0
		for _, block := range payload.Message.Content {
			if block.Type == "tool-call" {
				count++
			}
		}
		return count, nil
	case session.EventToolResult:
		return -1, nil
	default:
		return 0, nil
	}
}

// cutBalances computes the balance of every surface cut: a surface of N
// sequences has N+1 cuts, entry i being the cut before sequence i (the final
// entry is the cut after the surface tail).
func cutBalances(events []session.SessionEnvelope, nodes []int) ([]bool, error) {
	bySeq := make(map[int]*session.SessionEnvelope, len(events))
	for i := range events {
		bySeq[events[i].Seq] = &events[i]
	}
	balances := make([]bool, 0, len(nodes)+1)
	inProgress := 0
	balances = append(balances, inProgress == 0)
	for _, seq := range nodes {
		event, ok := bySeq[seq]
		if !ok {
			return nil, fmt.Errorf("tool-pairing balance: surface seq %d has no matching session event (corrupt surface)", seq)
		}
		delta, err := eventToolDelta(event)
		if err != nil {
			return nil, err
		}
		inProgress += delta
		if inProgress < 0 {
			return nil, fmt.Errorf("tool-pairing balance: tool/result at surface seq %d has no matching tool-call (corrupt surface)", seq)
		}
		balances = append(balances, inProgress == 0)
	}
	return balances, nil
}

// ToolPairingBalancedBefore reports whether the cut immediately before the
// surface node at position idx is tool-pairing balanced.
func ToolPairingBalancedBefore(events []session.SessionEnvelope, nodes []int, idx int) (bool, error) {
	balances, err := cutBalances(events, nodes)
	if err != nil {
		return false, err
	}
	if idx < 0 || idx >= len(balances) {
		return false, fmt.Errorf("tool-pairing balance: surface position %d not found", idx)
	}
	return balances[idx], nil
}

// ToolPairingBalancedAfter reports whether the cut immediately after the
// surface node at position idx is tool-pairing balanced.
func ToolPairingBalancedAfter(events []session.SessionEnvelope, nodes []int, idx int) (bool, error) {
	balances, err := cutBalances(events, nodes)
	if err != nil {
		return false, err
	}
	if idx+1 < 0 || idx+1 >= len(balances) {
		return false, fmt.Errorf("tool-pairing balance: surface position %d not found", idx)
	}
	return balances[idx+1], nil
}
