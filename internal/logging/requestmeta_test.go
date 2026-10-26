package logging

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestWithFreshResponseHeadersHolderIsolatesAttempts(t *testing.T) {
	requestCtx := WithResponseHeadersHolder(context.Background())
	SetResponseHeaders(requestCtx, http.Header{"X-Upstream-Attempt": []string{"request"}})

	firstAttemptCtx := WithFreshResponseHeadersHolder(requestCtx)
	if headers := GetResponseHeaders(firstAttemptCtx); len(headers) != 0 {
		t.Fatalf("fresh attempt inherited request headers: %#v", headers)
	}
	SetResponseHeaders(firstAttemptCtx, http.Header{"X-Upstream-Attempt": []string{"first"}})

	secondAttemptCtx := WithFreshResponseHeadersHolder(firstAttemptCtx)
	if headers := GetResponseHeaders(secondAttemptCtx); len(headers) != 0 {
		t.Fatalf("second attempt inherited first attempt headers: %#v", headers)
	}
	SetResponseHeaders(secondAttemptCtx, http.Header{"X-Upstream-Attempt": []string{"second"}})

	if got := GetResponseHeaders(requestCtx).Get("X-Upstream-Attempt"); got != "request" {
		t.Fatalf("request holder = %q, want request", got)
	}
	if got := GetResponseHeaders(firstAttemptCtx).Get("X-Upstream-Attempt"); got != "first" {
		t.Fatalf("first attempt holder = %q, want first", got)
	}
	if got := GetResponseHeaders(secondAttemptCtx).Get("X-Upstream-Attempt"); got != "second" {
		t.Fatalf("second attempt holder = %q, want second", got)
	}
}

func TestResponseHeadersObserverSeesStreamUpdatesOutsideLock(t *testing.T) {
	ctx := WithFreshResponseHeadersHolder(context.Background())
	var snapshots []http.Header
	ctx = WithResponseHeadersObserver(ctx, func(headers http.Header, observedAt time.Time) {
		if observedAt.IsZero() {
			t.Error("missing observation timestamp")
		}
		// Re-entry must not deadlock, and mutating the callback's snapshot must
		// not corrupt the header holder used by retries and result accounting.
		_ = GetResponseHeaders(ctx)
		snapshots = append(snapshots, headers.Clone())
		headers.Set("X-Codex-Primary-Used-Percent", "999")
	})
	SetResponseHeaders(ctx, http.Header{"X-Codex-Plan-Type": {"pro"}})
	MergeResponseHeaders(ctx, http.Header{"X-Codex-Primary-Used-Percent": {"42"}})
	if len(snapshots) != 2 || snapshots[1].Get("X-Codex-Plan-Type") != "pro" || snapshots[1].Get("X-Codex-Primary-Used-Percent") != "42" {
		t.Fatalf("unexpected stream observations: %+v", snapshots)
	}
	if GetResponseHeaders(ctx).Get("X-Codex-Primary-Used-Percent") != "42" {
		t.Fatal("observer modified the stored response headers")
	}
}
