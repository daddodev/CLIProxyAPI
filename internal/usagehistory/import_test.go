package usagehistory

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const importTestTime = "2020-01-02T10:00:00Z"

func writeTranscript(t *testing.T, root, name string, lines ...string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o700); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	if errWrite := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	return path
}

func claudeImportLine(id, at string) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"requestId":"req-%s","message":{"id":%q,"model":"claude-opus-4-5","usage":{"input_tokens":10,"cache_read_input_tokens":20,"cache_creation_input_tokens":30,"output_tokens":40,"speed":"fast"}}}`, at, id, id)
}

func codexImportLine(at string, input, output, cumulativeInput, cumulativeOutput int) string {
	return fmt.Sprintf(`{"type":"event_msg","timestamp":%q,"payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":%d,"cached_input_tokens":20,"cache_write_input_tokens":10,"output_tokens":%d,"reasoning_output_tokens":5},"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"cache_write_input_tokens":%d,"output_tokens":%d,"reasoning_output_tokens":%d}}}}`,
		at, input, output, cumulativeInput, cumulativeInput/input*20, cumulativeInput/input*10, cumulativeOutput, cumulativeInput/input*5)
}

func codexImportHeader(session string) []string {
	return []string{
		fmt.Sprintf(`{"type":"session_meta","timestamp":%q,"payload":{"id":%q,"source":"cli"}}`, importTestTime, session),
		`{"type":"turn_context","payload":{"model":"gpt-test"}}`,
		`{"type":"event_msg","payload":{"type":"thread_settings_applied","thread_settings":{"service_tier":"priority"}}}`,
	}
}

func TestImportPreviewDeduplicationAndAppend(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	claude := claudeImportLine("msg-1", importTestTime)
	writeTranscript(t, root, "claude/one.jsonl", claude, claude, `{"type":"assistant","message":{"usage":`, claudeImportLine("msg-2", importTestTime))
	writeTranscript(t, root, "claude/subagents/copy.jsonl", claude)
	codexLines := append(codexImportHeader("session-1"),
		codexImportLine(importTestTime, 100, 50, 100, 50),
		codexImportLine("2020-01-02T10:00:01Z", 100, 50, 100, 50),
		codexImportLine("2020-01-02T10:00:02Z", 100, 50, 200, 100))
	path := writeTranscript(t, root, "codex/one.jsonl", codexLines...)
	writeTranscript(t, root, "archived_sessions/copy.jsonl", codexLines...)
	sources := []ImportSource{{"claude", filepath.Join(root, "claude")}, {"codex", filepath.Join(root, "codex")}, {"codex", filepath.Join(root, "archived_sessions")}}
	database := filepath.Join(root, "database", "history.db")
	preview, errPreview := Import(ctx, database, ImportOptions{Sources: sources, DryRun: true})
	if errPreview != nil {
		t.Fatal(errPreview)
	}
	if preview.Imported != 4 || preview.Duplicates != 4 || preview.Malformed != 1 || preview.Files != 4 {
		t.Fatalf("unexpected preview: %+v", preview)
	}
	if _, errStat := os.Stat(filepath.Dir(database)); !os.IsNotExist(errStat) {
		t.Fatalf("dry run created the database directory: %v", errStat)
	}
	first, errImport := Import(ctx, database, ImportOptions{Sources: sources})
	if errImport != nil {
		t.Fatal(errImport)
	}
	if first.Imported != preview.Imported || first.Counters != preview.Counters {
		t.Fatalf("preview and import differ: %+v / %+v", preview, first)
	}
	if first.Counters != (Counters{Requests: 4, UncachedInput: 160, CacheRead: 80, CacheWrite: 80, Output: 180, Reasoning: 10}) {
		t.Fatalf("incorrect token accounting: %+v", first.Counters)
	}
	for _, dryRun := range []bool{true, false} {
		repeat, errRepeat := Import(ctx, database, ImportOptions{Sources: sources, DryRun: dryRun})
		if errRepeat != nil || repeat.Imported != 0 || repeat.Duplicates != 8 {
			t.Fatalf("repeat dryRun=%v: %+v / %v", dryRun, repeat, errRepeat)
		}
		if !repeat.Before.Equal(first.Before) {
			t.Fatal("the cutoff changed after importing older records")
		}
	}
	codexLines = append(codexLines, codexImportLine("2020-01-02T10:00:03Z", 100, 50, 300, 150))
	writeTranscript(t, root, "codex/one.jsonl", codexLines...)
	appended, errAppend := Import(ctx, database, ImportOptions{Sources: []ImportSource{{"codex", path}}})
	if errAppend != nil || appended.Imported != 1 {
		t.Fatalf("append: %+v / %v", appended, errAppend)
	}
	store, errOpen := Open(database)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	defer func() {
		if errClose := store.Close(); errClose != nil {
			t.Error(errClose)
		}
	}()
	from, _ := time.Parse(time.RFC3339, importTestTime)
	history, errHistory := store.History(ctx, HistoryQuery{Granularity: GranularityDay, From: from, To: from.Add(24 * time.Hour)}, nil)
	if errHistory != nil || history.Totals.Requests != 5 || history.Totals.TotalTokens != 650 {
		t.Fatalf("dashboard history: %+v / %v", history, errHistory)
	}
	var authIndex, tier string
	if errQuery := store.db.QueryRow(`SELECT auth_index, tier FROM usage_hour LIMIT 1`).Scan(&authIndex, &tier); errQuery != nil {
		t.Fatal(errQuery)
	}
	if authIndex != importedAuthIndex || tier != TierFast {
		t.Fatalf("import falsely attributed an account or lost fast pricing: %q %q", authIndex, tier)
	}
}

