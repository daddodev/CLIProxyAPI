package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagehistory"
)

// DoUsageImport runs the local historical usage importer without starting the server.
func DoUsageImport(ctx context.Context, args []string, defaultConfig string) int {
	return runUsageImport(ctx, args, defaultConfig, os.Stdout, os.Stderr)
}

func runUsageImport(ctx context.Context, args []string, defaultConfig string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("usage-import", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if defaultConfig == "" {
		defaultConfig = "config.yaml"
	}
	configPath := flags.String("config", defaultConfig, "Server config used to locate the history database")
	database := flags.String("database", "", "History database path (overrides --config)")
	provider := flags.String("provider", "both", "Import codex, claude, or both")
	dryRun := flags.Bool("dry-run", false, "Preview the import without changing the database or config")
	beforeRaw := flags.String("before", "", "Earlier exclusive RFC3339 cutoff (cannot extend into live history)")
	var codexHomes, claudeHomes []string
	appendHome := func(homes *[]string) func(string) error {
		return func(value string) error {
			if strings.TrimSpace(value) == "" {
				return errors.New("home directory must not be empty")
			}
			*homes = append(*homes, value)
			return nil
		}
	}
	flags.Func("codex-home", "Codex home to read; repeat for multiple homes (default CODEX_HOME or ~/.codex)", appendHome(&codexHomes))
	flags.Func("claude-home", "Claude home to read; repeat for multiple homes (default CLAUDE_CONFIG_DIR or ~/.claude)", appendHome(&claudeHomes))
	if errParse := flags.Parse(args); errParse != nil {
		if errors.Is(errParse, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	fail := func(err error) int {
		_, _ = fmt.Fprintf(stderr, "Usage import failed: %v\n", err)
		return 1
	}
	if flags.NArg() != 0 {
		return fail(errors.New("unexpected positional arguments"))
	}
	if *provider != "codex" && *provider != "claude" && *provider != "both" {
		return fail(errors.New("--provider must be codex, claude, or both"))
	}
	if (*provider == "codex" && len(claudeHomes) > 0) || (*provider == "claude" && len(codexHomes) > 0) {
		return fail(errors.New("home flags must match --provider"))
	}
	var before time.Time
	if *beforeRaw != "" {
		var errBefore error
		before, errBefore = time.Parse(time.RFC3339, *beforeRaw)
		if errBefore != nil {
			return fail(fmt.Errorf("--before must be an RFC3339 timestamp: %w", errBefore))
		}
	}
	_ = godotenv.Load()
	databasePath := *database
	if databasePath == "" {
		raw, errRead := os.ReadFile(*configPath)
		if errRead != nil {
			return fail(fmt.Errorf("read config: %w", errRead))
		}
		cfg, errParse := config.ParseConfigBytes(raw)
		if errParse != nil {
			return fail(fmt.Errorf("parse config: %w", errParse))
		}
		if cfg.UsageHistory.Disabled {
			return fail(errors.New("usage history is disabled in the server config"))
		}
		var errPath error
		databasePath, errPath = usagehistory.ResolvePath(cfg)
		if errPath != nil {
			return fail(errPath)
		}
	}
	homeDirectory, errHome := os.UserHomeDir()
	if errHome != nil {
		return fail(errHome)
	}
	databasePath = expandUsageHome(databasePath, homeDirectory)
	var sources []usagehistory.ImportSource
	for _, candidate := range []struct {
		provider string
		homes    []string
		env      string
		fallback string
		dirs     []string
	}{
		{"codex", codexHomes, "CODEX_HOME", ".codex", []string{"sessions", "archived_sessions"}},
		{"claude", claudeHomes, "CLAUDE_CONFIG_DIR", ".claude", []string{"projects"}},
	} {
		if *provider != "both" && *provider != candidate.provider {
			continue
		}
		explicit := len(candidate.homes) > 0
		if !explicit {
			home := strings.TrimSpace(os.Getenv(candidate.env))
			if home == "" {
				home = filepath.Join(homeDirectory, candidate.fallback)
			}
			candidate.homes = []string{home}
		}
		for _, home := range candidate.homes {
			found := false
			for _, dir := range candidate.dirs {
				path := filepath.Join(expandUsageHome(home, homeDirectory), dir)
				info, errStat := os.Stat(path)
				if errors.Is(errStat, os.ErrNotExist) {
					continue
				}
				if errStat != nil {
					return fail(errStat)
				}
				if !info.IsDir() {
					return fail(fmt.Errorf("transcript path is not a directory: %s", path))
				}
				sources = append(sources, usagehistory.ImportSource{Provider: candidate.provider, Path: path})
				found = true
			}
			if !found && explicit {
				return fail(fmt.Errorf("no %s transcript directories found under %s", candidate.provider, home))
			}
		}
	}
	if len(sources) == 0 {
		return fail(errors.New("no transcript directories found; specify --codex-home or --claude-home"))
	}
	result, errImport := usagehistory.Import(ctx, databasePath, usagehistory.ImportOptions{Sources: sources, Before: before, DryRun: *dryRun})
	// On failure, earlier batches may already be committed. The summary makes that visible.
	if errEncode := json.NewEncoder(stdout).Encode(result); errEncode != nil {
		return fail(fmt.Errorf("write import summary: %w", errEncode))
	}
	if errImport != nil {
		return fail(errImport)
	}
	return 0
}

func expandUsageHome(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		return filepath.Join(home, path[2:])
	}
	return path
}
