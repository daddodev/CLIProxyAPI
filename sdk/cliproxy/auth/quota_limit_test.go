package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestQuotaLimitRoutingAndReset(t *testing.T) {
	now := time.Unix(1800000000, 0)
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			a := &Auth{ID: "a", Provider: provider, Attributes: map[string]string{"priority": "10"}, Metadata: map[string]any{QuotaLimitPercentKey: 80.0}}
			h := make(http.Header)
			reset := now.Add(time.Hour)
			if provider == "claude" {
				h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.8")
				h.Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(reset.Unix(), 10))
			} else {
				h.Set("X-Codex-Primary-Used-Percent", "80")
				h.Set("X-Codex-Primary-Reset-After-Seconds", "3600")
			}
			a.Quota.ObserveResponseHeadersForProvider(provider, h, now)
			blocked, reason, until := isAuthBlockedForModel(a, "model", now)
			if !blocked || reason != blockReasonCooldown || !until.Equal(reset) {
				t.Fatalf("blocked=%v reason=%v until=%v", blocked, reason, until)
			}
			if !isAuthBlockedByQuotaOrAuthForModel(a, "model", now) {
				t.Fatal("affinity must release account")
			}
			if blocked, _, _ := isAuthBlockedForModel(a, "model", reset); blocked {
				t.Fatal("expired quota still blocks")
			}
			a.Metadata[QuotaLimitPercentKey] = 81.0
			if blocked, _, _ := isAuthBlockedForModel(a, "model", now); blocked {
				t.Fatal("below ceiling blocked")
			}
		})
	}
}

func TestQuotaLimitChecksEveryWindow(t *testing.T) {
	now := time.Now()
	a := &Auth{Provider: "codex", Metadata: map[string]any{QuotaLimitPercentKey: 75.0}, Quota: QuotaState{Signals: map[string]string{
		"X-Codex-Primary-Used-Percent": "90", "X-Codex-Primary-Reset-At": now.Add(time.Hour).Format(time.RFC3339),
		"X-Codex-Secondary-Used-Percent": "80", "X-Codex-Secondary-Reset-At": now.Add(24 * time.Hour).Format(time.RFC3339),
	}}}
	blocked, reset := QuotaLimitReached(a, now)
	if !blocked || reset.Unix() != now.Add(24*time.Hour).Unix() {
		t.Fatalf("blocked=%v reset=%v", blocked, reset)
	}
	delete(a.Quota.Signals, "X-Codex-Secondary-Reset-At")
	if blocked, reset = QuotaLimitReached(a, now); !blocked || !reset.IsZero() {
		t.Fatal("unknown reset should remain blocked")
	}
	for _, raw := range []any{nil, 0.0, -1.0, 101.0, "NaN", "bad"} {
		a.Metadata[QuotaLimitPercentKey] = raw
		if blocked, _ := QuotaLimitReached(a, now); blocked {
			t.Fatalf("invalid/unset limit %v blocked", raw)
		}
	}
}

func TestFillFirstSkipsLimitedAndDisabledAccounts(t *testing.T) {
	a := &Auth{ID: "a", Provider: "codex", Attributes: map[string]string{"priority": "10"}, Metadata: map[string]any{QuotaLimitPercentKey: 80.0}, Quota: QuotaState{Signals: map[string]string{"X-Codex-Primary-Used-Percent": "80"}}}
	b := &Auth{ID: "b", Provider: "codex", Disabled: true, Attributes: map[string]string{"priority": "5"}}
	c := &Auth{ID: "c", Provider: "codex"}
	selected, err := (&FillFirstSelector{}).Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, []*Auth{a, b, c})
	if err != nil || selected.ID != "c" {
		t.Fatalf("selected=%v err=%v", selected, err)
	}
}

