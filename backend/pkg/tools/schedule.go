package tools

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"dsh-go/pkg/session"
)

// Schedule domain constants (upstream @deepseek-ai/dsh-schedule v1).
const (
	scheduleChangeVersion         = 1
	minEveryIntervalSeconds       = 300
	minFourDigitYearMs      int64 = -62135596800000 // 0001-01-01T00:00:00.000Z
	maxFourDigitYearMs      int64 = 253402300799999 // 9999-12-31T23:59:59.999Z
)

// scheduleToolError is the closed public failure union for the Schedule tools.
type scheduleToolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

var utcInstantRe = regexp.MustCompile(`^[0-9]{4}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12][0-9]|3[01])T(?:[01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]\.\d{3}Z$`)

// scheduleFolded is the pure replay result: active records in creation order
// and every id ever seen (upstream FoldedSchedules).
type scheduleFolded struct {
	active  []sessionScheduleRecord
	seenIDs map[string]bool
}

// sessionScheduleRecord is the durable record in live memory.
type sessionScheduleRecord struct {
	ID           string
	Kind         string // "after" | "at" | "every"
	Prompt       string
	ScheduledAt  time.Time
	AfterSeconds int64
	EverySeconds int64
}

// foldScheduleEvents replays the session log's schedule/change stream after
// the seed boundary (upstream foldScheduleEvents): last write wins per id,
// delete removes, dispatch advances a fixed-rate record or removes it at
// exhaustion. Reused ids and delete/dispatch of inactive ids are corrupt.
func foldScheduleEvents(events []*session.SessionEnvelope, seedLength int) (scheduleFolded, error) {
	if seedLength < 0 || seedLength > len(events) {
		return scheduleFolded{}, fmt.Errorf("schedule seedLength must be within the supplied event log")
	}
	seen := map[string]bool{}
	active := map[string]sessionScheduleRecord{}
	for _, env := range events[seedLength:] {
		if env.Type != session.EventScheduleChange {
			continue
		}
		var ch session.ScheduleChangePayload
		if err := json.Unmarshal(env.Data, &ch); err != nil || ch.Version != 1 {
			return scheduleFolded{}, fmt.Errorf("corrupt schedule/change payload")
		}
		switch ch.Operation {
		case "create":
			if ch.Schedule == nil {
				return scheduleFolded{}, fmt.Errorf("schedule create must carry a schedule")
			}
			if seen[ch.Schedule.ID] {
				return scheduleFolded{}, fmt.Errorf("schedule id %s was reused", ch.Schedule.ID)
			}
			rec, err := decodeScheduleRecord(*ch.Schedule)
			if err != nil {
				return scheduleFolded{}, err
			}
			seen[rec.ID] = true
			active[rec.ID] = rec
		case "delete":
			if _, ok := active[ch.ID]; !ok {
				return scheduleFolded{}, fmt.Errorf("schedule delete targets inactive id %s", ch.ID)
			}
			delete(active, ch.ID)
		case "dispatch":
			rec, ok := active[ch.ID]
			if !ok {
				return scheduleFolded{}, fmt.Errorf("schedule dispatch targets inactive id %s", ch.ID)
			}
			if rec.Kind != "every" {
				delete(active, ch.ID)
				continue
			}
			acceptedAt, err := parseAcceptedAt(ch.AcceptedAt)
			if err != nil {
				return scheduleFolded{}, fmt.Errorf("every dispatch must contain acceptedAt")
			}
			next := nextOccurrence(rec.ScheduledAt, rec.EverySeconds, acceptedAt)
			if next == nil {
				delete(active, ch.ID)
			} else {
				rec.ScheduledAt = *next
				active[ch.ID] = rec
			}
		default:
			return scheduleFolded{}, fmt.Errorf("schedule/change operation must be create, delete, or dispatch")
		}
	}
	var ids []string
	for id := range active {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]sessionScheduleRecord, 0, len(ids))
	for _, id := range ids {
		out = append(out, active[id])
	}
	return scheduleFolded{active: out, seenIDs: seen}, nil
}

