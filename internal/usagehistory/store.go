// Package usagehistory persists per-minute token usage rollups in SQLite for
// the usage dashboard and computes cost estimates from public model prices.
package usagehistory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	_ "modernc.org/sqlite"
	// Embed zone data so dashboard time zones resolve in minimal containers.
	_ "time/tzdata"
)

const (
	// minuteRetention bounds how long per-minute rows are kept before they are
	// folded into hourly rows.
	minuteRetention = 7 * 24 * time.Hour
	flushInterval   = 2 * time.Second
	compactInterval = time.Hour
	// maxPending caps buffered rollup keys so a stalled database cannot grow memory without bound.
	maxPending = 50000
)

const schema = `
CREATE TABLE IF NOT EXISTS usage_minute (
	bucket INTEGER NOT NULL,
	provider TEXT NOT NULL,
	model TEXT NOT NULL,
	auth_index TEXT NOT NULL,
	tier TEXT NOT NULL,
	requests INTEGER NOT NULL DEFAULT 0 CHECK (requests >= 0),
	failed INTEGER NOT NULL DEFAULT 0 CHECK (failed >= 0),
	uncached_input INTEGER NOT NULL DEFAULT 0 CHECK (uncached_input >= 0),
	cache_read INTEGER NOT NULL DEFAULT 0 CHECK (cache_read >= 0),
	cache_write INTEGER NOT NULL DEFAULT 0 CHECK (cache_write >= 0),
	output INTEGER NOT NULL DEFAULT 0 CHECK (output >= 0),
	reasoning INTEGER NOT NULL DEFAULT 0 CHECK (reasoning >= 0),
	unclassified INTEGER NOT NULL DEFAULT 0 CHECK (unclassified >= 0),
	PRIMARY KEY (bucket, provider, model, auth_index, tier)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS usage_hour (
	bucket INTEGER NOT NULL,
	provider TEXT NOT NULL,
	model TEXT NOT NULL,
	auth_index TEXT NOT NULL,
	tier TEXT NOT NULL,
	requests INTEGER NOT NULL DEFAULT 0 CHECK (requests >= 0),
	failed INTEGER NOT NULL DEFAULT 0 CHECK (failed >= 0),
	uncached_input INTEGER NOT NULL DEFAULT 0 CHECK (uncached_input >= 0),
	cache_read INTEGER NOT NULL DEFAULT 0 CHECK (cache_read >= 0),
	cache_write INTEGER NOT NULL DEFAULT 0 CHECK (cache_write >= 0),
	output INTEGER NOT NULL DEFAULT 0 CHECK (output >= 0),
	reasoning INTEGER NOT NULL DEFAULT 0 CHECK (reasoning >= 0),
	unclassified INTEGER NOT NULL DEFAULT 0 CHECK (unclassified >= 0),
	PRIMARY KEY (bucket, provider, model, auth_index, tier)
) WITHOUT ROWID;
`

const counterColumns = "requests, failed, uncached_input, cache_read, cache_write, output, reasoning, unclassified"

const upsertSuffix = ` ON CONFLICT (bucket, provider, model, auth_index, tier) DO UPDATE SET
	requests = requests + excluded.requests,
	failed = failed + excluded.failed,
	uncached_input = uncached_input + excluded.uncached_input,
	cache_read = cache_read + excluded.cache_read,
	cache_write = cache_write + excluded.cache_write,
	output = output + excluded.output,
	reasoning = reasoning + excluded.reasoning,
	unclassified = unclassified + excluded.unclassified`

// rollupKey identifies one per-minute aggregation row.
type rollupKey struct {
	Bucket    int64
	Provider  string
	Model     string
	AuthIndex string
	Tier      string
}

// Counters holds additive token and request counts.
type Counters struct {
	Requests      int64
	Failed        int64
	UncachedInput int64
	CacheRead     int64
	CacheWrite    int64
	Output        int64
	Reasoning     int64
	Unclassified  int64
}

func (c *Counters) add(o Counters) {
	c.Requests += o.Requests
	c.Failed += o.Failed
	c.UncachedInput += o.UncachedInput
	c.CacheRead += o.CacheRead
	c.CacheWrite += o.CacheWrite
	c.Output += o.Output
	c.Reasoning += o.Reasoning
	c.Unclassified += o.Unclassified
}

