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
	observed := time.Now().Add(-10 * time.Minute)
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

func TestUsageLimitsPreferRequestsUntilIdle(t *testing.T) {
	now := time.Now()
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			auth := claudeTestAuth("live", "live@example.com")
			headers := http.Header{
				"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.42"},
				"Anthropic-Ratelimit-Unified-5h-Reset":       {strconv.FormatInt(now.Add(time.Hour).Unix(), 10)},
			}
			if provider == "codex" {
				auth = codexTestAuth("live", "live@example.com")
				headers = http.Header{
					"X-Codex-Primary-Used-Percent":   {"42"},
					"X-Codex-Primary-Window-Minutes": {"300"},
					"X-Codex-Primary-Reset-At":       {strconv.FormatInt(now.Add(time.Hour).Unix(), 10)},
				}
			}
			auth.Quota.ObserveResponseHeadersForProvider(provider, headers, now)
			h, recorded := newUsageTestHandler(t, []*coreauth.Auth{auth}, func(*http.Request, []byte) *http.Response {
				return usageJSONResponse(http.StatusTooManyRequests, `{}`)
			})
			current, _ := h.authManager.GetByID(auth.ID)
			current.EnsureIndex()
			// A stale probe with a conflicting value must not mask live traffic.
			h.usageLimits.put(current.Index, 0, &usageProbe{at: now.Add(-time.Minute), windows: []usageWindow{{UsedPercent: 90}}})
			for _, force := range []bool{false, true} {
				account := h.usageAccountFor(context.Background(), current, now, force)
				if account.Source != "headers" || account.Windows[0].UsedPercent != 42 || len(*recorded) != 0 {
					t.Fatalf("force=%v: live observation did not win: %+v, calls=%d", force, account, len(*recorded))
				}
			}
			// A request with no quota headers still prevents an extra usage read.
			current.LastRequestAt = now.Add(4 * time.Minute)
			h.usageAccountFor(context.Background(), current, now.Add(6*time.Minute), false)
			if len(*recorded) != 0 {
				t.Fatal("active credential triggered a standalone usage read")
			}
			h.usageAccountFor(context.Background(), current, now.Add(9*time.Minute), false)
			if len(*recorded) != 1 {
				t.Fatalf("idle credential made %d usage reads, want 1", len(*recorded))
			}
		})
	}
}

func TestUsageProbeSharedReadSurvivesCanceledSubscriber(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h, recorded := newUsageTestHandler(t, []*coreauth.Auth{claudeTestAuth("shared", "test@example.com")}, func(req *http.Request, _ []byte) *http.Response {
		close(started)
		<-release
		if req.Context().Err() != nil {
			t.Error("a subscriber canceled the shared provider read")
		}
		return usageJSONResponse(http.StatusOK, `{"five_hour":{"utilization":25}}`)
	})
	auth, _ := h.authManager.GetByID("shared")
	auth.EnsureIndex()
	now := time.Now()
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	first := make(chan *usageProbe, 1)
	go func() { first <- h.cachedUsageProbe(firstCtx, auth, now, false) }()
	<-started
	cancelFirst()
	if probe := <-first; probe != nil {
		t.Fatal("canceled subscriber must return without waiting for the provider")
	}
	// A canceled waiter still joins the existing read rather than creating one.
	if probe := h.cachedUsageProbe(firstCtx, auth, now, false); probe != nil {
		t.Fatal("canceled subscriber returned a probe")
	}
	close(release)
	probe := h.cachedUsageProbe(context.Background(), auth, now, false)
	if probe.err != "" || len(probe.windows) != 1 || len(*recorded) != 1 {
		t.Fatalf("expected one successful shared read: probe=%+v calls=%d", probe, len(*recorded))
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

func TestUsageRateLimitRetryAt(t *testing.T) {
	now := time.Date(2026, time.October, 26, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, header string
		wait         time.Duration
	}{
		{"seconds", "900", 15 * time.Minute},
		{"short provider deadline", "10", 10 * time.Second},
		{"whitespace", " 900 ", 15 * time.Minute},
		{"HTTP date", now.Add(time.Hour).Format(http.TimeFormat), time.Hour},
		{"missing", "", usageLimitsRateLimitTTL},
		{"invalid", "later", usageLimitsRateLimitTTL},
		{"zero", "0", usageLimitsRateLimitTTL},
		{"negative", "-1", usageLimitsRateLimitTTL},
		{"past date", now.Add(-time.Hour).Format(http.TimeFormat), usageLimitsRateLimitTTL},
		{"duration overflow", "9223372036854775807", usageLimitsRateLimitTTL},
		{"integer overflow", "9223372036854775808", usageLimitsRateLimitTTL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := usageRateLimitRetryAt(tc.header, now); !got.Equal(now.Add(tc.wait)) {
				t.Fatalf("retry deadline = %v, want %v", got, now.Add(tc.wait))
			}
		})
	}
}

func TestUsageLimitsRateLimitBackoffIncreasesAndResets(t *testing.T) {
	var cache usageLimitsCache
	now := time.Date(2026, time.October, 26, 12, 0, 0, 0, time.UTC)
	for _, wait := range []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute, 30 * time.Minute} {
		probe := &usageProbe{at: now, err: "rate limited", retryAt: now.Add(time.Second)}
		cache.put("account", 0, probe)
		if !probe.retryAt.Equal(now.Add(wait)) {
			t.Fatalf("retry wait = %v, want %v", probe.retryAt.Sub(now), wait)
		}
		now = probe.retryAt
	}
	cache.put("account", 0, &usageProbe{at: now})
	probe := &usageProbe{at: now, err: "rate limited", retryAt: now.Add(time.Hour)}
	cache.put("account", 0, probe)
	if probe.backoff != 5*time.Minute || !probe.retryAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("success must reset backoff and preserve longer Retry-After: %+v", probe)
	}
}

