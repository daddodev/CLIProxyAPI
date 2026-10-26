package management

import (
	"context"
	"errors"
	"net/http"
	"net/url"
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
	result, failure := h.usageHistorySnapshot(c.Request.Context(), c.Request.URL.Query(), time.Now())
	if failure != nil {
		writeUsageError(c, failure.status, failure.code, failure.message)
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handler) usageHistorySnapshot(ctx context.Context, query url.Values, now time.Time) (*usagehistory.HistoryResult, *usageFailure) {
	store := usagehistory.Current()
	if store == nil {
		return nil, &usageFailure{http.StatusServiceUnavailable, "history_disabled", "Usage history is disabled."}
	}

	granularity := strings.ToLower(strings.TrimSpace(query.Get("granularity")))
	if granularity == "" {
		granularity = usagehistory.GranularityHour
	}
	lookback, ok := defaultHistoryRange[granularity]
	if !ok {
		return nil, &usageFailure{http.StatusBadRequest, "invalid_request", "granularity must be minute, hour, day or month."}
	}
	loc := time.UTC
	if tz := strings.TrimSpace(query.Get("tz")); tz != "" {
		loaded, errLoad := time.LoadLocation(tz)
		if errLoad != nil {
			return nil, &usageFailure{http.StatusBadRequest, "invalid_request", "Unknown time zone."}
		}
		loc = loaded
	}
	to := now
	if raw := strings.TrimSpace(query.Get("to")); raw != "" {
		parsed, errParse := time.Parse(time.RFC3339, raw)
		if errParse != nil {
			return nil, &usageFailure{http.StatusBadRequest, "invalid_request", "to must be an RFC3339 time."}
		}
		to = parsed
	}
	from := lookback(to)
	if raw := strings.TrimSpace(query.Get("from")); raw != "" {
		parsed, errParse := time.Parse(time.RFC3339, raw)
		if errParse != nil {
			return nil, &usageFailure{http.StatusBadRequest, "invalid_request", "from must be an RFC3339 time."}
		}
		from = parsed
	}

	rates, status, updatedAt := usagehistory.CurrentPricing().Snapshot()
	result, errHistory := store.History(ctx, usagehistory.HistoryQuery{
		Granularity: granularity,
		From:        from,
		To:          to,
		Location:    loc,
	}, rates)
	if errHistory != nil {
		if errors.Is(errHistory, usagehistory.ErrInvalidQuery) {
			return nil, &usageFailure{http.StatusBadRequest, "invalid_request", errHistory.Error()}
		}
		log.WithError(errHistory).Error("usage history query failed")
		return nil, &usageFailure{http.StatusInternalServerError, "history_failed", "Could not read usage history."}
	}
	result.Pricing.Status = status
	if !updatedAt.IsZero() {
		u := updatedAt.UTC()
		result.Pricing.UpdatedAt = &u
	}
	return result, nil
}
