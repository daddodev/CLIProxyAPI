package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	fileauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestQuotaLimitPatchValidation(t *testing.T) {
	for _, key := range []string{coreauth.QuotaLimitPercentKey, coreauth.QuotaFiveHourRemainingPercentKey, coreauth.QuotaWeeklyRemainingPercentKey} {
		for _, raw := range []string{"0", "20.5", "100", "null"} {
			if _, err := normalizeAuthFilePatchFields(map[string]json.RawMessage{key: json.RawMessage(raw)}); err != nil {
				t.Fatalf("%s=%s: %v", key, raw, err)
			}
		}
		for _, raw := range []string{"-1", "101", `"20"`, "true", "{}", "1e1000"} {
			if _, err := normalizeAuthFilePatchFields(map[string]json.RawMessage{key: json.RawMessage(raw)}); err == nil {
				t.Fatalf("accepted %s=%s", key, raw)
			}
		}
		if _, err := normalizeAuthFilePatchFields(map[string]json.RawMessage{key + ".x": json.RawMessage("20")}); err == nil {
			t.Fatalf("accepted nested limit %s", key)
		}
	}
}

func TestUsageProbeWindowsFeedQuotaLimit(t *testing.T) {
	now := time.Unix(1800000000, 0)
	reset := now.Add(time.Hour)
	for _, provider := range []string{"claude", "codex"} {
		a := &coreauth.Auth{Provider: provider, Metadata: map[string]any{coreauth.QuotaLimitPercentKey: 80.0}}
		h := usageWindowHeaders(provider, []usageWindow{{Kind: windowFiveHour, UsedPercent: 80, ResetsAt: &reset}})
		a.Quota.ObserveResponseHeadersForProvider(provider, h, now)
		if blocked, until := coreauth.QuotaLimitReached(a, now); !blocked || !until.Equal(reset) {
			t.Fatalf("%s: blocked=%v until=%v", provider, blocked, until)
		}
	}
}

func TestQuotaLimitPatchPersistsAndClears(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	dir := t.TempDir()
	store := fileauth.NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	path := filepath.Join(dir, "limit.json")
	_, err := manager.Register(context.Background(), &coreauth.Auth{ID: "limit.json", FileName: "limit.json", Provider: "codex", Attributes: map[string]string{"path": path}, Metadata: map[string]any{"type": "codex"}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, manager)
	for _, value := range []string{"82.5", "null"} {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v8/management/credentials/fields", strings.NewReader(`{"name":"limit.json","quota_limit_percent":`+value+`}`))
		ctx.Request.Header.Set("Content-Type", "application/json")
		handler.PatchAuthFileFields(ctx)
		if rec.Code != http.StatusOK {
			t.Fatalf("patch failed: %d %s", rec.Code, rec.Body.String())
		}
		updated, _ := manager.GetByID("limit.json")
		expected := 82.5
		if value == "null" {
			expected = 0
		}
		if got := coreauth.QuotaLimitPercent(updated); got != expected {
			t.Fatalf("runtime=%v expected=%v", got, expected)
		}
		raw, errRead := os.ReadFile(path)
		if errRead != nil {
			t.Fatal(errRead)
		}
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			t.Fatal(err)
		}
		if value == "null" {
			if data[coreauth.QuotaLimitPercentKey] != nil {
				t.Fatal("cleared ceiling remains in auth file")
			}
		} else if data[coreauth.QuotaLimitPercentKey] != expected {
			t.Fatal("ceiling not persisted")
		}
	}
}

