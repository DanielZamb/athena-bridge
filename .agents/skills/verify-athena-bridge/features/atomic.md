# Publish tabular output atomically

Write a private output file without overwriting existing files or publishing incomplete pages.

## Sub-features

- `atomic-1` Successful CSV publication has private 0600 permissions and OUTPUT_PUBLISHED.
- `atomic-2` Existing files and destination symlinks fail without changing content.
- `atomic-3` Late token/schema/identity failures remove temporary siblings and leave no destination.
- `atomic-4` Single-page --out intentionally publishes partial retrieval metadata.

## How to get to it (user POV)

`results --query-id local-query --all --format csv --out PATH`, or `--format json --out PATH` without --all.

## Driving it with the local CLI driver

Preconditions: launch and doctor succeeded; ROOT, DRIVER and RUN are the absolute paths from the parent skill.

```sh
python3 "$DRIVER" drive --run "$RUN" --feature atomic
```

Checks actual output bytes, permissions and publication diagnostics. Repeats against an existing destination and a symlink, then injects later-page failures and checks absence of both destination and .athena-bridge temporary siblings. Retains the published CSV as evidence.

The numbered receipts retain the actual argv, stdout, stderr and exit code. Read them with summary.json and resulting file evidence.

## Gotchas

Hard-link support and an existing parent directory are required. Disk-full/fsync failures and adversarial publication races need separate fault checks.
