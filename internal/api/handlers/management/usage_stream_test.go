package management

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestUsageStreamKeepsBreakdownOptIn(t *testing.T) {
	for _, value := range []string{"", "true"} {
		query, errQuery := usageStreamHistoryQuery(url.Values{"granularity": {"hour"}, "buckets": {"48"}, "breakdown": {value}}, time.Now())
		if errQuery != nil || query.Get("breakdown") != value {
			t.Fatalf("breakdown %q: query %v, error %v", value, query, errQuery)
		}
	}
}

func TestStreamUsagePushesPassiveObservations(t *testing.T) {
	auth := claudeTestAuth("stream", "stream@example.com")
	auth.Quota.ObserveResponseHeadersForProvider("claude", http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.25"},
	}, time.Now())
	var providerReads atomic.Int32
	h, _ := newUsageTestHandler(t, nil, func(req *http.Request, _ []byte) *http.Response {
		providerReads.Add(1)
		if req.Method != http.MethodGet || req.URL.String() != claudeUsageURL {
			t.Errorf("unexpected provider read: %s %s", req.Method, req.URL)
		}
		response := usageJSONResponse(http.StatusTooManyRequests, `{}`)
		response.Header.Set("Retry-After", "900")
		return response
	})
	registered, errRegister := h.authManager.Register(context.Background(), auth)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	ended := make(chan struct{}, 2)
	router := gin.New()
	router.GET("/stream", func(c *gin.Context) {
		defer func() { ended <- struct{}{} }()
		h.StreamUsage(c)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	openStream := func() <-chan []byte {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/stream?granularity=hour&buckets=48", nil)
		response, errGet := server.Client().Do(req)
		if errGet != nil {
			t.Fatal(errGet)
		}
		t.Cleanup(func() {
			if errClose := response.Body.Close(); errClose != nil {
				t.Error(errClose)
			}
		})
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
		return frames
	}
	readPercent := func(frames <-chan []byte, want float64) {
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
			if accounts[0].Error != "The provider is rate limiting usage reads." {
				t.Fatalf("stream lost the supplemental read error: %+v", accounts[0])
			}
		case <-time.After(5 * time.Second):
			t.Fatal("stream did not deliver a usage snapshot")
		}
	}
	first, second := openStream(), openStream()
	readPercent(first, 25)
	readPercent(second, 25)
	if reads := providerReads.Load(); reads != 1 {
		t.Fatalf("two subscribers made %d supplemental provider reads, want exactly 1", reads)
	}
	index := authIndexFor(t, h, auth.ID)
	probe, _ := h.usageLimits.get(index, time.Now(), false)
	if probe == nil || probe.retryAt.Before(probe.at.Add(15*time.Minute)) {
		t.Fatalf("stream did not preserve provider Retry-After: %+v", probe)
	}
	// Crossing the ordinary TTL, including an explicit dashboard refresh,
	// cannot bypass the read deadline shared with both live streams.
	for _, force := range []bool{false, true} {
		if _, errSnapshot := h.usageLimitsSnapshot(ctx, probe.at.Add(usageLimitsTTL+time.Minute), force); errSnapshot != nil {
			t.Fatal(errSnapshot)
		}
	}
	for i, utilization := range []string{"0.5", "0.75"} {
		h.authManager.ObserveQuotaHeaders(auth.ID, registered.RegistrationEpoch, http.Header{
			"Anthropic-Ratelimit-Unified-5h-Utilization": {utilization},
		}, time.Now().Add(time.Duration(i+1)*time.Second))
		want := float64(50 + i*25)
		readPercent(first, want)
		readPercent(second, want)
		if reads := providerReads.Load(); reads != 1 {
			t.Fatalf("observation event %d or refresh bypassed shared backoff: %d provider reads, want 1", i+1, reads)
		}
	}
	cancel()
	for range 2 {
		select {
		case <-ended:
		case <-time.After(5 * time.Second):
			t.Fatal("stream did not unsubscribe after client cancellation")
		}
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
