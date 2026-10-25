package bridge

import (
	"reflect"
	"strings"
	"testing"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

func fp(f float64) *float64 { return &f }

func windows() map[string]*UsageWindow {
	return map[string]*UsageWindow{
		"five_hour":                  {Utilization: fp(0.14), ResetsAt: fp(1789770600)},
		"seven_day":                  {Utilization: fp(0.01), ResetsAt: fp(1790042400)},
		"seven_day_overage_included": {Utilization: fp(0.5), ResetsAt: fp(1790042400)},
	}
}

func TestRateLimitHeaders(t *testing.T) {
	t.Run("names each unified window with Claude Code's header abbreviation", func(t *testing.T) {
		got := rateLimitHeaders(RateLimitInfo{
			Status: "allowed", ResetsAt: fp(1789770600), RateLimitType: "five_hour", UnifiedWindows: windows(),
		})
		want := map[string]string{
			"anthropic-ratelimit-unified-status":               "allowed",
			"anthropic-ratelimit-unified-reset":                "1789770600",
			"anthropic-ratelimit-unified-representative-claim": "five_hour",
			"anthropic-ratelimit-unified-5h-utilization":       "0.14",
			"anthropic-ratelimit-unified-5h-reset":             "1789770600",
			"anthropic-ratelimit-unified-7d-utilization":       "0.01",
			"anthropic-ratelimit-unified-7d-reset":             "1790042400",
			"anthropic-ratelimit-unified-7d_oi-utilization":    "0.5",
			"anthropic-ratelimit-unified-7d_oi-reset":          "1790042400",
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("headers = %v", got)
		}
	})
	t.Run("keeps a window name Claude Code has no abbreviation for", func(t *testing.T) {
		got := rateLimitHeaders(RateLimitInfo{Status: "allowed", UnifiedWindows: map[string]*UsageWindow{"seven_day_opus": {Utilization: fp(0.3)}}})
		if got["anthropic-ratelimit-unified-seven_day_opus-utilization"] != "0.3" {
			t.Fatalf("headers = %v", got)
		}
	})
	t.Run("reports a rejection in the headers", func(t *testing.T) {
		got := rateLimitHeaders(RateLimitInfo{Status: "rejected", RateLimitType: "five_hour", OverageStatus: "rejected", OverageResetsAt: fp(1790812800)})
		for name, want := range map[string]string{
			"anthropic-ratelimit-unified-status":         "rejected",
			"anthropic-ratelimit-unified-overage-status": "rejected",
			"anthropic-ratelimit-unified-overage-reset":  "1790812800",
		} {
			if got[name] != want {
				t.Fatalf("%s = %q, want %q", name, got[name], want)
			}
		}
	})
	t.Run("sets nothing for absent fields and absent windows", func(t *testing.T) {
		got := rateLimitHeaders(RateLimitInfo{UnifiedWindows: map[string]*UsageWindow{"five_hour": nil}})
		if len(got) != 0 {
			t.Fatalf("headers = %v", got)
		}
	})
}

func TestResultErrorText(t *testing.T) {
	errorResult := map[string]any{"type": "result", "subtype": "success", "is_error": true, "result": "Prompt is too long"}
	cases := []struct {
		name  string
		frame map[string]any
		want  string
		ok    bool
	}{
		{"treats is_error on a success-shaped result as a failure", errorResult, "Prompt is too long", true},
		{"returns not-ok for a genuine success", map[string]any{"subtype": "success", "is_error": false, "result": "a summary"}, "", false},
		{"joins errors[] for the dedicated error subtypes", map[string]any{"subtype": "error_during_execution", "errors": []any{"boom", "bang"}}, "boom\nbang", true},
		{"names an empty is_error result", map[string]any{"subtype": "success", "is_error": true, "result": ""}, "Claude Code reported an error", true},
		{"names a subtype with no detail", map[string]any{"subtype": "error_max_budget_usd"}, "Claude Code failed: error_max_budget_usd", true},
		{"names an empty errors[]", map[string]any{"subtype": "error_during_execution", "errors": []any{}}, "Claude Code failed: error_during_execution", true},
		{"uses a string error field", map[string]any{"subtype": "error_during_execution", "error": "bad"}, "bad", true},
		{"names a result with no subtype", map[string]any{}, "Claude Code failed: unknown result", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := resultErrorText(c.frame)
			if got != c.want || ok != c.ok {
				t.Fatalf("got (%q, %v), want (%q, %v)", got, ok, c.want, c.ok)
			}
		})
	}
}

func useUTC(t *testing.T) {
	t.Helper()
	saved := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = saved })
}

