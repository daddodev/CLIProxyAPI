package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"golang.org/x/sync/errgroup"
)

const (
	claudeUsageURL       = "https://api.anthropic.com/api/oauth/usage?cedar_ember=1&skip_spend=1"
	claudeResetURLFormat = "https://api.anthropic.com/api/organizations/%s/reset_rate_limits"
	claudeResetProgram   = "cedar_ember"
	claudeUsageUserAgent = "claude-cli/2.1.280 (external, cli)"
	codexUsageURL        = "https://chatgpt.com/backend-api/wham/usage"
	codexCreditsURL      = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"

	usageLimitsTTL        = 60 * time.Second
	usageLimitsErrorTTL   = 30 * time.Second
	usageLimitsMinRefresh = 10 * time.Second
	usageLimitsWorkers    = 4

	windowFiveHour = "five_hour"
	windowWeekly   = "weekly"
	windowOther    = "other"
)

// codexRedeemNamespace matches t3code's UUIDv5 namespace so a credit redeemed
// from either tool deduplicates on the same request ID.
var codexRedeemNamespace = uuid.UUID{0x6f, 0x1c, 0x2a, 0x9e, 0x2d, 0x4b, 0x4c, 0x1e, 0x9a, 0x7f, 0x3b, 0x8d, 0x5e, 0x0c, 0x1a, 0x42}

var (
	claudeGrantIDPattern = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
	creditIDPattern      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	requestIDPattern     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

type usageWindow struct {
	Kind          string     `json:"kind"`
	Label         string     `json:"label"`
	UsedPercent   float64    `json:"used_percent"`
	ResetsAt      *time.Time `json:"resets_at"`
	WindowMinutes int64      `json:"window_minutes,omitempty"`
}

type usageResets struct {
	Available     int64      `json:"available"`
	NextCreditID  string     `json:"next_credit_id,omitempty"`
	NextExpiresAt *time.Time `json:"next_expires_at,omitempty"`
}

type usageAccount struct {
	Priority                      int           `json:"priority"`
	QuotaLimitPercent             float64       `json:"quota_limit_percent"`
	QuotaFiveHourRemainingPercent float64       `json:"quota_5h_remaining_percent"`
	QuotaWeeklyRemainingPercent   float64       `json:"quota_weekly_remaining_percent"`
	LimitReached                  bool          `json:"limit_reached"`
	AuthIndex                     string        `json:"auth_index"`
	Label                         string        `json:"label"`
	Plan                          string        `json:"plan"`
	Disabled                      bool          `json:"disabled"`
	Status                        string        `json:"status"`
	CooldownUntil                 *time.Time    `json:"cooldown_until"`
	Source                        string        `json:"source"`
	ObservedAt                    *time.Time    `json:"observed_at"`
	Error                         string        `json:"error"`
	Windows                       []usageWindow `json:"windows"`
	Resets                        *usageResets  `json:"resets"`
}

type combinedWindow struct {
	Kind              string     `json:"kind"`
	Label             string     `json:"label"`
	UsedPercent       float64    `json:"used_percent"`
	CapacityPercent   float64    `json:"capacity_percent"`
	ReportingAccounts int        `json:"reporting_accounts"`
	NextResetAt       *time.Time `json:"next_reset_at"`
}

type usageProvider struct {
	Provider        string           `json:"provider"`
	Label           string           `json:"label"`
	AccountCount    int              `json:"account_count"`
	Windows         []combinedWindow `json:"windows"`
	ResetsAvailable int64            `json:"resets_available"`
	Accounts        []usageAccount   `json:"accounts"`
}

// usageProbe is one cached upstream read for an account.
type usageProbe struct {
	at      time.Time
	plan    string
	windows []usageWindow
	resets  *usageResets
	err     string
}

type usageLimitsCache struct {
	mu      sync.Mutex
	entries map[string]*usageProbe
	// generations advance on invalidate so a probe started before a reset
	// cannot restore pre-reset data when it finishes.
	generations map[string]uint64
}

func (c *usageLimitsCache) get(authIndex string, now time.Time, force bool) (*usageProbe, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[authIndex]
	if entry == nil {
		return nil, false
	}
	age := now.Sub(entry.at)
	ttl := usageLimitsTTL
	if entry.err != "" {
		ttl = usageLimitsErrorTTL
	}
	if force {
		ttl = usageLimitsMinRefresh
	}
	return entry, age < ttl
}

func (c *usageLimitsCache) generation(authIndex string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generations[authIndex]
}

// put stores a probe unless the account was invalidated after the probe began.
func (c *usageLimitsCache) put(authIndex string, generation uint64, probe *usageProbe) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generations[authIndex] != generation {
		return
	}
	if c.entries == nil {
		c.entries = make(map[string]*usageProbe)
	}
	c.entries[authIndex] = probe
}