func TestQuotaProbeObservationRejectsStaleData(t *testing.T) {
	m := NewManager(nil, &FillFirstSelector{}, nil)
	now := time.Now()
	a, err := m.Register(context.Background(), &Auth{ID: "probe", Provider: "codex", Metadata: map[string]any{QuotaLimitPercentKey: 80.0}})
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{"X-Codex-Primary-Used-Percent": []string{"90"}}
	m.ObserveQuotaHeaders(a.ID, a.RegistrationEpoch, headers, now)
	updated, _ := m.GetByID(a.ID)
	if blocked, _ := QuotaLimitReached(updated, now); !blocked {
		t.Fatal("probe not used for routing")
	}
	headers.Set("X-Codex-Primary-Used-Percent", "0")
	m.ObserveQuotaHeaders(a.ID, a.RegistrationEpoch, headers, now.Add(-time.Second))
	updated, _ = m.GetByID(a.ID)
	if blocked, _ := QuotaLimitReached(updated, now); !blocked {
		t.Fatal("older probe replaced current observation")
	}
	m.ObserveQuotaHeaders(a.ID, a.RegistrationEpoch+1, headers, now.Add(time.Second))
	updated, _ = m.GetByID(a.ID)
	if blocked, _ := QuotaLimitReached(updated, now); !blocked {
		t.Fatal("wrong credential epoch replaced observation")
	}
	m.ObserveQuotaHeaders(a.ID, a.RegistrationEpoch, headers, now.Add(2*time.Second))
	updated, _ = m.GetByID(a.ID)
	if blocked, _ := QuotaLimitReached(updated, now); blocked {
		t.Fatal("fresh probe failed to restore availability")
	}
}

func TestQuotaRemainingLimitsIndependentWindows(t *testing.T) {
	now := time.Unix(1800000000, 0)
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			for _, tc := range []struct {
				name                                                     string
				fiveHourReserve, weeklyReserve, fiveHourUsed, weeklyUsed float64
				blocked                                                  bool
			}{
				{"below both", 20, 10, 79.9, 89.9, false},
				{"five hour boundary", 20, 10, 80, 32, true},
				{"weekly boundary", 20, 10, 0, 90, true},
				{"five hour only", 20, 0, 79, 100, false},
				{"weekly only", 0, 10, 100, 89, false},
				{"both disabled", 0, 0, 100, 100, false},
				{"preserve all five hour", 100, 0, 0, 0, true},
				{"preserve all weekly", 0, 100, 0, 0, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					a := &Auth{Provider: provider, Metadata: map[string]any{
						QuotaFiveHourRemainingPercentKey: tc.fiveHourReserve,
						QuotaWeeklyRemainingPercentKey:   tc.weeklyReserve,
					}}
					headers := make(http.Header)
					reset := now.Add(time.Hour).Format(time.RFC3339)
					if provider == "claude" {
						headers.Set("Anthropic-Ratelimit-Unified-5h-Utilization", strconv.FormatFloat(tc.fiveHourUsed/100, 'f', -1, 64))
						headers.Set("Anthropic-Ratelimit-Unified-7d-Utilization", strconv.FormatFloat(tc.weeklyUsed/100, 'f', -1, 64))
						headers.Set("Anthropic-Ratelimit-Unified-5h-Reset", reset)
						headers.Set("Anthropic-Ratelimit-Unified-7d-Reset", reset)
					} else {
						headers.Set("X-Codex-Primary-Used-Percent", strconv.FormatFloat(tc.fiveHourUsed, 'f', -1, 64))
						headers.Set("X-Codex-Secondary-Used-Percent", strconv.FormatFloat(tc.weeklyUsed, 'f', -1, 64))
						headers.Set("X-Codex-Primary-Reset-At", reset)
						headers.Set("X-Codex-Secondary-Reset-At", reset)
					}
					a.Quota.ObserveResponseHeadersForProvider(provider, headers, now)
					if blocked, _ := QuotaLimitReached(a, now); blocked != tc.blocked {
						t.Fatalf("blocked=%v want=%v", blocked, tc.blocked)
					}
					if blocked, _ := QuotaLimitReached(a, now.Add(time.Hour)); blocked {
						t.Fatal("expired windows still block")
					}
				})
			}
		})
	}
}

