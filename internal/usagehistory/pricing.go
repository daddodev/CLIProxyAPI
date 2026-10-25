package usagehistory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

const (
	// TierStandard and TierFast are the stored pricing tiers.
	TierStandard = "standard"
	TierFast     = "fast"

	pricingURL           = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
	pricingCacheFileName = "usage-pricing.json"
	pricingMaxAge        = 24 * time.Hour
	pricingRetryInterval = 10 * time.Minute
	// pricingFetchTimeout bounds the price table download. This is a public
	// catalogue fetch, not upstream model traffic.
	pricingFetchTimeout = 60 * time.Second
	pricingMaxBytes     = 32 << 20
)

// Pricing status values reported to the dashboard.
const (
	PricingFresh       = "fresh"
	PricingCached      = "cached"
	PricingUnavailable = "unavailable"
)

// TokenRates are USD prices per token.
type TokenRates struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
}

// ModelRate holds standard and fast-tier rates for one model.
type ModelRate struct {
	Standard TokenRates
	Fast     TokenRates
}

// RateTable maps normalized model names to rates.
type RateTable map[string]ModelRate

// CostBreakdown is the USD cost split by token category.
type CostBreakdown struct {
	Input      float64 `json:"input"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Output     float64 `json:"output"`
}

// Total returns the summed cost.
func (c CostBreakdown) Total() float64 { return c.Input + c.CacheRead + c.CacheWrite + c.Output }

func (c *CostBreakdown) add(o CostBreakdown) {
	c.Input += o.Input
	c.CacheRead += o.CacheRead
	c.CacheWrite += o.CacheWrite
	c.Output += o.Output
}

var (
	bracketSuffix  = regexp.MustCompile(`\[[^\]]*\]$`)
	thinkingSuffix = regexp.MustCompile(`\([^)]*\)$`)
	dateSuffix     = regexp.MustCompile(`-(\d{8}|\d{4}-\d{2}-\d{2})$`)
)

func normalizeModelName(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	m = strings.TrimSpace(bracketSuffix.ReplaceAllString(m, ""))
	m = strings.TrimSpace(thinkingSuffix.ReplaceAllString(m, ""))
	return m
}

// Lookup finds rates for model, falling back to the undated name.
func (t RateTable) Lookup(model string) (ModelRate, bool) {
	m := normalizeModelName(model)
	if rate, ok := t[m]; ok {
		return rate, true
	}
	if undated := dateSuffix.ReplaceAllString(m, ""); undated != m {
		rate, ok := t[undated]
		return rate, ok
	}
	return ModelRate{}, false
}

// Cost prices counters at the given tier. It reports false for unknown models.
func (t RateTable) Cost(model, tier string, c Counters) (CostBreakdown, bool) {
	rate, ok := t.Lookup(model)
	if !ok {
		return CostBreakdown{}, false
	}
	r := rate.Standard
	if tier == TierFast {
		r = rate.Fast
	}
	return CostBreakdown{
		Input:      float64(c.UncachedInput) * r.Input,
		CacheRead:  float64(c.CacheRead) * r.CacheRead,
		CacheWrite: float64(c.CacheWrite) * r.CacheWrite,
		Output:     float64(c.Output) * r.Output,
	}, true
}

type liteLLMEntry map[string]json.RawMessage

func (e liteLLMEntry) number(key string) (float64, bool) {
	raw, ok := e[key]
	if !ok {
		return 0, false
	}
	var v float64
	if errUnmarshal := json.Unmarshal(raw, &v); errUnmarshal != nil || v < 0 {
		return 0, false
	}
	return v, true
}

func (e liteLLMEntry) fastMultiplier() (float64, bool) {
	raw, ok := e["provider_specific_entry"]
	if !ok {
		return 0, false
	}
	var specific map[string]any
	if errUnmarshal := json.Unmarshal(raw, &specific); errUnmarshal != nil {
		return 0, false
	}
	v, ok := specific["fast"].(float64)
	return v, ok && v > 0
}

// readRates reads one tier. Missing cache prices fall back to the input price
// so cached tokens are never treated as free.
func (e liteLLMEntry) readRates(suffix string) (TokenRates, bool) {
	input, okInput := e.number("input_cost_per_token" + suffix)
	output, okOutput := e.number("output_cost_per_token" + suffix)
	if !okInput || !okOutput {
		return TokenRates{}, false
	}
	r := TokenRates{Input: input, Output: output, CacheRead: input, CacheWrite: input}
	if v, ok := e.number("cache_read_input_token_cost" + suffix); ok {
		r.CacheRead = v
	}
	if v, ok := e.number("cache_creation_input_token_cost" + suffix); ok {
		r.CacheWrite = v
	}
	return r, true
}

// ParseRateTable builds a rate table from LiteLLM's price JSON.
func ParseRateTable(data []byte) (RateTable, error) {
	var raw map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(data, &raw); errUnmarshal != nil {
		return nil, fmt.Errorf("parse price table: %w", errUnmarshal)
	}
	table := make(RateTable, len(raw))
	aliases := make(map[string][]ModelRate)
	for key, value := range raw {
		var entry liteLLMEntry
		if json.Unmarshal(value, &entry) != nil {
			continue
		}
		standard, ok := entry.readRates("")
		if !ok {
			continue
		}
		fast := standard
		if priority, okPriority := entry.readRates("_priority"); okPriority {
			// Tier cache prices that are missing keep the standard cache-to-input ratio.
			if _, ok := entry.number("cache_read_input_token_cost_priority"); !ok && standard.Input > 0 {
				priority.CacheRead = priority.Input * standard.CacheRead / standard.Input
			}
			if _, ok := entry.number("cache_creation_input_token_cost_priority"); !ok && standard.Input > 0 {
				priority.CacheWrite = priority.Input * standard.CacheWrite / standard.Input
			}
			fast = priority
		} else if multiplier, okMultiplier := entry.fastMultiplier(); okMultiplier {
			fast = TokenRates{
				Input:      standard.Input * multiplier,
				Output:     standard.Output * multiplier,
				CacheRead:  standard.CacheRead * multiplier,
				CacheWrite: standard.CacheWrite * multiplier,
			}
		}
		rate := ModelRate{Standard: standard, Fast: fast}
		name := normalizeModelName(key)
		table[name] = rate
		if _, bare, found := strings.Cut(name, "/"); found && bare != "" && !strings.Contains(bare, "/") {
			aliases[bare] = append(aliases[bare], rate)
		}
	}
	// A provider-prefixed key also prices its bare name when no bare key exists
	// and every prefixed variant agrees.
	for bare, rates := range aliases {
		if _, exists := table[bare]; exists {
			continue
		}
		agree := true
		for _, r := range rates[1:] {
			if r != rates[0] {
				agree = false
				break
			}
		}
		if agree {
			table[bare] = rates[0]
		}
	}
	if len(table) == 0 {
		return nil, errors.New("price table has no priced models")
	}
	return table, nil
}

// Pricing loads the price table from disk and refreshes it daily.
type Pricing struct {
	cachePath string

	mu        sync.RWMutex
	table     RateTable
	updatedAt time.Time
	proxyURL  string

	stop     chan struct{}
	stopOnce sync.Once
}

// NewPricing loads the cached table and starts the refresh loop.
func NewPricing(cachePath, proxyURL string) *Pricing {
	p := &Pricing{cachePath: cachePath, proxyURL: proxyURL, stop: make(chan struct{})}
	p.loadCache()
	go p.run()
	return p
}

// SetProxyURL updates the proxy used for future downloads.
func (p *Pricing) SetProxyURL(proxyURL string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.proxyURL = proxyURL
	p.mu.Unlock()
}

// Stop ends the refresh loop.
func (p *Pricing) Stop() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() { close(p.stop) })
}

// Snapshot returns the current table, its status and its update time.
func (p *Pricing) Snapshot() (RateTable, string, time.Time) {
	if p == nil {
		return nil, PricingUnavailable, time.Time{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.table == nil {
		return nil, PricingUnavailable, time.Time{}
	}
	if time.Since(p.updatedAt) > pricingMaxAge+time.Hour {
		return p.table, PricingCached, p.updatedAt
	}
	return p.table, PricingFresh, p.updatedAt
}

func (p *Pricing) loadCache() {
	info, errStat := os.Stat(p.cachePath)
	if errStat != nil {
		return
	}
	data, errRead := os.ReadFile(p.cachePath)
	if errRead != nil {
		log.WithError(errRead).Warn("usage pricing: read cache")
		return
	}
	table, errParse := ParseRateTable(data)
	if errParse != nil {
		log.WithError(errParse).Warn("usage pricing: invalid cache")
		return
	}
	p.mu.Lock()
	p.table = table
	p.updatedAt = info.ModTime()
	p.mu.Unlock()
}

func (p *Pricing) run() {
	for {
		p.mu.RLock()
		age := time.Since(p.updatedAt)
		hasTable := p.table != nil
		p.mu.RUnlock()

		wait := pricingMaxAge - age
		if !hasTable || wait <= 0 {
			if errRefresh := p.refresh(); errRefresh != nil {
				log.WithError(errRefresh).Warn("usage pricing: refresh failed")
				wait = pricingRetryInterval
			} else {
				wait = pricingMaxAge
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-p.stop:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (p *Pricing) refresh() error {
	p.mu.RLock()
	proxyURL := p.proxyURL
	p.mu.RUnlock()

	client := &http.Client{Timeout: pricingFetchTimeout}
	if strings.TrimSpace(proxyURL) != "" {
		transport, _, errTransport := proxyutil.BuildHTTPTransport(proxyURL)
		if errTransport != nil {
			return fmt.Errorf("build proxy transport: %w", errTransport)
		}
		if transport != nil {
			client.Transport = transport
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-p.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, pricingURL, nil)
	if errReq != nil {
		return errReq
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		return errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("usage pricing: close body")
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("price table returned status %d", resp.StatusCode)
	}
	data, errRead := io.ReadAll(io.LimitReader(resp.Body, pricingMaxBytes))
	if errRead != nil {
		return errRead
	}
	table, errParse := ParseRateTable(data)
	if errParse != nil {
		return errParse
	}
	tmp := p.cachePath + ".tmp"
	if errWrite := os.WriteFile(tmp, data, 0o600); errWrite != nil {
		log.WithError(errWrite).Warn("usage pricing: write cache")
	} else if errRename := os.Rename(tmp, p.cachePath); errRename != nil {
		log.WithError(errRename).Warn("usage pricing: replace cache")
	}
	p.mu.Lock()
	p.table = table
	p.updatedAt = time.Now()
	p.mu.Unlock()
	return nil
}
