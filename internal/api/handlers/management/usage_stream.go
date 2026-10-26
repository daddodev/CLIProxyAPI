package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usageupdates"
)

const usageStreamInterval = 30 * time.Second

// StreamUsage pushes snapshots on upstream quota observations and recorded usage.
// The timer advances idle probe deadlines and chart windows even without traffic;
// opening more streams does not bypass the shared account probe cache.
func (h *Handler) StreamUsage(c *gin.Context) {
	query := c.Request.URL.Query()
	if _, errQuery := usageStreamHistoryQuery(query, time.Now()); errQuery != nil {
		writeUsageError(c, http.StatusBadRequest, "invalid_request", errQuery.Error())
		return
	}
	updates, unsubscribe := usageupdates.Subscribe()
	defer unsubscribe()
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache, no-transform")
	c.Header("X-Accel-Buffering", "no")

	send := func() bool {
		now := time.Now()
		limits, errLimits := h.usageLimitsSnapshot(c.Request.Context(), now, false)
		historyQuery, _ := usageStreamHistoryQuery(query, now)
		history, historyFailure := h.usageHistorySnapshot(c.Request.Context(), historyQuery, now)
		payload := gin.H{"limits": limits, "history": history}
		if errLimits != nil {
			payload["limits_error"] = errLimits.Error()
		}
		if historyFailure != nil {
			payload["history_error"] = historyFailure.message
		}
		if c.Request.Context().Err() != nil {
			return false
		}
		data, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return false
		}
		if _, errWrite := fmt.Fprintf(c.Writer, "event: snapshot\ndata: %s\n\n", data); errWrite != nil {
			return false
		}
		c.Writer.Flush()
		return true
	}
	if !send() {
		return
	}
	heartbeat := time.NewTicker(usageStreamInterval)
	defer heartbeat.Stop()
	// Coalesce request bursts without indefinitely delaying a busy dashboard.
	var pending <-chan time.Time
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-updates:
			if pending == nil {
				timer.Reset(250 * time.Millisecond)
				pending = timer.C
			}
		case <-pending:
			pending = nil
			if !send() {
				return
			}
		case <-heartbeat.C:
			if !send() {
				return
			}
		}
	}
}

// usageStreamHistoryQuery keeps the selected number of calendar buckets moving
// in the client's time zone for the lifetime of the stream.
func usageStreamHistoryQuery(query url.Values, now time.Time) (url.Values, error) {
	granularity := query.Get("granularity")
	if granularity == "" {
		granularity = "hour"
	}
	if _, ok := defaultHistoryRange[granularity]; !ok {
		return nil, fmt.Errorf("granularity must be minute, hour, day or month")
	}
	loc := time.UTC
	if tz := query.Get("tz"); tz != "" {
		loaded, errLoad := time.LoadLocation(tz)
		if errLoad != nil {
			return nil, fmt.Errorf("unknown time zone")
		}
		loc = loaded
	}
	result := url.Values{"granularity": {granularity}, "tz": {loc.String()}}
	if raw := query.Get("buckets"); raw != "" {
		count, errCount := strconv.Atoi(raw)
		if errCount != nil || count < 1 || count > 1000 {
			return nil, fmt.Errorf("buckets must be between 1 and 1000")
		}
		now = now.In(loc)
		var from time.Time
		switch granularity {
		case "minute":
			from = now.Add(-time.Duration(count-1) * time.Minute)
		case "hour":
			from = now.Add(-time.Duration(count-1) * time.Hour)
		case "day":
			from = now.AddDate(0, 0, -(count - 1))
		case "month":
			from = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc).AddDate(0, -(count - 1), 0)
		}
		result.Set("from", from.Format(time.RFC3339))
	}
	return result, nil
}
