package auth

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	// QuotaLimitPercentKey is the legacy shared percentage-used ceiling.
	QuotaLimitPercentKey             = "quota_limit_percent"
	QuotaFiveHourRemainingPercentKey = "quota_5h_remaining_percent"
	QuotaWeeklyRemainingPercentKey   = "quota_weekly_remaining_percent"
)

// QuotaLimitPercent returns the optional credential-wide utilization ceiling.
// Zero means that only upstream quota limits apply.
func QuotaLimitPercent(a *Auth) float64 {
	value, _ := quotaPercent(a, QuotaLimitPercentKey)
	return value
}

func quotaPercent(a *Auth, key string) (float64, bool) {
	if a == nil {
		return 0, false
	}
	var raw string
	switch v := a.Metadata[key].(type) {
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
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 100 {
		return 0, false
	}
	return n, true
}

// quotaWindowLimit returns the percentage-used ceiling for one window. Explicit
// zero disables that window's reserve, overriding the legacy shared ceiling.
func quotaWindowLimit(a *Auth, key string) (float64, bool) {
	if key != "" {
		if remaining, ok := quotaPercent(a, key); ok {
			return 100 - remaining, remaining > 0
		}
	}
	limit := QuotaLimitPercent(a)
	return limit, limit > 0
}

// QuotaRemainingPercent returns the effective reserve for one of the two
// remaining-percent metadata keys, translating a legacy ceiling when needed.
func QuotaRemainingPercent(a *Auth, key string) float64 {
	if remaining, ok := quotaPercent(a, key); ok {
		return remaining
	}
	if limit := QuotaLimitPercent(a); limit > 0 {
		return 100 - limit
	}
	return 0
}

// HasQuotaLimits reports whether quota observations can affect any window's routing.
func HasQuotaLimits(a *Auth) bool {
	_, fiveHour := quotaWindowLimit(a, QuotaFiveHourRemainingPercentKey)
	_, weekly := quotaWindowLimit(a, QuotaWeeklyRemainingPercentKey)
	return fiveHour || weekly
}

// QuotaLimitReached checks every known account-wide window. Expired windows
// cease blocking even when the account has not made another upstream request.
func QuotaLimitReached(a *Auth, now time.Time) (bool, time.Time) {
	if !HasQuotaLimits(a) {
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
	check := func(key, raw string, scale float64, reset time.Time) {
		limit, enabled := quotaWindowLimit(a, key)
		if !enabled {
			return
		}
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
		for _, window := range []struct{ prefix, key string }{
			{"5h", QuotaFiveHourRemainingPercentKey},
			{"7d", QuotaWeeklyRemainingPercentKey},
		} {
			prefix := "Anthropic-Ratelimit-Unified-" + window.prefix
			check(window.key, signal(prefix+"-Utilization"), 100, parseTime(signal(prefix+"-Reset")))
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
			key := QuotaFiveHourRemainingPercentKey
			if window == "Secondary" {
				key = QuotaWeeklyRemainingPercentKey
			}
			// Codex can report the weekly quota as Primary on some plans.
			// Prefer the advertised duration; use the conventional order only
			// when the duration is absent.
			if minutes := signal(prefix + "-Window-Minutes"); minutes != "" {
				switch minutes {
				case "300":
					key = QuotaFiveHourRemainingPercentKey
				case "10080":
					key = QuotaWeeklyRemainingPercentKey
				default:
					key = ""
				}
			}
			check(key, signal(prefix+"-Used-Percent"), 1, reset)
		}
	}
	if unknownReset {
		return blocked, time.Time{}
	}
	return blocked, recoverAt
}
