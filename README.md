# Athena Bridge

A Go CLI that signs requests to your AWS Lambda worker and displays Athena schemas and query results. It supports paginated datasets and complete file downloads. Agents can call the same binary with `--format json`.

The CLI connects to a user-managed Lambda worker. Adapt the [example worker](examples/worker.md) or implement the [version-one worker contract](docs/lambda-contract.md), then supply its function name or ARN. An optional disposable AWS sandbox in this repository provides a test worker and synthetic data.

## Use a binary download

Download the archive for your platform from [GitHub Releases](https://github.com/DanielZamb/athena-bridge/releases/latest). Builds exist for macOS, Linux, and Windows on `amd64` and `arm64`.

The compact distribution contains a stripped executable, an [agent skill](skills/athena-bridge/SKILL.md), setup instructions, a [troubleshooting guide](docs/troubleshooting.md), the example worker, its contract, and license notices. It excludes tests, databases, sandbox tooling, and development scripts. Running the CLI requires no Go, Python, or system SQLite installation. See the [download setup instructions](docs/distribution.md).

Download the archive's `.sha256` file too, and check the archive before extracting it:

```sh
shasum -a 256 -c athena-bridge_0.1.0_darwin_arm64.tar.gz.sha256
tar -xzf athena-bridge_0.1.0_darwin_arm64.tar.gz
```

On Linux, use `sha256sum -c` instead of `shasum -a 256 -c`. In PowerShell, compare `(Get-FileHash athena-bridge_0.1.0_windows_amd64.zip).Hash` with the first field of the `.sha256` file, ignoring case.

Each archive also has a GitHub build provenance attestation. With the [GitHub CLI](https://cli.github.com/), confirm that this repository's release workflow built it:

```sh
gh attestation verify athena-bridge_0.1.0_darwin_arm64.tar.gz --repo DanielZamb/athena-bridge
```

The macOS executables are not signed or notarized. A browser download marks the archive as quarantined, and macOS then refuses to run the extracted executable. After checking the checksum, remove the mark from the extracted directory:

```sh
xattr -dr com.apple.quarantine athena-bridge
```

Archives downloaded with `curl` or `gh release download` are not quarantined.

From a source checkout, run `make release` to build the host package in `dist/`. See [binary packaging](docs/publishing.md#package-a-binary-download) for other platforms.

## Build

Install [Go 1.24 or later](https://go.dev/doc/install). From the repository root, build and check the CLI:

```sh
go build -trimpath -o bin/ ./cmd/athena-bridge
./bin/athena-bridge --help
./bin/athena-bridge version
```

On Windows, use PowerShell:

```powershell
go build -trimpath -o bin/ ./cmd/athena-bridge
.\bin\athena-bridge.exe --help
.\bin\athena-bridge.exe version
```

The first build downloads the Go dependencies listed in `go.mod` and verified by `go.sum`. Building and displaying help require no AWS account, credentials, worker, Python, or C compiler. The CLI builds on macOS, Linux, and Windows. `make build` runs the same build with CGO disabled.

To install the latest release without cloning:

```sh
go install github.com/DanielZamb/athena-bridge/cmd/athena-bridge@latest
```

To install from this checkout:

```sh
go install -trimpath ./cmd/athena-bridge
```

Add the directory printed by `go env GOBIN` to your `PATH`. If that command prints nothing, use the `bin` directory under `go env GOPATH`. With Make installed, `make install` does the same installation with CGO disabled.

To run queries, configure an existing Lambda worker and AWS authentication below. The CLI does not deploy a worker. Contributor checks and cross-compilation commands are in [CONTRIBUTING.md](CONTRIBUTING.md).

## Configure authentication

The AWS SDK for Go v2 signs direct Lambda `Invoke` requests with SigV4. Configure your persistent credentials using an AWS shared profile outside this repository:

```sh
aws configure --profile athena-bridge
export AWS_PROFILE=athena-bridge
export AWS_REGION=us-east-1
export ATHENA_BRIDGE_FUNCTION=your-worker-name
```

Enter real keys only into your credential manager or the AWS CLI prompt. The application has no access-key flags or credential files of its own. The SDK also supports environment credentials, session tokens, SSO, and role profiles. `--profile` and `--region` override profile selection and region for one command. Flags go after the command.

Without an explicit profile, the SDK uses its normal credential chain, including environment credentials before shared files. The caller needs `lambda:InvokeFunction` for the selected function or alias, separately from the worker's company data permissions. The Lambda's execution role owns Athena, Glue, and S3 permissions. Giving that role access does not grant the caller invocation access. [AWS SDK configuration](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-gosdk.html), [Lambda invocation permissions](https://docs.aws.amazon.com/lambda/latest/api/API_Invoke.html).

PowerShell configuration uses the same names:

```powershell
$env:AWS_PROFILE = 'athena-bridge'
$env:AWS_REGION = 'us-east-1'
$env:ATHENA_BRIDGE_FUNCTION = 'your-worker-name'
```

## Explore and query

```sh
./bin/athena-bridge info --format json
./bin/athena-bridge search --database analytics --term session --kind column --all --format json
./bin/athena-bridge tables --database analytics --format json
./bin/athena-bridge describe --database analytics --table events
./bin/athena-bridge query --database analytics --sql-file examples/query.sql
./bin/athena-bridge query --database analytics --sql 'SELECT 1' --format json
```

`info` reports explicit worker configuration with `basis: "configured"`: worker identity, databases, catalog, workgroup, supported actions, features, and limits. It makes zero IAM calls and discloses no IAM policy, role information, bucket paths, credentials, or environment dump. It does not prove effective access to company data.

`search` matches literal text in table and column names and descriptions, case-insensitively. `--kind` accepts `all`, `table`, or `column`; `--table events` restricts the search to one exact table. The sandbox reads at most one bounded Glue table page per invocation, or uses `GetTable` for an exact table. An empty result page can still have a continuation token. Use `--all` to follow every page; a single empty page does not establish that no matches exist. This searches metadata, not row values.

`query` starts a query, polls until it finishes, and retrieves results. Each Lambda invocation returns promptly. The default overall command timeout is two minutes. Use `--timeout 10m` for a longer query. `--poll-interval` defaults to one second.

The default page size is 200 rows, with a maximum of 1000. It is an upper bound; a worker may return fewer rows. Add `--all` to retrieve every page, or resume from an opaque token with `--next-token`. Follow tokens rather than calculating row offsets. The CLI checks repeated pagination tokens, query identity, row widths, and consistent column metadata. Output streams page by page with bounded memory.

The worker owns query policy, allowed databases, workgroups, scan limits, and result retention. Client-side flags are not authorization controls.

### Read results in the terminal

Add `--pretty` for bordered tables, readable capability and status records, row counts, and human-readable diagnostics:

```sh
./bin/athena-bridge info --pretty
./bin/athena-bridge tables --database analytics --all --pretty
./bin/athena-bridge query --database analytics --sql 'SELECT 1 AS connection_test' --pretty
```

`--pretty` overrides a valid `--format` for console output. Wide values, Unicode text, and wide schemas use expanded rows without truncating values. SQL null displays as `NULL`; empty text displays as `""`. Terminal control characters are escaped. Each page renders independently, followed by a total row count and completeness indicator after retrieval succeeds. Long expanded values use the terminal's natural line wrapping. The renderer uses no color codes or terminal detection.

With `--out`, the file keeps its selected encoding and diagnostics become readable. `export --format csv --pretty` keeps exact CSV bytes and displays a readable receipt. `export --resume PATH --pretty` resumes the same checkpoint with readable output. Human continuation notices quote the complete token as a Go string literal. Keep the default JSONL diagnostics and `--format json` when another program consumes output. Use `--pretty=false` to disable pretty mode explicitly.

## Resume work and export datasets

```sh
./bin/athena-bridge start --database analytics --sql-file examples/query.sql --format json
./bin/athena-bridge status --query-id QUERY_ID --format json
./bin/athena-bridge results --query-id QUERY_ID --wait --all --format json

mkdir -p data
./bin/athena-bridge results --query-id QUERY_ID --all --page-size 1000 --format csv --out data/results.csv
./bin/athena-bridge download --query-id QUERY_ID --out data/native-results.csv
```

`results` retrieves an existing query without starting a new query. `--wait` waits for completion first. `status` reports the current state and exits successfully when the status lookup succeeds, even when the reported query state is `FAILED`. `query` and `results --wait` fail if the query fails or is cancelled.

`status` can include `statistics` with exact decimal-string byte counts and millisecond durations. Missing quantities are unknown; `"0"` is a known zero. `result_reused` is an optional boolean. A failed query can include sanitized `failure` categories and retryability, plus recovery guidance in `reason`. See the [status fields](docs/lambda-contract.md#read-query-state).

`cancel --query-id QUERY_ID --format json` authorizes the query, requests a stop if it is still active, and returns the observed status. `cancellation_requested: true` means the stop request was accepted; the state can still be `QUEUED` or `RUNNING`. Read `status` until a terminal state appears. A query that already finished returns `cancellation_requested: false`. A successful cancellation request exits 0 even if the observed state is `FAILED` or `CANCELLED`.

`download` requests a fresh expiring HTTPS URL from the worker and saves the returned file unchanged. It supports CSV, Parquet, JSON, and JSONL when the worker provides those formats. The default download ceiling is 1 GiB. Change it with `--max-bytes`. Redirects are rejected, AWS credentials are not forwarded, and signed URLs are never printed by the CLI's download path. After an expired link or interrupted transfer, rerun `download` to request a new link and retry the whole file. It has no byte-range resume. [S3 expiring links](https://docs.aws.amazon.com/AmazonS3/latest/userguide/using-presigned-url.html).

`results --out` and `download` create private temporary sibling files and publish without replacing an existing destination. They remove the temporary file on failure. The filesystem must support hard links for atomic publication. Destination directories must already exist. A timeout or Ctrl-C leaves the Athena query running. Use the diagnostic `query_id` on standard error to retrieve it later, or explicitly request cancellation.

### Resume a checkpoint export

Use `export` for durable progress. Ordinary `results --out` and native `download` do not create resumable checkpoints.

```sh
./bin/athena-bridge export --query-id QUERY_ID --out data/results.json --format json \
  --checkpoint data/results.checkpoint
./bin/athena-bridge export --resume data/results.checkpoint
```

`export` always retrieves every row and accepts `json` or `csv`. `--checkpoint` selects a checkpoint path; the default is `PATH.checkpoint.json` beside the destination. Export uses 200-row pages. Resume accepts `--profile`, `--timeout`, and `--pretty` overrides, reads the stored query, destination, format, page size, function, and resolved region, and never submits SQL. Both formats return a JSON receipt on stdout by default, with query/result identity, paths, format, rows, bytes, SHA-256, and publication state. A checkpoint also binds the ordered columns, opaque result identity, durable byte offset, row count, hash, and next cursor. It contains no result URL or credentials.

Resume verifies the committed bytes and result identity before appending, and truncates only an uncommitted suffix. Output is synced before replacing the checkpoint. Finishing, sealing, and publication state make JSON trailers and publication repeatable after interruption. OS-managed locks prevent concurrent writers; publication never overwrites a destination. An expired cursor requires replay and verification of the delivered logical prefix. Corruption or a changed result fails closed and retains progress. Workers without result identity can serve existing commands, but cannot support checkpoint exports.

## Retry an uncertain start

The CLI prints a request ID on standard error before it submits a query. If the call fails without confirming a query ID, rerun the same command with the same SQL, database, and `--request-id`:

```sh
./bin/athena-bridge start --database analytics --sql-file examples/query.sql \
  --request-id 0123456789abcdef0123456789abcdef --format json
```

The worker must pass that ID to Athena's `ClientRequestToken` and keep the start parameters identical. It must reject an ID reused for different parameters. This is required for safe SDK retries and for recovery after a lost response. A new ID starts a new query. The CLI does not retain SQL or receipts on disk. [Athena idempotency](https://docs.aws.amazon.com/athena/latest/APIReference/API_StartQueryExecution.html).

## Usage log

Every invocation attempts to append local metadata to SQLite. The default path is `$XDG_STATE_HOME/athena-bridge/usage.db`, or `~/.local/state/athena-bridge/usage.db` on Linux, `~/Library/Application Support/athena-bridge/usage.db` on macOS, and `%AppData%\athena-bridge\usage.db` on Windows. `ATHENA_BRIDGE_USAGE_DB` overrides the path; `ATHENA_BRIDGE_USAGE_DB=off` disables logging. New directories use mode 0700 and the database uses 0600 on systems with POSIX permissions.

Logging is silent and best effort. WAL permits concurrent readers and writers; a busy lock waits at most 25 ms. Identity lookup and SQL share a 100 ms deadline, with at most 25 ms assigned to model discovery. Logging failures preserve stdout, stderr, and exit codes. A killed process may leave no row.

The version-two [STRICT schema](cmd/athena-bridge/usage-schema.sql) stores UTC Unix milliseconds, duration in milliseconds, separate dictionary IDs for launcher/agent/model and profile/database/command/error/version, optional reasoning effort, exit code, format, option bits, and counts of complete pages and rows written. Existing v1 timestamps migrate in place from seconds to milliseconds. SQL, session IDs, and query IDs contribute only 8-byte SHA-256 fingerprints. SQL fingerprinting folds case and whitespace, including inside literals; it groups approximate intent without parsing SQL. Counts describe this invocation; resumed exports exclude previously written rows. Database and profile names describe explicit selections, not inferred worker defaults or credential sources.

`ATHENA_BRIDGE_LAUNCHER`, `ATHENA_BRIDGE_AGENT`, and `ATHENA_BRIDGE_MODEL` override each dimension independently. T3 launch signals preserve the underlying agent. `AI_AGENT` takes precedence over agent-specific markers. Without an agent signal, terminal stdin/stdout indicate `human`; other callers are `unknown`. `ATHENA_BRIDGE_SESSION` supplies a grouping ID when the agent has none. `ATHENA_BRIDGE_EFFORT` overrides the optional effort enum; Claude's `CLAUDE_EFFORT` also supplies it.

Models come from the explicit override, Claude's `ANTHROPIC_MODEL`, or bounded reads of the current Claude/Codex session file: the last 64 KiB, then the first 256 KiB. Codex effort comes from the same record. Only a validated model name leaves that parser. Missing or unreadable records leave model NULL; Cursor models require the explicit override. No SQL text, credentials, results, continuation tokens, file paths, or other transcript content enter the log. See [sources, precedence, and migration details](docs/design.md#local-usage-recording).

Use `sqlite3 PATH` to read the database. Runs per agent per UTC day:

```sql
SELECT date(r.started_at / 1000.0, 'unixepoch') AS day, a.value AS agent, count(*) AS runs
FROM runs r JOIN labels a ON a.id = r.agent_id
GROUP BY day, r.agent_id ORDER BY day, agent;
```

Find sessions with at least two adjacent invocations in the local log:

```sql
WITH numbered AS (
  SELECT agent_id, session_id, started_at,
    row_number() OVER (ORDER BY started_at, id) -
    row_number() OVER (PARTITION BY agent_id, session_id ORDER BY started_at, id) AS streak
  FROM runs
)
SELECT a.value AS agent, session_id, count(*) AS runs,
  strftime('%Y-%m-%dT%H:%M:%fZ', min(started_at) / 1000.0, 'unixepoch') AS first_run,
  strftime('%Y-%m-%dT%H:%M:%fZ', max(started_at) / 1000.0, 'unixepoch') AS last_run
FROM numbered n JOIN labels a ON a.id = n.agent_id
WHERE session_id IS NOT NULL
GROUP BY agent_id, session_id, streak HAVING count(*) >= 2
ORDER BY first_run;
```

## Output formats

Standard output contains results or requested help. Standard-error diagnostics are versioned JSON Lines by default, independent of `--format`. Explicit `--pretty` selects human-readable diagnostics and console output. Exit codes are `0` for success, `1` for execution or protocol failure, `2` for invalid arguments, and `3` for client-side cancellation or deadline expiry. An Athena query in `FAILED` or `CANCELLED` state makes `query` and `results --wait` exit with code 1.

Each diagnostic has `version`, `type`, stable local `code`, `operation`, sanitized `message`, `exit_code`, `recovery`, `partial_output`, and `published`. Known safe `request_id` and `query_id` values accompany notices and failures. Worker rejections also carry `worker_code`; pagination notices can carry `next_token`. For example:

```json
{"version":1,"type":"error","code":"QUERY_FAILED","operation":"wait_query","message":"query failed; inspect its status","query_id":"example-query","exit_code":1,"recovery":"inspect_query","partial_output":false,"published":false}
```

See [Troubleshoot Athena Bridge](docs/troubleshooting.md) for the next step after each code. Local codes distinguish arguments, configuration, authentication, authorization, throttling, transport, worker rejection/runtime, results not ready, protocol, query failure/cancellation, local I/O, context cancellation/deadline, output limit, result change, and internal failures. Recovery values are `none`, `fix_arguments`, `check_configuration`, `retry_operation`, `retry_same_request`, `inspect_query`, `wait_query`, `check_output`, and `check_worker`. Worker `NOT_READY` uses local code `NOT_READY` with `wait_query`; worker `RESULT_CHANGED` uses `RESULT_CHANGED` with `inspect_query`; worker `NOT_ALLOWED` uses `WORKER_REJECTED` with `none`. An uncertain submission uses `retry_same_request`; retain the request ID and identical SQL. `partial_output` reports incomplete streamed output or retained export progress, and `published` reports whether the destination was published. Raw SDK errors, worker messages, SQL, and signed URLs are not serialized into diagnostics.

JSON output for tabular commands is one object:

```json
{
  "query_id": "example-query",
  "columns": [{"name": "id", "type": "bigint"}],
  "rows": [["9007199254740993"], [null]],
  "next_token": null,
  "complete": true
}
```

Cells are strings or JSON null, preserving Athena decimals, large integers, timestamps, duplicate column names, and column order. Convert values using column types in your analysis tool. For example, decode a non-null `bigint` with Python's `int`, a `decimal` with `decimal.Decimal`, and a `boolean` by comparing the text to `"true"`. Keep null as `None`; Python's `bool("false")` is true and does not decode a SQL boolean.

A non-null `next_token` and `complete: false` indicate a partial retrieval. `info`, `start`, `status`, `cancel`, and `download` return their own metadata objects with `--format json`. `export` returns a JSON receipt on stdout by default, even for a CSV file. Worker pages can include an opaque `result_id`; the CLI checks that revision across pages when provided. It is not a download URL or an authorization grant.

CSV uses a header row, `\N` for SQL null, and doubles literal backslashes in non-null values. An empty string remains an empty CSV field. After reading CSV, interpret an exact `\N` as null, then replace doubled backslashes in other fields with one backslash. A literal `\N` value is therefore distinct from SQL null. CSV obtained through `download` retains the worker file's native conventions and may use different null handling.

Piped output can be incomplete if a later page fails. Check the exit code before using it. Use `results --all --out` for an atomic export of every page. Without `--all`, `results --out` publishes only the requested page and includes a continuation token in JSON when more rows remain.

For a reproducible analysis, retain SQL files, stdout, stderr, argument arrays, and exit codes in a fresh directory for each run. Read `query_id` from JSON output to retrieve the same query later. Keep replay scripts configurable through the environment. A check over another query's derived output proves consistency; an independent metric check starts from the underlying records.

## Verify locally

```sh
make verify
make smoke
```

These checks need Go, Make, Python 3, and a C compiler for Go's race detector. The Go tests use local HTTP and TLS servers with fake credentials. They verify actual SDK signing, query coordination, exact values and nulls, pagination failures, file publication, and safe download failures. `make smoke` additionally runs the built binary against a local worker using a temporary named AWS profile. Neither command uses real AWS credentials or contacts AWS.

See [the design decision](docs/design.md) for module ownership and tradeoffs.

## Test against a disposable AWS sandbox

The sandbox creates a real Lambda worker, private S3 bucket, Glue database with five synthetic tables, Athena workgroup, execution role, and CloudWatch log group. Queries execute in Athena engine version 3. All rows are fictional. The workgroup enforces a 10 MiB scan ceiling per query, and S3 objects expire after seven days as a fallback. Remove the sandbox after testing.

Use Python 3 and AWS CLI v2. An administrator first applies the [temporary scoped access policies](sandbox/iam/README.md). Creation checks the selected account before making changes.

Choose an unused manifest path for each new sandbox. The following examples use `.scratch/my-new-lab/manifest.json`; replace that path after removing the sandbox.

```sh
python3 scripts/sandbox-create.py \
  --profile sandbox --account YOUR_ACCOUNT_ID --region us-east-1 \
  --manifest .scratch/my-new-lab/manifest.json

source .scratch/my-new-lab/connection.sh
make build
./bin/athena-bridge tables --database "$ATHENA_BRIDGE_DATABASE" --all --format json
./bin/athena-bridge describe --database "$ATHENA_BRIDGE_DATABASE" --table ac_us_sessions --all --format json
./bin/athena-bridge query --database "$ATHENA_BRIDGE_DATABASE" \
  --sql 'SELECT setting, definition FROM fixture_contract ORDER BY setting' --all --format json
```

Read table and column descriptions, then query `fixture_contract` for identity, eligibility, quarantine, AI-event, product, session, and calendar assumptions. The expanded fixture has 1,115 login rows, 4,263 sessions, and 14,298 events. It includes missing eligibility data, unmatched relations, session IDs reused across logins, other-product control activity, and multiple streaks for one user. Session and user-month exports cross Athena's native page boundary.

These assumptions support synthetic exercises. Define your own production eligibility and session rules before adapting the example SQL.

`ATHENA_BRIDGE_DATABASE` is a shell convenience for these examples. The CLI selects its database through `--database`, or uses the worker's default when that flag is omitted.

The sandbox serves native Athena CSV downloads. Common CSV readers can collapse unquoted empty fields and quoted empty strings to the same value. Use `results --format csv --all --out` with the documented decoding rules when you need straightforward null-preserving analysis.

The worker accepts analytical `SELECT`, `WITH`, and `EXPLAIN` statements. A conservative token guard rejects writes and multiple statements. This guard is for the lab; IAM limits reads to synthetic sources and writes to query results. The worker uses one shared authorization domain. A query ID must belong to its configured database and workgroup. Continuation tokens are signed, scoped to their operation, and expire after one hour.

Run explicit live verification after creation.

```sh
python3 scripts/sandbox-verify.py --manifest .scratch/my-new-lab/manifest.json
# Also exercise export and interrupted resume:
python3 scripts/sandbox-verify.py --manifest .scratch/my-new-lab/manifest.json --check-export
```

This opt-in command contacts AWS. It checks queries, metadata, configured info, search, exact status statistics, cancellation, and structured failures through the built CLI. `--check-export` adds JSON and CSV export, interruption, and checkpoint resume checks. It regenerates fixtures from the current source, compares those hashes with the manifest and deployed objects, and records the clean source revision, binary hash, command argument arrays, output files, and exit codes in a fresh verification directory. Commit tracked changes and build the binary before running it. `make verify` and `make smoke` remain local and use fake credentials.

The sandbox policies exercise the relevant Athena, Glue, S3, and Lambda subset against synthetic resources. Passing this lab does not prove access to a full company account, its Lake Formation grants, encryption keys, or other organization controls. `info` reports configuration rather than effective IAM access.

Creation records intent before deploying. Rerun with the same manifest to resume an interrupted deployment. A failed stack retains its identity for cleanup. Use a new manifest path after deleting a sandbox.

```sh
python3 scripts/sandbox-cleanup.py --profile sandbox \
  --manifest .scratch/my-new-lab/manifest.json
```

Cleanup checks account and stack ownership, disables the worker, waits for in-flight invocations, stops unfinished queries, deletes owned objects and multipart uploads, deletes the stack, and verifies removal. It can be rerun after interruption. Recovery state stays in the selected manifest. IAM access policies remain available for future sandboxes. AWS service history and billing records follow AWS retention.

## License

Athena Bridge is licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) for the full text.