func TestUsageLimitsResetRefreshesDataWithoutBypassingReadBackoff(t *testing.T) {
	now := time.Now()
	auth := claudeTestAuth("reset-cache", "test@example.com")
	auth.Quota.ObserveResponseHeadersForProvider("claude", http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.9"},
	}, now)
	h, recorded := newUsageTestHandler(t, []*coreauth.Auth{auth}, func(*http.Request, []byte) *http.Response {
		return usageJSONResponse(http.StatusOK, `{"five_hour":{"utilization":0}}`)
	})
	current, _ := h.authManager.GetByID(auth.ID)
	current.EnsureIndex()
	h.usageLimits.invalidate(current.Index)
	account := h.usageAccountFor(context.Background(), current, now, false)
	if account.Source != "probe" || account.Windows[0].UsedPercent != 0 || len(*recorded) != 1 {
		t.Fatalf("pre-reset observations masked refreshed usage: %+v calls=%d", account, len(*recorded))
	}
	limited := &usageProbe{at: now, err: "rate limited", retryAt: now.Add(time.Hour)}
	h.usageLimits.put(current.Index, h.usageLimits.generation(current.Index), limited)
	h.usageLimits.invalidate(current.Index)
	if got, fresh := h.usageLimits.get(current.Index, now, true); !fresh || got != limited {
		t.Fatal("reset invalidation must preserve active usage-read backoff")
	}
}

func TestGetUsageLimitsRateLimitBackoff(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, retryAfter := range []string{"", "900"} {
			t.Run(provider+"/retry-after="+retryAfter, func(t *testing.T) {
				auth := claudeTestAuth("account", "test@example.com")
				if provider == "codex" {
					auth = codexTestAuth("account", "test@example.com")
				}
				auth.Quota = coreauth.QuotaState{ObservedAt: time.Now().Add(-10 * time.Minute), Signals: map[string]string{}}
				if provider == "claude" {
					auth.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Utilization"] = "0.4"
				}
				h, recorded := newUsageTestHandler(t, []*coreauth.Auth{auth}, func(*http.Request, []byte) *http.Response {
					resp := usageJSONResponse(http.StatusTooManyRequests, `{}`)
					resp.Header.Set("Retry-After", retryAfter)
					return resp
				})

				account := getUsageLimits(t, h).Providers[0].Accounts[0]
				if account.Error != "The provider is rate limiting usage reads." {
					t.Fatalf("unexpected probe error: %q", account.Error)
				}
				if provider == "claude" && (account.Source != "headers" || account.Status != "ok" || len(account.Windows) != 1 || account.Windows[0].UsedPercent != 40) {
					t.Fatalf("usage throttling must preserve header fallback: %+v", account)
				}
				current, _ := h.authManager.GetByID(auth.ID)
				if current.Quota.Exceeded || current.Unavailable || !current.NextRetryAfter.IsZero() {
					t.Fatalf("usage throttling must not cool inference credentials: %+v", current.Quota)
				}

				index := authIndexFor(t, h, auth.ID)
				probe, _ := h.usageLimits.get(index, time.Now(), false)
				if probe == nil || probe.retryAt.IsZero() {
					t.Fatal("rate-limited probe must carry a retry deadline")
				}
				// Advance the read time explicitly; neither automatic polling nor
				// refresh=1 may bypass backoff after the usual error TTL expires.
				for _, force := range []bool{false, true} {
					before := probe.retryAt.Add(-time.Nanosecond)
					h.usageAccountFor(context.Background(), current, before, force)
					if len(*recorded) != 1 {
						t.Fatalf("force=%v: backoff triggered %d upstream requests, want 1", force, len(*recorded))
					}
					if _, fresh := h.usageLimits.get(index, probe.retryAt, force); fresh {
						t.Fatalf("force=%v: probe must expire at the provider retry deadline", force)
					}
				}
				// An expired deadline permits a new probe.
				h.usageAccountFor(context.Background(), current, probe.retryAt, false)
				if len(*recorded) != 2 {
					t.Fatalf("expired backoff: got %d requests, want 2", len(*recorded))
				}
			})
		}
	}
}
