# Lambda worker contract, version 1

Implement this interface in the Lambda you create. The CLI invokes the function directly through AWS Lambda `Invoke` with `InvocationType: RequestResponse`. The body is the JSON event, not an API Gateway request. Return a JSON object, not a serialized JSON string or an API Gateway `statusCode`/`body` wrapper.

The CLI uses no Athena credentials in this event. The caller separately needs `lambda:InvokeFunction` for the selected function or alias. The function's execution role supplies its company data permissions. Enforce database and workgroup policy in the worker.

## Envelopes

Every request has `version: 1` and `action`. Responses contain `version: 1` and exactly one of `result` or `error`. The CLI permits additive fields but rejects a different version or malformed results.

```json
{"version":1,"action":"query_status","query_id":"example-query"}
```

```json
{"version":1,"result":{"query_id":"example-query","state":"SUCCEEDED"}}
```

```json
{"version":1,"error":{"code":"NOT_ALLOWED","message":"This database is not available."}}
```

Error codes and messages must be nonempty. Return sanitized messages without credentials, signed URLs, or internal stack traces. Unexpected Lambda runtime errors also cause a nonzero CLI exit, including a Lambda HTTP 200 response with `FunctionError`.

The CLI converts errors into typed local diagnostics. By default, stderr is version-one JSON Lines regardless of stdout format. Explicit `--pretty` selects human-readable CLI presentation without changing this worker contract. Events include local `code`, `operation`, `exit_code`, `recovery`, known safe request/query identity, `partial_output`, and `published`. Worker rejections use local code `WORKER_REJECTED`, except `NOT_READY` and `RESULT_CHANGED`, which retain their names as local codes. Each carries a separate `worker_code`; its raw message is not echoed. These diagnostic events are a CLI output contract, not worker response envelopes. See [output formats](https://github.com/DanielZamb/athena-bridge#output-formats).

## Report configured capabilities

```json
{"version":1,"action":"worker_info"}
```

```json
{
  "version": 1,
  "result": {
    "worker_id": "analytics-worker",
    "basis": "configured",
    "default_database": "analytics",
    "databases": ["analytics"],
    "catalog": "AwsDataCatalog",
    "workgroup": "analytics-read",
    "actions": ["worker_info", "list_tables", "describe_table", "search_metadata", "start_query", "query_status", "cancel_query", "get_results", "download_result"],
    "features": ["result_identity"],
    "limits": {"default_page_size":200,"max_page_size":1000,"metadata_tables_per_call":100}
  }
}
```

Return explicit application configuration only. `basis` must be `configured`. `worker_id` must be nonempty; `databases`, `actions`, and `features` must be arrays, and `limits` must be an object of nonnegative integer values. Advertise `result_identity` only when enforced. The sandbox also reports `max_response_bytes`, `max_scan_bytes`, `max_sql_bytes`, `cursor_ttl_seconds`, and `result_retention_days` from its configured limits.

Omit unknown limits. The reference worker accepts an unset `RESULT_RETENTION_DAYS` and omits `result_retention_days` in that case. A periodic cleanup schedule alone does not establish a result retention period.

This action makes zero IAM calls. It must not inspect or disclose IAM policies, roles, bucket paths, credentials, or environment dumps. It does not prove effective company-account permissions. The sandbox returns this object without creating any downstream AWS client.

## Start a query

```json
{
  "version": 1,
  "action": "start_query",
  "request_id": "0123456789abcdef0123456789abcdef",
  "database": "analytics",
  "sql": "SELECT id FROM events LIMIT 100"
}
```

```json
{"version":1,"result":{"query_id":"example-query"}}
```

`database` can be omitted to use the worker's configured default. `request_id` contains 32 to 128 ASCII letters, digits, dots, underscores, colons, or hyphens. Pass it unchanged as Athena's `ClientRequestToken`. Configure the catalog, workgroup, output location, encryption, and other execution parameters deterministically. Repeating the same request must return the same execution ID, including after an ambiguous timeout. Reusing an ID with different parameters must fail. Do not generate a new Athena token on each invocation.

Return once Athena accepts the query. Do not wait inside this action. [Athena submission and idempotency](https://docs.aws.amazon.com/athena/latest/APIReference/API_StartQueryExecution.html).

## Read query state

```json
{"version":1,"action":"query_status","query_id":"example-query"}
```

```json
{
  "version": 1,
  "result": {
    "query_id":"example-query",
    "state":"RUNNING",
    "statistics":{"data_scanned_bytes":"9007199254740993","queue_ms":"0","result_reused":false}
  }
}
```

State must be `QUEUED`, `RUNNING`, `SUCCEEDED`, `FAILED`, or `CANCELLED`. Include a sanitized `reason` for failure or cancellation. Echo the requested query ID exactly.

Optional `statistics` fields `data_scanned_bytes`, `engine_execution_ms`, `total_execution_ms`, `queue_ms`, and `planning_ms` are nonnegative decimal strings, preserving exact quantities. `result_reused` is an optional boolean. Omit unknown values; `"0"` and `false` are known values, not missing data.

Optional `failure` contains nonnegative integer `error_category` and `error_type`, plus boolean `retryable`, when Athena supplies them. The sandbox can add positive integer `sql_line` and `sql_column` extracted from a bounded leading SQL location. It replaces raw Athena error text with recovery guidance, such as checking metadata, reducing scan scope, or contacting the worker administrator. Do not expose SQL, storage locations, or raw AWS messages in `reason`.

A valid `status` lookup exits 0 for every state, including `FAILED` and `CANCELLED`. Waiting commands exit 1 for those terminal states. Client cancellation or deadline expiry exits 3 and does not stop Athena.

## Request query cancellation

```json
{"version":1,"action":"cancel_query","query_id":"example-query"}
```

```json
{"version":1,"result":{"query_id":"example-query","state":"RUNNING","cancellation_requested":true}}
```

Authorize the query before calling Athena. For a terminal query, return its current status with `cancellation_requested: false` and do not call `StopQueryExecution`. Otherwise call `StopQueryExecution`, reread the authorized query status, and return `cancellation_requested: true` with that observed state. Use the same optional status fields as `query_status`.

Acceptance is not completion. `true` can accompany `QUEUED`, `RUNNING`, or a terminal state reached during the request. Call `query_status` to observe completion. A valid response exits 0; an authorization or stop failure returns an error, not claimed acceptance.

## Return table metadata

```json
{"version":1,"action":"list_tables","database":"analytics","page_size":200}
```

```json
{"version":1,"action":"describe_table","database":"analytics","table":"events","page_size":200}
```

Both actions return a tabular page in `result`. For `list_tables`, use columns named `table_name`, `table_type`, and `description`, with type `varchar`. For `describe_table`, use `column_name`, `data_type`, `description`, and `partition_key`, also with type `varchar`. Return `true` or `false` as text in the partition-key column. Preserve the source column order. Do not include a `query_id` in metadata pages.

```json
{
  "version": 1,
  "result": {
    "columns": [
      {"name":"table_name","type":"varchar"},
      {"name":"table_type","type":"varchar"},
      {"name":"description","type":"varchar"}
    ],
    "rows": [["events","EXTERNAL_TABLE",null]],
    "next_token": null
  }
}
```

## Return query rows

```json
{"version":1,"action":"get_results","query_id":"example-query","page_size":200,"expected_result_id":"opaque-revision"}
```

```json
{
  "version": 1,
  "result": {
    "query_id": "example-query",
    "result_id": "opaque-revision",
    "columns": [
      {"name":"id","type":"bigint"},
      {"name":"amount","type":"decimal(20,4)"}
    ],
    "rows": [["9007199254740993","1.2300"],["2",null]],
    "next_token": "opaque-cursor"
  }
}
```

Echo the requested query ID. Repeat the same ordered column metadata on every page, including an empty page. `columns` and `rows` must be arrays, and every row must have exactly one cell per column. Cells are strings or null, never JSON numbers, booleans, or objects. Use `[]` for no rows. Remove Athena's header row from the first results page, and preserve missing values as null rather than converting them to empty strings. Column names and types must be nonempty. Duplicate column names are permitted.

`result_id` is an opaque, stable identity for this query's authorized result revision. A first request can omit `expected_result_id`; subsequent requests can require that identity. Check it before and after reading each page, and bind continuation tokens to the same revision. Return `RESULT_CHANGED` without rows when it differs. Return `RESULT_IDENTITY_UNAVAILABLE` when a worker advertising this feature cannot identify the revision.

The sandbox derives identity from query and scope identity plus S3 object revision metadata, including version, ETag, length, and modification time. It returns an opaque digest, not those storage details. Identity is not a URL, content checksum, or authorization grant. Existing CLI commands accept older workers without identity; when supplied, the CLI requires it to remain stable across pages. Checkpoint export requires identity and fails cleanly when absent. Checkpoints store the identity, never a result URL. See the [checkpoint export](https://github.com/DanielZamb/athena-bridge#resume-a-checkpoint-export).

All tabular actions accept `page_size` from 1 to 1000 and an optional opaque `next_token`. Return `next_token: null`, an empty string, or omit it when no pages remain. A nonempty token means more data exists. Bind cursors to the authorized operation, database, and query. Reject invalid cursors. The worker must check access to each requested query, rather than assuming possession of its ID grants access.

Keep each encoded response below Lambda's synchronous response limit. Return a smaller page when necessary and retain a continuation token. Return `NOT_READY` when query results cannot yet be retrieved. [Athena results and pagination](https://docs.aws.amazon.com/athena/latest/APIReference/API_GetQueryResults.html), [Lambda invocation](https://docs.aws.amazon.com/lambda/latest/api/API_Invoke.html).

## Search metadata

```json
{"version":1,"action":"search_metadata","database":"analytics","term":"session","kind":"column","table":"events","page_size":200}
```

Return an ordinary tabular page with ordered `varchar` columns `kind`, `table_name`, `column_name`, `data_type`, `description`, and `partition_key`. Table rows use `kind: "table"` and null column-specific cells. Column rows use `kind: "column"` and `"true"` or `"false"` for `partition_key`. Include partition columns.

Match names and comments literally and case-insensitively. `term` is required and contains 1 to 1024 characters. `kind` defaults to `all` and accepts `all`, `table`, or `column`. Optional `table` restricts the search to an exact table. The sandbox normalizes term case, kind, and table before binding them into the cursor.

Bound work per invocation. The sandbox reads at most one `GetTables` page with `MaxResults: 100`, or uses `GetTable` for an exact table. Carry the native page and local offset in signed scan state; do not scan the whole catalog in one invocation. Empty pages can have continuation tokens. A budget-limited scan must preserve continuation rather than claim complete coverage. Cursors bind normalized filters, database, operation, and scan state. The CLI's `--all` follows tokens until exhaustion.

## Provide a complete result file

```json
{"version":1,"action":"download_result","query_id":"example-query"}
```

```json
{
  "version": 1,
  "result": {
    "url": "https://example.invalid/result.csv",
    "format": "csv",
    "size_bytes": 1240
  }
}
```

Replace the example URL with a fresh, short-lived presigned S3 GET URL for this authorized query's complete result. Do not return AWS credentials. A bare `s3://` URI is insufficient because the caller only needs Lambda invocation permission. S3 can remain private. If your Athena workgroup uses managed results, the worker must materialize an authorized downloadable object itself or return `DOWNLOAD_UNAVAILABLE`.

Supported formats are `csv`, `parquet`, `json`, and `jsonl`. `size_bytes` is optional. When present it must be the exact nonnegative byte count of the downloaded object. Serve the file directly with HTTP 200 and no redirects. The CLI saves bytes unchanged, rejects mismatched lengths and excessive sizes, and does not follow redirects. Avoid setting HTTP content encoding when it would cause the decoded byte count to differ from `size_bytes`. File null handling follows the chosen format, independently of the CLI's paginated CSV encoding.

Native download recovery requests a fresh URL and retries the whole file. It does not use byte ranges or checkpoint resume. Paginated export and resume use `get_results` with result identity instead.

## Worker responsibilities

The worker owns authentication policy beyond Lambda invocation, table access, query restrictions, workgroup selection, result storage, expiry, and cleanup. Use a dedicated results location and enforce access there as well as in Athena. Return promptly from each operation because the CLI caps an individual invocation at 30 seconds and the whole command at its selected timeout.

The SDK may retry a request after a transport or service failure. The start request ID and immutable Athena parameters are required to avoid duplicate query submissions. A client timeout or Ctrl-C does not cancel Athena. Cancellation requires an explicit authorized `cancel_query` request.

The sandbox policy covers the relevant service subset for synthetic data. It is not proof of full company-account access. Production data permissions and caller invocation permission remain separate deployment inputs.
