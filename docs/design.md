# CLI design decision

The Go CLI signs direct Lambda invocations using locally managed AWS credentials. The worker owns Athena access, query policy, metadata, and result storage. The caller needs Lambda invocation permission separately from the worker's company data permissions.

## Commands and ownership

`query --sql-file query.sql` submits, waits, and retrieves rows. `start`, `status`, and `results --wait` let callers recover by query ID without resubmitting SQL. `info` reports configured capabilities, `search` finds metadata, and `cancel` explicitly requests a stop. `download --out` retrieves a complete native file.

One executable package keeps the call path short:

- `main.go` coordinates arguments, identity, SDK configuration selection, streams, deadlines, and exit codes.
- `diagnostics.go` owns typed `Fault` errors and versioned JSONL event emission.
- `worker.go` owns typed wire data, AWS invocation, and external errors.
- `query.go` owns request IDs, validated states, polling, and consistent pagination.
- `output.go` owns streamed tables, JSON and CSV, private file publication, and HTTPS download rules.
- `export.go` owns durable progress, resume, and publication; platform lock files provide OS-managed exclusion.
- `usage.go` owns private local usage metadata and the embedded, versioned SQLite schema.
- `usage_identity.go` detects launcher/agent identity and reads bounded local transcript windows for model names and Codex effort.

The AWS SDK owns credential resolution and SigV4 signing. Keys never enter worker events. Ordered string-or-null cells preserve exact numbers, nulls, duplicate column names, and column order.

## Diagnostics and recovery

All stderr diagnostics are versioned JSON Lines, regardless of stdout format. Typed errors carry a stable local code, operation, sanitized message, and recovery action. Wrapped causes stay private for `errors.Is` and `errors.As`; raw SDK payloads, worker messages, URLs, and SQL are never serialized.

Events carry known safe request/query IDs, exit code, and output state. `partial_output` identifies incomplete streams or retained export progress; `published` identifies a published destination. An ambiguous start keeps its request ID and uses `retry_same_request`. A definite validation or authorization rejection can report ordinary worker recovery. The CLI does not persist SQL.

A valid status lookup returns terminal states successfully. Waiting for a failed or cancelled query exits 1. Context cancellation or deadline expiry exits 3 and leaves the Athena query running. `cancel` reports stop acceptance separately from observed completion, so callers continue reading status when necessary.

## Configured scope and bounded metadata

`info` returns explicit configuration with `basis=configured`. It makes zero IAM calls and discloses no IAM policies, role information, bucket paths, credentials, or environment dump. Configuration does not establish effective company-account access.

Metadata search matches literal names and comments case-insensitively, including partition columns. Each sandbox invocation reads at most one bounded Glue table page, or one exact table. Signed cursors bind normalized filters and scan state. Empty pages can continue; exhausting an invocation's budget does not mean the catalog search is complete.

Status quantities are optional decimal strings, so large counts remain exact. Missing means unknown, while zero is known. Sanitized failure categories and retryability accompany actionable guidance without exposing raw Athena text.

## Result identity and checkpoint agreement

The worker's opaque `result_id` binds a query and its authorized result revision. The sandbox uses S3 revision metadata and checks identity before and after reading each page. The client supplies `expected_result_id` after learning it. Changed identity fails closed. Older workers remain usable for existing commands, but checkpoint exports require identity.

The API is `export --query-id ID --out PATH --format json|csv` with optional `--checkpoint PATH`, followed by `export --resume PATH`. The default checkpoint is `PATH.checkpoint.json` beside the destination. Export retrieves all rows using 200-row pages. Resume accepts only profile and timeout overrides and never starts SQL. Stdout is a JSON receipt regardless of file format.

Exporter-owned byte checkpoints reuse the existing encoders without a second complete data copy. A checkpoint binds version/encoding, function and resolved region, query/result identity, destination, ordered columns, format, page size, durable byte offset, row count, hash, and next cursor. It contains no credentials or result URL.