func TestDescribeRateLimitFailure(t *testing.T) {
	useUTC(t)
	const limitText = "You're out of extra usage · resets 6:30pm (America/New_York)"
	t.Run("is named as a rate limit so fallback chains fire", func(t *testing.T) {
		got := describeRateLimitFailure(rateLimitRejection{RateLimitType: "five_hour", ResetsAt: fp(1786141800)}, limitText)
		want := "Claude quota exceeded — rate limit (five_hour) — resets 10:30:00 PM: " + limitText
		if got != want {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("omits the type and reset time the rejection lacks", func(t *testing.T) {
		got := describeRateLimitFailure(rateLimitRejection{}, "x")
		if got != "Claude quota exceeded — rate limit: x" {
			t.Fatalf("got %q", got)
		}
	})
}

func rateLimitQuery() *QueryContext {
	q := newQueryContext()
	q.currentPiStream = &sdk.ModelEventStream{}
	return q
}

func TestReportHeldRateLimit(t *testing.T) {
	t.Run("writes the held headers to the debug log and clears the event", func(t *testing.T) {
		b, _, agentDir := newTestBridge(t, true)
		q := rateLimitQuery()
		q.heldRateLimit = &RateLimitInfo{Status: "allowed", RateLimitType: "five_hour", UnifiedWindows: map[string]*UsageWindow{"five_hour": {Utilization: fp(0.1), ResetsAt: fp(1789770600)}}}
		b.reportHeldRateLimit(q)
		want := `rate limit: headers {"anthropic-ratelimit-unified-5h-reset":"1789770600","anthropic-ratelimit-unified-5h-utilization":"0.1","anthropic-ratelimit-unified-representative-claim":"five_hour","anthropic-ratelimit-unified-status":"allowed"}`
		if log := readDebugLog(t, agentDir); !strings.Contains(log, want) {
			t.Fatalf("log = %s", log)
		}
		if q.heldRateLimit != nil {
			t.Fatal("held event not cleared")
		}
	})
	t.Run("a call whose stream has ended hears nothing and keeps the event held", func(t *testing.T) {
		b, _, _ := newTestBridge(t, true)
		q := newQueryContext()
		q.heldRateLimit = &RateLimitInfo{Status: "allowed"}
		b.reportHeldRateLimit(q)
		if q.heldRateLimit == nil {
			t.Fatal("held event dropped")
		}
	})
	t.Run("a newer event replaces a held one", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		q := newQueryContext()
		b.handleRateLimitEvent(&recordingHost{}, q, RateLimitInfo{Status: "allowed", Utilization: fp(0.05)})
		b.handleRateLimitEvent(&recordingHost{}, q, RateLimitInfo{Status: "allowed", Utilization: fp(0.1)})
		if q.heldRateLimit == nil || *q.heldRateLimit.Utilization != 0.1 {
			t.Fatalf("held = %+v", q.heldRateLimit)
		}
	})
}

func TestHandleRateLimitEvent(t *testing.T) {
	useUTC(t)
	warn := func(u float64, thr float64) RateLimitInfo {
		return RateLimitInfo{Status: "allowed_warning", RateLimitType: "five_hour", Utilization: fp(u), SurpassedThreshold: fp(thr)}
	}
	t.Run("a rejection notifies with the reset time and is recorded", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		q, h := rateLimitQuery(), &recordingHost{}
		b.handleRateLimitEvent(h, q, RateLimitInfo{Status: "rejected", RateLimitType: "five_hour", ResetsAt: fp(1786141800)})
		want := []string{"warning: Claude subscription rate limit reached (five_hour) — resets at 10:30:00 PM"}
		if !reflect.DeepEqual(h.notices, want) {
			t.Fatalf("notices = %q", h.notices)
		}
		if q.rateLimitRejection == nil || q.rateLimitRejection.RateLimitType != "five_hour" || *q.rateLimitRejection.ResetsAt != 1786141800 {
			t.Fatalf("rejection = %+v", q.rateLimitRejection)
		}
	})
	t.Run("a rejection without type or reset says unknown", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		h := &recordingHost{}
		b.handleRateLimitEvent(h, newQueryContext(), RateLimitInfo{Status: "rejected"})
		want := []string{"warning: Claude subscription rate limit reached (unknown) — resets at unknown"}
		if !reflect.DeepEqual(h.notices, want) {
			t.Fatalf("notices = %q", h.notices)
		}
	})
	t.Run("warns once per 5% step and again when the threshold changes", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		q, h := newQueryContext(), &recordingHost{}
		for _, info := range []RateLimitInfo{warn(0.76, 0.75), warn(0.77, 0.75), warn(0.79, 0.75), warn(0.8, 0.75), warn(0.8, 0.9), warn(0.7, 0.9)} {
			b.handleRateLimitEvent(h, q, info)
		}
		want := []string{
			"warning: Claude rate limit warning: 76% used (five_hour)",
			"warning: Claude rate limit warning: 80% used (five_hour)",
			"warning: Claude rate limit warning: 80% used (five_hour)",
		}
		if !reflect.DeepEqual(h.notices, want) {
			t.Fatalf("notices = %q", h.notices)
		}
	})
	t.Run("an allowed event re-arms the warning", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		q, h := newQueryContext(), &recordingHost{}
		b.handleRateLimitEvent(h, q, warn(0.8, 0.75))
		b.handleRateLimitEvent(h, q, RateLimitInfo{Status: "allowed"})
		if q.lastRateLimitWarnStep != nil || q.lastRateLimitWarnThreshold != nil {
			t.Fatal("dedupe not re-armed")
		}
		b.handleRateLimitEvent(h, q, warn(0.8, 0.75))
		if len(h.notices) != 2 {
			t.Fatalf("notices = %q", h.notices)
		}
	})
	t.Run("a rejection re-arms the warning", func(t *testing.T) {
		b, _, _ := newTestBridge(t, false)
		q, h := newQueryContext(), &recordingHost{}
		b.handleRateLimitEvent(h, q, warn(0.8, 0.75))
		b.handleRateLimitEvent(h, q, RateLimitInfo{Status: "rejected"})
		if q.lastRateLimitWarnStep != nil || q.lastRateLimitWarnThreshold != nil {
			t.Fatal("dedupe not re-armed")
		}
	})
}
