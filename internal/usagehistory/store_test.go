package usagehistory

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	return openTestStoreWithClock(t, time.Now)
}

func openTestStoreWithClock(t *testing.T, now func() time.Time) *Store {
	t.Helper()
	store, errOpen := openWithClock(filepath.Join(t.TempDir(), "usage.db"), now)
	if errOpen != nil {
		t.Fatalf("open store: %v", errOpen)
	}
	t.Cleanup(func() {
		if errClose := store.Close(); errClose != nil {
			t.Errorf("close store: %v", errClose)
		}
	})
	return store
}

func testRates() RateTable {
	return RateTable{
		"claude-opus-4-5": {
			Standard: TokenRates{Input: 5e-6, Output: 25e-6, CacheRead: 0.5e-6, CacheWrite: 6.25e-6},
			Fast:     TokenRates{Input: 10e-6, Output: 50e-6, CacheRead: 1e-6, CacheWrite: 12.5e-6},
		},
	}
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestHistoryAggregatesMinuteRowsIntoHourBuckets(t *testing.T) {
	store := openTestStore(t)
	base := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	store.add(base.Add(5*time.Minute), "claude", "claude-opus-4-5", "a1", TierStandard,
		Counters{Requests: 1, UncachedInput: 1000, CacheRead: 10000, CacheWrite: 2000, Output: 500, Reasoning: 100})
	store.add(base.Add(50*time.Minute), "claude", "claude-opus-4-5", "a2", TierFast,
		Counters{Requests: 1, UncachedInput: 1000, Output: 500})
	store.add(base.Add(70*time.Minute), "codex", "mystery-model", "c1", TierStandard,
		Counters{Requests: 2, Failed: 1, UncachedInput: 300, Output: 200})

	result, errHistory := store.History(context.Background(), HistoryQuery{
		Granularity: GranularityHour,
		From:        base,
		To:          base.Add(2 * time.Hour),
	}, testRates())
	if errHistory != nil {
		t.Fatalf("history: %v", errHistory)
	}
	if len(result.Buckets) != 2 {
		t.Fatalf("buckets = %d, want 2", len(result.Buckets))
	}
	first := result.Buckets[0]
	if len(first.Series) != 1 {
		t.Fatalf("first bucket series = %d, want 1", len(first.Series))
	}
	opus := first.Series[0]
	if opus.Requests != 2 || opus.InputTokens != 2000 || opus.CacheReadTokens != 10000 || opus.OutputTokens != 1000 {
		t.Fatalf("unexpected opus entry: %+v", opus)
	}
	standard := 1000*5e-6 + 10000*0.5e-6 + 2000*6.25e-6 + 500*25e-6
	fast := 1000*10e-6 + 500*50e-6
	if !opus.Priced || !approx(opus.CostUSD, standard+fast) {
		t.Fatalf("opus cost = %v priced=%v, want %v", opus.CostUSD, opus.Priced, standard+fast)
	}

	mystery := result.Buckets[1].Series[0]
	if mystery.Priced || mystery.CostBreakdown != nil || mystery.FailedRequests != 1 || mystery.TotalTokens != 500 {
		t.Fatalf("unexpected unpriced entry: %+v", mystery)
	}
	if result.Totals.Priced || !approx(result.Totals.CostUSD, standard+fast) || result.Totals.Requests != 4 {
		t.Fatalf("unexpected totals: %+v", result.Totals)
	}
	if len(result.Models) != 2 || result.Models[0].Model != "claude-opus-4-5" {
		t.Fatalf("models not sorted by cost: %+v", result.Models)
	}
}

func TestHistoryIncludesEmptyBuckets(t *testing.T) {
	store := openTestStore(t)
	base := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	store.add(base, "claude", "claude-opus-4-5", "a1", TierStandard, Counters{Requests: 1, Output: 10})

	result, errHistory := store.History(context.Background(), HistoryQuery{
		Granularity: GranularityMinute,
		From:        base,
		To:          base.Add(5 * time.Minute),
	}, nil)
	if errHistory != nil {
		t.Fatalf("history: %v", errHistory)
	}
	if len(result.Buckets) != 5 {
		t.Fatalf("buckets = %d, want 5", len(result.Buckets))
	}
	for i, bucket := range result.Buckets[1:] {
		if len(bucket.Series) != 0 {
			t.Fatalf("bucket %d should be empty", i+1)
		}
	}
}

func TestHistoryDayBucketsUseLocation(t *testing.T) {
	store := openTestStore(t)
	rome, errLoad := time.LoadLocation("Europe/Rome")
	if errLoad != nil {
		t.Fatalf("load location: %v", errLoad)
	}
	// 23:30 UTC on Oct 24 is 01:30 on Oct 25 in Rome (UTC+2 before the DST switch).
	store.add(time.Date(2026, 10, 24, 23, 30, 0, 0, time.UTC), "claude", "m", "a", TierStandard, Counters{Requests: 1})

	result, errHistory := store.History(context.Background(), HistoryQuery{
		Granularity: GranularityDay,
		From:        time.Date(2026, 10, 24, 0, 0, 0, 0, rome),
		To:          time.Date(2026, 10, 26, 0, 0, 0, 0, rome),
		Location:    rome,
	}, nil)
	if errHistory != nil {
		t.Fatalf("history: %v", errHistory)
	}
	if len(result.Buckets) != 2 {
		t.Fatalf("buckets = %d, want 2", len(result.Buckets))
	}
	if len(result.Buckets[0].Series) != 0 || len(result.Buckets[1].Series) != 1 {
		t.Fatalf("record landed in the wrong local day: %+v", result.Buckets)
	}
}

func TestCompactionKeepsTotals(t *testing.T) {
	old := time.Date(2026, 10, 1, 10, 15, 0, 0, time.UTC)
	store := openTestStoreWithClock(t, func() time.Time { return old.Add(30 * 24 * time.Hour) })
	store.add(old, "claude", "claude-opus-4-5", "a1", TierStandard, Counters{Requests: 1, Output: 100})
	store.add(old.Add(20*time.Minute), "claude", "claude-opus-4-5", "a1", TierStandard, Counters{Requests: 1, Output: 50})
	store.flush()

	store.compact()

	var minuteRows, hourRows int
	if errScan := store.db.QueryRow(`SELECT COUNT(*) FROM usage_minute`).Scan(&minuteRows); errScan != nil {
		t.Fatalf("count minute rows: %v", errScan)
	}
	if errScan := store.db.QueryRow(`SELECT COUNT(*) FROM usage_hour`).Scan(&hourRows); errScan != nil {
		t.Fatalf("count hour rows: %v", errScan)
	}
	if minuteRows != 0 || hourRows != 1 {
		t.Fatalf("minute rows = %d, hour rows = %d; want 0 and 1", minuteRows, hourRows)
	}

	result, errHistory := store.History(context.Background(), HistoryQuery{
		Granularity: GranularityDay,
		From:        time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		To:          time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC),
	}, nil)
	if errHistory != nil {
		t.Fatalf("history: %v", errHistory)
	}
	if got := result.Totals.OutputTokens; got != 150 {
		t.Fatalf("output tokens after compaction = %d, want 150", got)
	}
}

