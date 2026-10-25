package auth

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"
)

const QuotaLimitPercentKey = "quota_limit_percent"

// QuotaLimitPercent returns the optional credential-wide utilization ceiling.
// Zero means that only upstream quota limits apply.
func QuotaLimitPercent(a *Auth) float64 {
	if a == nil {
		return 0
	}
	var raw string
	switch v := a.Metadata[QuotaLimitPercentKey].(type) {
	case float64:
		raw = strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		raw = strconv.Itoa(v)
	case json.Number:
		raw = v.String()
	case string:
		raw = v
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 || n > 100 {
		return 0
	}
	return n
}

// QuotaLimitReached checks every known account-wide window. Expired windows
// cease blocking even when the account has not made another upstream request.
func QuotaLimitReached(a *Auth, now time.Time) (bool, time.Time) {
	limit := QuotaLimitPercent(a)
	if limit == 0 {
		return false, time.Time{}
	}
	signal := func(name string) string {
		for k, v := range a.Quota.Signals {
			if strings.EqualFold(k, name) {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	parseTime := func(raw string) time.Time {
		if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			return t
		}
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
			return time.Unix(n, 0)
		}
		return time.Time{}
	}
	blocked, unknownReset := false, false
	var recoverAt time.Time
	check := func(raw string, scale float64, reset time.Time) {
		used, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(used) || math.IsInf(used, 0) || used*scale < limit {
			return
		}
		if !reset.IsZero() && !reset.After(now) {
			return
		}
		blocked = true
		if reset.IsZero() {
			unknownReset = true
		} else if reset.After(recoverAt) {
			recoverAt = reset
		}
	}
	switch strings.ToLower(strings.TrimSpace(a.Provider)) {
	case "claude":
		for _, window := range []string{"5h", "7d"} {
			prefix := "Anthropic-Ratelimit-Unified-" + window
			check(signal(prefix+"-Utilization"), 100, parseTime(signal(prefix+"-Reset")))
		}
	case "codex":
		for _, window := range []string{"Primary", "Secondary"} {
			prefix := "X-Codex-" + window
			reset := parseTime(signal(prefix + "-Reset-At"))
			if reset.IsZero() && !a.Quota.ObservedAt.IsZero() {
				if seconds, err := strconv.ParseFloat(signal(prefix+"-Reset-After-Seconds"), 64); err == nil && !math.IsNaN(seconds) && !math.IsInf(seconds, 0) && seconds >= 0 && seconds < 365*24*3600 {
					reset = a.Quota.ObservedAt.Add(time.Duration(seconds * float64(time.Second)))
				}
			}
			check(signal(prefix+"-Used-Percent"), 1, reset)
		}
	}
	if unknownReset {
		return blocked, time.Time{}
	}
	return blocked, recoverAt
}