func TestImportBoundaryAndLegacyPreview(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	boundary := time.Date(2020, 1, 2, 12, 0, 0, 0, time.UTC)
	store.add(boundary, "claude", "m", "live-account", TierStandard, Counters{Requests: 1, Output: 7})
	if errFlush := store.flush(); errFlush != nil {
		t.Fatal(errFlush)
	}
	// Reproduce a database created before import metadata existed.
	for _, table := range []string{"usage_import_state", "usage_import_events"} {
		if _, errDrop := store.db.Exec(`DROP TABLE ` + table); errDrop != nil {
			t.Fatal(errDrop)
		}
	}
	if errClose := store.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	root := t.TempDir()
	path := writeTranscript(t, root, "events.jsonl", claudeImportLine("before", "2020-01-02T11:59:59Z"),
		claudeImportLine("at", "2020-01-02T12:00:00Z"), claudeImportLine("after", "2020-01-02T12:00:01Z"))
	opts := ImportOptions{Sources: []ImportSource{{"claude", path}}, DryRun: true, Before: boundary.Add(24 * time.Hour)}
	unchanged, errRead := os.ReadFile(store.Path())
	if errRead != nil {
		t.Fatal(errRead)
	}
	preview, errPreview := Import(ctx, store.Path(), opts)
	if errPreview != nil || preview.Imported != 1 || preview.OutsideRange != 2 || !preview.Before.Equal(boundary) {
		t.Fatalf("legacy preview: %+v / %v", preview, errPreview)
	}
	afterPreview, errRead := os.ReadFile(store.Path())
	if errRead != nil || !bytes.Equal(unchanged, afterPreview) {
		t.Fatalf("dry run changed the legacy database: %v", errRead)
	}
	readOnly, errOpen := Open(store.Path())
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	var count int
	if errQuery := readOnly.db.QueryRow(`SELECT COUNT(*) FROM usage_import_events`).Scan(&count); errQuery != nil || count != 0 {
		t.Fatalf("preview wrote events: count=%d err=%v", count, errQuery)
	}
	if errClose := readOnly.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	opts.DryRun = false
	imported, errImport := Import(ctx, store.Path(), opts)
	if errImport != nil || imported.Imported != 1 || imported.OutsideRange != 2 {
		t.Fatalf("boundary import: %+v / %v", imported, errImport)
	}
	opts.Before, opts.DryRun = boundary.Add(-time.Second), true
	earlier, errEarlier := Import(ctx, store.Path(), opts)
	if errEarlier != nil || earlier.Imported != 0 || earlier.OutsideRange != 3 {
		t.Fatalf("exclusive explicit cutoff: %+v / %v", earlier, errEarlier)
	}
}

func TestImportKeepsStartupBoundaryWithoutLiveRecords(t *testing.T) {
	boundary := time.Date(2020, 1, 2, 12, 0, 0, 0, time.UTC)
	store := openTestStoreWithClock(t, func() time.Time { return boundary.Add(30 * time.Second) })
	if errClose := store.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	path := writeTranscript(t, t.TempDir(), "events.jsonl", claudeImportLine("before", "2020-01-02T11:59:59Z"),
		claudeImportLine("after", "2020-01-02T12:00:01Z"))
	for range 2 {
		result, errImport := Import(context.Background(), store.Path(), ImportOptions{Sources: []ImportSource{{"claude", path}}, DryRun: true})
		if errImport != nil || result.Imported != 1 || result.OutsideRange != 1 || !result.Before.Equal(boundary) {
			t.Fatalf("startup boundary changed: %+v / %v", result, errImport)
		}
	}
}

