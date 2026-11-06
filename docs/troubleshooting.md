# Troubleshoot Athena Bridge

Use this guide when a command exits with a nonzero code. The examples assume `athena-bridge` is on `PATH`. From an extracted download, run `./athena-bridge`, or `.\athena-bridge.exe` on Windows.

## Capture the failure

Keep standard output and standard error separate, and record the exit code:

```sh
athena-bridge info --format json > info.json 2> info.err; echo "exit $?"
```

In PowerShell:

```powershell
.\athena-bridge.exe info --format json > info.json 2> info.err; "exit $LASTEXITCODE"
```

Standard error contains one JSON object per line. The last line with `"type":"error"` describes the failure. Read these fields first:

- `code` names the failure category. The tables below list each code.
- `recovery` names the next action.
- `exit_code` is `1` for execution or protocol failures, `2` for invalid arguments, and `3` for client cancellation or timeout.
- `request_id` and `query_id` identify a submitted query. Keep them for recovery.
- `worker_code` is the worker's own error code. The CLI never prints the worker's message, so read the worker's logs for details.
- `partial_output` is `true` when standard output or an export holds incomplete results.
- `published` is `true` when the destination file exists and is complete.

Add `--pretty` to read the same diagnostics as text. Keep the JSON form for scripts and agents.

## Act on the recovery value

