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

	"github.com/MoeForge/moego/ojson"
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

func TestGetUsageLimitsCodexConsecutiveSnapshotsPreserveWindowKinds(t *testing.T) {
	auth := codexTestAuth("consecutive", "test@example.com")
	h, recorded := newUsageTestHandler(t, []*coreauth.Auth{auth}, func(req *http.Request, _ []byte) *http.Response {
		switch req.URL.String() {
		case codexUsageURL:
			return usageJSONResponse(http.StatusOK, `{"rate_limit":{
				"primary_window":{"used_percent":30,"limit_window_seconds":18000},
				"secondary_window":{"used_percent":70,"limit_window_seconds":604800}
			}}`)
		case codexCreditsURL:
			return usageJSONResponse(http.StatusOK, `{"credits":[]}`)
		default:
			t.Errorf("unexpected provider read: %s", req.URL)
			return usageJSONResponse(http.StatusNotFound, `{}`)
		}
	})
	for snapshot := 1; snapshot <= 2; snapshot++ {
		payload := getUsageLimits(t, h)
		account := payload.Providers[0].Accounts[0]
		if len(account.Windows) != 2 || account.Windows[0].Kind != windowFiveHour || account.Windows[0].WindowMinutes != 300 || account.Windows[0].UsedPercent != 30 || account.Windows[1].Kind != windowWeekly || account.Windows[1].WindowMinutes != 10080 || account.Windows[1].UsedPercent != 70 {
			t.Fatalf("snapshot %d changed or duplicated Codex windows: %+v", snapshot, account.Windows)
		}
		current, ok := h.authManager.GetByID(auth.ID)
		if !ok || current.Quota.ObservedAt.IsZero() {
			t.Fatalf("snapshot %d did not feed the auth manager", snapshot)
		}
		headerWindows := usageWindowsFromSignals(current.Provider, current.Quota, time.Now())
		if len(headerWindows) != 2 || headerWindows[0].Kind != windowFiveHour || headerWindows[1].Kind != windowWeekly {
			t.Fatalf("snapshot %d lost durations in synthetic headers: %+v", snapshot, headerWindows)
		}
	}
	if len(*recorded) != 2 {
		t.Fatalf("consecutive snapshots made %d provider reads, want one usage and one credit read", len(*recorded))
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

func TestUsageLimitsSelectsClaudeResetGrant(t *testing.T) {
	expiresAt := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	h, recorded := newUsageTestHandler(t, []*coreauth.Auth{claudeTestAuth("claude-a", "a@example.com")},
		func(req *http.Request, _ []byte) *http.Response {
			if req.Method == http.MethodPost {
				return usageJSONResponse(http.StatusOK, `{"result":"reset"}`)
			}
			return usageJSONResponse(http.StatusOK, fmt.Sprintf(`{"cedar_ember":{
				"eligible":true,"next_grant_id":"session-grant","grants":[
					{"id":"session-grant","type":"session","label":"Session credit","clears":["five_hour"],"resets_left":2,"usable_now":true,"ends_at":%q},
					{"id":"weekly-grant","type":"weekly","label":"Weekly credit","clears":["seven_day"],"resets_left":1,"usable_now":true,"ends_at":%q}
				]}}`, expiresAt.Format(time.RFC3339), expiresAt.Format(time.RFC3339)))
		})

	payload := getUsageLimits(t, h)
	provider := payload.Providers[0]
	resets := provider.Accounts[0].Resets
	if provider.ResetsAvailable != 3 || resets.Available != 3 || resets.NextCreditID != "session-grant" || len(resets.Options) != 2 {
		t.Fatalf("unexpected reset inventory: %+v", resets)
	}
	for i, wantType := range []string{"session", "weekly"} {
		option := resets.Options[i]
		wantWindow := "five_hour"
		wantLabel := "Session credit"
		if i == 1 {
			wantWindow = "seven_day"
			wantLabel = "Weekly credit"
		}
		if option.ResetType != wantType || option.Label != wantLabel || len(option.Clears) != 1 || option.Clears[0] != wantWindow || option.ExpiresAt == nil || !option.ExpiresAt.Equal(expiresAt) {
			t.Fatalf("option %d lost grant details: %+v", i, option)
		}
	}

	// Choosing the weekly grant must not silently spend the suggested session grant.
	index := authIndexFor(t, h, "claude-a")
	rec := postUsageReset(t, h, fmt.Sprintf(`{"auth_index":%q,"credit_id":"weekly-grant","request_id":"selected-weekly"}`, index))
	if rec.Code != http.StatusOK || len(*recorded) != 2 {
		t.Fatalf("reset status=%d calls=%d body=%s", rec.Code, len(*recorded), rec.Body.String())
	}
	sent := ojson.ParseBytes((*recorded)[1].body)
	if sent.Get("grant_id").String() != "weekly-grant" || sent.Get("request_id").String() != "selected-weekly" || sent.Get("program").String() != claudeResetProgram {
		t.Fatalf("wrong grant redeemed: %s", (*recorded)[1].body)
	}
}

func TestParseClaudeResetCreditsFiltersUnavailableGrants(t *testing.T) {
	now := time.Date(2026, time.November, 14, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, fields string
	}{
		{"paused", `"paused":true,"usable_now":true,"resets_left":2`},
		{"not usable", `"usable_now":false,"resets_left":2`},
		{"exhausted", `"usable_now":true,"resets_left":0`},
		{"expired", `"usable_now":true,"resets_left":2,"ends_at":"2026-11-14T12:00:00Z"`},
		{"malformed expiry", `"usable_now":true,"resets_left":2,"ends_at":"invalid"`},
		{"not started", `"usable_now":true,"resets_left":2,"starts_at":"2026-11-15T00:00:00Z"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := ojson.Parse(fmt.Sprintf(`{"eligible":true,"next_grant_id":"blocked","grants":[
				{"id":"blocked",%s},
				{"id":"usable","usable_now":true,"resets_left":1,"type":"full","clears":["five_hour","seven_day"]}
			]}`, tc.fields))
			resets := parseClaudeResetCredits(block, now)
			if resets.Available != 1 || resets.NextCreditID != "" || len(resets.Options) != 1 || resets.Options[0].CreditID != "usable" || len(resets.Options[0].Clears) != 2 {
				t.Fatalf("unavailable grant offered or usable grant hidden: %+v", resets)
			}
		})
	}
	for _, fields := range []string{`"eligible":false`, `"eligible":true,"cooldown_until":"2026-11-15T00:00:00Z"`} {
		block := ojson.Parse(fmt.Sprintf(`{%s,"grants":[{"id":"usable","usable_now":true,"resets_left":1}]}`, fields))
		if resets := parseClaudeResetCredits(block, now); resets.Available != 0 || len(resets.Options) != 0 {
			t.Fatalf("ineligible or cooling-down account offered resets: %+v", resets)
		}
	}
}

func TestParseCodexResetCreditsExposesSortedOptions(t *testing.T) {
	now := time.Date(2026, time.November, 14, 12, 0, 0, 0, time.UTC)
	root := ojson.Parse(`{"credits":[
		{"id":"late","status":"available","reset_type":"codex_rate_limits","expires_at":"2026-11-16T00:00:00Z"},
		{"id":"soon","status":"available","reset_type":"codex_rate_limits","expires_at":"2026-11-15T00:00:00Z"},
		{"id":"used","status":"redeemed","reset_type":"codex_rate_limits","expires_at":"2026-11-15T00:00:00Z"},
		{"id":"expired","status":"available","reset_type":"codex_rate_limits","expires_at":"2026-11-14T12:00:00Z"}
	]}`)
	resets := parseCodexResetCredits(root, now)
	if resets.Available != 2 || resets.NextCreditID != "soon" || len(resets.Options) != 2 {
		t.Fatalf("unexpected Codex reset inventory: %+v", resets)
	}
	for i, id := range []string{"soon", "late"} {
		option := resets.Options[i]
		if option.CreditID != id || option.Available != 1 || option.ResetType != "codex_rate_limits" || option.Label == "" || option.ExpiresAt == nil {
			t.Fatalf("unexpected Codex option: %+v", option)
		}
	}
}

func TestUsageLimitsPreferHeadersWithBoundedSupplementalReads(t *testing.T) {
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
			probe := &usageProbe{at: now.Add(-time.Minute), windowsAt: now.Add(-time.Minute), windows: []usageWindow{{Kind: windowFiveHour, UsedPercent: 90}}}
			h.usageLimits.put(current.Index, 0, probe)
			for _, force := range []bool{false, true} {
				account := h.usageAccountFor(context.Background(), current, now, force)
				if account.Source != "headers" || account.Windows[0].UsedPercent != 42 || len(*recorded) != 0 {
					t.Fatalf("force=%v: live observation did not win: %+v, calls=%d", force, account, len(*recorded))
				}
			}
			// Traffic cannot postpone credit/scoped-window reads indefinitely.
			current.LastRequestAt = now
			stale := *probe
			stale.at = now.Add(-usageLimitsTTL)
			h.usageLimits.put(current.Index, 0, &stale)
			h.usageAccountFor(context.Background(), current, now, false)
			if len(*recorded) != 1 {
				t.Fatalf("active credential made %d usage reads, want 1", len(*recorded))
			}
			h.usageAccountFor(context.Background(), current, now.Add(time.Minute), true)
			if len(*recorded) != 1 {
				t.Fatal("active credential bypassed usage-read backoff")
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

func TestUsageLimitsActiveAccountsRetainSupplementalDataAfterReadFailure(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			now := time.Now()
			auth := claudeTestAuth("active", "active@example.com")
			if provider == "codex" {
				auth = codexTestAuth("active", "active@example.com")
			}
			auth.LastRequestAt = now
			failed := false
			h, recorded := newUsageTestHandler(t, []*coreauth.Auth{auth}, func(req *http.Request, _ []byte) *http.Response {
				if failed && (provider == "claude" || req.URL.String() == codexCreditsURL) {
					return usageJSONResponse(http.StatusServiceUnavailable, `{}`)
				}
				if provider == "claude" {
					return usageJSONResponse(http.StatusOK, `{"five_hour":{"utilization":25},"seven_day":{"utilization":30},"limits":[{"kind":"weekly_scoped","percent":71,"scope":{"model":{"display_name":"Fable"}}}],"cedar_ember":{"eligible":true,"next_grant_id":"grant","grants":[{"id":"grant","resets_left":3,"usable_now":true}]}}`)
				}
				if req.URL.String() == codexUsageURL {
					return usageJSONResponse(http.StatusOK, `{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":25,"limit_window_seconds":18000},"secondary_window":{"used_percent":30,"limit_window_seconds":604800}}}`)
				}
				return usageJSONResponse(http.StatusOK, fmt.Sprintf(`{"credits":[{"id":"credit","status":"available","reset_type":"codex_rate_limits","expires_at":%q}]}`, now.Add(time.Hour).Format(time.RFC3339)))
			})
			current, _ := h.authManager.GetByID(auth.ID)
			current.EnsureIndex()
			// An active account with no probe must still discover its credits.
			first := h.usageAccountFor(context.Background(), current, now, false)
			if first.Resets == nil || first.Resets.Available == 0 || first.ResetsStale || first.ResetsObservedAt == nil {
				t.Fatalf("active account did not discover fresh credits: %+v", first)
			}
			calls := 1
			if provider == "codex" {
				calls = 2
			}
			old, _ := h.usageLimits.get(current.Index, now, false)
			aged := *old
			aged.at, aged.windowsAt, aged.resetsAt = now.Add(-6*time.Minute), now.Add(-6*time.Minute), now.Add(-6*time.Minute)
			h.usageLimits.put(current.Index, 0, &aged)
			current.Quota = coreauth.QuotaState{ObservedAt: now, Signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": "0.42",
			}}
			if provider == "codex" {
				current.Quota.Signals = map[string]string{"X-Codex-Primary-Used-Percent": "42", "X-Codex-Primary-Window-Minutes": "300"}
			}
			failed = true
			second := h.usageAccountFor(context.Background(), current, now, false)
			if second.Resets == nil || second.Resets.Available != first.Resets.Available || !second.ResetsStale || second.ResetsError == "" || !second.ResetsObservedAt.Equal(aged.resetsAt) {
				t.Fatalf("failed read lost inventory or changed its freshness: %+v", second)
			}
			if provider == "claude" {
				if second.Source != "headers" || len(second.Windows) != 3 || second.Windows[0].UsedPercent != 42 || second.Windows[0].Stale || second.Windows[2].Label != "Weekly · Fable" || !second.Windows[2].Stale || !second.Windows[2].ObservedAt.Equal(aged.windowsAt) {
					t.Fatalf("fresh headers lost scoped data or its own age: %+v", second.Windows)
				}
			} else if second.Error != "" || second.Source != "probe" || second.Windows[0].Stale {
				t.Fatalf("optional credit failure contaminated successful usage: %+v", second)
			}
			for _, force := range []bool{false, true} {
				h.usageAccountFor(context.Background(), current, now.Add(time.Minute), force)
			}
			if len(*recorded) != calls*2 {
				t.Fatalf("refresh was not shared/bounded: calls=%d, want %d", len(*recorded), calls*2)
			}
		})
	}
}

