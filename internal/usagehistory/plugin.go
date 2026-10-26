package usagehistory

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usageupdates"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

// DefaultFileName is the database file created inside the auth directory.
const DefaultFileName = "usage-history.db"

var (
	currentMu sync.RWMutex
	current   *Store
	pricing   *Pricing
	// started is set by Start so config reloads outside a running service
	// (for example in tests) never create a database.
	started bool
)

func init() {
	coreusage.RegisterNamedPlugin("usage-history", plugin{})
}

type plugin struct{}

func (plugin) HandleUsage(_ context.Context, record coreusage.Record) {
	// Holding the read lock across add keeps Configure and Shutdown from
	// retiring the store between selecting it and buffering into it.
	currentMu.RLock()
	defer currentMu.RUnlock()
	store := current
	if store == nil {
		return
	}
	provider := strings.ToLower(strings.TrimSpace(record.Provider))
	if provider == "" {
		provider = "unknown"
	}
	model := strings.TrimSpace(record.Model)
	if model == "" {
		model = "unknown"
	}
	at := record.RequestedAt
	if at.IsZero() {
		at = time.Now()
	}
	detail := coreusage.EnsureTokenBreakdownForProvider(record.Detail, provider, record.ExecutorType)
	breakdown := detail.TokenBreakdown
	counters := Counters{
		Requests:      1,
		UncachedInput: breakdown.Input.UncachedTokens,
		CacheRead:     breakdown.Input.CacheReadTokens,
		CacheWrite:    breakdown.Input.CacheWriteTokens,
		Output:        breakdown.Output.TotalTokens,
		Reasoning:     breakdown.Output.ReasoningTokens,
		Unclassified:  breakdown.UnclassifiedTokens,
	}
	if record.Failed {
		counters.Failed = 1
	}
	store.add(at, provider, model, strings.TrimSpace(record.AuthIndex), normalizeTier(record), counters)
	usageupdates.Notify()
}

// normalizeTier maps the billed service tier onto the pricing tiers.
func normalizeTier(record coreusage.Record) string {
	tier := strings.TrimSpace(record.ResponseServiceTier)
	if tier == "" {
		tier = strings.TrimSpace(record.Detail.ResponseServiceTier)
	}
	if tier == "" {
		tier = strings.TrimSpace(record.EffectiveServiceTier)
	}
	switch strings.ToLower(tier) {
	case "priority", "fast":
		return TierFast
	default:
		return TierStandard
	}
}

// Current returns the open store, or nil when history is disabled.
func Current() *Store {
	currentMu.RLock()
	defer currentMu.RUnlock()
	return current
}

// CurrentPricing returns the active price table loader, or nil when history is disabled.
func CurrentPricing() *Pricing {
	currentMu.RLock()
	defer currentMu.RUnlock()
	return pricing
}

// ResolvePath returns the configured database path.
func ResolvePath(cfg *config.Config) (string, error) {
	if path := strings.TrimSpace(cfg.UsageHistory.Path); path != "" {
		return path, nil
	}
	authDir, errDir := util.ResolveAuthDir(cfg.AuthDir)
	if errDir != nil {
		return "", errDir
	}
	return filepath.Join(authDir, DefaultFileName), nil
}

// Start enables history for a running service and applies cfg.
func Start(cfg *config.Config) {
	currentMu.Lock()
	started = true
	currentMu.Unlock()
	Configure(cfg)
}

// Configure opens, reopens or closes the store to match cfg. It does nothing
// until Start has been called and is safe to call on every config reload.
func Configure(cfg *config.Config) {
	if cfg == nil {
		return
	}
	currentMu.RLock()
	running := started
	currentMu.RUnlock()
	if !running {
		return
	}
	var path string
	if !cfg.UsageHistory.Disabled {
		resolved, errPath := ResolvePath(cfg)
		if errPath != nil {
			log.WithError(errPath).Error("usage history: resolve database path")
			return
		}
		path = resolved
	}

	currentMu.Lock()
	defer currentMu.Unlock()
	if !started {
		return
	}
	if current != nil && current.Path() == path {
		pricing.SetProxyURL(cfg.ProxyURL)
		return
	}
	if current != nil {
		if errClose := current.Close(); errClose != nil {
			log.WithError(errClose).Warn("usage history: close database")
		}
		current = nil
	}
	if pricing != nil {
		pricing.Stop()
		pricing = nil
	}
	if path == "" {
		return
	}
	store, errOpen := Open(path)
	if errOpen != nil {
		log.WithError(errOpen).Error("usage history: disabled")
		return
	}
	current = store
	pricing = NewPricing(filepath.Join(filepath.Dir(path), pricingCacheFileName), cfg.ProxyURL)
	log.WithField("path", path).Info("usage history enabled")
}

// Shutdown flushes and closes the store.
func Shutdown() {
	currentMu.Lock()
	defer currentMu.Unlock()
	started = false
	if current != nil {
		if errClose := current.Close(); errClose != nil {
			log.WithError(errClose).Warn("usage history: close database")
		}
		current = nil
	}
	if pricing != nil {
		pricing.Stop()
		pricing = nil
	}
}