func (c *usageLimitsCache) invalidate(authIndex string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, authIndex)
	if c.generations == nil {
		c.generations = make(map[string]uint64)
	}
	c.generations[authIndex]++
}

// usageLimitsAccount reports whether auth is a Claude or Codex OAuth account.
// API-key accounts are excluded: their key belongs to a configured base URL
// and must never be sent to the public subscription endpoints.
func usageLimitsAccount(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if provider != "claude" && provider != "codex" {
		return false
	}
	if auth.Attributes != nil && strings.TrimSpace(auth.Attributes["api_key"]) != "" {
		return false
	}
	return usageOAuthToken(auth) != ""
}

// usageOAuthToken returns only the OAuth access token. tokenValueFromMetadata
// is not used because it falls back to stored API keys.
func usageOAuthToken(auth *coreauth.Auth) string {
	if v := usageMetadataString(auth, "access_token"); v != "" {
		return v
	}
	return usageMetadataString(auth, "accessToken")
}

// GetUsageLimits reports subscription windows and reset credits for Claude and Codex accounts.
func (h *Handler) GetUsageLimits(c *gin.Context) {
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil {
		writeUsageError(c, http.StatusServiceUnavailable, "unavailable", "core auth manager unavailable")
		return
	}
	force := c.Query("refresh") == "1"
	now := time.Now()

	var auths []*coreauth.Auth
	for _, auth := range manager.List() {
		if !usageLimitsAccount(auth) {
			continue
		}
		auth.EnsureIndex()
		auths = append(auths, auth)
	}
	sort.Slice(auths, func(i, j int) bool {
		if auths[i].Provider != auths[j].Provider {
			return auths[i].Provider < auths[j].Provider
		}
		return usageAccountLabel(auths[i]) < usageAccountLabel(auths[j])
	})

	accounts := make([]usageAccount, len(auths))
	group, ctx := errgroup.WithContext(c.Request.Context())
	group.SetLimit(usageLimitsWorkers)
	for i, auth := range auths {
		group.Go(func() error {
			accounts[i] = h.usageAccountFor(ctx, auth, now, force)
			return nil
		})
	}
	_ = group.Wait()

	providers := make([]usageProvider, 0, 2)
	for _, provider := range []string{"claude", "codex"} {
		entry := usageProvider{Provider: provider, Label: usageProviderLabel(provider), Accounts: []usageAccount{}}
		for i, auth := range auths {
			if strings.ToLower(strings.TrimSpace(auth.Provider)) != provider {
				continue
			}
			entry.Accounts = append(entry.Accounts, accounts[i])
		}
		if len(entry.Accounts) == 0 {
			continue
		}
		entry.AccountCount = len(entry.Accounts)
		entry.Windows = combineUsageWindows(entry.Accounts, now)
		for _, account := range entry.Accounts {
			if account.Resets != nil && !account.Disabled {
				entry.ResetsAvailable += account.Resets.Available
			}
		}
		providers = append(providers, entry)
	}
	c.JSON(http.StatusOK, gin.H{"checked_at": now.UTC(), "providers": providers, "routing_strategy": h.cfg.Routing.Strategy})
}

func usageProviderLabel(provider string) string {
	if provider == "codex" {
		return "Codex"
	}
	return "Claude"
}

func usageAccountLabel(auth *coreauth.Auth) string {
	if auth.Metadata != nil {
		if email, ok := auth.Metadata["email"].(string); ok && strings.TrimSpace(email) != "" {
			return strings.TrimSpace(email)
		}
	}
	if label := strings.TrimSpace(auth.Label); label != "" {
		return label
	}
	return auth.ID
}

