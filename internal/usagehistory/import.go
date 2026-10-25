package usagehistory

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const importedAuthIndex = "imported:local"
const importBatchSize = 500
const maxTranscriptLine = 32 << 20

// ImportSource identifies a transcript file or a directory to scan recursively.
type ImportSource struct {
	Provider string
	Path     string
}

// ImportOptions limits imports to history preceding the first live usage bucket.
// Before can further restrict that exclusive boundary, but cannot extend it.
type ImportOptions struct {
	Sources []ImportSource
	Before  time.Time
	DryRun  bool
}

// ImportResult describes committed records, or the records a dry run would import.
type ImportResult struct {
	Before       time.Time `json:"before"`
	DryRun       bool      `json:"dry_run"`
	Files        int       `json:"files"`
	Imported     int64     `json:"imported"`
	Duplicates   int64     `json:"duplicates"`
	OutsideRange int64     `json:"outside_range"`
	Malformed    int64     `json:"malformed"`
	Counters     Counters  `json:"counters"`
}

type importRecord struct {
	id       string
	at       time.Time
	provider string
	model    string
	tier     string
	counters Counters
}

func importID(parts ...string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(parts, "\x00"))))
}

func initializeImportCutoff(db *sql.DB, now time.Time) error {
	_, errInsert := db.Exec(`INSERT OR IGNORE INTO usage_import_state (id, before)
		SELECT 1, MIN(bucket) FROM (
			SELECT ? AS bucket
			UNION ALL SELECT MIN(bucket) FROM usage_minute WHERE auth_index != ?
			UNION ALL SELECT MIN(bucket) FROM usage_hour WHERE auth_index != ?
		)`, now.UTC().Truncate(time.Minute).Unix(), importedAuthIndex, importedAuthIndex)
	return errInsert
}

