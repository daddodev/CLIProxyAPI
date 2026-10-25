package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type usageRoundTripFunc func(*http.Request) (*http.Response, error)

func (f usageRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func usageJSONResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

type recordedUsageRequest struct {
	method, url string
	header      http.Header
	body        []byte
}

func newUsageTestHandler(t *testing.T, auths []*coreauth.Auth, respond func(*http.Request, []byte) *http.Response) (*Handler, *[]recordedUsageRequest) {
	t.Helper()
	t.Setenv("MANAGEMENT_PASSWORD", "")
	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range auths {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register %s: %v", auth.ID, errRegister)
		}
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	var mu sync.Mutex
	recorded := &[]recordedUsageRequest{}
	h.usageTransport = usageRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body []byte
		if req.Body != nil {
			body, _ = io.ReadAll(req.Body)
		}
		mu.Lock()
		*recorded = append(*recorded, recordedUsageRequest{req.Method, req.URL.String(), req.Header.Clone(), body})
		mu.Unlock()
		return respond(req, body), nil
	})
	return h, recorded
}

func authIndexFor(t *testing.T, h *Handler, id string) string {
	t.Helper()
	for _, auth := range h.authManager.List() {
		if auth.ID == id {
			auth.EnsureIndex()
			return auth.Index
		}
	}
	t.Fatalf("auth %s not found", id)
	return ""
}

type usageLimitsPayload struct {
	Providers []usageProvider `json:"providers"`
}

func getUsageLimits(t *testing.T, h *Handler) usageLimitsPayload {
	t.Helper()
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/usage/limits", nil)
	h.GetUsageLimits(ginCtx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var payload usageLimitsPayload
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}
	return payload
}

func postUsageReset(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v8/management/observability/usage/limits/reset", bytes.NewBufferString(body))
	ginCtx.Request.Header.Set("Content-Type", "application/json")
	h.PostUsageLimitsReset(ginCtx)
	return rec
}

func claudeTestAuth(id, email string) *coreauth.Auth {
	return &coreauth.Auth{ID: id, Provider: "claude", Metadata: map[string]any{
		"type": "claude", "email": email, "access_token": "tok-" + id, "organization_uuid": "org-" + id,
	}}
}

func codexTestAuth(id, email string) *coreauth.Auth {
	return &coreauth.Auth{ID: id, Provider: "codex", Metadata: map[string]any{
		"type": "codex", "email": email, "access_token": "tok-" + id, "account_id": "acct-" + id,
	}}
}