func (h *Handler) usageAccountFor(ctx context.Context, auth *coreauth.Auth, now time.Time, force bool) (account usageAccount) {
	defer func() {
		account.QuotaLimitPercent = coreauth.QuotaLimitPercent(auth)
		account.QuotaFiveHourRemainingPercent = coreauth.QuotaRemainingPercent(auth, coreauth.QuotaFiveHourRemainingPercentKey)
		account.QuotaWeeklyRemainingPercent = coreauth.QuotaRemainingPercent(auth, coreauth.QuotaWeeklyRemainingPercentKey)
		account.Priority, _ = strconv.Atoi(auth.Attributes["priority"])
		// Successful probes also feed the scheduler, so thresholds do not depend
		// on whether the last business response carried quota headers.
		if account.Source == "probe" && account.ObservedAt != nil {
			headers := usageWindowHeaders(auth.Provider, account.Windows)
			if h.authManager != nil {
				h.authManager.ObserveQuotaHeaders(auth.ID, auth.RegistrationEpoch, headers, *account.ObservedAt)
			}
			auth = auth.Clone()
			if !auth.Quota.ObservedAt.After(*account.ObservedAt) {
				auth.Quota.ObserveResponseHeadersForProvider(auth.Provider, headers, *account.ObservedAt)
			}
		}
		account.LimitReached, _ = coreauth.QuotaLimitReached(auth, now)
		if account.LimitReached && account.Status == "ok" {
			account.Status = "quota_limit"
		}
	}()
	account = usageAccount{
		AuthIndex: auth.Index,
		Label:     usageAccountLabel(auth),
		Disabled:  auth.Disabled,
		Status:    "ok",
		Source:    "none",
		Windows:   []usageWindow{},
	}
	if until := usageCooldownUntil(auth, now); until != nil {
		account.Status = "cooldown"
		account.CooldownUntil = until
	}
	if auth.Disabled {
		account.Status = "disabled"
	}

	var probe *usageProbe
	if !auth.Disabled {
		cached, fresh := h.usageLimits.get(auth.Index, now, force)
		probe = cached
		if !fresh {
			generation := h.usageLimits.generation(auth.Index)
			probe = h.probeUsage(ctx, auth)
			h.usageLimits.put(auth.Index, generation, probe)
		}
	}

	if probe != nil && probe.err == "" {
		account.Source = "probe"
		observed := probe.at.UTC()
		account.ObservedAt = &observed
		account.Plan = probe.plan
		account.Windows = expireUsageWindows(probe.windows, now)
		account.Resets = probe.resets
		return account
	}
	if probe != nil {
		account.Error = probe.err
		if probe.resets != nil {
			account.Resets = probe.resets
		}
	}
	if windows := usageWindowsFromSignals(auth.Provider, auth.Quota, now); len(windows) > 0 {
		account.Source = "headers"
		observed := auth.Quota.ObservedAt.UTC()
		account.ObservedAt = &observed
		account.Windows = windows
	} else if account.Error != "" && account.Status == "ok" {
		account.Status = "error"
	}
	return account
}

func usageCooldownUntil(auth *coreauth.Auth, now time.Time) *time.Time {
	var until time.Time
	if auth.Quota.Exceeded && auth.Quota.NextRecoverAt.After(now) {
		until = auth.Quota.NextRecoverAt
	}
	if auth.Unavailable && auth.NextRetryAfter.After(until) {
		until = auth.NextRetryAfter
	}
	if until.IsZero() || !until.After(now) {
		return nil
	}
	until = until.UTC()
	return &until
}

// expireUsageWindows zeroes windows whose reset time has already passed.
func expireUsageWindows(windows []usageWindow, now time.Time) []usageWindow {
	out := make([]usageWindow, 0, len(windows))
	for _, window := range windows {
		if window.ResetsAt != nil && !window.ResetsAt.After(now) {
			window.UsedPercent = 0
			window.ResetsAt = nil
		}
		out = append(out, window)
	}
	return out
}

func combineUsageWindows(accounts []usageAccount, now time.Time) []combinedWindow {
	combined := make([]combinedWindow, 0, 2)
	for _, kind := range []string{windowFiveHour, windowWeekly} {
		window := combinedWindow{Kind: kind, Label: usageWindowLabel(kind, 0)}
		for _, account := range accounts {
			if account.Disabled {
				continue
			}
			for _, w := range account.Windows {
				if w.Kind != kind {
					continue
				}
				window.ReportingAccounts++
				window.UsedPercent += w.UsedPercent
				window.CapacityPercent += 100
				if w.ResetsAt != nil && w.ResetsAt.After(now) && (window.NextResetAt == nil || w.ResetsAt.Before(*window.NextResetAt)) {
					resetAt := *w.ResetsAt
					window.NextResetAt = &resetAt
				}
				break
			}
		}
		if window.ReportingAccounts > 0 {
			combined = append(combined, window)
		}
	}
	return combined
}

