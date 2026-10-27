# Observe and cancel queries

Follow states, retain exact statistics, request cancellation, and recover query identity after local interruption.

## Sub-features

- `queries-1` Status succeeds for QUEUED, RUNNING, SUCCEEDED, FAILED and CANCELLED.
- `queries-2` Query and results --wait progress through queued/running/succeeded or fail on terminal failure/cancellation.
- `queries-3` Optional statistics preserve missing versus zero and exact decimal strings; failure categories and false flags survive.
- `queries-4` Cancel reports acceptance separately from the observed state.
- `queries-5` Deadline and real SIGINT exit 3 without cancelling the remote query; invalid states, IDs and statistics fail.

## How to get to it (user POV)

`query --sql TEXT`, `status --query-id local-query`, `results --query-id local-query --wait`, and `cancel --query-id local-query`.

## Driving it with the local CLI driver

Preconditions: launch and doctor succeeded; ROOT, DRIVER and RUN are the absolute paths from the parent skill.

```sh
python3 "$DRIVER" drive --run "$RUN" --feature queries
```

Uses `--poll-interval 1ms` for state progress and a long polling timer for cancellation. Sends SIGINT only after an actual status request drains. Checks exit 3, DEADLINE/CANCELLED, inspect_query and no cancel request. Runs status and cancel output as JSON/CSV/table.

The numbered receipts retain the actual argv, stdout, stderr and exit code. Read them with summary.json and resulting file evidence.

## Gotchas

Cancellation acceptance does not promise terminal cancellation. No AWS query exists in this recipe. Arbitrary signal timing inside the SDK response reader is outside the timer-interruption check.