func TestQuotaRemainingLimitsLegacyOverridesAndUnknownUsage(t *testing.T) {
	a := &Auth{Provider: "codex", Metadata: map[string]any{QuotaLimitPercentKey: 80.0}}
	if got := QuotaRemainingPercent(a, QuotaFiveHourRemainingPercentKey); got != 20 {
		t.Fatalf("legacy reserve=%v", got)
	}
	a.Metadata[QuotaFiveHourRemainingPercentKey] = 0.0
	a.Metadata[QuotaWeeklyRemainingPercentKey] = 20.1
	if got := QuotaRemainingPercent(a, QuotaFiveHourRemainingPercentKey); got != 0 {
		t.Fatalf("explicit zero did not override legacy: %v", got)
	}
	if got := QuotaRemainingPercent(a, QuotaWeeklyRemainingPercentKey); got != 20.1 {
		t.Fatalf("weekly reserve=%v", got)
	}
	now := time.Unix(1800000000, 0)
	if blocked, _ := QuotaLimitReached(a, now); blocked {
		t.Fatal("unknown usage should not block")
	}
	a.Quota.Signals = map[string]string{"X-Codex-Primary-Used-Percent": "100", "X-Codex-Secondary-Used-Percent": "79.8"}
	if blocked, _ := QuotaLimitReached(a, now); blocked {
		t.Fatal("disabled five hour limit blocked account")
	}
	a.Quota.Signals["X-Codex-Secondary-Used-Percent"] = "79.9"
	if blocked, _ := QuotaLimitReached(a, now); !blocked {
		t.Fatal("weekly reserve did not block at boundary")
	}
	a.Metadata[QuotaWeeklyRemainingPercentKey] = 0
	if HasQuotaLimits(a) {
		t.Fatal("explicit zeros did not disable legacy limits")
	}
	for _, invalid := range []any{-1.0, 101.0, "NaN", "bad"} {
		a.Metadata[QuotaWeeklyRemainingPercentKey] = invalid
		if got := QuotaRemainingPercent(a, QuotaWeeklyRemainingPercentKey); got != 20 {
			t.Fatalf("invalid reserve %v should fall back to legacy: %v", invalid, got)
		}
	}
}

func TestQuotaRemainingLimitsCodexWindowDuration(t *testing.T) {
	now := time.Unix(1800000000, 0)
	a := &Auth{Provider: "codex", Metadata: map[string]any{
		QuotaFiveHourRemainingPercentKey: 20.0,
		QuotaWeeklyRemainingPercentKey:   10.0,
	}, Quota: QuotaState{Signals: map[string]string{
		"X-Codex-Primary-Used-Percent": "85", "X-Codex-Primary-Window-Minutes": "10080",
		"X-Codex-Secondary-Used-Percent": "79", "X-Codex-Secondary-Window-Minutes": "300",
	}}}
	if blocked, _ := QuotaLimitReached(a, now); blocked {
		t.Fatal("primary weekly quota used five hour reserve")
	}
	a.Quota.Signals["X-Codex-Secondary-Used-Percent"] = "80"
	if blocked, _ := QuotaLimitReached(a, now); !blocked {
		t.Fatal("secondary five hour quota did not use five hour reserve")
	}
	a.Quota.Signals["X-Codex-Secondary-Window-Minutes"] = "1440"
	if blocked, _ := QuotaLimitReached(a, now); blocked {
		t.Fatal("unrelated duration used five hour reserve")
	}
}

func TestQuotaRemainingLimitReleasesAffinityAndSelectsFallback(t *testing.T) {
	now := time.Unix(1800000000, 0)
	a := &Auth{ID: "main", Provider: "claude", Attributes: map[string]string{"priority": "2"}, Metadata: map[string]any{
		QuotaFiveHourRemainingPercentKey: 20.0,
		QuotaWeeklyRemainingPercentKey:   10.0,
	}, Quota: QuotaState{Signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Utilization": "0.9"}}}
	b := &Auth{ID: "backup", Provider: "claude", Attributes: map[string]string{"priority": "1"}}
	if !isAuthBlockedByQuotaOrAuthForModel(a, "model", now) {
		t.Fatal("affinity must release a reserve-blocked account")
	}
	selected, err := (&FillFirstSelector{}).Pick(context.Background(), "claude", "model", cliproxyexecutor.Options{}, []*Auth{a, b})
	if err != nil || selected.ID != b.ID {
		t.Fatalf("selected=%v err=%v", selected, err)
	}
}
