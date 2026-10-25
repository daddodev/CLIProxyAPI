package management

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagehistory"
	log "github.com/sirupsen/logrus"
)

// defaultHistoryRange is the lookback used when the caller omits from.
var defaultHistoryRange = map[string]func(time.Time) time.Time{
	usagehistory.GranularityMinute: func(t time.Time) time.Time { return t.Add(-3 * time.Hour) },
	usagehistory.GranularityHour:   func(t time.Time) time.Time { return t.Add(-48 * time.Hour) },
	usagehistory.GranularityDay:    func(t time.Time) time.Time { return t.AddDate(0, 0, -30) },
	usagehistory.GranularityMonth:  func(t time.Time) time.Time { return t.AddDate(-1, 0, 0) },
}

// GetUsageHistory returns token and cost history bucketed by minute, hour, day or month.
func (h *Handler) GetUsageHistory(c *gin.Context) {
	store := usagehistory.Current()
	if store == nil {
		writeUsageError(c, http.StatusServiceUnavailable, "history_disabled", "Usage history is disabled.")
		return
	}

	granularity := strings.ToLower(strings.TrimSpace(c.DefaultQuery("granularity", usagehistory.GranularityHour)))
	lookback, ok := defaultHistoryRange[granularity]
	if !ok {
		writeUsageError(c, http.StatusBadRequest, "invalid_request", "granularity must be minute, hour, day or month.")
		return
	}
	loc := time.UTC
	if tz := strings.TrimSpace(c.Query("tz")); tz != "" {
		loaded, errLoad := time.LoadLocation(tz)
		if errLoad != nil {
			writeUsageError(c, http.StatusBadRequest, "invalid_request", "Unknown time zone.")
			return
		}
		loc = loaded
	}
	now := time.Now()
	to := now
	if raw := strings.TrimSpace(c.Query("to")); raw != "" {
		parsed, errParse := time.Parse(time.RFC3339, raw)
		if errParse != nil {
			writeUsageError(c, http.StatusBadRequest, "invalid_request", "to must be an RFC3339 time.")
			return
		}
		to = parsed
	}
	from := lookback(to)
	if raw := strings.TrimSpace(c.Query("from")); raw != "" {
		parsed, errParse := time.Parse(time.RFC3339, raw)
		if errParse != nil {
			writeUsageError(c, http.StatusBadRequest, "invalid_request", "from must be an RFC3339 time.")
			return
		}
		from = parsed
	}

	rates, status, updatedAt := usagehistory.CurrentPricing().Snapshot()
	result, errHistory := store.History(c.Request.Context(), usagehistory.HistoryQuery{
		Granularity: granularity,
		From:        from,
		To:          to,
		Location:    loc,
	}, rates)
	if errHistory != nil {
		if errors.Is(errHistory, usagehistory.ErrInvalidQuery) {
			writeUsageError(c, http.StatusBadRequest, "invalid_request", errHistory.Error())
			return
		}
		log.WithError(errHistory).Error("usage history query failed")
		writeUsageError(c, http.StatusInternalServerError, "history_failed", "Could not read usage history.")
		return
	}
	result.Pricing.Status = status
	if !updatedAt.IsZero() {
		u := updatedAt.UTC()
		result.Pricing.UpdatedAt = &u
	}
	c.JSON(http.StatusOK, result)
}