func usageWindowKind(minutes int64) string {
	switch minutes {
	case 300:
		return windowFiveHour
	case 7 * 24 * 60:
		return windowWeekly
	default:
		return windowOther
	}
}

func usageWindowLabel(kind string, minutes int64) string {
	switch kind {
	case windowFiveHour:
		return "5h"
	case windowWeekly:
		return "Weekly"
	}
	if minutes > 0 && minutes%(24*60) == 0 {
		return fmt.Sprintf("%dd", minutes/(24*60))
	}
	if minutes > 0 && minutes%60 == 0 {
		return fmt.Sprintf("%dh", minutes/60)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm", minutes)
	}
	return "Other"
}

func newUsageWindow(kind, label string, usedPercent float64, resetsAt time.Time, minutes int64) usageWindow {
	window := usageWindow{Kind: kind, Label: label, UsedPercent: roundPercent(usedPercent), WindowMinutes: minutes}
	if !resetsAt.IsZero() {
		r := resetsAt.UTC()
		window.ResetsAt = &r
	}
	return window
}

func roundPercent(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return math.Round(v*10) / 10
}

func parseFlexibleTime(raw gjson.Result) time.Time {
	switch raw.Type {
	case gjson.Number:
		if raw.Int() > 0 {
			return time.Unix(raw.Int(), 0)
		}
	case gjson.String:
		if t, errParse := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw.String())); errParse == nil {
			return t
		}
		if n, errAtoi := strconv.ParseInt(strings.TrimSpace(raw.String()), 10, 64); errAtoi == nil && n > 0 {
			return time.Unix(n, 0)
		}
	}
	return time.Time{}
}

// usageWindowsFromSignals rebuilds windows from the last upstream response headers.
func usageWindowsFromSignals(provider string, quota coreauth.QuotaState, now time.Time) []usageWindow {
	if len(quota.Signals) == 0 {
		return nil
	}
	signal := func(name string) (string, bool) {
		for key, value := range quota.Signals {
			if strings.EqualFold(key, name) {
				return strings.TrimSpace(value), true
			}
		}
		return "", false
	}
	number := func(name string) (float64, bool) {
		raw, ok := signal(name)
		if !ok {
			return 0, false
		}
		v, errParse := strconv.ParseFloat(raw, 64)
		return v, errParse == nil
	}
	resetAt := func(name string) time.Time {
		raw, ok := signal(name)
		if !ok {
			return time.Time{}
		}
		return parseFlexibleTime(gjson.Parse(strconv.Quote(raw)))
	}

	var windows []usageWindow
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		for _, spec := range []struct{ prefix, kind string }{{"5h", windowFiveHour}, {"7d", windowWeekly}} {
			utilization, ok := number("Anthropic-Ratelimit-Unified-" + spec.prefix + "-Utilization")
			if !ok {
				continue
			}
			minutes := int64(300)
			if spec.kind == windowWeekly {
				minutes = 7 * 24 * 60
			}
			windows = append(windows, newUsageWindow(spec.kind, usageWindowLabel(spec.kind, 0), utilization*100,
				resetAt("Anthropic-Ratelimit-Unified-"+spec.prefix+"-Reset"), minutes))
		}
	case "codex":
		for _, prefix := range []string{"Primary", "Secondary"} {
			used, ok := number("X-Codex-" + prefix + "-Used-Percent")
			if !ok {
				continue
			}
			minutesFloat, _ := number("X-Codex-" + prefix + "-Window-Minutes")
			minutes := int64(minutesFloat)
			reset := resetAt("X-Codex-" + prefix + "-Reset-At")
			if reset.IsZero() {
				if after, okAfter := number("X-Codex-" + prefix + "-Reset-After-Seconds"); okAfter && !quota.ObservedAt.IsZero() {
					reset = quota.ObservedAt.Add(time.Duration(after) * time.Second)
				}
			}
			kind := usageWindowKind(minutes)
			windows = append(windows, newUsageWindow(kind, usageWindowLabel(kind, minutes), used, reset, minutes))
		}
	}
	return expireUsageWindows(windows, now)
}

// usageUpstreamResponse is the raw result of a provider usage call.
type usageUpstreamResponse struct {
	status int
	body   []byte
}