func TestQuotaRemainingPatchPersistsIndependentWindows(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	dir := t.TempDir()
	store := fileauth.NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	path := filepath.Join(dir, "reserves.json")
	_, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: "reserves.json", FileName: "reserves.json", Provider: "claude",
		Attributes: map[string]string{"path": path},
		Metadata:   map[string]any{"type": "claude", coreauth.QuotaLimitPercentKey: 20.0},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, manager)
	for _, tc := range []struct {
		body             string
		fiveHour, weekly float64
	}{
		{`{"name":"reserves.json","quota_limit_percent":null,"quota_5h_remaining_percent":20,"quota_weekly_remaining_percent":35.5}`, 20, 35.5},
		{`{"name":"reserves.json","quota_5h_remaining_percent":0}`, 0, 35.5},
		{`{"name":"reserves.json","quota_5h_remaining_percent":null}`, 0, 35.5},
	} {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v8/management/credentials/fields", strings.NewReader(tc.body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		handler.PatchAuthFileFields(ctx)
		if rec.Code != http.StatusOK {
			t.Fatalf("patch failed: %d %s", rec.Code, rec.Body.String())
		}
		updated, _ := manager.GetByID("reserves.json")
		if got := coreauth.QuotaRemainingPercent(updated, coreauth.QuotaFiveHourRemainingPercentKey); got != tc.fiveHour {
			t.Fatalf("five hour=%v want=%v", got, tc.fiveHour)
		}
		if got := coreauth.QuotaRemainingPercent(updated, coreauth.QuotaWeeklyRemainingPercentKey); got != tc.weekly {
			t.Fatalf("weekly=%v want=%v", got, tc.weekly)
		}
		raw, errRead := os.ReadFile(path)
		if errRead != nil {
			t.Fatal(errRead)
		}
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			t.Fatal(err)
		}
		if data[coreauth.QuotaLimitPercentKey] != nil || data[coreauth.QuotaWeeklyRemainingPercentKey] != tc.weekly {
			t.Fatalf("unexpected persisted limits: %s", raw)
		}
		if data[coreauth.QuotaFiveHourRemainingPercentKey] != nil && data[coreauth.QuotaFiveHourRemainingPercentKey] != tc.fiveHour {
			t.Fatalf("unexpected persisted five hour limit: %s", raw)
		}
	}
}

func TestUsageProbeIndependentRemainingLimits(t *testing.T) {
	reset := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	for _, provider := range []string{"claude", "codex"} {
		for _, weeklyReserve := range []float64{20, 70} {
			t.Run(fmt.Sprintf("%s/weekly-reserve-%v", provider, weeklyReserve), func(t *testing.T) {
				a := claudeTestAuth("reserve-probe", "reserve@example.com")
				if provider == "codex" {
					a = codexTestAuth("reserve-probe", "reserve@example.com")
				}
				a.Metadata[coreauth.QuotaFiveHourRemainingPercentKey] = 20.0
				a.Metadata[coreauth.QuotaWeeklyRemainingPercentKey] = weeklyReserve
				h, _ := newUsageTestHandler(t, []*coreauth.Auth{a}, func(req *http.Request, _ []byte) *http.Response {
					if provider == "claude" {
						return usageJSONResponse(http.StatusOK, fmt.Sprintf(`{"five_hour":{"utilization":10,"resets_at":%q},"seven_day":{"utilization":32,"resets_at":%q}}`, reset, reset))
					}
					if req.URL.String() == codexUsageURL {
						return usageJSONResponse(http.StatusOK, fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":10,"reset_at":%q,"limit_window_seconds":18000},"secondary_window":{"used_percent":32,"reset_at":%q,"limit_window_seconds":604800}}}`, reset, reset))
					}
					return usageJSONResponse(http.StatusOK, `{}`)
				})
				payload := getUsageLimits(t, h)
				account := payload.Providers[0].Accounts[0]
				wantBlocked := weeklyReserve == 70
				if account.QuotaFiveHourRemainingPercent != 20 || account.QuotaWeeklyRemainingPercent != weeklyReserve || account.LimitReached != wantBlocked {
					t.Fatalf("unexpected probe response: %+v", account)
				}
				updated, _ := h.authManager.GetByID(a.ID)
				if blocked, _ := coreauth.QuotaLimitReached(updated, time.Now()); blocked != wantBlocked {
					t.Fatalf("probe did not feed routing: blocked=%v want=%v", blocked, wantBlocked)
				}
			})
		}
	}
}
