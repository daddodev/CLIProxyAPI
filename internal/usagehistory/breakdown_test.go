package usagehistory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestBreakdownPreservesPairsThroughCompaction(t *testing.T) {
	base := time.Date(2026, 10, 9, 11, 0, 0, 0, time.UTC)
	store := openTestStoreWithClock(t, func() time.Time { return base.Add(30 * 24 * time.Hour) })
	const model = "claude-opus-4-5"
	store.add(base, "claude", model, "a1", TierStandard, "web", Counters{Requests: 1, Output: 40})
	store.add(base, "claude", model, "a1", TierStandard, "jobs", Counters{Requests: 1, Output: 30})
	store.add(base, "claude", model, "a2", TierFast, "web", Counters{Requests: 1, Output: 20})
	store.add(base, "claude", "unpriced", "a1", TierStandard, "web", Counters{Requests: 1, Failed: 1, Output: 5})
	store.add(base, "claude", model, importedAuthIndex, TierStandard, "", Counters{Requests: 1, Output: 1})

	q := HistoryQuery{Granularity: GranularityHour, From: base, To: base.Add(time.Hour), Breakdown: true}
	for i := 0; i < 2; i++ {
		result, errHistory := store.History(t.Context(), q, testRates())
		if errHistory != nil {
			t.Fatal(errHistory)
		}
		if result.Totals.Requests != 5 || result.Totals.FailedRequests != 1 || result.Totals.TotalTokens != 96 || result.Totals.Priced {
			t.Fatalf("incorrect totals: %+v", result.Totals)
		}
		b := result.Breakdown
		if len(b.Projects) != 3 || len(b.Accounts) != 3 || len(b.ProjectAccounts) != 4 {
			t.Fatalf("incorrect group counts: %+v", b)
		}
		web, a1, pair := b.Projects[0], b.Accounts[0], b.ProjectAccounts[0]
		if web.Project != "web" || web.TotalTokens != 65 || web.CostBreakdown == nil || web.Priced || !approx(web.CostUSD, 40*25e-6+20*50e-6) ||
			a1.AuthIndex != "a1" || a1.TotalTokens != 75 || pair.Project != "web" || pair.AuthIndex != "a1" || pair.TotalTokens != 45 {
			t.Fatalf("incorrect breakdown: project %+v, account %+v, pair %+v", web, a1, pair)
		}
		unknown := b.ProjectAccounts[3]
		if unknown.Project != "" || unknown.AuthIndex != "" || unknown.TotalTokens != 1 {
			t.Fatalf("incorrect imported usage: %+v", unknown)
		}
		store.compact()
	}
	q.Breakdown = false
	result, errHistory := store.History(t.Context(), q, testRates())
	if errHistory != nil || result.Breakdown != nil || result.Totals.TotalTokens != 96 {
		t.Fatalf("default history changed: %+v, %v", result, errHistory)
	}
}

func TestProjectAttributionRecordsOnlyConfiguredNames(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Hour)
	store := openTestStoreWithClock(t, func() time.Time { return base })
	const key = "test-client-key"
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
	currentMu.Lock()
	current, projects = store, projectLabels(map[string]string{fingerprint: " web ", strings.Repeat("a", 66): "invalid"})
	currentMu.Unlock()
	t.Cleanup(func() {
		currentMu.Lock()
		current, projects = nil, nil
		currentMu.Unlock()
	})
	for _, apiKey := range []string{key, "unmapped-client-key", ""} {
		plugin{}.HandleUsage(context.Background(), coreusage.Record{APIKey: apiKey, AuthIndex: "a1", Provider: "claude", Model: "test", RequestedAt: base})
	}
	result, errHistory := store.History(t.Context(), HistoryQuery{Granularity: GranularityHour, From: base, To: base.Add(time.Hour), Breakdown: true}, nil)
	if errHistory != nil {
		t.Fatal(errHistory)
	}
	entries := result.Breakdown.Projects
	if len(entries) != 2 || entries[0].Project != "" || entries[0].Requests != 2 || entries[1].Project != "web" || entries[1].Requests != 1 {
		t.Fatalf("incorrect project attribution: %+v", result.Breakdown)
	}
}