func TestUsageLimitsMergesPartialHeadersWithIndependentProbeFreshness(t *testing.T) {
	now := time.Now()
	for _, age := range []time.Duration{time.Minute, 6 * time.Minute} {
		t.Run(age.String(), func(t *testing.T) {
			auth := claudeTestAuth("merged", "test@example.com")
			auth.Quota = coreauth.QuotaState{ObservedAt: now, Signals: map[string]string{"Anthropic-Ratelimit-Unified-5h-Utilization": "0.42"}}
			h, recorded := newUsageTestHandler(t, []*coreauth.Auth{auth}, func(*http.Request, []byte) *http.Response {
				t.Fatal("a retained read backoff must not be bypassed")
				return nil
			})
			current, _ := h.authManager.GetByID(auth.ID)
			current.EnsureIndex()
			at := now.Add(-age)
			h.usageLimits.put(current.Index, 0, &usageProbe{at: at, windowsAt: at, resetsAt: at, retryAt: now.Add(time.Hour),
				windows: []usageWindow{
					newUsageWindow(windowFiveHour, "5h", 80, time.Time{}, 300),
					newUsageWindow(windowWeekly, "Weekly", 60, time.Time{}, 10080),
					newUsageWindow(windowOther, "Weekly · Fable", 71, time.Time{}, 10080),
				}, resets: &usageResets{Available: 2}})
			account := h.usageAccountFor(context.Background(), current, now, true)
			stale := age >= usageLimitsTTL
			if len(account.Windows) != 3 || account.Windows[0].UsedPercent != 42 || account.Windows[0].Stale || account.Windows[1].UsedPercent != 60 || account.Windows[2].Label != "Weekly · Fable" || account.Windows[2].Stale != stale || !account.Windows[2].ObservedAt.Equal(at) {
				t.Fatalf("partial headers lost probe windows or freshness: %+v", account.Windows)
			}
			if account.Resets == nil || account.Resets.Available != 2 || account.ResetsStale != stale || !account.ResetsObservedAt.Equal(at) || len(*recorded) != 0 {
				t.Fatalf("inference timestamps refreshed credit data: %+v", account)
			}
		})
	}
}