// usageUpstreamCall sends an authenticated request with the account's token
// through the management APICall transport and timeout.
func (h *Handler) usageUpstreamCall(ctx context.Context, auth *coreauth.Auth, method, rawURL string, payload any) (usageUpstreamResponse, error) {
	// Only the OAuth access token is used; see usageLimitsAccount.
	token := usageOAuthToken(auth)
	if token == "" || !usageLimitsAccount(auth) {
		return usageUpstreamResponse{}, errUsageSignedOut
	}
	var body io.Reader
	if payload != nil {
		data, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return usageUpstreamResponse{}, errMarshal
		}
		body = bytes.NewReader(data)
	}
	req, errReq := http.NewRequestWithContext(ctx, method, rawURL, body)
	if errReq != nil {
		return usageUpstreamResponse{}, errReq
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		req.Header.Set("anthropic-beta", "oauth-2025-04-20")
		req.Header.Set("User-Agent", claudeUsageUserAgent)
	case "codex":
		req.Header.Set("OpenAI-Beta", "codex-1")
		req.Header.Set("Originator", "Codex Desktop")
		if accountID := usageMetadataString(auth, "account_id"); accountID != "" {
			req.Header.Set("Chatgpt-Account-Id", accountID)
		}
	}
	transport := h.usageTransport
	if transport == nil {
		transport = h.apiCallTransport(auth, "")
	}
	client := &http.Client{Timeout: defaultAPICallTimeout, Transport: transport}
	resp, errDo := client.Do(req)
	if errDo != nil {
		return usageUpstreamResponse{}, errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("usage limits: close response body")
		}
	}()
	data, errRead := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if errRead != nil {
		return usageUpstreamResponse{}, errRead
	}
	return usageUpstreamResponse{status: resp.StatusCode, body: data}, nil
}

var errUsageSignedOut = fmt.Errorf("account token unavailable; sign in again")

func usageMetadataString(auth *coreauth.Auth, key string) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	if v, ok := auth.Metadata[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func (h *Handler) probeUsage(ctx context.Context, auth *coreauth.Auth) *usageProbe {
	probe := &usageProbe{at: time.Now()}
	var errProbe error
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		errProbe = h.probeClaudeUsage(ctx, auth, probe)
	case "codex":
		errProbe = h.probeCodexUsage(ctx, auth, probe)
	}
	if errProbe != nil {
		log.WithFields(log.Fields{"provider": auth.Provider, "auth_index": auth.Index}).WithError(errProbe).Debug("usage limits probe failed")
		probe.err = usageProbeMessage(errProbe)
	}
	return probe
}

type usageStatusError struct{ status int }

func (e usageStatusError) Error() string { return fmt.Sprintf("upstream returned HTTP %d", e.status) }

func usageProbeMessage(err error) string {
	if err == errUsageSignedOut {
		return err.Error()
	}
	if statusErr, ok := err.(usageStatusError); ok {
		switch statusErr.status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return "The provider rejected the account token; sign in again."
		case http.StatusTooManyRequests:
			return "The provider is rate limiting usage reads."
		}
		return fmt.Sprintf("The provider answered HTTP %d.", statusErr.status)
	}
	return "Could not read usage from the provider."
}

func (h *Handler) probeClaudeUsage(ctx context.Context, auth *coreauth.Auth, probe *usageProbe) error {
	resp, errCall := h.usageUpstreamCall(ctx, auth, http.MethodGet, claudeUsageURL, nil)
	if errCall != nil {
		return errCall
	}
	if resp.status < 200 || resp.status >= 300 {
		return usageStatusError{resp.status}
	}
	if !gjson.ValidBytes(resp.body) {
		return fmt.Errorf("invalid usage response")
	}
	root := gjson.ParseBytes(resp.body)
	probe.windows = parseClaudeUsageWindows(root)
	probe.resets = parseClaudeResetCredits(root.Get("cedar_ember"), time.Now())
	if plan := usageMetadataString(auth, "subscription_type"); plan != "" {
		probe.plan = plan
	}
	return nil
}

func parseClaudeUsageWindows(root gjson.Result) []usageWindow {
	windows := []usageWindow{}
	for _, spec := range []struct {
		key, kind string
		minutes   int64
	}{{"five_hour", windowFiveHour, 300}, {"seven_day", windowWeekly, 7 * 24 * 60}} {
		window := root.Get(spec.key)
		if !window.IsObject() || window.Get("utilization").Type != gjson.Number {
			continue
		}
		windows = append(windows, newUsageWindow(spec.kind, usageWindowLabel(spec.kind, 0),
			window.Get("utilization").Float(), parseFlexibleTime(window.Get("resets_at")), spec.minutes))
	}
	root.Get("limits").ForEach(func(_, limit gjson.Result) bool {
		name := strings.TrimSpace(limit.Get("scope.model.display_name").String())
		if limit.Get("kind").String() != "weekly_scoped" || name == "" || limit.Get("percent").Type != gjson.Number {
			return true
		}
		windows = append(windows, newUsageWindow(windowOther, "Weekly · "+name,
			limit.Get("percent").Float(), parseFlexibleTime(limit.Get("resets_at")), 7*24*60))
		return true
	})
	return windows
}

