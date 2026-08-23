package tools

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"dsh-go/pkg/session"
)

// Schedule selectors (upstream validateCreateArgs: exactly one of
// after_seconds | at | every_seconds; prompt non-empty after trimming).
var (
	offsetInstantRe = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,3}))?(Z|[+-](\d{2}):(\d{2}))$`)
	localDateRe     = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})$`)
	localTimeRe     = regexp.MustCompile(`^(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,3}))?$`)
	ianaZoneRe      = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+.-]*(?:\/[A-Za-z0-9_+.-]+)+$`)
)

// scheduleCreateArgs mirrors the model-facing schedule_create schema.
type scheduleCreateArgs struct {
	Prompt       string `json:"prompt"`
	AfterSeconds *int64 `json:"after_seconds"`
	EverySeconds *int64 `json:"every_seconds"`
	At           any    `json:"at"`
}

type scheduleLocalAt struct {
	Date     string `json:"date"`
	Time     string `json:"time"`
	TimeZone string `json:"time_zone"`
}

// scheduleToolErr builds the stable public failure union.
func scheduleToolErr(code, message string) map[string]any {
	return map[string]any{"code": code, "message": message}
}

// validateScheduleCreate enforces the v1 selector constraints the open
// parameter root cannot express (upstream validateCreateArgs).
func validateScheduleCreate(args *scheduleCreateArgs) (string, string) {
	selectors := 0
	if args.AfterSeconds != nil {
		selectors++
	}
	if args.EverySeconds != nil {
		selectors++
	}
	if args.At != nil {
		selectors++
	}
	if selectors != 1 {
		return "invalid_selector", "schedule_create accepts exactly one of after_seconds, at, or every_seconds."
	}
	if strings.TrimSpace(args.Prompt) == "" {
		return "invalid_prompt", "prompt must be non-empty after trimming."
	}
	if args.AfterSeconds != nil && (*args.AfterSeconds <= 0) {
		return "invalid_rule", "after_seconds must be a positive safe integer."
	}
	if args.EverySeconds != nil && *args.EverySeconds < minEveryIntervalSeconds {
		return "frequency_too_high", fmt.Sprintf("every_seconds must be at least %d.", minEveryIntervalSeconds)
	}
	return "", ""
}

// parseAtTarget resolves the at selector to a canonical UTC instant
// (upstream createAtScheduleRecord: explicit offset instant or structured
// local calendar value with an IANA zone).
func parseAtTarget(at any, now time.Time) (time.Time, error) {
	var target time.Time
	switch v := at.(type) {
	case string:
		instant, err := parseOffsetInstant(v)
		if err != nil {
			return target, err
		}
		target = instant
	default:
		raw, err := json.Marshal(at)
		if err != nil {
			return target, fmt.Errorf("invalid_rule|at must be an explicit-offset string or local calendar object.")
		}
		var local scheduleLocalAt
		if err := json.Unmarshal(raw, &local); err != nil || local.Date == "" || local.Time == "" || local.TimeZone == "" {
			return target, fmt.Errorf("invalid_rule|Local at must contain exactly date, time, and time_zone.")
		}
		parts, err := parseLocalCalendar(local.Date, local.Time)
		if err != nil {
			return target, err
		}
		loc, err := canonicalizeTimeZone(local.TimeZone)
		if err != nil {
			return target, err
		}
		instant := time.Date(parts.year, time.Month(parts.month), parts.day, parts.hour, parts.minute, parts.second, parts.millisecond*1e6, loc)
		if instant.Year() != parts.year || instant.Month() != time.Month(parts.month) || instant.Day() != parts.day {
			return target, fmt.Errorf("invalid_rule|The local at value must be a real ISO calendar date and time.")
		}
		target = instant.UTC()
	}
	// Strictly future and four-digit-year representable.
	if target.UnixMilli() <= now.UnixMilli() {
		return target, fmt.Errorf("not_future|The scheduled time must be strictly in the future.")
	}
	if target.Year() < 1 || target.Year() > 9999 {
		return target, fmt.Errorf("time_out_of_range|The scheduled time must be representable as a four-digit-year RFC 3339 UTC instant.")
	}
	return target, nil
}

// parseOffsetInstant parses a strict RFC 3339 instant with explicit offset.
func parseOffsetInstant(value string) (time.Time, error) {
	m := offsetInstantRe.FindStringSubmatch(value)
	if m == nil {
		return time.Time{}, fmt.Errorf("invalid_rule|at must use YYYY-MM-DDTHH:mm:ss with optional 1-3 digit fractional seconds and an explicit Z or numeric offset.")
	}
	year, _ := strconv.Atoi(m[1])
	month, _ := strconv.Atoi(m[2])
	day, _ := strconv.Atoi(m[3])
	hour, _ := strconv.Atoi(m[4])
	minute, _ := strconv.Atoi(m[5])
	second, _ := strconv.Atoi(m[6])
	millis := 0
	if m[7] != "" {
		millis, _ = strconv.Atoi(m[7] + strings.Repeat("0", 3-len(m[7])))
	}
	if year == 0 || hour > 23 || minute > 59 || second > 59 {
		return time.Time{}, fmt.Errorf("invalid_rule|The at value must be a real ISO calendar date and time.")
	}
	base := time.Date(year, time.Month(month), day, hour, minute, second, millis*1e6, time.UTC)
	if base.Year() != year || base.Month() != time.Month(month) || base.Day() != day {
		return time.Time{}, fmt.Errorf("invalid_rule|The at value must be a real ISO calendar date and time.")
	}
	if m[8] == "Z" {
		return base, nil
	}
	sign := 1
	if m[9] == "-" {
		sign = -1
	}
	offH, _ := strconv.Atoi(m[10])
	offM, _ := strconv.Atoi(m[11])
	if offH > 23 || offM > 59 || (sign < 0 && offH == 0 && offM == 0) {
		return time.Time{}, fmt.Errorf("invalid_rule|The at numeric offset is invalid.")
	}
	return base.Add(time.Duration(sign) * time.Duration(offH*3600+offM*60) * time.Second), nil
}

type calendarParts struct {
	year, month, day, hour, minute, second, millisecond int
}

// parseLocalCalendar validates strict local calendar fields.
func parseLocalCalendar(date, clock string) (calendarParts, error) {
	dm := localDateRe.FindStringSubmatch(date)
	tm := localTimeRe.FindStringSubmatch(clock)
	if dm == nil || tm == nil {
		return calendarParts{}, fmt.Errorf("invalid_rule|Local at requires date YYYY-MM-DD and time HH:mm:ss with optional one-to-three digit milliseconds.")
	}
	parts := calendarParts{}
	parts.year, _ = strconv.Atoi(dm[1])
	parts.month, _ = strconv.Atoi(dm[2])
	parts.day, _ = strconv.Atoi(dm[3])
	parts.hour, _ = strconv.Atoi(tm[1])
	parts.minute, _ = strconv.Atoi(tm[2])
	parts.second, _ = strconv.Atoi(tm[3])
	if tm[4] != "" {
		parts.millisecond, _ = strconv.Atoi(tm[4] + strings.Repeat("0", 3-len(tm[4])))
	}
	if parts.year == 0 || parts.hour > 23 || parts.minute > 59 || parts.second > 59 {
		return calendarParts{}, fmt.Errorf("invalid_rule|The local at value must be a real ISO calendar date and time.")
	}
	check := time.Date(parts.year, time.Month(parts.month), parts.day, parts.hour, parts.minute, parts.second, parts.millisecond*1e6, time.UTC)
	if check.Year() != parts.year || check.Month() != time.Month(parts.month) || check.Day() != parts.day {
		return calendarParts{}, fmt.Errorf("invalid_rule|The at value must be a real ISO calendar date and time.")
	}
	return parts, nil
}

// canonicalizeTimeZone validates UTC or an IANA Area/Location name.
func canonicalizeTimeZone(value string) (*time.Location, error) {
	if value != "UTC" && !ianaZoneRe.MatchString(value) {
		return nil, fmt.Errorf("invalid_time_zone|time_zone must be UTC or a valid IANA Area/Location name.")
	}
	loc, err := time.LoadLocation(value)
	if err != nil {
		return nil, fmt.Errorf("invalid_time_zone|time_zone must be UTC or a valid IANA Area/Location name.")
	}
	return loc, nil
}

// scheduleErrorFrom decodes the stable public error from a wrapped internal
// error (code|message convention used above).
func scheduleErrorFrom(err error) map[string]any {
	msg := err.Error()
	if idx := strings.Index(msg, "|"); idx >= 0 {
		return scheduleToolErr(msg[:idx], msg[idx+1:])
	}
	return scheduleToolErr("internal_error", "The schedule operation failed.")
}

// RegisterScheduleTools registers schedule_create, schedule_list and
// schedule_delete (upstream @deepseek-ai/dsh-schedule tools.ts). The session
// log is the single source of truth; the tools fold it on every call.
func (r *ToolRegistry) RegisterScheduleTools() {
	r.Register(ToolDefinition{
		Name: "schedule_create",
		Description: "Create one reminder in the current session. Supply a non-empty prompt and exactly one selector: " +
			"a positive safe-integer after_seconds delay, at as a strict offset date-time or local date/time object, or " +
			fmt.Sprintf("safe-integer every_seconds of at least %d. ", minEveryIntervalSeconds) +
			"Fixed-rate reminders stay creation-aligned, skip missed occurrences, and batch one latest occurrence per overdue rule. " +
			"Delivery is session-local: the reminder runs on time only while this session is live and otherwise becomes overdue until the session is resumed.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"prompt": { "type": "string", "description": "Reminder content to present when the target becomes due." },
				"after_seconds": { "type": "number", "description": "Positive safe-integer delay in seconds." },
				"every_seconds": { "type": "number", "description": "Fixed-rate safe-integer interval in seconds." },
				"at": { "description": "Absolute target as strict RFC 3339 or local date/time with an explicit IANA zone." }
			},
			"required": ["prompt"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args scheduleCreateArgs
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return scheduleToolErr("invalid_rule", "schedule_create arguments are malformed."), nil
			}
			if code, msg := validateScheduleCreate(&args); code != "" {
				return scheduleToolErr(code, msg), nil
			}
			folded, err := foldScheduleForTool(ctx.SessionID)
			if err != nil {
				return scheduleToolErr("corrupt_schedule_log", err.Error()), nil
			}
			id := allocateScheduleID(folded.seenIDs)
			now := time.Now().UTC()
			var rec sessionScheduleRecord
			switch {
			case args.AfterSeconds != nil:
				target := now.Add(time.Duration(*args.AfterSeconds) * time.Second)
				if target.Year() > 9999 {
					return scheduleToolErr("time_out_of_range", "The scheduled time must be representable as a four-digit-year RFC 3339 UTC instant."), nil
				}
				rec = sessionScheduleRecord{ID: id, Kind: "after", Prompt: strings.TrimSpace(args.Prompt), ScheduledAt: target, AfterSeconds: *args.AfterSeconds}
			case args.EverySeconds != nil:
				target := now.Add(time.Duration(*args.EverySeconds) * time.Second)
				if target.Year() > 9999 {
					return scheduleToolErr("time_out_of_range", "The scheduled time must be representable as a four-digit-year RFC 3339 UTC instant."), nil
				}
				rec = sessionScheduleRecord{ID: id, Kind: "every", Prompt: strings.TrimSpace(args.Prompt), ScheduledAt: target, EverySeconds: *args.EverySeconds}
			case args.At != nil:
				target, err := parseAtTarget(args.At, now)
				if err != nil {
					return scheduleErrorFrom(err), nil
				}
				rec = sessionScheduleRecord{ID: id, Kind: "at", Prompt: strings.TrimSpace(args.Prompt), ScheduledAt: target}
			}
			if ctx.Emit != nil {
				// Exact v1 record keys per kind (upstream strict decoding:
				// at carries no interval field).
				durable := &session.ScheduleRecord{
					ID:          rec.ID,
					Kind:        rec.Kind,
					Prompt:      rec.Prompt,
					ScheduledAt: formatCanonical(rec.ScheduledAt),
				}
				if rec.Kind == "after" {
					durable.AfterSeconds = &rec.AfterSeconds
				}
				if rec.Kind == "every" {
					durable.EverySeconds = &rec.EverySeconds
				}
				ctx.Emit(session.EventScheduleChange, session.ScheduleChangePayload{
					Version:   scheduleChangeVersion,
					Operation: "create",
					Schedule:  durable,
				})
			}
			return scheduleView(rec, now), nil
		},
	})

	r.Register(ToolDefinition{
		Name: "schedule_list",
		Description: "List every active reminder in the current session in creation order, including its exact id, " +
			"UTC target, scheduled or overdue state, and session-local delivery mode.",
		ParametersJSON: json.RawMessage(`{"type": "object", "properties": {}, "required": []}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			folded, err := foldScheduleForTool(ctx.SessionID)
			if err != nil {
				return scheduleToolErr("corrupt_schedule_log", err.Error()), nil
			}
			now := time.Now().UTC()
			views := make([]map[string]any, 0, len(folded.active))
			for _, rec := range folded.active {
				views = append(views, scheduleView(rec, now))
			}
			return views, nil
		},
	})

	r.Register(ToolDefinition{
		Name:        "schedule_delete",
		Description: "Delete one active reminder in the current session by the exact id returned by schedule_create or schedule_list. Unknown or already-finished ids return deleted false.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": { "type": "string", "description": "Exact session-local schedule id." }
			},
			"required": ["id"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return scheduleToolErr("invalid_rule", "schedule_delete id must be non-empty without surrounding whitespace."), nil
			}
			if args.ID == "" || strings.TrimSpace(args.ID) != args.ID {
				return scheduleToolErr("invalid_rule", "schedule_delete id must be non-empty without surrounding whitespace."), nil
			}
			folded, err := foldScheduleForTool(ctx.SessionID)
			if err != nil {
				return scheduleToolErr("corrupt_schedule_log", err.Error()), nil
			}
			found := false
			for _, rec := range folded.active {
				if rec.ID == args.ID {
					found = true
					break
				}
			}
			if !found {
				return map[string]any{"id": args.ID, "deleted": false, "code": "schedule_not_found"}, nil
			}
			if ctx.Emit != nil {
				ctx.Emit(session.EventScheduleChange, session.ScheduleChangePayload{
					Version:   scheduleChangeVersion,
					Operation: "delete",
					ID:        args.ID,
				})
			}
			return map[string]any{"id": args.ID, "deleted": true}, nil
		},
	})
}
