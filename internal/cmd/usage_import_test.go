package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagehistory"
)

func TestUsageImportConfigPreviewDoesNotRewriteFiles(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	home := filepath.Join(root, "claude")
	projects := filepath.Join(home, "projects")
	if errMkdir := os.MkdirAll(projects, 0o700); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	line := `{"type":"assistant","timestamp":"2020-01-02T10:00:00Z","requestId":"req-one","message":{"id":"msg-one","model":"claude-test","usage":{"input_tokens":10,"output_tokens":20}}}`
	if errWrite := os.WriteFile(filepath.Join(projects, "one.jsonl"), []byte(line), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	configPath := filepath.Join(root, "config.yaml")
	raw := []byte("auth-dir: " + filepath.ToSlash(filepath.Join(root, "auths")) + "\nremote-management:\n  secret-key: example-plaintext-secret\n")
	if errWrite := os.WriteFile(configPath, raw, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	var stdout, stderr bytes.Buffer
	code := runUsageImport(context.Background(), []string{"--config", configPath, "--provider", "claude", "--dry-run"}, "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("import failed: %s", &stderr)
	}
	var result usagehistory.ImportResult
	if errJSON := json.Unmarshal(stdout.Bytes(), &result); errJSON != nil || !result.DryRun || result.Imported != 1 {
		t.Fatalf("invalid summary %s: %v", &stdout, errJSON)
	}
	after, errRead := os.ReadFile(configPath)
	if errRead != nil || !bytes.Equal(raw, after) {
		t.Fatalf("config was rewritten: %v", errRead)
	}
	if _, errStat := os.Stat(filepath.Join(root, "auths")); !os.IsNotExist(errStat) {
		t.Fatal("preview created the auth directory")
	}
}

func TestUsageImportMultipleHomesAndArchives(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	var args []string
	for i, dir := range []string{"sessions", "archived_sessions"} {
		home := filepath.Join(root, strings.Repeat("a", i+1))
		path := filepath.Join(home, dir)
		if errMkdir := os.MkdirAll(path, 0o700); errMkdir != nil {
			t.Fatal(errMkdir)
		}
		lines := `{"type":"session_meta","payload":{"id":"` + strings.Repeat("s", i+1) + `","source":"cli"}}
{"type":"turn_context","payload":{"model":"gpt-test"}}
{"type":"event_msg","timestamp":"2020-01-02T10:00:00Z","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":10,"output_tokens":20}}}}`
		if errWrite := os.WriteFile(filepath.Join(path, "one.jsonl"), []byte(lines), 0o600); errWrite != nil {
			t.Fatal(errWrite)
		}
		args = append(args, "--codex-home", home)
	}
	args = append(args, "--provider", "codex", "--database", filepath.Join(root, "history.db"))
	var stdout, stderr bytes.Buffer
	if code := runUsageImport(context.Background(), args, "missing.yaml", &stdout, &stderr); code != 0 {
		t.Fatalf("import failed: %s", &stderr)
	}
	var result usagehistory.ImportResult
	if errJSON := json.Unmarshal(stdout.Bytes(), &result); errJSON != nil || result.Imported != 2 || result.Files != 2 {
		t.Fatalf("multiple homes or archives were missed: %s / %v", &stdout, errJSON)
	}
}

func TestUsageImportRejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		{"--provider", "unknown"},
		{"--before", "yesterday"},
		{"--codex-home", ""},
		{"--provider", "claude", "--codex-home", "/somewhere"},
		{"unexpected"},
	} {
		var stdout, stderr bytes.Buffer
		if code := runUsageImport(context.Background(), args, "", &stdout, &stderr); code == 0 {
			t.Fatalf("invalid arguments were accepted: %v", args)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := runUsageImport(context.Background(), []string{"--help"}, "", &stdout, &stderr); code != 0 || !strings.Contains(stderr.String(), "dry-run") {
		t.Fatalf("help failed: %d %s", code, &stderr)
	}
}