func TestHistoryRejectsInvalidQueries(t *testing.T) {
	store := openTestStore(t)
	now := time.Now()
	cases := []HistoryQuery{
		{Granularity: "week", From: now.Add(-time.Hour), To: now},
		{Granularity: GranularityHour, From: now, To: now.Add(-time.Hour)},
		{Granularity: GranularityMinute, From: now.Add(-8 * 24 * time.Hour), To: now},
		{Granularity: GranularityMinute, From: now.Add(-6 * 24 * time.Hour), To: now},
	}
	for i, q := range cases {
		if _, errHistory := store.History(context.Background(), q, nil); errHistory == nil {
			t.Fatalf("case %d: expected an error", i)
		}
	}
}

func TestPluginRecordsCanonicalBreakdown(t *testing.T) {
	store := openTestStore(t)
	currentMu.Lock()
	current = store
	currentMu.Unlock()
	t.Cleanup(func() {
		currentMu.Lock()
		current = nil
		currentMu.Unlock()
	})

	at := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	plugin{}.HandleUsage(context.Background(), coreusage.Record{
		Provider:            "claude",
		Model:               "claude-opus-4-5",
		AuthIndex:           "a1",
		RequestedAt:         at,
		ResponseServiceTier: "priority",
		Detail: coreusage.Detail{
			TokenBreakdown: coreusage.NewIndependentTokenBreakdown(100, 1000, 50, 200, 30, 1380),
		},
	})
	store.flush()

	var tier string
	var input, cacheRead, cacheWrite, output, reasoning int64
	errScan := store.db.QueryRow(`SELECT tier, uncached_input, cache_read, cache_write, output, reasoning FROM usage_minute`).
		Scan(&tier, &input, &cacheRead, &cacheWrite, &output, &reasoning)
	if errScan != nil {
		t.Fatalf("scan: %v", errScan)
	}
	if tier != TierFast || input != 100 || cacheRead != 1000 || cacheWrite != 50 || output != 230 || reasoning != 30 {
		t.Fatalf("unexpected row: tier=%s input=%d read=%d write=%d output=%d reasoning=%d",
			tier, input, cacheRead, cacheWrite, output, reasoning)
	}
}

