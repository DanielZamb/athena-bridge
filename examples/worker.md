# Set up the example worker

The download's `worker.py` is the same worker exercised by this repository's sandbox tests. It implements the [version-one contract](../docs/lambda-contract.md), including schema discovery, query execution, cancellation, pagination, result identity, and CSV download.

## Configure Lambda

Create a Python 3.13 Lambda function with handler `worker.handler`. Put `worker.py` at the root of its deployment ZIP. The worker imports `boto3` and `botocore`. Include those dependencies in your deployment or use the runtime-provided SDK. Python runs in Lambda. The CLI does not need Python locally.

Configure these environment variables on the function:

- `DATABASE` selects the single allowed Glue database.
- `WORKGROUP` selects the Athena workgroup.
- `BUCKET` selects the S3 bucket used for results under `results/`.
- `CURSOR_KEY` supplies a private, high-entropy signing secret for pagination tokens. Configure it privately and keep it stable while tokens remain valid.
- `MAX_SCAN_BYTES` reports the workgroup's scan limit. Enforce the matching limit in the Athena workgroup.
- `RESULT_RETENTION_DAYS` optionally reports retention. Configure the matching S3 lifecycle policy separately.
- `WORKER_ID` optionally supplies a public worker identifier.

The repository's sandbox uses a 25-second Lambda timeout. The CLI caps each invocation at 30 seconds, so keep worker calls bounded.

## Configure access and storage

Give the Lambda execution role access to Athena query execution, status, results, and cancellation in the selected workgroup. It also needs Glue metadata access, S3 access to source data and result objects, and CloudWatch logging. Account controls such as Lake Formation and KMS can require additional permissions.

Configure the workgroup for `AwsDataCatalog`, the selected database, and `s3://BUCKET/results/` with SSE-S3 encryption. The worker checks query scope and result location against that configuration. This example serves Athena results in S3 and does not implement managed query results.

The worker allows one `SELECT`, `WITH`, or `EXPLAIN` statement and rejects a bounded set of write keywords. Enforce data and query policy with AWS permissions and workgroup settings. The SQL filter is not a complete SQL parser. All authorized callers of the same worker share its configured database and workgroup scope. Adapt authorization if callers need separate access to queries.

Give CLI callers `lambda:InvokeFunction` on the worker separately from its execution-role permissions. The worker uses its execution role through the SDK credential chain. Do not put AWS keys in its code or environment.

## Connect the CLI

Set `ATHENA_BRIDGE_FUNCTION` to the deployed function name or ARN, select its region and caller profile, then run:

```sh
athena-bridge info --format json
athena-bridge query --sql 'SELECT 1' --all --format json
```

The source repository keeps this file at `sandbox/worker.py`. The downloadable package includes that file directly, without the sandbox provisioner, synthetic data, or lifecycle scripts.