func TestImportRollsBackDeduplicationWithFailedBatch(t *testing.T) {
	store := openTestStore(t)
	for _, table := range []string{"usage_minute", "usage_hour"} {
		if _, errTrigger := store.db.Exec(`CREATE TRIGGER reject_` + table + ` BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT, 'test write failure'); END`); errTrigger != nil {
			t.Fatal(errTrigger)
		}
	}
	path := writeTranscript(t, t.TempDir(), "events.jsonl", claudeImportLine("one", importTestTime))
	opts := ImportOptions{Sources: []ImportSource{{"claude", path}}}
	result, errImport := Import(context.Background(), store.Path(), opts)
	if errImport == nil || result.Imported != 0 {
		t.Fatalf("failed batch reported success: %+v / %v", result, errImport)
	}
	var count int
	if errQuery := store.db.QueryRow(`SELECT COUNT(*) FROM usage_import_events`).Scan(&count); errQuery != nil || count != 0 {
		t.Fatalf("failed batch retained dedupe keys: %d / %v", count, errQuery)
	}
	for _, table := range []string{"usage_minute", "usage_hour"} {
		if _, errDrop := store.db.Exec(`DROP TRIGGER reject_` + table); errDrop != nil {
			t.Fatal(errDrop)
		}
	}
	result, errImport = Import(context.Background(), store.Path(), opts)
	if errImport != nil || result.Imported != 1 {
		t.Fatalf("retry after rollback: %+v / %v", result, errImport)
	}
}

func TestImportReportsCommittedBatchesAndRetries(t *testing.T) {
	store := openTestStore(t)
	if _, errTrigger := store.db.Exec(`CREATE TRIGGER reject_model BEFORE INSERT ON usage_hour
		WHEN NEW.model = 'reject' BEGIN SELECT RAISE(ABORT, 'test batch failure'); END`); errTrigger != nil {
		t.Fatal(errTrigger)
	}
	lines := make([]string, 0, importBatchSize+1)
	for i := range importBatchSize {
		lines = append(lines, claudeImportLine(fmt.Sprintf("msg-%d", i), importTestTime))
	}
	lines = append(lines, strings.Replace(claudeImportLine("last", importTestTime), "claude-opus-4-5", "reject", 1))
	path := writeTranscript(t, t.TempDir(), "events.jsonl", lines...)
	opts := ImportOptions{Sources: []ImportSource{{"claude", path}}}
	result, errImport := Import(context.Background(), store.Path(), opts)
	if errImport == nil || result.Imported != importBatchSize {
		t.Fatalf("partial progress not reported: %+v / %v", result, errImport)
	}
	if _, errDrop := store.db.Exec(`DROP TRIGGER reject_model`); errDrop != nil {
		t.Fatal(errDrop)
	}
	result, errImport = Import(context.Background(), store.Path(), opts)
	if errImport != nil || result.Imported != 1 || result.Duplicates != importBatchSize {
		t.Fatalf("retry did not preserve committed batches: %+v / %v", result, errImport)
	}
}

