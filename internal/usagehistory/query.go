package usagehistory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Granularity values accepted by History.
const (
	GranularityMinute = "minute"
	GranularityHour   = "hour"
	GranularityDay    = "day"
	GranularityMonth  = "month"
)

const maxBuckets = 2000

// ErrInvalidQuery marks a request the caller must fix.
var ErrInvalidQuery = errors.New("invalid usage history query")

// HistoryQuery selects a time range and bucket size.
type HistoryQuery struct {
	Granularity string
	From        time.Time
	To          time.Time
	Location    *time.Location
}

// UsageEntry is one aggregated provider/model series value.
type UsageEntry struct {
	Provider         string         `json:"provider,omitempty"`
	Model            string         `json:"model,omitempty"`
	Requests         int64          `json:"requests"`
	FailedRequests   int64          `json:"failed_requests"`
	InputTokens      int64          `json:"input_tokens"`
	CacheReadTokens  int64          `json:"cache_read_tokens"`
	CacheWriteTokens int64          `json:"cache_write_tokens"`
	OutputTokens     int64          `json:"output_tokens"`
	ReasoningTokens  int64          `json:"reasoning_tokens"`
	TotalTokens      int64          `json:"total_tokens"`
	CostUSD          float64        `json:"cost_usd"`
	Priced           bool           `json:"priced"`
	CostBreakdown    *CostBreakdown `json:"cost_breakdown"`

	// unpricedRows counts contributing rows without a price. Their tokens are
	// counted but their cost is excluded from the estimate.
	unpricedRows int64
}

func (e *UsageEntry) add(c Counters, cost CostBreakdown, priced bool) {
	e.Requests += c.Requests
	e.FailedRequests += c.Failed
	e.InputTokens += c.UncachedInput
	e.CacheReadTokens += c.CacheRead
	e.CacheWriteTokens += c.CacheWrite
	e.OutputTokens += c.Output
	e.ReasoningTokens += c.Reasoning
	e.TotalTokens += c.UncachedInput + c.CacheRead + c.CacheWrite + c.Output + c.Unclassified
	if !priced {
		e.unpricedRows++
		return
	}
	if e.CostBreakdown == nil {
		e.CostBreakdown = &CostBreakdown{}
	}
	e.CostBreakdown.add(cost)
	e.CostUSD = e.CostBreakdown.Total()
}

func (e *UsageEntry) finish() {
	e.Priced = e.CostBreakdown != nil && e.unpricedRows == 0
	if e.CostBreakdown == nil {
		e.CostUSD = 0
	}
}

// Bucket is one time bucket.
type Bucket struct {
	Start  time.Time     `json:"start"`
	Series []*UsageEntry `json:"series"`
}