Output is synced before atomic checkpoint replacement. Resume verifies committed bytes and identity, then truncates only an uncommitted suffix. Finishing, sealed, and publication state prevent duplicate JSON trailers or publication after interruption. OS-managed locks exclude concurrent writers, and publication never overwrites an existing destination. Expired cursors require replay and verification of the delivered logical prefix. Corruption or a changed result retains progress and fails closed.

Ordinary `results --all --out` uses a private temporary file and no-overwrite publication, but has no checkpoint. Native `download` preserves file bytes and native null conventions. Recovery requests a fresh URL and retries the whole file; it has no byte-range resume.

## Local usage recording

`run` records one completion attempt, including help, version, argument errors, and context cancellation. The recorder runs after command diagnostics and uses an independent context so cancellation can still produce a failure row. It stays silent when storage fails, preserving the CLI's machine output contract. There is no network path and no worker protocol change. SIGKILL, power loss, unwritable storage, and sustained contention can lose rows.

The [v2 schema](../cmd/athena-bridge/usage-schema.sql) initializes an empty/version-zero database. The [v1 migration](../cmd/athena-bridge/usage-v1-migration.sql) rebuilds labels/runs inside the same database and transaction, preserves IDs and sessions, and multiplies old timestamps by 1000 once. Old combined launcher labels such as `t3-code` move to `launcher_id`; their underlying agent becomes `unknown`. Old `human` rows gain launcher `terminal`. Existing agent-only labels stay agents with an unknown launcher. Historical model and effort remain NULL. A newer schema is left unchanged.

Migration, dictionary insertion, and the run row share one immediate transaction. Foreign keys remain enabled throughout table rebuilds. WAL and a 25 ms busy timeout bound lock contention. Identity lookup and SQL share a 100 ms context deadline; a 25 ms model lookup deadline reserves time for recording a NULL model on discovery failure. Filesystem access and SQLite close/checkpoint work also depend on the local filesystem. Avoid placing this database or session records on a network filesystem.

Each column retains a distinct part of intent:

- `started_at` costs six payload bytes for current UTC Unix milliseconds. `duration_ms` costs zero to four bytes for ordinary commands. The rowid orders ties within a millisecond by recording order.
- Agent, launcher, model, profile, database, command, error class, and CLI version use one `labels` dictionary with a kind enum. Agent retains kind 1; new kinds 7 and 8 identify launcher and model. Common IDs cost one or two payload bytes. NULL represents absent selections and successful error classes. Exit code costs zero payload bytes for 0/1, one byte for 2/3; a separate success boolean would be redundant.
- `effort` is NULL when unknown, or enum 1 low, 2 medium, 3 high, 4 xhigh, 5 max. It adds one serial-type header byte and zero or one payload byte. Transcript reasoning content is never read into this field.
- `session_id` references a deduplicated 8-byte hash. The ID usually costs one byte per run. Hashing excludes raw identifiers, including explicit session overrides, from storage.
- `format` is an integer enum and `flags` packs output-file, pretty, all-pages, wait, resume, SQL-file/stdin, continuation, and explicit-request-ID intent. It stores neither paths nor tokens.
- `sql_hash` costs eight payload bytes when SQL has been read, plus its serial-type header byte. It hashes case-folded, whitespace-collapsed text with SHA-256 truncated to 64 bits. `verb` is a one-byte enum recognized only at the first whitespace-delimited word. Comments and unfamiliar statements leave it NULL. This is approximate grouping, not semantic SQL equivalence.
- `query_hash` adds eight payload bytes when a query ID is known, connecting start/query/status/results/export/download/cancel calls. Request IDs are omitted to save another eight bytes per submission; the explicit-request-ID flag retains retry intent.
- `rows_written` and `pages_written` count complete page writes to stdout or the invocation's output stream, including export. They are zero before a tabular command writes a complete page and NULL for other commands. They exclude previously committed export rows and any replayed prefix. Publication can still fail afterward; the exit and error class record that outcome.

