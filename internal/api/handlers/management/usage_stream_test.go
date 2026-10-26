package management

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestStreamUsagePushesPassiveObservations(t *testing.T) {
	auth := claudeTestAuth("stream", "stream@example.com")
	auth.Quota.ObserveResponseHeadersForProvider("claude", http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.25"},
	}, time.Now())
	h, recorded := newUsageTestHandler(t, nil, func(*http.Request, []byte) *http.Response {
		return usageJSONResponse(http.StatusTooManyRequests, `{}`)
	})
	registered, errRegister := h.authManager.Register(context.Background(), auth)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	ended := make(chan struct{})
	router := gin.New()
	router.GET("/stream", func(c *gin.Context) {
		defer close(ended)
		h.StreamUsage(c)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/stream?granularity=hour&buckets=48", nil)
	response, errGet := server.Client().Do(req)
	if errGet != nil {
		t.Fatal(errGet)
	}
	defer func() {
		if errClose := response.Body.Close(); errClose != nil {
			t.Error(errClose)
		}
	}()
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") || response.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("unexpected stream headers: %v", response.Header)
	}
	frames := make(chan []byte, 4)
	go func() {
		defer close(frames)
		scanner := bufio.NewScanner(response.Body)
		for scanner.Scan() {
			if line := scanner.Text(); strings.HasPrefix(line, "data: ") {
				select {
				case frames <- []byte(strings.TrimPrefix(line, "data: ")):
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	readPercent := func(want float64) {
		t.Helper()
		select {
		case frame := <-frames:
			var snapshot struct {
				Limits usageLimitsPayload `json:"limits"`
			}
			if errDecode := json.Unmarshal(frame, &snapshot); errDecode != nil {
				t.Fatal(errDecode)
			}
			accounts := snapshot.Limits.Providers[0].Accounts
			if accounts[0].Source != "headers" || accounts[0].Windows[0].UsedPercent != want {
				t.Fatalf("unexpected streamed account: %+v", accounts[0])
			}
		case <-time.After(5 * time.Second):
			t.Fatal("stream did not deliver a usage snapshot")
		}
	}
	readPercent(25)
	h.authManager.ObserveQuotaHeaders(auth.ID, registered.RegistrationEpoch, http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.5"},
	}, time.Now().Add(time.Second))
	readPercent(50)
	if len(*recorded) != 0 {
		t.Fatalf("passive stream triggered %d provider reads", len(*recorded))
	}
	cancel()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not unsubscribe after client cancellation")
	}
}

func TestUsageStreamHistoryQueryRollsAcrossCalendarBoundaries(t *testing.T) {
	query := url.Values{"granularity": {"month"}, "buckets": {"12"}, "tz": {"Europe/Rome"}}
	now := time.Date(2026, time.December, 31, 23, 30, 0, 0, time.UTC)
	result, errQuery := usageStreamHistoryQuery(query, now)
	if errQuery != nil {
		t.Fatal(errQuery)
	}
	if result.Get("from") != "2026-02-01T00:00:00+01:00" || result.Get("to") != "" {
		t.Fatalf("expected rolling local calendar range, got %v", result)
	}
	for _, bad := range []url.Values{
		{"granularity": {"year"}}, {"tz": {"Unknown/Zone"}},
		{"buckets": {"0"}}, {"buckets": {"1001"}}, {"buckets": {"not-a-number"}},
	} {
		if _, errBad := usageStreamHistoryQuery(bad, now); errBad == nil {
			t.Fatalf("invalid stream query accepted: %v", bad)
		}
	}
}