func TestGetUsageLimitsCombinesProbedAccounts(t *testing.T) {
	fiveHourReset := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	weeklyReset := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
	creditExpiry := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	h, recorded := newUsageTestHandler(t, []*coreauth.Auth{
		claudeTestAuth("claude-a", "a@example.com"),
		claudeTestAuth("claude-b", "b@example.com"),
		codexTestAuth("codex-a", "c@example.com"),
	}, func(req *http.Request, _ []byte) *http.Response {
		switch {
		case strings.HasPrefix(req.URL.String(), "https://api.anthropic.com/api/oauth/usage"):
			return usageJSONResponse(http.StatusOK, fmt.Sprintf(`{
				"five_hour": {"utilization": 65, "resets_at": %q},
				"seven_day": {"utilization": 40, "resets_at": %q},
				"limits": [{"kind": "weekly_scoped", "percent": 12, "resets_at": %q, "scope": {"model": {"display_name": "Opus"}}}],
				"cedar_ember": {"eligible": true, "next_grant_id": "g1", "grants": [
					{"id": "g1", "resets_left": 2, "usable_now": true, "ends_at": %q},
					{"id": "g2", "resets_left": 5, "usable_now": true, "paused": true}
				]}
			}`, fiveHourReset.Format(time.RFC3339), weeklyReset.Format(time.RFC3339), weeklyReset.Format(time.RFC3339), creditExpiry.Format(time.RFC3339)))
		case req.URL.String() == codexUsageURL:
			return usageJSONResponse(http.StatusOK, fmt.Sprintf(`{"plan_type": "pro", "rate_limit": {
				"primary_window": {"used_percent": 30, "reset_at": %d, "limit_window_seconds": 18000},
				"secondary_window": {"used_percent": 70, "reset_at": %d, "limit_window_seconds": 604800}
			}}`, fiveHourReset.Unix(), weeklyReset.Unix()))
		case req.URL.String() == codexCreditsURL:
			return usageJSONResponse(http.StatusOK, fmt.Sprintf(`{"credits": [
				{"id": "late", "status": "available", "reset_type": "codex_rate_limits", "expires_at": %q},
				{"id": "soon", "status": "available", "reset_type": "codex_rate_limits", "expires_at": %q},
				{"id": "used", "status": "redeemed", "reset_type": "codex_rate_limits", "expires_at": %q}
			]}`, weeklyReset.Format(time.RFC3339), creditExpiry.Format(time.RFC3339), creditExpiry.Format(time.RFC3339)))
		}
		return usageJSONResponse(http.StatusNotFound, `{}`)
	})

	payload := getUsageLimits(t, h)
	if len(payload.Providers) != 2 {
		t.Fatalf("providers = %d, want 2", len(payload.Providers))
	}
	claude := payload.Providers[0]
	if claude.Provider != "claude" || claude.AccountCount != 2 || claude.ResetsAvailable != 4 {
		t.Fatalf("unexpected claude summary: %+v", claude)
	}
	if len(claude.Windows) != 2 {
		t.Fatalf("claude combined windows = %d, want 2", len(claude.Windows))
	}
	five := claude.Windows[0]
	if five.Kind != windowFiveHour || five.UsedPercent != 130 || five.CapacityPercent != 200 || five.NextResetAt == nil || !five.NextResetAt.Equal(fiveHourReset) {
		t.Fatalf("unexpected combined 5h window: %+v", five)
	}
	account := claude.Accounts[0]
	if account.Source != "probe" || account.Label != "a@example.com" || len(account.Windows) != 3 || account.Windows[2].Label != "Weekly · Opus" {
		t.Fatalf("unexpected claude account: %+v", account)
	}
	if account.Resets == nil || account.Resets.NextCreditID != "g1" || account.Resets.Available != 2 {
		t.Fatalf("unexpected claude resets: %+v", account.Resets)
	}

	codex := payload.Providers[1]
	codexAccount := codex.Accounts[0]
	if codexAccount.Plan != "Pro" || codexAccount.Windows[1].Kind != windowWeekly || codexAccount.Windows[1].UsedPercent != 70 {
		t.Fatalf("unexpected codex account: %+v", codexAccount)
	}
	if codexAccount.Resets == nil || codexAccount.Resets.Available != 2 || codexAccount.Resets.NextCreditID != "soon" {
		t.Fatalf("unexpected codex resets: %+v", codexAccount.Resets)
	}

	for _, req := range *recorded {
		if !strings.HasPrefix(req.header.Get("Authorization"), "Bearer tok-") {
			t.Fatalf("missing bearer token on %s", req.url)
		}
		if strings.Contains(req.url, "chatgpt.com") && req.header.Get("Chatgpt-Account-Id") != "acct-codex-a" {
			t.Fatalf("missing Chatgpt-Account-Id on %s", req.url)
		}
	}

	// A second read inside the TTL is served from cache.
	before := len(*recorded)
	getUsageLimits(t, h)
	if len(*recorded) != before {
		t.Fatalf("expected cached probes, got %d new requests", len(*recorded)-before)
	}
}

func TestGetUsageLimitsFallsBackToHeaderSignals(t *testing.T) {
	observed := time.Now().Add(-time.Minute)
	reset := time.Now().Add(90 * time.Minute).Unix()
	expired := time.Now().Add(-time.Minute).Unix()
	auth := claudeTestAuth("claude-a", "a@example.com")
	auth.Quota = coreauth.QuotaState{ObservedAt: observed, Signals: map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0.5",
		"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(reset, 10),
		"Anthropic-Ratelimit-Unified-7d-Utilization": "0.9",
		"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(expired, 10),
	}}
	h, _ := newUsageTestHandler(t, []*coreauth.Auth{auth}, func(*http.Request, []byte) *http.Response {
		return usageJSONResponse(http.StatusInternalServerError, `{}`)
	})

	account := getUsageLimits(t, h).Providers[0].Accounts[0]
	if account.Source != "headers" || account.Error == "" || account.Status != "ok" {
		t.Fatalf("unexpected fallback account: %+v", account)
	}
	if len(account.Windows) != 2 || account.Windows[0].UsedPercent != 50 || account.Windows[0].ResetsAt == nil {
		t.Fatalf("unexpected 5h window: %+v", account.Windows)
	}
	if account.Windows[1].UsedPercent != 0 || account.Windows[1].ResetsAt != nil {
		t.Fatalf("an elapsed window should read as reset: %+v", account.Windows[1])
	}
}

