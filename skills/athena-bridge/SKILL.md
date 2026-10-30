---
name: athena-bridge
description: Query Athena through an existing Athena Bridge Lambda worker with the packaged CLI. Use for schema discovery, SQL queries, result retrieval, and checkpoint exports.
---

# Use Athena Bridge

Resolve the executable beside this file, `athena-bridge` or `athena-bridge.exe`, to an absolute path. Use `athena-bridge` from `PATH` if installed there. The download contains a ready-to-run binary. Run `--help` for command flags. Flags follow the command.

Use the configured AWS SDK credential chain and worker. `AWS_PROFILE`, `AWS_REGION`, and `ATHENA_BRIDGE_FUNCTION` select the profile, region, and Lambda function name or ARN. Never put credentials in command arguments, SQL, output, or worker requests. The caller needs Lambda invocation permission. The worker owns Athena, Glue, and result-file access.

## Discover and query

The examples assume the executable is on `PATH`:

```sh
athena-bridge info --format json
athena-bridge search --database analytics --term session --kind column --all --format json
athena-bridge tables --database analytics --all --format json
athena-bridge describe --database analytics --table events --all --format json
athena-bridge query --database analytics --sql-file query.sql --all --format json
```

Use metadata to confirm names and types before writing SQL. `info` reports configured capabilities, not effective access to every table. `search` searches metadata, not row values.

Use `--format json` and leave `--pretty` disabled for machine output. Stdout contains results. Stderr contains JSON Lines diagnostics. Capture them separately and check the exit code before consuming results:

- `0` means the command succeeded.
- `1` means execution or protocol failure.
- `2` means invalid arguments.
- `3` means client cancellation or timeout.

Cells are strings or JSON null. Preserve decimal and large-integer text, nulls, duplicate column names, and column order. Decode values using the ordered `columns` metadata. Empty text differs from null.

Use `--all` for a complete tabular result. Otherwise, check `complete` and follow the opaque `next_token` with `--next-token`. An empty page can have another token. A failed command can leave partial stdout.

## Retrieve and recover

```sh
athena-bridge start --database analytics --sql-file query.sql --format json
athena-bridge status --query-id QUERY_ID --format json
athena-bridge results --query-id QUERY_ID --wait --all --format json
athena-bridge export --query-id QUERY_ID --out results.json --format json --checkpoint results.checkpoint
athena-bridge export --resume results.checkpoint
athena-bridge download --query-id QUERY_ID --out native-results.csv
```

`status` exits 0 when the lookup succeeds, even for a failed query. Read its `state`. Waiting commands fail when the query fails or is cancelled.

After an uncertain submission, retry with the diagnostic `request_id`, identical SQL, and the same database using `--request-id`. A new ID submits a new query. A timeout leaves the query running. Use its `query_id` to retrieve it, or use `cancel` when cancellation is requested.

Output directories must exist. File commands never replace an existing destination. `export` saves resumable progress and returns a JSON receipt. `download` saves the native file unchanged and retries from the beginning when rerun. Neither command prints signed download URLs.

Usage metadata stays in local SQLite. Set `ATHENA_BRIDGE_USAGE_DB=off` to disable recording.

When a command fails, read the last `"type":"error"` diagnostic and act on its `recovery` value. `docs/troubleshooting.md` in the extracted download maps each `code`, `recovery`, and `worker_code` to the next step. Do not retry `retry_same_request` failures with a new request ID.

For worker setup, read `worker/README.md` in the extracted download. For protocol details, read `docs/lambda-contract.md`. Deploying the example worker is a separate operation from running queries.
