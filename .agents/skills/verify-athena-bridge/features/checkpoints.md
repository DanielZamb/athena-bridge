# Export and resume durable results

Export every row to a private file and resume after a real CLI process crash without changing the final bytes.

## Sub-features

- `checkpoints-1` JSON and CSV uninterrupted exports produce complete files and JSON publication receipts.
- `checkpoints-2` SIGKILL after a durable first page preserves progress; an OS lock rejects a concurrent actual resume process.
- `checkpoints-3` Resume discards an uncommitted suffix, releases crashed-process locks and produces byte-identical output.
- `checkpoints-4` Expired cursor recovery replays the delivered prefix before continuing.
- `checkpoints-5` Changed result identity, changed replay prefix, corrupt committed bytes and absent result identity fail closed.
- `checkpoints-6` Published resumes are idempotent, never submit SQL and never overwrite an unrelated destination.

## How to get to it (user POV)

`export --query-id local-query --out PATH --format json|csv [--checkpoint PATH]`, then `export --resume CHECKPOINT [--profile local-proof] [--timeout 5s]`.

## Driving it with the local CLI driver

Preconditions: launch and doctor succeeded; ROOT, DRIVER and RUN are the absolute paths from the parent skill.

```sh
python3 "$DRIVER" drive --run "$RUN" --feature checkpoints
```

For each encoding, creates an uninterrupted reference file. Blocks the external second-page invocation after the first page is durable, attempts a second CLI resume to prove exclusive locking, then SIGKILLs the first process. Resumes real checkpoints with suffix, expired cursor, result change, corruption and changed replay fixtures. Successful outputs must equal the uninterrupted file byte-for-byte; failures preserve the checkpoint and committed bytes. Copies successful recovered files into evidence and records checkpoints and export-recovery.json.

The numbered receipts retain the actual argv, stdout, stderr and exit code. Read them with summary.json and resulting file evidence.

## Gotchas

The process crash is real; no internal setter or fabricated checkpoint phase is used. Missing result identity must fail. Default and explicit checkpoint paths, valid profile/timeout overrides and invalid format overrides are exercised. Exact finishing/sealed crash windows, power loss, disk-full/sync faults and repartitioned replay pages remain separate fault checks.
