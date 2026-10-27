# Sign and invoke the worker

Invoke the configured Lambda through the real AWS SDK and distinguish transport, function, protocol and worker failures.

## Sub-features

- `transport-1` SigV4 verification with a disposable named profile and fake environment session credentials.
- `transport-2` Synchronous invocation and no CloudWatch log tail request.
- `transport-3` Malformed JSON/version/envelopes, Lambda FunctionError, sanitized worker rejection, and rejected HTTP redirects.
- `transport-4` Worker NOT_READY maps to NOT_READY/wait_query, RESULT_CHANGED to RESULT_CHANGED/inspect_query, and NOT_ALLOWED to WORKER_REJECTED/none while retaining query identity.
- `transport-5` SDK denial, expired credentials, bad signatures, throttling and invalid targets produce typed diagnostics; an uncertain throttled start retains its request ID and retry_same_request.

## How to get to it (user POV)

Any worker command, exercised through `tables` with the run-owned Lambda endpoint.

## Driving it with the local CLI driver

Preconditions: launch and doctor succeeded; ROOT, DRIVER and RUN are the absolute paths from the parent skill.

```sh
python3 "$DRIVER" drive --run "$RUN" --feature transport
```

Recomputes the observed SDK signature. Changes only the external fixture response and repeats the actual command. Requires exact diagnostic codes, recoveries, identities and exit 1. SDK failures distinguish AUTHORIZATION, AUTHENTICATION, THROTTLED and CONFIGURATION. Worker INVALID_REQUEST uses fix_arguments; unknown codes retain check_worker. A Lambda redirect must produce exactly one invocation.

The numbered receipts retain the actual argv, stdout, stderr and exit code. Read them with summary.json and resulting file evidence.

## Gotchas

HTTP is used for loopback Lambda fixtures; production signing still belongs to the SDK. SDK warnings can occur if a signal interrupts response draining. The query recipe interrupts the polling timer after the response drains; arbitrary in-flight stderr behavior is not claimed.