// Store owns the SQLite database and the in-memory write buffer. A single
// background goroutine is the only writer.
type Store struct {
	db   *sql.DB
	path string
	now  func() time.Time

	mu      sync.Mutex
	pending map[rollupKey]Counters
	dropped int64

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	// finalErr records a final flush that could not be written before Close.
	finalErr error
}

// Open creates or opens the history database at path and starts the writer.
func Open(path string) (*Store, error) {
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o700); errMkdir != nil {
		return nil, fmt.Errorf("create usage history directory: %w", errMkdir)
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, errOpen := sql.Open("sqlite", dsn)
	if errOpen != nil {
		return nil, fmt.Errorf("open usage history: %w", errOpen)
	}
	// One connection serializes writers and readers inside the process; WAL
	// keeps reads cheap and the batched writer keeps transactions short.
	db.SetMaxOpenConns(1)
	if _, errSchema := db.Exec(schema); errSchema != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create usage history schema: %w", errSchema)
	}
	s := &Store{
		db:      db,
		path:    path,
		now:     time.Now,
		pending: make(map[rollupKey]Counters),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go s.run()
	return s, nil
}

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// Close flushes buffered rows and closes the database.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	var errClose error
	s.closeOnce.Do(func() {
		close(s.stop)
		<-s.done
		errClose = errors.Join(s.finalErr, s.db.Close())
	})
	return errClose
}

// add buffers counters for the minute containing at.
func (s *Store) add(at time.Time, provider, model, authIndex, tier string, c Counters) {
	key := rollupKey{
		Bucket:    at.UTC().Truncate(time.Minute).Unix(),
		Provider:  provider,
		Model:     model,
		AuthIndex: authIndex,
		Tier:      tier,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.pending[key]
	if !ok && len(s.pending) >= maxPending {
		s.dropped++
		return
	}
	existing.add(c)
	s.pending[key] = existing
}

func (s *Store) run() {
	defer close(s.done)
	flush := time.NewTicker(flushInterval)
	defer flush.Stop()
	compact := time.NewTicker(compactInterval)
	defer compact.Stop()
	s.compact()
	for {
		select {
		case <-s.stop:
			s.finalFlush()
			return
		case <-flush.C:
			if errFlush := s.flush(); errFlush != nil {
				log.WithError(errFlush).Error("usage history: failed to write batch; will retry")
			}
		case <-compact.C:
			s.compact()
		}
	}
}

// finalFlush retries the last write a few times so a transient lock or I/O
// error at shutdown does not silently discard buffered usage.
func (s *Store) finalFlush() {
	var errFlush error
	for attempt := 1; attempt <= 3; attempt++ {
		if errFlush = s.flush(); errFlush == nil {
			return
		}
		time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
	}
	s.mu.Lock()
	lost := len(s.pending)
	s.mu.Unlock()
	s.finalErr = fmt.Errorf("usage history: %d buffered rollups not written: %w", lost, errFlush)
}

// flush writes buffered rollups. On failure the batch stays buffered for the next flush.
func (s *Store) flush() error {
	s.mu.Lock()
	if len(s.pending) == 0 {
		s.mu.Unlock()
		return nil
	}
	batch := s.pending
	dropped := s.dropped
	s.pending = make(map[rollupKey]Counters, len(batch))
	s.dropped = 0
	s.mu.Unlock()

	if dropped > 0 {
		log.WithField("dropped", dropped).Warn("usage history buffer full; dropped usage records")
	}
	if errWrite := s.write(batch); errWrite != nil {
		s.requeue(batch)
		return errWrite
	}
	return nil
}

// requeue returns a failed batch to the buffer so the next flush retries it.
func (s *Store) requeue(batch map[rollupKey]Counters) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, c := range batch {
		existing, ok := s.pending[key]
		if !ok && len(s.pending) >= maxPending {
			s.dropped++
			continue
		}
		existing.add(c)
		s.pending[key] = existing
	}
}

