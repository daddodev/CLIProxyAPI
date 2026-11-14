package main

import (
	"strings"
	"testing"
)

func TestUsageImportSubcommandExitStatus(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		wantStatus int
		wantStderr string
	}{
		{"help", []string{"usage-import", "--help"}, 0, "Usage of usage-import"},
		{"invalid provider", []string{"usage-import", "--provider", "invalid"}, 1, "--provider must be codex, claude, or both"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command, stdout, stderr := newServerExitCommand(t, t.TempDir(), tc.args...)
			errRun := command.Run()
			if command.ProcessState == nil {
				t.Fatalf("subcommand did not start: %v", errRun)
			}
			if got := command.ProcessState.ExitCode(); got != tc.wantStatus {
				t.Fatalf("exit status = %d, want %d: %v\nstdout:\n%s\nstderr:\n%s", got, tc.wantStatus, errRun, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.wantStderr) || stdout.Len() != 0 {
				t.Fatalf("unexpected subcommand output\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
			}
		})
	}
}
