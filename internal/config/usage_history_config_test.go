package config

import "testing"

func TestUsageHistoryConfigV8Path(t *testing.T) {
	const digest = "aa6dcb22065270ba050988c38044bd9ff024c7a941ba98e3afc25dcf6811949e"
	cfg, errParse := ParseConfigBytes([]byte("observability:\n  usage:\n    history:\n      disabled: true\n      path: /tmp/usage.db\n      projects:\n        " + digest + ": docs\n"))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if !cfg.UsageHistory.Disabled || cfg.UsageHistory.Path != "/tmp/usage.db" || cfg.UsageHistory.Projects[digest] != "docs" {
		t.Fatalf("usage history = %+v", cfg.UsageHistory)
	}
}