| `recovery` | Next action |
| --- | --- |
| `fix_arguments` | Correct the flags. Run the command with `--help`. |
| `check_configuration` | Check the profile, region, credentials, and function name. See [check configuration](#check-configuration). |
| `retry_operation` | Retry the same command. For throttling, wait first. |
| `retry_same_request` | Rerun `start` or `query` with the diagnostic's `--request-id`, the identical SQL, and the same database. A new request ID submits a second query. |
| `wait_query` | The query is still running. Run `results --query-id QUERY_ID --wait`. |
| `inspect_query` | Run `status --query-id QUERY_ID --format json` and read `state`, `failure`, and `reason`. |
| `check_output` | Check the destination directory and file. See [local files](#local-files). |
| `check_worker` | The worker failed or broke the contract. Read the worker's CloudWatch logs. |
| `none` | Retrying will not help. A worker policy refused the request, or the command already finished. |

## Look up the code

| `code` | Usual cause | Check |
| --- | --- | --- |
| `INVALID_ARGUMENT` | Unknown command, bad flag, or missing value. | `athena-bridge COMMAND --help`. Flags go after the command. |
| `CONFIGURATION` | No region, unreadable AWS configuration, wrong function name, or function in another region. | `AWS_REGION`, `--region`, `ATHENA_BRIDGE_FUNCTION`, and `--function`. |
| `AUTHENTICATION` | Missing, expired, or invalid credentials. | `aws sts get-caller-identity --profile PROFILE`. For SSO, run `aws sso login --profile PROFILE`. |
| `AUTHORIZATION` | The caller lacks `lambda:InvokeFunction` on the function or alias. | Ask the worker owner for invocation access. The worker's own role does not grant it. |
| `THROTTLED` | Lambda concurrency or request limits. | Retry later, or raise the function's concurrency. |
| `TRANSPORT` | Network failure, or a failed result download. | Connectivity and proxy settings. For `download`, rerun it to request a new link. |
| `WORKER_FUNCTION` | The Lambda raised an unhandled error or timed out. | The function's CloudWatch logs. |
| `WORKER_REJECTED` | The worker returned an error. | `worker_code`. See [worker codes](#worker-codes). |
| `NOT_READY` | Results were requested before the query finished. | `results --query-id QUERY_ID --wait`. |
| `PROTOCOL` | The worker response does not follow [the version-one contract](lambda-contract.md). | The worker version and its logs. |
| `QUERY_FAILED` | Athena reported `FAILED`. | `status --query-id QUERY_ID --format json`. |
| `QUERY_CANCELLED` | Athena reported `CANCELLED`. | `status`, then whether someone requested cancellation. |
| `LOCAL_IO` | A local file could not be read, written, or published. | See [local files](#local-files). |
| `CANCELLED` | Ctrl-C or another signal stopped the CLI. | The Athena query keeps running. Use `query_id` to retrieve or cancel it. |
| `DEADLINE` | The `--timeout` limit expired, two minutes by default. | The Athena query keeps running. Retrieve it with `results --query-id QUERY_ID --wait --timeout 10m`. |
| `RESULT_CHANGED` | The worker's result identity changed between pages, or an export replay did not match delivered rows. | Rerun the query to get a stable result. |
| `OUTPUT_LIMIT` | A download exceeded `--max-bytes`, or the worker's column metadata and cursor made a checkpoint larger than 8 MiB. | Raise `--max-bytes`. For a checkpoint, use `download` or ask the worker owner to shorten cursors. |
| `INTERNAL` | An unexpected CLI failure. | [Report a bug](#report-a-bug). |

### Worker codes

`worker_code` comes from the worker, so a custom worker can return other codes. The example worker returns these:

| `worker_code` | Meaning |
| --- | --- |
| `NOT_ALLOWED` | The worker's policy refused the database, table, or statement. Retrying will not help. |
| `INVALID_REQUEST` | The worker rejected the request arguments, such as a missing database or an unsupported statement. |
| `INVALID_CURSOR` | A pagination token is invalid or expired. Restart from the first page, or use `export` for resumable retrieval. |
| `RESULT_IDENTITY_UNAVAILABLE` | The worker cannot identify a stable result, which `export` requires. |
| `ROW_TOO_LARGE` | One row exceeds the worker's response size. Select fewer or narrower columns, or use `download`. |
| `INVALID_RESULT` | The worker received an unexpected result from Athena. Read the worker's logs. |
| `QUERY_FAILED` | `results` or `download` asked for a query that failed or was cancelled. Run `status --query-id QUERY_ID --format json` to see why. |
| `DOWNLOAD_UNAVAILABLE` | The query has no result file the worker can serve. Use `results --all --out PATH` or `export` instead of `download`. |
| `WORKER_ERROR` | The worker hit an unexpected error. Read the worker's logs. |
| AWS error names | The example worker passes through AWS codes. `InvalidRequestException` means Athena rejected the SQL or a request ID reused with different SQL. `EntityNotFoundException` means the table does not exist. `AccessDeniedException` means the worker's execution role lacks access. |

## Check configuration

Run these checks in order. Each one depends on the previous one.

1. `athena-bridge version` prints the version. If it fails on macOS, see [the download instructions](https://github.com/DanielZamb/athena-bridge#use-a-binary-download) about quarantine.
2. `aws sts get-caller-identity` succeeds with the same profile. This confirms the credentials. It needs the AWS CLI, which Athena Bridge itself does not.
3. `athena-bridge info --format json` succeeds. This confirms the region, function name, invocation access, and a compatible worker.
4. `athena-bridge query --sql 'SELECT 1' --all --format json` succeeds. This confirms the worker can run Athena queries.

At step 3, `CONFIGURATION`, `AUTHENTICATION`, and `AUTHORIZATION` point to the caller's setup. `WORKER_REJECTED`, `WORKER_FUNCTION`, and `PROTOCOL` point to the worker. A failure at step 4 after step 3 succeeds is usually on the worker side: its execution role, workgroup, or result bucket.

## Recover a query

A timeout, Ctrl-C, or lost connection does not stop the Athena query. Use the `query_id` from standard error:

```sh
athena-bridge status --query-id QUERY_ID --format json
athena-bridge results --query-id QUERY_ID --wait --all --format json
athena-bridge cancel --query-id QUERY_ID --format json
```

If a failure arrives before any `query_id` and its `recovery` is `retry_same_request`, the query may have started. The `START_REQUEST` notice before it holds the `request_id`. Rerun the same command with `--request-id REQUEST_ID`, the identical SQL, and the same database. The worker returns the existing query instead of starting another. Any other `recovery` value before a `query_id` means no query started. Follow that value instead.

When `partial_output` is `true`, standard output is incomplete. Discard it, or continue from the last `next_token` with `--next-token`.

## Local files

`LOCAL_IO` failures mean one of these:

- The destination directory does not exist. Create it first.
- The destination file already exists. The CLI never replaces it. Choose another path or remove the old file.
- The filesystem does not support hard links, which publication requires. Write to a local disk.
- The disk is full, or the directory is not writable.
- A checkpoint is unreadable, from another version, or locked by a running export. Wait for the other export to finish.

A failed `export` keeps its checkpoint. Resume it with `athena-bridge export --resume CHECKPOINT`. A `CONFIGURATION` failure on resume means the AWS configuration could not load, for example a missing profile or region, or the configured function or region differs from the one stored in the checkpoint.

## Report a bug

Open an issue at <https://github.com/DanielZamb/athena-bridge/issues/new/choose>. Include:

- The output of `athena-bridge version`, plus your operating system and architecture.
- The command with its flags. Replace SQL, database names, and table names with placeholders if they are private.
- The full standard error output and the exit code.
- Whether `info --format json` succeeds.
- Whether you use the example worker or your own.

Remove AWS keys, session tokens, account IDs, signed URLs, query results, and private SQL before posting. Report security issues privately as described in [SECURITY.md](https://github.com/DanielZamb/athena-bridge/blob/main/SECURITY.md).