func TestImportLargeLinesAndIncompleteTail(t *testing.T) {
	root := t.TempDir()
	oversized := strings.Repeat("x", maxTranscriptLine+1)
	path := writeTranscript(t, root, "events.jsonl", oversized, claudeImportLine("complete", importTestTime))
	file, errOpen := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	_, errWrite := file.WriteString(`{"type":"assistant","message":{"usage":`)
	errClose := file.Close()
	if errWrite != nil || errClose != nil {
		t.Fatalf("write tail: %v / %v", errWrite, errClose)
	}
	database := filepath.Join(root, "history.db")
	result, errImport := Import(context.Background(), database, ImportOptions{Sources: []ImportSource{{"claude", path}}, DryRun: true})
	if errImport != nil || result.Imported != 1 || result.Malformed != 2 {
		t.Fatalf("large line prevented recovery: %+v / %v", result, errImport)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, errImport := Import(ctx, database, ImportOptions{Sources: []ImportSource{{"claude", path}}}); errImport != context.Canceled {
		t.Fatalf("cancellation ignored: %v", errImport)
	}
	if _, errStat := os.Stat(database); !os.IsNotExist(errStat) {
		t.Fatal("canceled import created a database")
	}
}

func TestCodexImportForksAndLegacyCounters(t *testing.T) {
	t.Run("forked history", func(t *testing.T) {
		lines := []string{
			`{"type":"session_meta","timestamp":"2020-01-02T09:59:00Z","payload":{"id":"child","source":{"subagent":{"thread_spawn":{"parent_thread_id":"parent"}}}}}`,
			`{"type":"session_meta","timestamp":"2020-01-02T09:59:00Z","payload":{"id":"parent","source":"cli"}}`,
			`{"type":"turn_context","payload":{"model":"gpt-test"}}`,
			codexImportLine("2020-01-02T09:59:00.010Z", 100, 50, 100, 50),
			codexImportLine("2020-01-02T09:59:00.020Z", 100, 50, 200, 100),
			codexImportLine(importTestTime, 100, 50, 300, 150),
		}
		parser := transcriptParser{provider: "codex"}
		var records []*importRecord
		for _, line := range lines {
			record, malformed := parser.parse([]byte(line))
			if malformed {
				t.Fatalf("malformed fork event: %s", line)
			}
			if record != nil {
				records = append(records, record)
			}
		}
		if len(records) != 1 || parser.session != "child" || records[0].counters.Output != 50 {
			t.Fatalf("copied parent history was counted: %+v", records)
		}
	})
	t.Run("legacy re-emission", func(t *testing.T) {
		parser := transcriptParser{provider: "codex"}
		for _, line := range codexImportHeader("legacy") {
			parser.parse([]byte(line))
		}
		legacy := `{"type":"event_msg","timestamp":"2020-01-02T10:00:00Z","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"cached_input_tokens":20,"cache_write_input_tokens":10,"output_tokens":50,"reasoning_output_tokens":5}}}}`
		record, malformed := parser.parse([]byte(legacy))
		if malformed || record == nil {
			t.Fatal("valid legacy event was rejected")
		}
		for _, line := range []string{legacy, codexImportLine(importTestTime, 100, 50, 100, 50)} {
			if record, malformed := parser.parse([]byte(line)); malformed || record != nil {
				t.Fatal("re-emitted legacy usage was counted")
			}
		}
		if record, malformed := parser.parse([]byte(codexImportLine(importTestTime, 100, 50, 200, 100))); malformed || record == nil {
			t.Fatal("distinct identical request with advanced cumulative totals was lost")
		}
	})
}

func TestImportRejectsMalformedTokensAndRecovers(t *testing.T) {
	parser := transcriptParser{provider: "claude"}
	valid := claudeImportLine("valid", importTestTime)
	for _, line := range []string{
		strings.Replace(valid, `"input_tokens":10`, `"input_tokens":-1`, 1),
		strings.Replace(valid, `"input_tokens":10`, `"input_tokens":9223372036854775807`, 1),
		strings.Replace(valid, `"input_tokens":10`, `"input_tokens":1.5`, 1),
		strings.Replace(valid, `"input_tokens":10`, `"input_tokens":"10"`, 1),
	} {
		if record, malformed := parser.parse([]byte(line)); !malformed || record != nil {
			t.Fatalf("invalid usage accepted: %s", line)
		}
	}
	if record, malformed := parser.parse([]byte(valid)); malformed || record == nil {
		t.Fatal("valid record after malformed records was lost")
	}
	codex := transcriptParser{provider: "codex"}
	for _, line := range codexImportHeader("malformed") {
		codex.parse([]byte(line))
	}
	if record, malformed := codex.parse([]byte(codexImportLine(importTestTime, 100, 50, 200, 100))); !malformed || record != nil {
		t.Fatal("inconsistent cumulative counters accepted")
	}
	if record, malformed := codex.parse([]byte(codexImportLine(importTestTime, 100, 50, 300, 150))); malformed || record == nil {
		t.Fatal("valid counters after inconsistent snapshot were lost")
	}
}

func TestCodexImportWaitsForContextBeforeConsumingCounters(t *testing.T) {
	parser := transcriptParser{provider: "codex"}
	parser.parse([]byte(codexImportHeader("late-context")[0]))
	line := codexImportLine(importTestTime, 100, 50, 100, 50)
	if record, malformed := parser.parse([]byte(line)); !malformed || record != nil {
		t.Fatal("usage without a model was accepted")
	}
	parser.parse([]byte(`{"type":"turn_context","payload":{"model":"gpt-test"}}`))
	if record, malformed := parser.parse([]byte(line)); malformed || record == nil {
		t.Fatal("re-emitted usage with recovered context was lost")
	}
}
