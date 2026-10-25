# Import historical Codex and Claude usage

The `usage-import` command reconstructs token usage from local Codex and Claude
Code transcripts and adds it to the SQLite history displayed at `/usage.html`.
It does not start the proxy server or contact either provider.

Preview the import with the same config and working directory as your server:

```bash
./cli-proxy-api usage-import --config config.yaml --dry-run
```

Run the same command without `--dry-run` to import:

```bash
./cli-proxy-api usage-import --config config.yaml
```

From a source checkout, replace `./cli-proxy-api` with `go run ./cmd/server`.
An existing server can keep running while you import. Refresh `/usage.html`
afterward and select a date range that includes the imported records.

## Transcript locations

By default the command reads both providers:

- Codex: `sessions/**/*.jsonl` and `archived_sessions/**/*.jsonl` under
  `CODEX_HOME`, falling back to `~/.codex`.
- Claude Code: `projects/**/*.jsonl` under `CLAUDE_CONFIG_DIR`, falling back to
  `~/.claude`. Nested subagent transcripts are included.

The command loads `.env` from the working directory. Override the homes with
`--codex-home` and `--claude-home`; repeat either flag to include multiple homes:

```bash
./cli-proxy-api usage-import --config config.yaml \
  --codex-home ~/.codex --codex-home ~/.codex-cliproxy \
  --claude-home ~/.claude --dry-run
```

Use `--provider codex` or `--provider claude` to select one provider.
Missing default directories are skipped; an explicitly selected home with no
transcript directories is an error. Symlinked source directories are resolved,
but nested symlinks are not followed.

Use `--database /absolute/path/usage-history.db` to select the database directly
instead of reading the server config. Otherwise the command honors
`observability.usage.history.path` and `auth-dir`, and refuses a config that
disables history. In containers, make the transcript directories available
inside the container and pass their container paths.

## Overlap and repeat imports

The exclusive import cutoff defaults to the first live usage bucket already in
the database. For a new database it is the minute the history store was first
opened. This boundary is persisted independently of imported records, so importing
older history never moves it backward. With an older database whose first live
records have been compacted, the boundary is the start of their hour.

Use `--before 2026-10-25T08:00:00Z` to choose an earlier exclusive cutoff. A later
value cannot extend imports into live history. The first live bucket is excluded
entirely because the database does not retain individual request timestamps.

Each imported event has a persistent deduplication key. Repeated imports, copied
transcripts, and sessions moved into Codex archives do not add the same events
again. Codex repeated counters and copied fork history are suppressed; Claude
repeated assistant content blocks are deduplicated by message/request identity.

Deduplication keys and token rollups commit together in batches. If an import
fails or is interrupted, rerun it: completed batches remain and uncommitted
batches are retried. Usage older than seven days is stored at hourly resolution.

## Results and limitations

The command prints a JSON summary with the effective `before` cutoff, files
scanned, imported events, duplicates, events outside the cutoff, malformed lines,
and imported token counters. With `--dry-run`, `imported` means events that would
be added. A dry run opens an existing database read-only, creates no database,
and does not rewrite the config. On failure the command exits nonzero and reports
any batches already committed.

Malformed usage records and transcript lines larger than 32 MiB are skipped and
counted. Incomplete trailing records can be recovered by rerunning after the CLI
finishes writing them. No conversation text or credentials are stored by the
importer; the database retains token rollups and hashed event identities.

Imported events use the internal account marker `imported:local`. Session files
cannot reliably identify the upstream account chosen by proxy load balancing.
All recorded local usage before the cutoff is eligible, including requests that
bypassed this proxy. Requests from other clients or machines require their saved
transcripts; missing token records cannot be reconstructed from conversation
text. Failures and token usage absent from transcripts cannot be recovered.

Dashboard costs use the current price table and are API-equivalent estimates,
not historical subscription charges. Codex service tiers absent from old
transcripts default to standard pricing; this importer does not read T3 Code's
database to recover those tiers. Claude thinking remains included in output
tokens when the transcript does not provide a separate breakdown.