Typical live SQLite cells are approximately 53 bytes and approximately 73 bytes with populated SQL/query intent. Compared with v1, milliseconds add two payload bytes; launcher/model/effort add three header bytes and up to three payload bytes with small IDs. The schema comment explains the estimate. Page slack, fixed schema pages, and dictionaries must be reported separately from record payload. No per-run secondary indexes duplicate timestamps or sessions; local analysis scans `runs`.

Detection checks only named signals. The dimensions have independent precedence:

| Dimension | Sources in precedence order |
| --- | --- |
| Launcher | `ATHENA_BRIDGE_LAUNCHER`; T3 bundle identifier, `T3CODE_CLI_PATH`, or Codex T3 originator marker; GitHub Actions; GitLab CI; generic CI; terminal stdin/stdout; NULL |
| Agent | `ATHENA_BRIDGE_AGENT`; the tool-name prefix of `AI_AGENT` before `_`; Claude markers; Codex thread/session/CI markers; `CURSOR_AGENT`; human terminal caller outside CI; `unknown` |
| Model | `ATHENA_BRIDGE_MODEL`; `ANTHROPIC_MODEL` for Claude; current Claude/Codex session tail, then its opening records; NULL. No Cursor-specific model signal was verified. |
| Effort | `ATHENA_BRIDGE_EFFORT`; `CLAUDE_EFFORT` for Claude; Codex `turn_context` `payload.effort` from the record that supplied the model; NULL. No transcript reasoning or inferred harness settings. |

Claude session lookup uses `CLAUDE_CODE_SESSION_ID` before `CLAUDE_SESSION_ID`, under `CLAUDE_CONFIG_DIR` or `~/.claude`, at `projects/<cwd-slug>/<session>.jsonl`. Only assistant `message.model` is extracted. Codex uses `CODEX_THREAD_ID` before `CODEX_SESSION_ID`, under `CODEX_HOME` or `~/.codex`, at `sessions/**/rollout-*<id>*.jsonl`. Only `turn_context` or `session_meta` `payload.model` and `payload.effort` are extracted.

Codex UUIDv7 lookup checks the creation date in local time and neighboring UTC dates. Other IDs use a scan limited to four levels and 1024 directory entries, read in batches of at most 64. Session identifiers must be UUIDs before they can form a lookup path. The reader rejects non-regular files, reads at most the final 64 KiB, skips a leading partial line, and searches backward for the newest valid model name. A long tool-heavy turn can push the last model record out of that tail. Then the reader takes the first valid model in the opening 256 KiB. A model switched mid-session can therefore record the opening model. It never invokes another CLI or contacts a service. Models in neither window remain NULL.

Session grouping uses the matching agent's thread/session ID. Cursor can use `CURSOR_TRACE_ID` when supplied, but its stability was not verified here. CI jobs supply fallback grouping through GitHub run ID/attempt/job or GitLab job ID. No CI launcher becomes an inferred AI agent. `ATHENA_BRIDGE_SESSION` overrides grouping independently of model lookup. Installed SDK versions and terminal application names alone do not prove agent execution; process IDs are never invented as sessions.

The driver is `modernc.org/sqlite` v1.45.0, a pure-Go SQLite implementation compatible with the Go 1.24 minimum. It increases binary size and adds its pinned Go runtime dependencies, while preserving builds without cgo or a system SQLite installation. `go-isatty` handles the terminal fallback. These dependencies make offline cross-compilation possible after module download. No external SQLite process is required for recording.

## Verification boundary

`make verify` runs local tests with fake credentials and no AWS access. They exercise SDK signing, worker boundaries, exact values, metadata pagination, typed diagnostics, and safe file handling. The binary smoke check uses a temporary named AWS profile and a local worker.

The opt-in `scripts/sandbox-verify.py` checks query execution, metadata, configured info, search, statistics, cancellation, and structured failures. `--check-export` exercises fresh JSON and CSV exports and interrupted checkpoint resume through the binary. It records arguments, stdout, stderr, exit codes, source revision, and binary hash.

A live sandbox verifies the relevant service subset against synthetic data. It does not prove full company-account access or organization-specific controls. The selected function, region, caller profile, and worker data permissions remain deployment inputs.