// PricingInfo reports the price table state.
type PricingInfo struct {
	Status    string     `json:"status"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// HistoryResult is the dashboard history response.
type HistoryResult struct {
	Granularity string        `json:"granularity"`
	TZ          string        `json:"tz"`
	From        time.Time     `json:"from"`
	To          time.Time     `json:"to"`
	Pricing     PricingInfo   `json:"pricing"`
	Buckets     []Bucket      `json:"buckets"`
	Totals      *UsageEntry   `json:"totals"`
	Models      []*UsageEntry `json:"models"`
}

// bucketStart returns the start of the bucket containing t. Minute and hour
// buckets are aligned on absolute time, which matches local boundaries for
// every whole-hour UTC offset.
func bucketStart(t time.Time, granularity string, loc *time.Location) time.Time {
	switch granularity {
	case GranularityMinute:
		return t.Truncate(time.Minute).In(loc)
	case GranularityHour:
		return t.Truncate(time.Hour).In(loc)
	case GranularityDay:
		l := t.In(loc)
		return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
	default:
		l := t.In(loc)
		return time.Date(l.Year(), l.Month(), 1, 0, 0, 0, 0, loc)
	}
}

func nextBucket(t time.Time, granularity string) time.Time {
	switch granularity {
	case GranularityMinute:
		return t.Add(time.Minute)
	case GranularityHour:
		return t.Add(time.Hour)
	case GranularityDay:
		return t.AddDate(0, 0, 1)
	default:
		return t.AddDate(0, 1, 0)
	}
}

// History aggregates stored usage for the dashboard.
func (s *Store) History(ctx context.Context, q HistoryQuery, rates RateTable) (*HistoryResult, error) {
	if q.Location == nil {
		q.Location = time.UTC
	}
	switch q.Granularity {
	case GranularityMinute, GranularityHour, GranularityDay, GranularityMonth:
	default:
		return nil, fmt.Errorf("%w: granularity must be minute, hour, day or month", ErrInvalidQuery)
	}
	if !q.To.After(q.From) {
		return nil, fmt.Errorf("%w: to must be after from", ErrInvalidQuery)
	}
	if q.Granularity == GranularityMinute && s.now().Sub(q.From) > minuteRetention {
		return nil, fmt.Errorf("%w: minute granularity covers only the last 7 days", ErrInvalidQuery)
	}

	first := bucketStart(q.From, q.Granularity, q.Location)
	var starts []time.Time
	for t := first; t.Before(q.To); t = nextBucket(t, q.Granularity) {
		starts = append(starts, t)
		if len(starts) > maxBuckets {
			return nil, fmt.Errorf("%w: range spans more than %d buckets", ErrInvalidQuery, maxBuckets)
		}
	}
	end := nextBucket(starts[len(starts)-1], q.Granularity)

	rows, errRows := s.queryRows(ctx, first.UTC(), end.UTC())
	if errRows != nil {
		return nil, errRows
	}

	type seriesKey struct{ provider, model string }
	index := make(map[int64]int, len(starts))
	buckets := make([]Bucket, len(starts))
	bucketSeries := make([]map[seriesKey]*UsageEntry, len(starts))
	for i, start := range starts {
		index[start.Unix()] = i
		buckets[i] = Bucket{Start: start, Series: []*UsageEntry{}}
		bucketSeries[i] = make(map[seriesKey]*UsageEntry)
	}
	totals := &UsageEntry{}
	models := make(map[seriesKey]*UsageEntry)

	for _, row := range rows {
		i, ok := index[bucketStart(row.Bucket, q.Granularity, q.Location).Unix()]
		if !ok {
			continue
		}
		var cost CostBreakdown
		priced := false
		if rates != nil {
			cost, priced = rates.Cost(row.Model, row.Tier, row.Counters)
		}
		key := seriesKey{row.Provider, row.Model}
		entry := bucketSeries[i][key]
		if entry == nil {
			entry = &UsageEntry{Provider: row.Provider, Model: row.Model}
			bucketSeries[i][key] = entry
			buckets[i].Series = append(buckets[i].Series, entry)
		}
		entry.add(row.Counters, cost, priced)
		model := models[key]
		if model == nil {
			model = &UsageEntry{Provider: row.Provider, Model: row.Model}
			models[key] = model
		}
		model.add(row.Counters, cost, priced)
		totals.add(row.Counters, cost, priced)
	}

	for i := range buckets {
		for _, entry := range buckets[i].Series {
			entry.finish()
		}
		sort.Slice(buckets[i].Series, func(a, b int) bool {
			sa, sb := buckets[i].Series[a], buckets[i].Series[b]
			if sa.Provider != sb.Provider {
				return sa.Provider < sb.Provider
			}
			return sa.Model < sb.Model
		})
	}
	modelList := make([]*UsageEntry, 0, len(models))
	for _, entry := range models {
		entry.finish()
		modelList = append(modelList, entry)
	}
	sort.Slice(modelList, func(a, b int) bool {
		if modelList[a].CostUSD != modelList[b].CostUSD {
			return modelList[a].CostUSD > modelList[b].CostUSD
		}
		return modelList[a].TotalTokens > modelList[b].TotalTokens
	})
	totals.finish()
	if totals.CostBreakdown == nil {
		totals.CostBreakdown = &CostBreakdown{}
	}

	return &HistoryResult{
		Granularity: q.Granularity,
		TZ:          q.Location.String(),
		From:        first,
		To:          end,
		Buckets:     buckets,
		Totals:      totals,
		Models:      modelList,
	}, nil
}