func TestBreakdownAndChartAgreeAtCompactedHalfHourBoundary(t *testing.T) {
	loc := time.FixedZone("UTC+05:30", 5*3600+30*60)
	base := time.Date(2026, 10, 9, 0, 0, 0, 0, loc)
	store := openTestStoreWithClock(t, func() time.Time { return base.Add(30 * 24 * time.Hour) })
	for _, at := range []time.Time{base, base.Add(time.Hour)} {
		store.add(at, "claude", "test", "a1", TierStandard, "web", Counters{Requests: 1, Output: 10})
	}
	if errFlush := store.flush(); errFlush != nil {
		t.Fatal(errFlush)
	}
	store.compact()
	result, errHistory := store.History(t.Context(), HistoryQuery{Granularity: GranularityDay, From: base, To: base.AddDate(0, 0, 1), Location: loc, Breakdown: true}, nil)
	if errHistory != nil {
		t.Fatal(errHistory)
	}
	if result.Totals.Requests != 1 || len(result.Breakdown.Projects) != 1 || result.Breakdown.Projects[0].Requests != 1 {
		t.Fatalf("unexpected chart or breakdown total: %+v", result)
	}
}

func TestBreakdownAndChartAgreeWhenDSTSkipsMidnight(t *testing.T) {
	for _, tc := range []struct {
		zone  string
		month time.Month
		day   int
	}{{"Africa/Cairo", time.April, 24}, {"America/Santiago", time.September, 6}} {
		t.Run(tc.zone, func(t *testing.T) {
			loc, errLocation := time.LoadLocation(tc.zone)
			if errLocation != nil {
				t.Fatal(errLocation)
			}
			before := time.Date(2026, tc.month, tc.day-1, 12, 0, 0, 0, loc)
			after := before.AddDate(0, 0, 2)
			store := openTestStoreWithClock(t, func() time.Time { return after })
			for _, at := range []time.Time{before, before.AddDate(0, 0, 1), after} {
				store.add(at, "claude", "test", "a1", TierStandard, "web", Counters{Requests: 1})
			}
			result, errHistory := store.History(t.Context(), HistoryQuery{Granularity: GranularityDay, From: before, To: after, Location: loc, Breakdown: true}, nil)
			if errHistory != nil {
				t.Fatal(errHistory)
			}
			if len(result.Buckets) != 3 || result.Buckets[1].Start.Hour() != 1 || result.Buckets[2].Start.Hour() != 0 || result.Totals.Requests != 3 || result.Breakdown.Projects[0].Requests != 3 {
				t.Fatalf("unexpected bucket starts or totals: %+v", result)
			}
		})
	}
}

func TestLegacyRollupsMigrateWithoutLosingAccountOrImportState(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Hour)
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, errOpen := sql.Open("sqlite", path)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	legacySchema := strings.ReplaceAll(strings.ReplaceAll(schema, "\tproject TEXT NOT NULL DEFAULT '',\n", ""), "tier, project)", "tier)")
	if _, errExec := db.Exec(legacySchema); errExec != nil {
		t.Fatal(errExec)
	}
	for i, table := range []string{"usage_minute", "usage_hour"} {
		if _, errExec := db.Exec(`INSERT INTO `+table+` (bucket, provider, model, auth_index, tier, requests, output) VALUES (?, 'claude', 'test', 'a1', 'standard', 1, 15)`, base.Add(time.Duration(i)*time.Hour).Unix()); errExec != nil {
			t.Fatal(errExec)
		}
	}
	if _, errExec := db.Exec(`INSERT INTO usage_import_events VALUES ('existing-import'); INSERT INTO usage_import_state VALUES (1, 123)`); errExec != nil {
		t.Fatal(errExec)
	}
	if errClose := db.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	for i := 0; i < 2; i++ {
		store, errStore := openWithClock(path, func() time.Time { return base })
		if errStore != nil {
			t.Fatal(errStore)
		}
		t.Cleanup(func() { _ = store.Close() })
		result, errHistory := store.History(t.Context(), HistoryQuery{Granularity: GranularityHour, From: base, To: base.Add(2 * time.Hour), Breakdown: true}, nil)
		if errHistory != nil {
			t.Fatal(errHistory)
		}
		pairs := result.Breakdown.ProjectAccounts
		if len(pairs) != 1 || pairs[0].Project != "" || pairs[0].AuthIndex != "a1" || pairs[0].Requests != 2 || pairs[0].TotalTokens != 30 {
			t.Fatalf("legacy usage missing: %+v", result.Breakdown)
		}
		var events, cutoff int
		if errScan := store.db.QueryRow(`SELECT (SELECT COUNT(*) FROM usage_import_events), before FROM usage_import_state WHERE id = 1`).Scan(&events, &cutoff); errScan != nil || events != 1 || cutoff != 123 {
			t.Fatalf("import state changed: events %d, cutoff %d, error %v", events, cutoff, errScan)
		}
		if errClose := store.Close(); errClose != nil {
			t.Fatal(errClose)
		}
	}
}
