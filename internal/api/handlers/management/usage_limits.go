package management

import (
	"bytes"
	"context"
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

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usageupdates"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
)

const (
	claudeUsageURL       = "https://api.anthropic.com/api/oauth/usage?cedar_ember=1&skip_spend=1"
	claudeResetURLFormat = "https://api.anthropic.com/api/organizations/%s/reset_rate_limits"
	claudeResetProgram   = "cedar_ember"
	claudeUsageUserAgent = "claude-cli/2.1.321 (external, cli)"
	codexUsageURL        = "https://chatgpt.com/backend-api/wham/usage"
	codexCreditsURL      = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"

	usageLimitsTTL          = 5 * time.Minute
	usageLimitsErrorTTL     = 5 * time.Minute
	usageLimitsRateLimitTTL = 5 * time.Minute
	usageLimitsMaxBackoff   = 30 * time.Minute
	usageLimitsMinRefresh   = 5 * time.Minute
	usageLimitsWorkers      = 4

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
	Source        string     `json:"source,omitempty"`
	ObservedAt    *time.Time `json:"observed_at,omitempty"`
	Stale         bool       `json:"stale"`
}

type usageResets struct {
	Available     int64              `json:"available"`
	NextCreditID  string             `json:"next_credit_id,omitempty"`
	NextExpiresAt *time.Time         `json:"next_expires_at,omitempty"`
	Options       []usageResetOption `json:"options"`
}