// parseClaudeResetCredits mirrors the Claude CLI: only unpaused grants usable
// now and not yet ended count, and only when the server names a next grant.
func parseClaudeResetCredits(block gjson.Result, now time.Time) *usageResets {
	if !block.IsObject() {
		return nil
	}
	if !block.Get("eligible").Bool() {
		return &usageResets{}
	}
	nextID := block.Get("next_grant_id").String()
	resets := &usageResets{}
	var nextFound bool
	block.Get("grants").ForEach(func(_, grant gjson.Result) bool {
		id := grant.Get("id").String()
		if !claudeGrantIDPattern.MatchString(id) || grant.Get("paused").Bool() || !grant.Get("usable_now").Bool() {
			return true
		}
		endsAt := parseFlexibleTime(grant.Get("ends_at"))
		if grant.Get("ends_at").Exists() && grant.Get("ends_at").Type != gjson.Null && !endsAt.After(now) {
			return true
		}
		if left := grant.Get("resets_left").Int(); left > 0 {
			resets.Available += left
		}
		if id == nextID {
			nextFound = true
			resets.NextCreditID = id
			if !endsAt.IsZero() {
				e := endsAt.UTC()
				resets.NextExpiresAt = &e
			}
		}
		return true
	})
	if !nextFound {
		return &usageResets{}
	}
	return resets
}

func (h *Handler) probeCodexUsage(ctx context.Context, auth *coreauth.Auth, probe *usageProbe) error {
	resp, errCall := h.usageUpstreamCall(ctx, auth, http.MethodGet, codexUsageURL, nil)
	if errCall != nil {
		return errCall
	}
	if resp.status < 200 || resp.status >= 300 {
		return usageStatusError{resp.status}
	}
	if !gjson.ValidBytes(resp.body) {
		return fmt.Errorf("invalid usage response")
	}
	root := gjson.ParseBytes(resp.body)
	probe.plan = codexPlanLabel(root.Get("plan_type").String())
	probe.windows = parseCodexUsageWindows(root.Get("rate_limit"))

	// Reset credits are optional; a failure leaves them unknown.
	credits, errCredits := h.usageUpstreamCall(ctx, auth, http.MethodGet, codexCreditsURL, nil)
	if errCredits == nil && credits.status >= 200 && credits.status < 300 && gjson.ValidBytes(credits.body) {
		probe.resets = parseCodexResetCredits(gjson.ParseBytes(credits.body), time.Now())
	}
	return nil
}

func parseCodexUsageWindows(rateLimit gjson.Result) []usageWindow {
	windows := []usageWindow{}
	for _, key := range []string{"primary_window", "secondary_window"} {
		window := rateLimit.Get(key)
		if !window.IsObject() || window.Get("used_percent").Type != gjson.Number {
			continue
		}
		minutes := window.Get("limit_window_seconds").Int() / 60
		kind := usageWindowKind(minutes)
		windows = append(windows, newUsageWindow(kind, usageWindowLabel(kind, minutes),
			window.Get("used_percent").Float(), parseFlexibleTime(window.Get("reset_at")), minutes))
	}
	return windows
}

func parseCodexResetCredits(root gjson.Result, now time.Time) *usageResets {
	type credit struct {
		id        string
		expiresAt time.Time
	}
	var available []credit
	root.Get("credits").ForEach(func(_, item gjson.Result) bool {
		id := item.Get("id").String()
		expiresAt := parseFlexibleTime(item.Get("expires_at"))
		if !creditIDPattern.MatchString(id) || item.Get("status").String() != "available" ||
			item.Get("reset_type").String() != "codex_rate_limits" || !expiresAt.After(now) {
			return true
		}
		available = append(available, credit{id: id, expiresAt: expiresAt})
		return true
	})
	sort.Slice(available, func(i, j int) bool { return available[i].expiresAt.Before(available[j].expiresAt) })
	resets := &usageResets{Available: int64(len(available))}
	if len(available) > 0 {
		resets.NextCreditID = available[0].id
		e := available[0].expiresAt.UTC()
		resets.NextExpiresAt = &e
	}
	return resets
}

