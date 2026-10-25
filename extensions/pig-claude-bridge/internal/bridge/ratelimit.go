package bridge

// UsageWindow is one unified rate-limit window. Nil fields were absent.
type UsageWindow struct {
	Utilization *float64
	ResetsAt    *float64
}

// RateLimitInfo is the rate_limit_event payload (rate-limit.ts RateLimitInfo).
// Optional strings use "" for absent, optional numbers use nil, and a nil map
// value is an absent window.
type RateLimitInfo struct {
	Status          string
	ResetsAt        *float64
	RateLimitType   string
	OverageStatus   string
	OverageResetsAt *float64
	UnifiedWindows  map[string]*UsageWindow
}