func TestCodexCreditReadsFailAndThrottleIndependently(t *testing.T) {
	for _, failure := range []string{"rate limit", "server error", "invalid JSON", "missing credits", "transport"} {
		t.Run(failure, func(t *testing.T) {
			now := time.Now()
			h := &Handler{}
			calls := 0
			h.usageTransport = usageRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.String() == codexUsageURL {
					return usageJSONResponse(http.StatusOK, `{"rate_limit":{"primary_window":{"used_percent":25,"limit_window_seconds":18000}}}`), nil
				}
				calls++
				switch failure {
				case "rate limit":
					resp := usageJSONResponse(http.StatusTooManyRequests, `{}`)
					resp.Header.Set("Retry-After", "900")
					return resp, nil
				case "server error":
					return usageJSONResponse(http.StatusServiceUnavailable, `{}`), nil
				case "invalid JSON":
					return usageJSONResponse(http.StatusOK, `invalid`), nil
				case "missing credits":
					return usageJSONResponse(http.StatusOK, `{}`), nil
				default:
					return nil, fmt.Errorf("transport failed")
				}
			})
			previous := &usageProbe{resets: &usageResets{Available: 2}, resetsAt: now.Add(-6 * time.Minute)}
			probe := h.probeUsage(context.Background(), codexTestAuth("credit-failure", "test@example.com"), previous)
			if probe.err != "" || len(probe.windows) != 1 || probe.resetsErr == "" || probe.resets.Available != 2 || !probe.resetsAt.Equal(previous.resetsAt) || !probe.resetsRetryAt.After(now) {
				t.Fatalf("credit failure was hidden or discarded successful data: %+v", probe)
			}
			if failure == "rate limit" && (probe.resetsRetryAt.Before(now.Add(15*time.Minute)) || probe.resetsBackoff != 5*time.Minute) {
				t.Fatalf("credit Retry-After not honored: %+v", probe)
			}
			next := h.probeUsage(context.Background(), codexTestAuth("credit-failure", "test@example.com"), probe)
			if calls != 1 || next.resetsErr != probe.resetsErr || !next.resetsAt.Equal(probe.resetsAt) {
				t.Fatalf("successful usage bypassed credit backoff or refreshed credits: calls=%d probe=%+v", calls, next)
			}
		})
	}
}