// decodeRecordRecord validates one durable record shape (upstream strict
// decoding: exact keys, trimmed prompt, safe integers, canonical instant).
func decodeScheduleRecord(p session.ScheduleRecord) (sessionScheduleRecord, error) {
	rec := sessionScheduleRecord{ID: p.ID, Kind: p.Kind, Prompt: p.Prompt}
	if rec.ID == "" || strings.TrimSpace(rec.ID) != rec.ID {
		return rec, fmt.Errorf("schedule id must be a non-empty string without surrounding whitespace")
	}
	if rec.Prompt == "" || strings.TrimSpace(rec.Prompt) != rec.Prompt {
		return rec, fmt.Errorf("schedule prompt must be non-empty and already trimmed")
	}
	switch rec.Kind {
	case "after":
		if p.AfterSeconds == nil || *p.AfterSeconds <= 0 {
			return rec, fmt.Errorf("afterSeconds must be a positive safe integer")
		}
		rec.AfterSeconds = *p.AfterSeconds
	case "at":
	case "every":
		if p.EverySeconds == nil || *p.EverySeconds < minEveryIntervalSeconds {
			return rec, fmt.Errorf("everySeconds must be at least %d", minEveryIntervalSeconds)
		}
		rec.EverySeconds = *p.EverySeconds
	default:
		return rec, fmt.Errorf("v1 schedule kind must be after, at, or every")
	}
	at, err := parseCanonicalInstant(p.ScheduledAt)
	if err != nil {
		return rec, err
	}
	rec.ScheduledAt = at
	return rec, nil
}

// parseCanonicalInstant parses the strict canonical four-digit-year UTC form
// (YYYY-MM-DDTHH:mm:ss.sssZ) and rejects everything else.
func parseCanonicalInstant(value string) (time.Time, error) {
	if !utcInstantRe.MatchString(value) {
		return time.Time{}, fmt.Errorf("scheduledAt must be a canonical four-digit-year RFC 3339 UTC instant")
	}
	at, err := time.Parse("2006-01-02T15:04:05.000Z", value)
	if err != nil || at.Year() < 1 || at.Year() > 9999 {
		return time.Time{}, fmt.Errorf("scheduledAt is not a real UTC calendar instant")
	}
	return at, nil
}

// parseAcceptedAt accepts the canonical form or the equivalent with any
// fractional width; dispatch frames always carry the canonical form.
func parseAcceptedAt(value string) (time.Time, error) {
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	if at.Year() < 1 || at.Year() > 9999 {
		return time.Time{}, fmt.Errorf("acceptedAt out of four-digit-year range")
	}
	return at, nil
}

// formatCanonical renders the canonical UTC instant (upstream
// Date.toISOString()).
func formatCanonical(at time.Time) string {
	return at.UTC().Format("2006-01-02T15:04:05.000Z")
}

// nextOccurrence computes the latest anchor-aligned occurrence at or before
// acceptedAt and the first strictly future target; the record advances to the
// next target, and exhaustion (past the 9999 year) ends the rule.
func nextOccurrence(anchor time.Time, everySeconds int64, acceptedAt time.Time) *time.Time {
	interval := time.Duration(everySeconds) * time.Second
	delta := acceptedAt.Sub(anchor)
	periods := delta / interval
	if delta < 0 && delta%interval != 0 {
		periods-- // Math.floor semantics for negative deltas (anchor before epoch)
	}
	latest := anchor.Add(periods * interval)
	next := latest.Add(interval)
	if next.Year() > 9999 {
		return nil
	}
	return &next
}

// allocateScheduleID picks the next readable session-local id without reusing
// any prior id (upstream allocateScheduleId: schedule-<sequence>).
func allocateScheduleID(seen map[string]bool) string {
	seq := len(seen) + 1
	for {
		candidate := "schedule-" + strconv.Itoa(seq)
		if !seen[candidate] {
			return candidate
		}
		seq++
	}
}

// scheduleView derives the execution-local view (state + deliveryMode).
func scheduleView(rec sessionScheduleRecord, now time.Time) map[string]any {
	state := "scheduled"
	if !now.Before(rec.ScheduledAt) {
		state = "overdue"
	}
	view := map[string]any{
		"id":           rec.ID,
		"kind":         rec.Kind,
		"prompt":       rec.Prompt,
		"scheduledAt":  formatCanonical(rec.ScheduledAt),
		"state":        state,
		"deliveryMode": "session-local",
	}
	switch rec.Kind {
	case "after":
		view["afterSeconds"] = rec.AfterSeconds
	case "every":
		view["everySeconds"] = rec.EverySeconds
	}
	return view
}
