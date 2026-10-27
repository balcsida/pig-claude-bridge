package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccproto"
)

const rateLimitHeaderPrefix = "anthropic-ratelimit-unified"

// windowAbbreviations is Claude Code's own window-name → header-abbreviation table.
var windowAbbreviations = map[string]string{
	"five_hour":                  "5h",
	"seven_day":                  "7d",
	"seven_day_overage_included": "7d_oi",
}

// UsageWindow is one unified rate-limit window. Nil fields were absent.
type UsageWindow struct {
	Utilization *float64
	ResetsAt    *float64
}

// RateLimitInfo is the rate_limit_event payload (rate-limit.ts RateLimitInfo plus
// the utilization and surpassedThreshold the consume loop reads, index.ts:1569).
// Optional strings use "" for absent, optional numbers use nil, and a nil map
// value is an absent window.
type RateLimitInfo struct {
	Status             string
	ResetsAt           *float64
	RateLimitType      string
	OverageStatus      string
	OverageResetsAt    *float64
	Utilization        *float64
	SurpassedThreshold *float64
	UnifiedWindows     map[string]*UsageWindow
}

// formatHeaderNumber renders a number as JavaScript's String(n) does for the
// magnitudes Claude Code sends (no exponent form).
func formatHeaderNumber(n float64) string { return strconv.FormatFloat(n, 'f', -1, 64) }

// rateLimitHeaders rebuilds the unified headers Claude Code can supply
// (rate-limit.ts rateLimitResponse; the status is always 200 there, so it is not
// returned). Only the headers that have a value are set.
func rateLimitHeaders(info RateLimitInfo) map[string]string {
	headers := map[string]string{}
	set := func(name, value string, present bool) {
		if present {
			headers[rateLimitHeaderPrefix+"-"+name] = value
		}
	}
	setNumber := func(name string, n *float64) {
		if n != nil {
			set(name, formatHeaderNumber(*n), true)
		}
	}
	set("status", info.Status, info.Status != "")
	setNumber("reset", info.ResetsAt)
	set("representative-claim", info.RateLimitType, info.RateLimitType != "")
	set("overage-status", info.OverageStatus, info.OverageStatus != "")
	setNumber("overage-reset", info.OverageResetsAt)
	for name, window := range info.UnifiedWindows {
		if window == nil {
			continue
		}
		abbreviation, ok := windowAbbreviations[name]
		if !ok {
			abbreviation = name
		}
		setNumber(abbreviation+"-utilization", window.Utilization)
		setNumber(abbreviation+"-reset", window.ResetsAt)
	}
	return headers
}

// resultErrorText is the failure text for an SDK result frame, or ok=false when it
// succeeded (index.ts resultErrorText). CC reports API failures (429 capacity,
// overload, prompt-too-long) with is_error on an otherwise success-shaped result;
// the dedicated error subtypes carry errors instead.
func resultErrorText(frame map[string]any) (text string, ok bool) {
	subtype, hasSubtype := frame["subtype"].(string)
	if subtype == "success" {
		if isError, _ := frame["is_error"].(bool); !isError {
			return "", false
		}
		if result, _ := frame["result"].(string); result != "" {
			return result, true
		}
		return "Claude Code reported an error", true
	}
	if errs, _ := frame["errors"].([]any); len(errs) > 0 {
		parts := make([]string, len(errs))
		for i, e := range errs {
			parts[i] = fmt.Sprint(e) // errors is string[] in the SDK; JavaScript's String() differs only for other types
		}
		return strings.Join(parts, "\n"), true
	}
	if s, isString := frame["error"].(string); isString {
		return s, true
	}
	if !hasSubtype {
		subtype = "unknown result"
	}
	return "Claude Code failed: " + subtype, true
}

// formatResetTime is the en-US rendering of JavaScript's toLocaleTimeString() in
// local time. resetsAt is Unix seconds (unit undocumented in the SDK; observed).
func formatResetTime(resetsAt float64) string {
	return time.Unix(int64(resetsAt), 0).Format("3:04:05 PM")
}

// describeRateLimitFailure names a failure as a rate limit when a rejection
// preceded it (index.ts describeRateLimitFailure). Leading with "Claude quota
// exceeded — rate limit" keeps both phrases in a truncated render and makes pi
// hand the rejected window to fallback instead of retrying it (issues #58, #183).
func describeRateLimitFailure(rej rateLimitRejection, failure string) string {
	kind := ""
	if rej.RateLimitType != "" {
		kind = " (" + rej.RateLimitType + ")"
	}
	resets := ""
	if rej.ResetsAt != nil && *rej.ResetsAt != 0 {
		resets = " — resets " + formatResetTime(*rej.ResetsAt)
	}
	return "Claude quota exceeded — rate limit" + kind + resets + ": " + failure
}

