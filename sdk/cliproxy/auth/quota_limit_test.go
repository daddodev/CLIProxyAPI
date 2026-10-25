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
