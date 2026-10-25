package config

import "testing"

func TestUsageHistoryConfigV8Path(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte("observability:\n  usage:\n    history:\n      disabled: true\n      path: /tmp/usage.db\n"))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if !cfg.UsageHistory.Disabled || cfg.UsageHistory.Path != "/tmp/usage.db" {
		t.Fatalf("usage history = %+v", cfg.UsageHistory)
	}
}