func codexPlanLabel(plan string) string {
	plan = strings.TrimSpace(plan)
	switch strings.ToLower(plan) {
	case "":
		return ""
	case "plus":
		return "Plus"
	case "pro":
		return "Pro"
	case "team":
		return "Team"
	case "business":
		return "Business"
	case "enterprise":
		return "Enterprise"
	case "free":
		return "Free"
	}
	return plan
}

type usageResetRequest struct {
	AuthIndex string `json:"auth_index"`
	CreditID  string `json:"credit_id"`
	RequestID string `json:"request_id"`
}

func writeUsageError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}

// PostUsageLimitsReset redeems one provider reset credit for an account.
func (h *Handler) PostUsageLimitsReset(c *gin.Context) {
	var body usageResetRequest
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		writeUsageError(c, http.StatusBadRequest, "invalid_request", "Invalid request body.")
		return
	}
	auth := h.authByIndex(body.AuthIndex)
	if auth == nil || auth.Disabled || !usageLimitsAccount(auth) {
		writeUsageError(c, http.StatusNotFound, "not_found", "The account is missing or disabled.")
		return
	}
	creditID := strings.TrimSpace(body.CreditID)
	requestID := strings.TrimSpace(body.RequestID)
	if requestID != "" && !requestIDPattern.MatchString(requestID) {
		writeUsageError(c, http.StatusBadRequest, "invalid_request", "Invalid request_id.")
		return
	}

	var outcome string
	var status int
	var code, message string
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		outcome, status, code, message = h.redeemClaudeReset(c.Request.Context(), auth, creditID, requestID)
	default:
		outcome, status, code, message = h.redeemCodexReset(c.Request.Context(), auth, creditID)
	}
	h.usageLimits.invalidate(auth.Index)
	if code != "" {
		writeUsageError(c, status, code, message)
		return
	}

	log.WithFields(log.Fields{"provider": auth.Provider, "auth_index": auth.Index, "outcome": outcome}).Info("usage reset credit redeemed")
	warning := ""
	if outcome == "reset" || outcome == "already_redeemed" {
		h.mu.Lock()
		manager := h.authManager
		h.mu.Unlock()
		if manager == nil {
			warning = "Credit redeemed, but the proxy cooldown could not be cleared."
		} else if _, _, errReset := manager.ResetQuota(c.Request.Context(), auth.ID); errReset != nil {
			log.WithError(errReset).Warn("usage reset: clear cooldown failed")
			warning = "Credit redeemed, but the proxy cooldown could not be cleared. Routing resumes when it expires."
		}
	}
	c.JSON(http.StatusOK, gin.H{"outcome": outcome, "warning": warning})
}

func (h *Handler) redeemCodexReset(ctx context.Context, auth *coreauth.Auth, creditID string) (string, int, string, string) {
	if !creditIDPattern.MatchString(creditID) {
		return "", http.StatusBadRequest, "invalid_request", "Invalid credit_id."
	}
	accountID := usageMetadataString(auth, "account_id")
	if accountID == "" {
		accountID = auth.ID
	}
	payload := map[string]string{
		"redeem_request_id": uuid.NewSHA1(codexRedeemNamespace, []byte(accountID+":"+creditID)).String(),
		"credit_id":         creditID,
	}
	resp, errCall := h.usageUpstreamCall(ctx, auth, http.MethodPost, codexCreditsURL+"/consume", payload)
	if errCall != nil {
		return usageCallFailure(errCall)
	}
	if failure := usageStatusFailure(resp.status); failure.code != "" {
		return "", failure.status, failure.code, failure.message
	}
	if !gjson.ValidBytes(resp.body) {
		return "", http.StatusBadGateway, "unconfirmed", "Codex returned an invalid reset response. Retry to check whether it landed."
	}
	switch code := gjson.GetBytes(resp.body, "code").String(); code {
	case "reset", "nothing_to_reset", "no_credit", "already_redeemed":
		return code, http.StatusOK, "", ""
	}
	return "", http.StatusBadGateway, "unconfirmed", "Codex returned an unexpected reset response. Retry to check whether it landed."
}

