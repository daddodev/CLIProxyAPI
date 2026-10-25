package management

import (
	"context"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	fileauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestQuotaLimitPatchValidation(t *testing.T) {
	for _, raw := range []string{"0", "80.5", "100", "null"} {
		if _, err := normalizeAuthFilePatchFields(map[string]json.RawMessage{"quota_limit_percent": json.RawMessage(raw)}); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
	}
	for _, raw := range []string{"-1", "101", `"80"`, "true", "{}"} {
		if _, err := normalizeAuthFilePatchFields(map[string]json.RawMessage{"quota_limit_percent": json.RawMessage(raw)}); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if _, err := normalizeAuthFilePatchFields(map[string]json.RawMessage{"quota_limit_percent.x": json.RawMessage("80")}); err == nil {
		t.Fatal("accepted nested limit")
	}
}

func TestUsageProbeWindowsFeedQuotaLimit(t *testing.T) {
	now := time.Unix(1800000000, 0)
	reset := now.Add(time.Hour)
	for _, provider := range []string{"claude", "codex"} {
		a := &coreauth.Auth{Provider: provider, Metadata: map[string]any{coreauth.QuotaLimitPercentKey: 80.0}}
		h := usageWindowHeaders(provider, []usageWindow{{Kind: windowFiveHour, UsedPercent: 80, ResetsAt: &reset}})
		a.Quota.ObserveResponseHeadersForProvider(provider, h, now)
		if blocked, until := coreauth.QuotaLimitReached(a, now); !blocked || !until.Equal(reset) {
			t.Fatalf("%s: blocked=%v until=%v", provider, blocked, until)
		}
	}
}

func TestQuotaLimitPatchPersistsAndClears(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	dir := t.TempDir()
	store := fileauth.NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	path := filepath.Join(dir, "limit.json")
	_, err := manager.Register(context.Background(), &coreauth.Auth{ID: "limit.json", FileName: "limit.json", Provider: "codex", Attributes: map[string]string{"path": path}, Metadata: map[string]any{"type": "codex"}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, manager)
	for _, value := range []string{"82.5", "null"} {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v8/management/credentials/fields", strings.NewReader(`{"name":"limit.json","quota_limit_percent":`+value+`}`))
		ctx.Request.Header.Set("Content-Type", "application/json")
		handler.PatchAuthFileFields(ctx)
		if rec.Code != http.StatusOK {
			t.Fatalf("patch failed: %d %s", rec.Code, rec.Body.String())
		}
		updated, _ := manager.GetByID("limit.json")
		expected := 82.5
		if value == "null" {
			expected = 0
		}
		if got := coreauth.QuotaLimitPercent(updated); got != expected {
			t.Fatalf("runtime=%v expected=%v", got, expected)
		}
		raw, errRead := os.ReadFile(path)
		if errRead != nil {
			t.Fatal(errRead)
		}
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			t.Fatal(err)
		}
		if value == "null" {
			if data[coreauth.QuotaLimitPercentKey] != nil {
				t.Fatal("cleared ceiling remains in auth file")
			}
		} else if data[coreauth.QuotaLimitPercentKey] != expected {
			t.Fatal("ceiling not persisted")
		}
	}
}
