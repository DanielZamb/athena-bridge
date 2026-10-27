# Submit SQL and retry a start

Submit text, file or stdin SQL while preserving a stable request ID for uncertain starts.

## Sub-features

- `sql-1` Both start and query accept inline, file and stdin SQL.
- `sql-2` Repeated identical IDs preserve requests; changed parameters trigger a worker fixture rejection.
- `sql-3` Generated IDs, invalid/oversized/empty SQL, missing files and conflicting inputs.
- `sql-4` Uncertain submission retains retry_same_request; an explicit NOT_ALLOWED rejection uses WORKER_REJECTED/none and retains its request ID without a query ID.

## How to get to it (user POV)

`start` or `query` with `--sql`, `--sql-file PATH`, or `--sql-file -`, plus optional `--request-id`.

## Driving it with the local CLI driver

Preconditions: launch and doctor succeeded; ROOT, DRIVER and RUN are the absolute paths from the parent skill.

```sh
python3 "$DRIVER" drive --run "$RUN" --feature sql
```

Writes a synthetic SQL file and passes the same text via all three input paths. Requires the same query ID, START_REQUEST before QUERY_STARTED, exact request ID forwarding and no SQL in diagnostics. Reuses one token with different SQL and observes exit 1. Lambda FunctionError retains the original ID for recovery.

The numbered receipts retain the actual argv, stdout, stderr and exit code. Read them with summary.json and resulting file evidence.

## Gotchas

The worker fixture enforces token reuse at the external boundary. Athena-side idempotency and query policy remain worker checks. The helper never reads personal SQL or credentials.