func TestPostUsageLimitsResetCodexUsesDeterministicRequestID(t *testing.T) {
	h, recorded := newUsageTestHandler(t, []*coreauth.Auth{codexTestAuth("codex-a", "c@example.com")},
		func(req *http.Request, _ []byte) *http.Response {
			if req.URL.String() == codexCreditsURL+"/consume" {
				return usageJSONResponse(http.StatusOK, `{"code": "reset"}`)
			}
			return usageJSONResponse(http.StatusNotFound, `{}`)
		})
	index := authIndexFor(t, h, "codex-a")

	rec := postUsageReset(t, h, fmt.Sprintf(`{"auth_index": %q, "credit_id": "credit_1"}`, index))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"outcome":"reset"`) {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(*recorded) != 1 || (*recorded)[0].method != http.MethodPost {
		t.Fatalf("unexpected upstream calls: %+v", *recorded)
	}
	var sent map[string]string
	if errDecode := json.Unmarshal((*recorded)[0].body, &sent); errDecode != nil {
		t.Fatalf("decode upstream body: %v", errDecode)
	}
	want := uuid.NewSHA1(codexRedeemNamespace, []byte("acct-codex-a:credit_1")).String()
	if sent["credit_id"] != "credit_1" || sent["redeem_request_id"] != want {
		t.Fatalf("unexpected consume payload: %+v", sent)
	}
}

func TestPostUsageLimitsResetClaudeOutcomes(t *testing.T) {
	cases := []struct {
		upstreamStatus int
		upstreamBody   string
		wantStatus     int
		wantFragment   string
	}{
		{http.StatusOK, `{"result": "reset"}`, http.StatusOK, `"outcome":"reset"`},
		{http.StatusOK, `{"result": "not_limited"}`, http.StatusOK, `"outcome":"nothing_to_reset"`},
		{http.StatusOK, `{"result": "already_used"}`, http.StatusOK, `"outcome":"already_redeemed"`},
		{http.StatusOK, `{"result": "cooldown"}`, http.StatusConflict, `"code":"cooling_down"`},
		{http.StatusOK, `{"result": "unavailable"}`, http.StatusBadGateway, `"code":"unconfirmed"`},
		{http.StatusTooManyRequests, `{}`, http.StatusTooManyRequests, `"code":"rate_limited"`},
		{http.StatusUnauthorized, `{}`, http.StatusConflict, `"code":"signed_out"`},
	}
	for _, tc := range cases {
		h, recorded := newUsageTestHandler(t, []*coreauth.Auth{claudeTestAuth("claude-a", "a@example.com")},
			func(*http.Request, []byte) *http.Response {
				return usageJSONResponse(tc.upstreamStatus, tc.upstreamBody)
			})
		index := authIndexFor(t, h, "claude-a")

		rec := postUsageReset(t, h, fmt.Sprintf(`{"auth_index": %q, "credit_id": "g1", "request_id": "req-1"}`, index))
		if rec.Code != tc.wantStatus || !strings.Contains(rec.Body.String(), tc.wantFragment) {
			t.Fatalf("%s: status = %d body=%s", tc.upstreamBody, rec.Code, rec.Body.String())
		}
		call := (*recorded)[0]
		if call.url != "https://api.anthropic.com/api/organizations/org-claude-a/reset_rate_limits" {
			t.Fatalf("unexpected url %s", call.url)
		}
		var sent map[string]string
		if errDecode := json.Unmarshal(call.body, &sent); errDecode != nil {
			t.Fatalf("decode upstream body: %v", errDecode)
		}
		if sent["program"] != "cedar_ember" || sent["grant_id"] != "g1" || sent["request_id"] != "req-1" {
			t.Fatalf("unexpected claim payload: %+v", sent)
		}
	}
}

func TestPostUsageLimitsResetRejectsBadInput(t *testing.T) {
	h, recorded := newUsageTestHandler(t, []*coreauth.Auth{claudeTestAuth("claude-a", "a@example.com")},
		func(*http.Request, []byte) *http.Response {
			return usageJSONResponse(http.StatusOK, `{"result": "reset"}`)
		})
	index := authIndexFor(t, h, "claude-a")

	for _, body := range []string{
		`{"auth_index": "missing", "credit_id": "g1"}`,
		fmt.Sprintf(`{"auth_index": %q, "credit_id": "../evil"}`, index),
		fmt.Sprintf(`{"auth_index": %q, "credit_id": "g1", "request_id": "bad id"}`, index),
	} {
		rec := postUsageReset(t, h, body)
		if rec.Code < 400 || rec.Code >= 500 {
			t.Fatalf("%s: status = %d, want 4xx", body, rec.Code)
		}
	}
	if len(*recorded) != 0 {
		t.Fatalf("invalid requests must not reach upstream: %+v", *recorded)
	}
}

func TestUsageLimitsSkipsAPIKeyAccounts(t *testing.T) {
	apiKeyAuth := &coreauth.Auth{ID: "claude-key", Provider: "claude", Attributes: map[string]string{
		"api_key": "sk-custom", "base_url": "https://claude.example.com",
	}}
	// File-backed key accounts keep the key in metadata only.
	metadataKeyAuth := &coreauth.Auth{ID: "codex-key", Provider: "codex", Metadata: map[string]any{
		"type": "codex", "api_key": "sk-file", "base_url": "https://codex.example.com",
	}}
	h, recorded := newUsageTestHandler(t, []*coreauth.Auth{apiKeyAuth, metadataKeyAuth}, func(*http.Request, []byte) *http.Response {
		return usageJSONResponse(http.StatusOK, `{}`)
	})

	if providers := getUsageLimits(t, h).Providers; len(providers) != 0 {
		t.Fatalf("API-key accounts must not be listed: %+v", providers)
	}
	rec := postUsageReset(t, h, fmt.Sprintf(`{"auth_index": %q, "credit_id": "g1", "request_id": "req-1"}`, authIndexFor(t, h, "claude-key")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("reset on API-key account: status = %d, want 404", rec.Code)
	}
	if len(*recorded) != 0 {
		t.Fatalf("an API key must never reach the subscription endpoints: %+v", *recorded)
	}
}

func TestPostUsageLimitsResetClaudeRequiresRequestID(t *testing.T) {
	h, recorded := newUsageTestHandler(t, []*coreauth.Auth{claudeTestAuth("claude-a", "a@example.com")},
		func(*http.Request, []byte) *http.Response {
			return usageJSONResponse(http.StatusOK, `{"result": "reset"}`)
		})

	rec := postUsageReset(t, h, fmt.Sprintf(`{"auth_index": %q, "credit_id": "g1"}`, authIndexFor(t, h, "claude-a")))
	if rec.Code != http.StatusBadRequest || len(*recorded) != 0 {
		t.Fatalf("status = %d, upstream calls = %d; want 400 and none", rec.Code, len(*recorded))
	}
}

func TestPostUsageLimitsResetAmbiguousFailuresAreUnconfirmed(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{http.StatusInternalServerError, `{}`},
		{http.StatusOK, `{"result": "surprise"}`},
		{http.StatusOK, `not json`},
		{http.StatusOK, `{"code": "reset"}garbage`},
	} {
		h, _ := newUsageTestHandler(t, []*coreauth.Auth{codexTestAuth("codex-a", "c@example.com")},
			func(*http.Request, []byte) *http.Response { return usageJSONResponse(tc.status, tc.body) })
		rec := postUsageReset(t, h, fmt.Sprintf(`{"auth_index": %q, "credit_id": "credit_1"}`, authIndexFor(t, h, "codex-a")))
		if !strings.Contains(rec.Body.String(), `"code":"unconfirmed"`) {
			t.Fatalf("%d %s: body = %s, want unconfirmed", tc.status, tc.body, rec.Body.String())
		}
	}
}

func TestUsageLimitsCacheIgnoresProbeStartedBeforeInvalidation(t *testing.T) {
	var cache usageLimitsCache
	generation := cache.generation("a1")
	cache.invalidate("a1")
	cache.put("a1", generation, &usageProbe{at: time.Now()})
	if _, fresh := cache.get("a1", time.Now(), false); fresh {
		t.Fatal("a probe that started before invalidation must not be cached")
	}
	cache.put("a1", cache.generation("a1"), &usageProbe{at: time.Now()})
	if _, fresh := cache.get("a1", time.Now(), false); !fresh {
		t.Fatal("a probe started after invalidation should be cached")
	}
}