// reportHeldRateLimit takes the held event of the call that owns the open stream
// and clears it. PiG offers no after_provider_response (D1), so the headers go to
// the debug log where the TypeScript handed them to onResponse.
func (b *Bridge) reportHeldRateLimit(q *QueryContext) {
	q.mu.Lock()
	info := q.heldRateLimit
	if info == nil || q.currentPiStream == nil {
		q.mu.Unlock()
		return
	}
	q.heldRateLimit = nil
	q.mu.Unlock()
	headers, err := json.Marshal(rateLimitHeaders(*info))
	if err != nil {
		b.debugf("rate limit: headers unmarshalable: %v", err)
		return
	}
	b.debugf("rate limit: headers %s", headers)
}

// handleRateLimitEvent is the consume loop's reaction to a rate_limit_event frame
// (index.ts:1550-1582): hold and report the event, then notify the user and keep
// the dedupe bookkeeping.
func (b *Bridge) handleRateLimitEvent(host Host, q *QueryContext, info RateLimitInfo) {
	q.mu.Lock()
	q.heldRateLimit = &info
	q.mu.Unlock()
	b.reportHeldRateLimit(q)

	q.mu.Lock()
	notice := ""
	switch info.Status {
	case "rejected":
		// Held so the failure Claude Code sends next can be named as a rate limit.
		q.rateLimitRejection = &rateLimitRejection{RateLimitType: info.RateLimitType, ResetsAt: info.ResetsAt}
		// The notice supersedes warnings; re-arm so the next window's warnings fire.
		q.lastRateLimitWarnStep, q.lastRateLimitWarnThreshold = nil, nil
		resetsAt := "unknown"
		if info.ResetsAt != nil && *info.ResetsAt != 0 {
			resetsAt = formatResetTime(*info.ResetsAt)
		}
		kind := info.RateLimitType
		if kind == "" {
			kind = "unknown"
		}
		notice = fmt.Sprintf("Claude subscription rate limit reached (%s) — resets at %s", kind, resetsAt)
	case "allowed":
		// Back under the threshold (window reset): re-arm the warning dedupe.
		q.lastRateLimitWarnStep, q.lastRateLimitWarnThreshold = nil, nil
	case "allowed_warning":
		// utilization is a fraction (0..1). The SDK emits one event per request, so
		// only re-notify when the level rises past a new 5% step or the threshold changes.
		utilization := 0.0
		if info.Utilization != nil {
			utilization = *info.Utilization
		}
		percent := int(math.Floor(utilization*100 + 0.5)) // Math.round
		step := percent / 5
		rose := q.lastRateLimitWarnStep == nil || step > *q.lastRateLimitWarnStep
		if rose || !sameFloat(info.SurpassedThreshold, q.lastRateLimitWarnThreshold) {
			q.lastRateLimitWarnStep = &step
			q.lastRateLimitWarnThreshold = info.SurpassedThreshold
			notice = fmt.Sprintf("Claude rate limit warning: %d%% used (%s)", percent, info.RateLimitType)
		}
	}
	q.mu.Unlock()
	if notice != "" && host != nil { // the TypeScript's piUI?.notify
		host.Notify(notice, "warning")
	}
	q.mu.Lock()
	client := q.activeQuery
	q.mu.Unlock()
	b.showUsage(host, client, info.UnifiedWindows)
}

// showUsage puts the plan's usage in the footer status. The event's windows lack the
// per-model limits' names, so it asks the live CLI for its /usage rows and falls back
// to the windows when there is no client or the CLI does not answer.
func (b *Bridge) showUsage(host Host, client ccproto.Client, windows map[string]*UsageWindow) {
	s, ok := host.(StatusSetter)
	if !ok {
		return
	}
	set := func(status string) {
		if status != "" {
			s.SetStatus(UsageStatusKey, status)
		}
	}
	if client == nil {
		set(usageStatus(windows, b.deps.Now()))
		return
	}
	b.queries.Add(1)
	go func() {
		defer b.queries.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		reply, err := client.Usage(ctx)
		status := planUsageStatus(reply, b.deps.Now())
		if err != nil || status == "" {
			b.debugf("rate limit: get_usage unusable (%v), showing the event's windows", err)
			status = usageStatus(windows, b.deps.Now())
		}
		set(status)
	}()
}