func (h *Handler) redeemClaudeReset(ctx context.Context, auth *coreauth.Auth, grantID, requestID string) (string, int, string, string) {
	if !claudeGrantIDPattern.MatchString(grantID) {
		return "", http.StatusBadRequest, "invalid_request", "Invalid credit_id."
	}
	organization := usageMetadataString(auth, "organization_uuid")
	if organization == "" {
		return "", http.StatusConflict, "signed_out", "The Claude account has no organization; sign in again."
	}
	// The caller owns the idempotency key: a retry after a lost response must
	// reuse it, so the server never invents one it cannot hand back.
	if requestID == "" {
		return "", http.StatusBadRequest, "invalid_request", "request_id is required for Claude resets."
	}
	payload := map[string]string{"program": claudeResetProgram, "grant_id": grantID, "request_id": requestID}
	resp, errCall := h.usageUpstreamCall(ctx, auth, http.MethodPost, fmt.Sprintf(claudeResetURLFormat, url.PathEscape(organization)), payload)
	if errCall != nil {
		return usageCallFailure(errCall)
	}
	if failure := usageStatusFailure(resp.status); failure.code != "" {
		return "", failure.status, failure.code, failure.message
	}
	if !gjson.ValidBytes(resp.body) {
		return "", http.StatusBadGateway, "unconfirmed", "Claude returned an invalid reset response. Retry to check whether it landed."
	}
	switch result := gjson.GetBytes(resp.body, "result").String(); result {
	case "reset":
		return "reset", http.StatusOK, "", ""
	case "not_limited":
		return "nothing_to_reset", http.StatusOK, "", ""
	case "already_used":
		return "already_redeemed", http.StatusOK, "", ""
	case "ineligible":
		return "no_credit", http.StatusOK, "", ""
	case "cooldown":
		return "", http.StatusConflict, "cooling_down", "Claude resets are cooling down. Try again later."
	case "unavailable":
		// Claude could not confirm the claim; a retry with the same request ID asks about the same claim.
		return "", http.StatusBadGateway, "unconfirmed", "Claude could not confirm the reset. Retry if you are still limited."
	}
	return "", http.StatusBadGateway, "unconfirmed", "Claude returned an unexpected reset response. Retry to check whether it landed."
}

type usageFailure struct {
	status        int
	code, message string
}

func usageStatusFailure(status int) usageFailure {
	switch {
	case status == http.StatusTooManyRequests:
		return usageFailure{http.StatusTooManyRequests, "rate_limited", "The provider is rate limiting resets. Try again soon."}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return usageFailure{http.StatusConflict, "signed_out", "The provider rejected the account token; sign in again."}
	case status >= 500:
		// A server error may arrive after the claim was applied.
		return usageFailure{http.StatusBadGateway, "unconfirmed", fmt.Sprintf("The provider answered HTTP %d. Retry to check whether the reset landed.", status)}
	case status < 200 || status >= 300:
		return usageFailure{http.StatusBadGateway, "upstream_failed", fmt.Sprintf("The provider answered HTTP %d.", status)}
	}
	return usageFailure{}
}

// usageCallFailure maps a transport failure. The claim may still have landed,
// so it is reported as unconfirmed rather than failed.
func usageCallFailure(err error) (string, int, string, string) {
	if err == errUsageSignedOut {
		return "", http.StatusConflict, "signed_out", err.Error()
	}
	return "", http.StatusBadGateway, "unconfirmed", "The reset request did not complete. Retry to check whether it landed."
}

// usageWindowHeaders translates the two account-wide windows into the same
// observation contract used by business responses. Model-specific windows are
// intentionally excluded from the credential-wide gate.
func usageWindowHeaders(provider string, windows []usageWindow) http.Header {
	headers := make(http.Header)
	for _, w := range windows {
		if w.Kind != windowFiveHour && w.Kind != windowWeekly {
			continue
		}
		prefix := ""
		value := w.UsedPercent
		usedSuffix, resetSuffix := "-Used-Percent", "-Reset-At"
		switch provider {
		case "claude":
			prefix = "Anthropic-Ratelimit-Unified-5h"
			if w.Kind == windowWeekly {
				prefix = "Anthropic-Ratelimit-Unified-7d"
			}
			value /= 100
			usedSuffix, resetSuffix = "-Utilization", "-Reset"
		case "codex":
			prefix = "X-Codex-Primary"
			if w.Kind == windowWeekly {
				prefix = "X-Codex-Secondary"
			}
		default:
			continue
		}
		headers.Set(prefix+usedSuffix, strconv.FormatFloat(value, 'f', -1, 64))
		if w.ResetsAt != nil {
			headers.Set(prefix+resetSuffix, w.ResetsAt.Format(time.RFC3339Nano))
		}
	}
	return headers
}