func TestUsageLimitsInvalidationClearsSupplementalDataButPreservesCreditBackoff(t *testing.T) {
	now := time.Now()
	var cache usageLimitsCache
	cache.put("account", 0, &usageProbe{at: now, windowsAt: now, resetsAt: now, resets: &usageResets{Available: 2}, windows: []usageWindow{{Kind: windowWeekly}},
		resetsErr: "rate limited", resetsRetryAt: now.Add(time.Hour), resetsBackoff: 10 * time.Minute})
	cache.invalidate("account")
	probe, fresh := cache.get("account", now, true)
	if probe == nil || fresh || probe.resets != nil || !probe.resetsAt.IsZero() || len(probe.windows) != 0 || !probe.resetsRetryAt.Equal(now.Add(time.Hour)) || probe.resetsBackoff != 10*time.Minute {
		t.Fatalf("reset retained old data or dropped independent backoff: %+v, fresh=%v", probe, fresh)
	}
	cache.put("account", 0, &usageProbe{at: now, resets: &usageResets{Available: 3}})
	if current, _ := cache.get("account", now, true); current != probe {
		t.Fatal("a pre-reset read restored credits across the generation barrier")
	}
}

func TestCodexCreditBackoffIncreasesAndFreshZeroReplacesLastKnownInventory(t *testing.T) {
	h := &Handler{}
	limited := true
	h.usageTransport = usageRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() == codexUsageURL {
			return usageJSONResponse(http.StatusOK, `{"rate_limit":{}}`), nil
		}
		if limited {
			return usageJSONResponse(http.StatusTooManyRequests, `{}`), nil
		}
		return usageJSONResponse(http.StatusOK, `{"credits":[]}`), nil
	})
	auth := codexTestAuth("credit-backoff", "test@example.com")
	previous := &usageProbe{resets: &usageResets{Available: 2}, resetsAt: time.Now().Add(-time.Hour)}
	for _, wait := range []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute, 30 * time.Minute} {
		// Move the deadline, not the wall clock, to permit another attempt.
		previous.resetsRetryAt = time.Time{}
		probe := h.probeUsage(context.Background(), auth, previous)
		if probe.resetsBackoff != wait || probe.resetsRetryAt.Before(probe.at.Add(wait-time.Second)) || !probe.resetsAt.Equal(previous.resetsAt) {
			t.Fatalf("independent credit backoff or observation changed: %+v, want %v", probe, wait)
		}
		previous = probe
	}
	limited = false
	previous.resetsRetryAt = time.Time{}
	probe := h.probeUsage(context.Background(), auth, previous)
	if probe.resets == nil || probe.resets.Available != 0 || probe.resetsErr != "" || !probe.resetsRetryAt.IsZero() || probe.resetsBackoff != 0 || !probe.resetsAt.After(previous.resetsAt) {
		t.Fatalf("successful empty inventory did not replace stale credits or clear backoff: %+v", probe)
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

func TestUsageProbeDoesNotReturnSupplementalDataAcrossResetGeneration(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h, _ := newUsageTestHandler(t, []*coreauth.Auth{claudeTestAuth("inflight-reset", "test@example.com")}, func(*http.Request, []byte) *http.Response {
		close(started)
		<-release
		return usageJSONResponse(http.StatusOK, `{"limits":[{"kind":"weekly_scoped","percent":71,"scope":{"model":{"display_name":"Fable"}}}],"cedar_ember":{"eligible":true,"grants":[{"id":"old-grant","resets_left":2,"usable_now":true}]}}`)
	})
	auth, _ := h.authManager.GetByID("inflight-reset")
	auth.EnsureIndex()
	result := make(chan *usageProbe, 1)
	go func() { result <- h.cachedUsageProbe(context.Background(), auth, time.Now(), false) }()
	<-started
	h.usageLimits.invalidate(auth.Index)
	close(release)
	if probe := <-result; probe != nil {
		t.Fatalf("pre-reset supplemental data returned to a subscriber: %+v", probe)
	}
	if probe, _ := h.usageLimits.get(auth.Index, time.Now(), false); probe != nil {
		t.Fatalf("pre-reset supplemental data restored to the cache: %+v", probe)
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
	if got, fresh := h.usageLimits.get(current.Index, now, true); !fresh || !got.retryAt.Equal(limited.retryAt) || got.backoff != limited.backoff || got.resets != nil || len(got.windows) != 0 {
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