func (s *Store) write(batch map[rollupKey]Counters) error {
	tx, errBegin := s.db.Begin()
	if errBegin != nil {
		return errBegin
	}
	stmt, errPrepare := tx.Prepare(`INSERT INTO usage_minute (bucket, provider, model, auth_index, tier, ` + counterColumns + `)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)` + upsertSuffix)
	if errPrepare != nil {
		_ = tx.Rollback()
		return errPrepare
	}
	for key, c := range batch {
		if _, errExec := stmt.Exec(key.Bucket, key.Provider, key.Model, key.AuthIndex, key.Tier,
			c.Requests, c.Failed, c.UncachedInput, c.CacheRead, c.CacheWrite, c.Output, c.Reasoning, c.Unclassified); errExec != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return errExec
		}
	}
	if errClose := stmt.Close(); errClose != nil {
		_ = tx.Rollback()
		return errClose
	}
	return tx.Commit()
}

// compact folds minute rows older than minuteRetention into hourly rows.
func (s *Store) compact() {
	cutoff := s.now().UTC().Add(-minuteRetention).Truncate(time.Hour).Unix()
	tx, errBegin := s.db.Begin()
	if errBegin != nil {
		log.WithError(errBegin).Error("usage history: compaction begin failed")
		return
	}
	_, errInsert := tx.Exec(`INSERT INTO usage_hour (bucket, provider, model, auth_index, tier, `+counterColumns+`)
		SELECT (bucket / 3600) * 3600, provider, model, auth_index, tier,
			SUM(requests), SUM(failed), SUM(uncached_input), SUM(cache_read), SUM(cache_write), SUM(output), SUM(reasoning), SUM(unclassified)
		FROM usage_minute WHERE bucket < ?
		GROUP BY (bucket / 3600) * 3600, provider, model, auth_index, tier`+upsertSuffix, cutoff)
	if errInsert != nil {
		_ = tx.Rollback()
		log.WithError(errInsert).Error("usage history: compaction failed")
		return
	}
	if _, errDelete := tx.Exec(`DELETE FROM usage_minute WHERE bucket < ?`, cutoff); errDelete != nil {
		_ = tx.Rollback()
		log.WithError(errDelete).Error("usage history: compaction cleanup failed")
		return
	}
	if errCommit := tx.Commit(); errCommit != nil {
		log.WithError(errCommit).Error("usage history: compaction commit failed")
	}
}

// Row is one aggregated query row.
type Row struct {
	Bucket   time.Time
	Provider string
	Model    string
	Tier     string
	Counters
}

// queryRows returns rows in [from, to) at their stored resolution: recent
// rows per minute and compacted rows per hour. Callers bucket them, so local
// day boundaries in half-hour time zones stay exact for the last 7 days.
// Older hourly rows land in the bucket containing their hour start.
func (s *Store) queryRows(ctx context.Context, from, to time.Time) ([]Row, error) {
	// Include rows still buffered in memory so the dashboard is live.
	if errFlush := s.flush(); errFlush != nil {
		log.WithError(errFlush).Warn("usage history: flush before query failed")
	}

	sums := "SUM(requests), SUM(failed), SUM(uncached_input), SUM(cache_read), SUM(cache_write), SUM(output), SUM(reasoning), SUM(unclassified)"
	query := `SELECT bucket, provider, model, tier, ` + sums + ` FROM (
			SELECT bucket, provider, model, tier, ` + counterColumns + ` FROM usage_minute WHERE bucket >= ? AND bucket < ?
			UNION ALL
			SELECT bucket, provider, model, tier, ` + counterColumns + ` FROM usage_hour WHERE bucket >= ? AND bucket < ?
		) GROUP BY bucket, provider, model, tier ORDER BY bucket`
	args := []any{from.Unix(), to.Unix(), from.Truncate(time.Hour).Unix(), to.Unix()}
	rows, errQuery := s.db.QueryContext(ctx, query, args...)
	if errQuery != nil {
		return nil, fmt.Errorf("query usage history: %w", errQuery)
	}
	defer func() {
		if errClose := rows.Close(); errClose != nil {
			log.WithError(errClose).Debug("usage history: close rows")
		}
	}()
	var out []Row
	for rows.Next() {
		var r Row
		var bucket int64
		if errScan := rows.Scan(&bucket, &r.Provider, &r.Model, &r.Tier,
			&r.Requests, &r.Failed, &r.UncachedInput, &r.CacheRead, &r.CacheWrite, &r.Output, &r.Reasoning, &r.Unclassified); errScan != nil {
			return nil, fmt.Errorf("scan usage history: %w", errScan)
		}
		r.Bucket = time.Unix(bucket, 0).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}