// UsageStatusKey is the footer status entry that shows the plan's usage windows.
const UsageStatusKey = "claude-usage"

// StatusSetter is a Host that can show a footer status; hosts without one skip it.
type StatusSetter interface{ SetStatus(key, text string) }

// usageStatus renders the unified windows as "Claude 5h 14% ↻3h · 7d 1% ↻2d", or ""
// when no window carries a utilization.
func usageStatus(windows map[string]*UsageWindow, now time.Time) string {
	var parts []string
	for _, name := range slices.Sorted(maps.Keys(windows)) {
		w := windows[name]
		if w == nil || w.Utilization == nil {
			continue
		}
		label, ok := windowAbbreviations[name]
		if !ok {
			label = name
		}
		var resetsAt time.Time
		if w.ResetsAt != nil {
			resetsAt = time.Unix(int64(*w.ResetsAt), 0)
		}
		parts = append(parts, fmt.Sprintf("%s %d%%", label, int(math.Floor(*w.Utilization*100+0.5)))+resetsIn(resetsAt, now))
	}
	if len(parts) == 0 {
		return ""
	}
	return "Claude " + strings.Join(parts, " · ")
}

// resetsIn is " ↻3h": the time to a window's reset, floored to its largest unit as the
// Claude app shows it, or "" when the reset is unknown or past.
func resetsIn(at, now time.Time) string {
	d := at.Sub(now)
	switch {
	case at.IsZero() || d <= 0:
		return ""
	case d < time.Hour:
		return fmt.Sprintf(" ↻%dm", max(1, int(d/time.Minute)))
	case d < 24*time.Hour:
		return fmt.Sprintf(" ↻%dh", int(d/time.Hour))
	}
	return fmt.Sprintf(" ↻%dd", int(d/(24*time.Hour)))
}

// usageWindow is one keyed limit of a get_usage reply; utilization is a percentage.
type usageWindow struct {
	DisplayName string    `json:"display_name"`
	Utilization float64   `json:"utilization"`
	ResetsAt    time.Time `json:"resets_at"`
}

// usageReply is the part of a get_usage reply the footer reads. It reads the keyed
// windows, not the labelled limits[] rows, which the server sometimes leaves out.
type usageReply struct {
	RateLimits *struct {
		FiveHour    *usageWindow  `json:"five_hour"`
		SevenDay    *usageWindow  `json:"seven_day"`
		ModelScoped []usageWindow `json:"model_scoped"`
		CedarEmber  any           `json:"cedar_ember"`
	} `json:"rate_limits"`
}

// planUsageStatus renders a get_usage reply as
// "Claude 5h 6% ↻3h · 7d 76% ↻8h · Fable 99% ↻8h", plus "reset available" when the
// account holds an unused rate-limit reset. "" when the reply has no windows.
func planUsageStatus(raw json.RawMessage, now time.Time) string {
	var r usageReply
	if json.Unmarshal(raw, &r) != nil || r.RateLimits == nil {
		return ""
	}
	var parts []string
	add := func(label string, w *usageWindow) {
		if w != nil && label != "" {
			parts = append(parts, fmt.Sprintf("%s %d%%", label, int(math.Round(w.Utilization)))+resetsIn(w.ResetsAt, now))
		}
	}
	add("5h", r.RateLimits.FiveHour)
	add("7d", r.RateLimits.SevenDay)
	for i := range r.RateLimits.ModelScoped {
		add(r.RateLimits.ModelScoped[i].DisplayName, &r.RateLimits.ModelScoped[i])
	}
	if len(parts) == 0 {
		return ""
	}
	if holdsUnusedReset(r.RateLimits.CedarEmber) {
		parts = append(parts, "reset available")
	}
	return "Claude " + strings.Join(parts, " · ")
}

// holdsUnusedReset reports whether the usage reply's cedar_ember block (the reset
// grants /reset_rate_limits claims) holds a grant with resets left.
// shortcut: the nesting of grants in the block is unobserved (it was null on the probed
// account), so any resets_left above 0 counts; pin it once a live reply carries a grant.
func holdsUnusedReset(v any) bool {
	switch v := v.(type) {
	case map[string]any:
		if n, ok := v["resets_left"].(float64); ok && n > 0 {
			return true
		}
		for _, child := range v {
			if holdsUnusedReset(child) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if holdsUnusedReset(child) {
				return true
			}
		}
	}
	return false
}

func sameFloat(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