func readImportCutoff(ctx context.Context, db *sql.DB, now time.Time) (time.Time, bool, error) {
	before := now.UTC().Truncate(time.Minute)
	if db == nil {
		return before, false, nil
	}
	var hasState, hasEvents bool
	if errQuery := db.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM sqlite_master WHERE name = 'usage_import_state'),
		EXISTS(SELECT 1 FROM sqlite_master WHERE name = 'usage_import_events')`).Scan(&hasState, &hasEvents); errQuery != nil {
		return time.Time{}, false, errQuery
	}
	if hasState {
		var cutoff int64
		errQuery := db.QueryRowContext(ctx, `SELECT before FROM usage_import_state WHERE id = 1`).Scan(&cutoff)
		if errQuery == nil {
			before = time.Unix(cutoff, 0).UTC()
		} else if !errors.Is(errQuery, sql.ErrNoRows) {
			return time.Time{}, false, errQuery
		}
	}
	// This also protects older databases and live records that arrived with an earlier request time.
	var earliest sql.NullInt64
	if errQuery := db.QueryRowContext(ctx, `SELECT MIN(bucket) FROM (
		SELECT MIN(bucket) AS bucket FROM usage_minute WHERE auth_index != ?
		UNION ALL SELECT MIN(bucket) FROM usage_hour WHERE auth_index != ?
	)`, importedAuthIndex, importedAuthIndex).Scan(&earliest); errQuery != nil {
		return time.Time{}, false, errQuery
	}
	if earliest.Valid && earliest.Int64 < before.Unix() {
		before = time.Unix(earliest.Int64, 0).UTC()
	}
	return before, hasEvents, nil
}

// Import reconstructs token history from local transcripts without assigning an upstream account.
// Dry runs open an existing database read-only and never create one. Each write batch commits
// its deduplication keys and rollups together, so interrupted imports can be safely repeated.
func Import(ctx context.Context, databasePath string, opts ImportOptions) (result ImportResult, err error) {
	if errContext := ctx.Err(); errContext != nil {
		return result, errContext
	}
	if len(opts.Sources) == 0 {
		return result, errors.New("at least one transcript source is required")
	}
	sources := make([]ImportSource, 0, len(opts.Sources))
	for _, source := range opts.Sources {
		if source.Provider != "codex" && source.Provider != "claude" {
			return result, fmt.Errorf("unsupported usage provider %q", source.Provider)
		}
		if strings.TrimSpace(source.Path) == "" {
			return result, errors.New("transcript source path is empty")
		}
		resolved, errResolve := filepath.EvalSymlinks(source.Path)
		if errResolve != nil {
			return result, fmt.Errorf("resolve %s transcripts: %w", source.Provider, errResolve)
		}
		sources = append(sources, ImportSource{Provider: source.Provider, Path: resolved})
	}

	var db *sql.DB
	if opts.DryRun {
		if _, errStat := os.Stat(databasePath); errStat == nil {
			absolute, errAbs := filepath.Abs(databasePath)
			if errAbs != nil {
				return result, errAbs
			}
			uriPath := filepath.ToSlash(absolute)
			if !strings.HasPrefix(uriPath, "/") {
				uriPath = "/" + uriPath
			}
			u := url.URL{Scheme: "file", Path: uriPath, RawQuery: "mode=ro"}
			var errOpen error
			db, errOpen = sql.Open("sqlite", u.String())
			if errOpen != nil {
				return result, fmt.Errorf("open history for preview: %w", errOpen)
			}
			defer func() { err = errors.Join(err, db.Close()) }()
		} else if !errors.Is(errStat, os.ErrNotExist) {
			return result, errStat
		}
	} else {
		store, errOpen := Open(databasePath)
		if errOpen != nil {
			return result, errOpen
		}
		defer func() { err = errors.Join(err, store.Close()) }()
		db = store.db
	}

	before, hasEvents, errCutoff := readImportCutoff(ctx, db, time.Now())
	if errCutoff != nil {
		return result, fmt.Errorf("read import boundary: %w", errCutoff)
	}
	if !opts.Before.IsZero() && opts.Before.Before(before) {
		before = opts.Before.UTC()
	}
	result.Before, result.DryRun = before, opts.DryRun
	seen := make(map[string]struct{})
	files := make(map[string]struct{})
	batch := make([]importRecord, 0, importBatchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if errWrite := importBatch(ctx, db, batch, opts.DryRun, hasEvents, &result); errWrite != nil {
			return errWrite
		}
		batch = batch[:0]
		return nil
	}
	for _, source := range sources {
		errWalk := filepath.WalkDir(source.Path, func(path string, entry fs.DirEntry, errEntry error) error {
			if errEntry != nil {
				return errEntry
			}
			if errContext := ctx.Err(); errContext != nil {
				return errContext
			}
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(entry.Name(), ".jsonl") {
				return nil
			}
			absolute, errAbs := filepath.Abs(path)
			if errAbs != nil {
				return errAbs
			}
			fileKey := source.Provider + "\x00" + absolute
			if _, exists := files[fileKey]; exists {
				return nil
			}
			files[fileKey] = struct{}{}
			result.Files++
			parser := transcriptParser{provider: source.Provider}
			return readImportLines(ctx, path, func(line []byte, oversized bool) error {
				if oversized {
					result.Malformed++
					return nil
				}
				record, malformed := parser.parse(line)
				if malformed {
					result.Malformed++
				}
				if record == nil {
					return nil
				}
				if !record.at.Before(before) {
					result.OutsideRange++
					return nil
				}
				if _, exists := seen[record.id]; exists {
					result.Duplicates++
					return nil
				}
				seen[record.id] = struct{}{}
				batch = append(batch, *record)
				if len(batch) == importBatchSize {
					return flush()
				}
				return nil
			})
		})
		if errWalk != nil {
			return result, fmt.Errorf("scan %s transcripts: %w", source.Provider, errWalk)
		}
	}
	if errFlush := flush(); errFlush != nil {
		return result, errFlush
	}
	return result, nil
}

func importBatch(ctx context.Context, db *sql.DB, records []importRecord, dryRun, hasEvents bool, result *ImportResult) error {
	var tx *sql.Tx
	if !dryRun {
		var errBegin error
		tx, errBegin = db.BeginTx(ctx, nil)
		if errBegin != nil {
			return fmt.Errorf("begin usage import: %w", errBegin)
		}
		defer func() { _ = tx.Rollback() }()
	}
	var imported, duplicates int64
	var counters Counters
	for _, record := range records {
		if errContext := ctx.Err(); errContext != nil {
			return errContext
		}
		if dryRun {
			if hasEvents {
				var exists bool
				if errQuery := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM usage_import_events WHERE id = ?)`, record.id).Scan(&exists); errQuery != nil {
					return errQuery
				}
				if exists {
					duplicates++
					continue
				}
			}
		} else {
			inserted, errInsert := tx.ExecContext(ctx, `INSERT OR IGNORE INTO usage_import_events (id) VALUES (?)`, record.id)
			if errInsert != nil {
				return errInsert
			}
			count, errCount := inserted.RowsAffected()
			if errCount != nil {
				return errCount
			}
			if count == 0 {
				duplicates++
				continue
			}
			table, resolution := "usage_minute", time.Minute
			if record.at.Before(time.Now().UTC().Add(-minuteRetention).Truncate(time.Hour)) {
				table, resolution = "usage_hour", time.Hour
			}
			c := record.counters
			if _, errWrite := tx.ExecContext(ctx, `INSERT INTO `+table+` (bucket, provider, model, auth_index, tier, `+counterColumns+`)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`+upsertSuffix,
				record.at.UTC().Truncate(resolution).Unix(), record.provider, record.model, importedAuthIndex, record.tier,
				c.Requests, c.Failed, c.UncachedInput, c.CacheRead, c.CacheWrite, c.Output, c.Reasoning, c.Unclassified); errWrite != nil {
				return errWrite
			}
		}
		imported++
		counters.add(record.counters)
	}
	if tx != nil {
		if errCommit := tx.Commit(); errCommit != nil {
			return fmt.Errorf("commit usage import: %w", errCommit)
		}
	}
	result.Imported += imported
	result.Duplicates += duplicates
	result.Counters.add(counters)
	return nil
}

func readImportLines(ctx context.Context, path string, visit func([]byte, bool) error) (err error) {
	file, errOpen := os.Open(path)
	if errOpen != nil {
		return errOpen
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	reader := bufio.NewReaderSize(file, 64<<10)
	var line []byte
	oversized := false
	for {
		if errContext := ctx.Err(); errContext != nil {
			return errContext
		}
		fragment, errRead := reader.ReadSlice('\n')
		if !oversized && len(line)+len(fragment) <= maxTranscriptLine {
			line = append(line, fragment...)
		} else {
			oversized = true
			line = line[:0]
		}
		if errors.Is(errRead, bufio.ErrBufferFull) {
			continue
		}
		if errRead != nil && !errors.Is(errRead, io.EOF) {
			return errRead
		}
		if len(line) > 0 || oversized {
			if errVisit := visit(line, oversized); errVisit != nil {
				return errVisit
			}
		}
		line, oversized = line[:0], false
		if errors.Is(errRead, io.EOF) {
			return nil
		}
	}
}