type usageResetOption struct {
	CreditID  string     `json:"credit_id"`
	Label     string     `json:"label"`
	ResetType string     `json:"reset_type,omitempty"`
	Clears    []string   `json:"clears,omitempty"`
	Available int64      `json:"available"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
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
	ResetsObservedAt              *time.Time    `json:"resets_observed_at"`
	ResetsStale                   bool          `json:"resets_stale"`
	ResetsError                   string        `json:"resets_error"`
	ResetsRetryAt                 *time.Time    `json:"resets_retry_at"`
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
	at            time.Time
	plan          string
	windows       []usageWindow
	resets        *usageResets
	err           string
	windowsAt     time.Time
	resetsAt      time.Time
	resetsErr     string
	resetsRetryAt time.Time
	resetsBackoff time.Duration
	// A provider usage-read limit is separate from inference cooldowns. Even a
	// forced dashboard refresh must wait until this deadline.
	retryAt time.Time
	backoff time.Duration
}

type usageLimitsCache struct {
	probes  singleflight.Group
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
	if !entry.retryAt.IsZero() {
		return entry, now.Before(entry.retryAt)
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
	previous := c.entries[authIndex]
	if previous != nil {
		if probe.err != "" {
			probe.windows, probe.windowsAt, probe.plan = previous.windows, previous.windowsAt, previous.plan
		}
		if probe.resets == nil {
			probe.resets, probe.resetsAt = previous.resets, previous.resetsAt
		}
	}
	if !probe.retryAt.IsZero() {
		probe.backoff = usageLimitsRateLimitTTL
		if previous != nil && previous.backoff > 0 {
			probe.backoff = min(previous.backoff*2, usageLimitsMaxBackoff)
		}
		if earliest := probe.at.Add(probe.backoff); probe.retryAt.Before(earliest) {
			probe.retryAt = earliest
		}
	}
	c.entries[authIndex] = probe
}

func (c *usageLimitsCache) invalidate(authIndex string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Resetting generation state does not remove a usage-endpoint rate limit.
	// A user retrying a reset must not accidentally bypass read backoff.
	if entry := c.entries[authIndex]; entry != nil && (!entry.retryAt.IsZero() || !entry.resetsRetryAt.IsZero()) {
		// Keep only throttling state. Pre-reset windows and credits cannot be
		// offered again, even when the next read must wait for Retry-After.
		c.entries[authIndex] = &usageProbe{err: entry.err, retryAt: entry.retryAt, backoff: entry.backoff,
			resetsErr: entry.resetsErr, resetsRetryAt: entry.resetsRetryAt, resetsBackoff: entry.resetsBackoff}
	} else {
		delete(c.entries, authIndex)
	}
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
	result, errSnapshot := h.usageLimitsSnapshot(c.Request.Context(), time.Now(), c.Query("refresh") == "1")
	if errSnapshot != nil {
		writeUsageError(c, http.StatusServiceUnavailable, "unavailable", errSnapshot.Error())
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handler) usageLimitsSnapshot(ctx context.Context, now time.Time, force bool) (gin.H, error) {
	h.mu.Lock()
	manager := h.authManager
	strategy := h.cfg.Routing.Strategy
	h.mu.Unlock()
	if manager == nil {
		return nil, fmt.Errorf("core auth manager unavailable")
	}

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
	group, ctx := errgroup.WithContext(ctx)
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
	return gin.H{"checked_at": now.UTC(), "providers": providers, "routing_strategy": strategy}, nil
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

	windows := usageWindowsFromSignals(auth.Provider, auth.Quota, now)
	probe, fresh := h.usageLimits.get(auth.Index, now, force)
	// Headers cannot report reset credits or model-scoped windows. Refresh
	// those at most once per TTL, shared by all dashboard clients, even during
	// inference traffic. Provider read backoff still takes precedence.
	if !auth.Disabled && !fresh {
		probe = h.cachedUsageProbe(ctx, auth, now, force)
	}
	if auth.Disabled {
		probe = nil
	}
	if probe != nil {
		account.Error = probe.err
		account.Plan = probe.plan
		account.Resets = probe.resets
		account.ResetsError = probe.resetsErr
		account.ResetsStale = probe.resetsAt.IsZero() || probe.resetsErr != "" || !now.Before(probe.resetsAt.Add(usageLimitsTTL))
		if !probe.resetsAt.IsZero() {
			observed := probe.resetsAt.UTC()
			account.ResetsObservedAt = &observed
		}
		retry := probe.resetsRetryAt
		if probe.retryAt.After(retry) {
			retry = probe.retryAt
		}
		if retry.After(now) {
			retry = retry.UTC()
			account.ResetsRetryAt = &retry
		}
		if !probe.windowsAt.IsZero() {
			account.Source = "probe"
			if probe.err != "" || !now.Before(probe.windowsAt.Add(usageLimitsTTL)) {
				account.Source = "cached"
			}
			observed := probe.windowsAt.UTC()
			account.ObservedAt = &observed
			account.Windows = observedUsageWindows(probe.windows, probe.windowsAt, "probe", probe.err != "", now)
		}
	}
	if len(windows) > 0 && (account.ObservedAt == nil || auth.Quota.ObservedAt.After(*account.ObservedAt)) {
		account.Source = "headers"
		observed := auth.Quota.ObservedAt.UTC()
		account.ObservedAt = &observed
		account.Windows = mergeUsageWindows(observedUsageWindows(windows, auth.Quota.ObservedAt, "headers", false, now), account.Windows)
		if plan := usageMetadataString(auth, "subscription_type"); plan != "" {
			account.Plan = plan
		}
		if strings.EqualFold(auth.Provider, "codex") {
			for name, value := range auth.Quota.Signals {
				if strings.EqualFold(name, "X-Codex-Plan-Type") {
					account.Plan = codexPlanLabel(value)
				}
			}
		}
	} else {
		account.Windows = mergeUsageWindows(account.Windows, observedUsageWindows(windows, auth.Quota.ObservedAt, "headers", false, now))
	}
	if len(account.Windows) == 0 && account.Error != "" && account.Status == "ok" {
		account.Status = "error"
	}
	return account
}

// cachedUsageProbe shares cache misses between SSE clients and manual reads.
// Generation keys prevent a reset from joining a probe started before it.
func (h *Handler) cachedUsageProbe(ctx context.Context, auth *coreauth.Auth, now time.Time, force bool) *usageProbe {
	generation := h.usageLimits.generation(auth.Index)
	key := fmt.Sprintf("%s:%d", auth.Index, generation)
	result := h.usageLimits.probes.DoChan(key, func() (any, error) {
		previous, fresh := h.usageLimits.get(auth.Index, now, force)
		if fresh {
			return previous, nil
		}
		// The management call has its own allowed timeout. One disconnected
		// dashboard must not cancel a read shared with other subscribers.
		probe := h.probeUsage(context.WithoutCancel(ctx), auth, previous)
		h.usageLimits.put(auth.Index, generation, probe)
		return probe, nil
	})
	select {
	case <-ctx.Done():
		return nil
	case completed := <-result:
		// A reset may have invalidated this read while it was in flight.
		if h.usageLimits.generation(auth.Index) != generation {
			return nil
		}
		return completed.Val.(*usageProbe)
	}
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

func observedUsageWindows(windows []usageWindow, at time.Time, source string, failed bool, now time.Time) []usageWindow {
	out := expireUsageWindows(windows, now)
	for i := range out {
		out[i].Source = source
		out[i].Stale = failed || at.IsZero() || !now.Before(at.Add(usageLimitsTTL))
		if !at.IsZero() {
			observed := at.UTC()
			out[i].ObservedAt = &observed
		}
	}
	return out
}

// mergeUsageWindows keeps the preferred source's account-wide values without
// dropping scoped windows or windows absent from a partial header observation.
func mergeUsageWindows(preferred, supplemental []usageWindow) []usageWindow {
	out := append([]usageWindow{}, preferred...)
	for _, window := range supplemental {
		found := false
		for _, existing := range out {
			if existing.Kind == window.Kind && (window.Kind != windowOther || existing.Label == window.Label) {
				found = true
				break
			}
		}
		if !found {
			out = append(out, window)
		}
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

func parseFlexibleTime(raw ojson.Result) time.Time {
	switch raw.Type {
	case ojson.Number:
		if raw.Int() > 0 {
			return time.Unix(raw.Int(), 0)
		}
	case ojson.String:
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
		return parseFlexibleTime(ojson.Parse(strconv.Quote(raw)))
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
	header http.Header
}

// usageUpstreamCall sends an authenticated request with the account's token
// through the management APICall transport and timeout.
func (h *Handler) usageUpstreamCall(ctx context.Context, auth *coreauth.Auth, method, rawURL string, payload map[string]string) (usageUpstreamResponse, error) {
	// Only the OAuth access token is used; see usageLimitsAccount.
	token := usageOAuthToken(auth)
	if token == "" || !usageLimitsAccount(auth) {
		return usageUpstreamResponse{}, errUsageSignedOut
	}
	var body io.Reader
	if payload != nil {
		doc := ojson.NewDocString("{}")
		for key, value := range payload {
			if errSet := doc.Set(key, value); errSet != nil {
				return usageUpstreamResponse{}, errSet
			}
		}
		body = bytes.NewReader(doc.Bytes())
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
	return usageUpstreamResponse{status: resp.StatusCode, body: data, header: resp.Header}, nil
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

func (h *Handler) probeUsage(ctx context.Context, auth *coreauth.Auth, previous *usageProbe) *usageProbe {
	probe := &usageProbe{at: time.Now()}
	if previous != nil {
		probe.resets, probe.resetsAt, probe.resetsErr = previous.resets, previous.resetsAt, previous.resetsErr
		probe.resetsRetryAt, probe.resetsBackoff = previous.resetsRetryAt, previous.resetsBackoff
	}
	var errProbe error
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		errProbe = h.probeClaudeUsage(ctx, auth, probe)
	case "codex":
		errProbe = h.probeCodexUsage(ctx, auth, probe)
	}
	probe.at = time.Now()
	if errProbe != nil {
		log.WithFields(log.Fields{"provider": auth.Provider, "auth_index": auth.Index}).WithError(errProbe).Debug("usage limits probe failed")
		probe.err = usageProbeMessage(errProbe)
		probe.resetsErr = probe.err
		if statusErr, ok := errProbe.(usageStatusError); ok && statusErr.status == http.StatusTooManyRequests {
			probe.retryAt = usageRateLimitRetryAt(statusErr.retryAfter, probe.at)
		}
	} else {
		probe.windowsAt = probe.at
	}
	return probe
}

type usageStatusError struct {
	status     int
	retryAfter string
}

// usageRateLimitRetryAt honors Retry-After without letting dashboard refreshes
// repeatedly hit a throttled endpoint when the provider omits a usable deadline.
func usageRateLimitRetryAt(raw string, now time.Time) time.Time {
	raw = strings.TrimSpace(raw)
	if seconds, errParse := strconv.ParseInt(raw, 10, 64); errParse == nil && seconds > 0 && seconds <= int64((1<<63-1)/time.Second) {
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if retryAt, errParse := http.ParseTime(raw); errParse == nil && retryAt.After(now) {
		return retryAt
	}
	return now.Add(usageLimitsRateLimitTTL)
}

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
		return usageStatusError{status: resp.status, retryAfter: resp.header.Get("Retry-After")}
	}
	if !ojson.ValidBytes(resp.body) {
		return fmt.Errorf("invalid usage response")
	}
	root := ojson.ParseBytes(resp.body)
	probe.windows = parseClaudeUsageWindows(root)
	probe.resets = parseClaudeResetCredits(root.Get("cedar_ember"), time.Now())
	probe.resetsErr = ""
	if probe.resets != nil {
		probe.resetsAt = time.Now()
	} else {
		probe.resetsErr = "The provider did not report reset credits."
	}
	if plan := usageMetadataString(auth, "subscription_type"); plan != "" {
		probe.plan = plan
	}
	return nil
}

func parseClaudeUsageWindows(root ojson.Result) []usageWindow {
	windows := []usageWindow{}
	for _, spec := range []struct {
		key, kind string
		minutes   int64
	}{{"five_hour", windowFiveHour, 300}, {"seven_day", windowWeekly, 7 * 24 * 60}} {
		window := root.Get(spec.key)
		if !window.IsObject() || window.Get("utilization").Type != ojson.Number {
			continue
		}
		windows = append(windows, newUsageWindow(spec.kind, usageWindowLabel(spec.kind, 0),
			window.Get("utilization").Float(), parseFlexibleTime(window.Get("resets_at")), spec.minutes))
	}
	root.Get("limits").ForEach(func(_, limit ojson.Result) bool {
		name := strings.TrimSpace(limit.Get("scope.model.display_name").String())
		if limit.Get("kind").String() != "weekly_scoped" || name == "" || limit.Get("percent").Type != ojson.Number {
			return true
		}
		windows = append(windows, newUsageWindow(windowOther, "Weekly · "+name,
			limit.Get("percent").Float(), parseFlexibleTime(limit.Get("resets_at")), 7*24*60))
		return true
	})
	return windows
}

// parseClaudeResetCredits keeps each usable grant so callers can choose its
// reset type instead of always spending the provider's suggested next grant.
func parseClaudeResetCredits(block ojson.Result, now time.Time) *usageResets {
	if !block.IsObject() {
		return nil
	}
	resets := &usageResets{Options: []usageResetOption{}}
	if !block.Get("eligible").Bool() || parseFlexibleTime(block.Get("cooldown_until")).After(now) {
		return resets
	}
	nextID := block.Get("next_grant_id").String()
	block.Get("grants").ForEach(func(_, grant ojson.Result) bool {
		id := grant.Get("id").String()
		left := grant.Get("resets_left").Int()
		if !claudeGrantIDPattern.MatchString(id) || left <= 0 || grant.Get("paused").Bool() || !grant.Get("usable_now").Bool() {
			return true
		}
		if parseFlexibleTime(grant.Get("starts_at")).After(now) {
			return true
		}
		endsAt := parseFlexibleTime(grant.Get("ends_at"))
		if grant.Get("ends_at").Exists() && grant.Get("ends_at").Type != ojson.Null && !endsAt.After(now) {
			return true
		}
		option := usageResetOption{
			CreditID: id, Label: strings.TrimSpace(grant.Get("label").String()),
			ResetType: grant.Get("type").String(), Available: left,
		}
		grant.Get("clears").ForEach(func(_, window ojson.Result) bool {
			if window.Type == ojson.String && strings.TrimSpace(window.String()) != "" {
				option.Clears = append(option.Clears, window.String())
			}
			return true
		})
		if !endsAt.IsZero() {
			e := endsAt.UTC()
			option.ExpiresAt = &e
		}
		resets.Options = append(resets.Options, option)
		resets.Available += left
		if id == nextID {
			resets.NextCreditID = id
			resets.NextExpiresAt = option.ExpiresAt
		}
		return true
	})
	return resets
}

func (h *Handler) probeCodexUsage(ctx context.Context, auth *coreauth.Auth, probe *usageProbe) error {
	resp, errCall := h.usageUpstreamCall(ctx, auth, http.MethodGet, codexUsageURL, nil)
	if errCall != nil {
		return errCall
	}
	if resp.status < 200 || resp.status >= 300 {
		return usageStatusError{status: resp.status, retryAfter: resp.header.Get("Retry-After")}
	}
	if !ojson.ValidBytes(resp.body) {
		return fmt.Errorf("invalid usage response")
	}
	root := ojson.ParseBytes(resp.body)
	probe.plan = codexPlanLabel(root.Get("plan_type").String())
	probe.windows = parseCodexUsageWindows(root.Get("rate_limit"))

	// Credit reads fail and throttle independently from ordinary usage reads.
	if time.Now().Before(probe.resetsRetryAt) {
		return nil
	}
	credits, errCredits := h.usageUpstreamCall(ctx, auth, http.MethodGet, codexCreditsURL, nil)
	if errCredits == nil && (credits.status < 200 || credits.status >= 300) {
		errCredits = usageStatusError{status: credits.status, retryAfter: credits.header.Get("Retry-After")}
	}
	if errCredits == nil && (!ojson.ValidBytes(credits.body) || !ojson.GetBytes(credits.body, "credits").IsArray()) {
		errCredits = fmt.Errorf("invalid reset credits response")
	}
	if errCredits != nil {
		probe.resetsErr = usageProbeMessage(errCredits)
		probe.resetsRetryAt = time.Now().Add(usageLimitsErrorTTL)
		if statusErr, ok := errCredits.(usageStatusError); ok && statusErr.status == http.StatusTooManyRequests {
			probe.resetsBackoff = min(max(probe.resetsBackoff*2, usageLimitsRateLimitTTL), usageLimitsMaxBackoff)
			probe.resetsRetryAt = usageRateLimitRetryAt(statusErr.retryAfter, time.Now())
			if earliest := time.Now().Add(probe.resetsBackoff); probe.resetsRetryAt.Before(earliest) {
				probe.resetsRetryAt = earliest
			}
		}
		return nil
	}
	probe.resets = parseCodexResetCredits(ojson.ParseBytes(credits.body), time.Now())
	probe.resetsAt = time.Now()
	probe.resetsErr = ""
	probe.resetsRetryAt = time.Time{}
	probe.resetsBackoff = 0
	return nil
}

func parseCodexUsageWindows(rateLimit ojson.Result) []usageWindow {
	windows := []usageWindow{}
	for _, key := range []string{"primary_window", "secondary_window"} {
		window := rateLimit.Get(key)
		if !window.IsObject() || window.Get("used_percent").Type != ojson.Number {
			continue
		}
		minutes := window.Get("limit_window_seconds").Int() / 60
		kind := usageWindowKind(minutes)
		windows = append(windows, newUsageWindow(kind, usageWindowLabel(kind, minutes),
			window.Get("used_percent").Float(), parseFlexibleTime(window.Get("reset_at")), minutes))
	}
	return windows
}

func parseCodexResetCredits(root ojson.Result, now time.Time) *usageResets {
	type credit struct {
		id        string
		expiresAt time.Time
	}
	var available []credit
	root.Get("credits").ForEach(func(_, item ojson.Result) bool {
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
	resets := &usageResets{Available: int64(len(available)), Options: []usageResetOption{}}
	for _, credit := range available {
		e := credit.expiresAt.UTC()
		resets.Options = append(resets.Options, usageResetOption{
			CreditID: credit.id, Label: "Codex rate-limit reset", ResetType: "codex_rate_limits",
			Available: 1, ExpiresAt: &e,
		})
	}
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
	if code == "" || code == "unconfirmed" {
		h.usageLimits.invalidate(auth.Index)
		defer usageupdates.Notify()
	}
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
	if !ojson.ValidBytes(resp.body) {
		return "", http.StatusBadGateway, "unconfirmed", "Codex returned an invalid reset response. Retry to check whether it landed."
	}
	switch code := ojson.GetBytes(resp.body, "code").String(); code {
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
	if !ojson.ValidBytes(resp.body) {
		return "", http.StatusBadGateway, "unconfirmed", "Claude returned an invalid reset response. Retry to check whether it landed."
	}
	switch result := ojson.GetBytes(resp.body, "result").String(); result {
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
		if w.Source != "" && (w.Source != "probe" || w.Stale) {
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
			headers.Set(prefix+"-Window-Minutes", strconv.FormatInt(w.WindowMinutes, 10))
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