func TestParseRateTable(t *testing.T) {
	data := []byte(`{
		"sample_spec": {"input_cost_per_token": "x"},
		"claude-opus-4-5": {"input_cost_per_token": 5e-6, "output_cost_per_token": 25e-6,
			"cache_read_input_token_cost": 5e-7, "cache_creation_input_token_cost": 6.25e-6},
		"claude-opus-5": {"input_cost_per_token": 5e-6, "output_cost_per_token": 25e-6,
			"provider_specific_entry": {"us": 1.1, "fast": 2}},
		"gpt-5.1": {"input_cost_per_token": 1.25e-6, "output_cost_per_token": 1e-5,
			"cache_read_input_token_cost": 1.25e-7,
			"input_cost_per_token_priority": 2.5e-6, "output_cost_per_token_priority": 2e-5},
		"openrouter/only-prefixed": {"input_cost_per_token": 1e-6, "output_cost_per_token": 2e-6},
		"no-output": {"input_cost_per_token": 1e-6}
	}`)
	table, errParse := ParseRateTable(data)
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if _, ok := table["no-output"]; ok {
		t.Fatal("entries without an output price must be skipped")
	}

	opus, ok := table.Lookup("claude-opus-4-5-20251101")
	if !ok || opus.Standard.CacheRead != 5e-7 || opus.Fast != opus.Standard {
		t.Fatalf("dated lookup failed: %+v ok=%v", opus, ok)
	}
	opus5, ok := table.Lookup("Claude-Opus-5[1m]")
	if !ok || !approx(opus5.Fast.Output, 50e-6) || opus5.Standard.CacheRead != 5e-6 {
		t.Fatalf("fast multiplier or cache fallback wrong: %+v", opus5)
	}
	gpt, ok := table.Lookup("gpt-5.1(high)")
	if !ok || gpt.Fast.Input != 2.5e-6 || !approx(gpt.Fast.CacheRead, 2.5e-7) {
		t.Fatalf("priority rates wrong: %+v", gpt)
	}
	if _, ok := table.Lookup("only-prefixed"); !ok {
		t.Fatal("provider-prefixed key should price its bare name")
	}
	if _, ok := table.Lookup("unknown-model"); ok {
		t.Fatal("unknown model should not be priced")
	}
}

func TestHistoryDayBucketsInHalfHourZone(t *testing.T) {
	kolkata, errLoad := time.LoadLocation("Asia/Kolkata")
	if errLoad != nil {
		t.Fatalf("load location: %v", errLoad)
	}
	// 00:10 local on Oct 25 is 18:40 UTC on Oct 24, inside the UTC hour that
	// starts before local midnight.
	at := time.Date(2026, 10, 25, 0, 10, 0, 0, kolkata)
	store := openTestStoreWithClock(t, func() time.Time { return at.Add(time.Hour) })
	store.add(at, "claude", "m", "a", TierStandard, Counters{Requests: 1, Output: 7})

	result, errHistory := store.History(context.Background(), HistoryQuery{
		Granularity: GranularityDay,
		From:        time.Date(2026, 10, 25, 0, 0, 0, 0, kolkata),
		To:          time.Date(2026, 10, 26, 0, 0, 0, 0, kolkata),
		Location:    kolkata,
	}, nil)
	if errHistory != nil {
		t.Fatalf("history: %v", errHistory)
	}
	if len(result.Buckets) != 1 || result.Totals.OutputTokens != 7 {
		t.Fatalf("usage at local 00:10 must land on Oct 25: %+v totals=%+v", result.Buckets, result.Totals)
	}
}

func TestCloseReportsUnwrittenFinalFlush(t *testing.T) {
	store, errOpen := Open(filepath.Join(t.TempDir(), "usage.db"))
	if errOpen != nil {
		t.Fatalf("open store: %v", errOpen)
	}
	if _, errDrop := store.db.Exec(`DROP TABLE usage_minute`); errDrop != nil {
		t.Fatalf("drop table: %v", errDrop)
	}
	store.add(time.Now(), "claude", "m", "a", TierStandard, Counters{Requests: 1})
	if errClose := store.Close(); errClose == nil {
		t.Fatal("Close must report buffered usage that could not be written")
	}
}

func TestHandleUsageAfterShutdownIsDropped(t *testing.T) {
	store := openTestStore(t)
	currentMu.Lock()
	current = store
	currentMu.Unlock()
	Shutdown()
	// Must not panic or write into the closed store.
	plugin{}.HandleUsage(context.Background(), coreusage.Record{Provider: "claude", Model: "m"})
}
