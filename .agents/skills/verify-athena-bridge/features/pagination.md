# Traverse pages and reject changed results

Retrieve a partial page, continue its opaque cursor, or stream all pages while rejecting inconsistent data.

## Sub-features

- `pagination-1` Partial JSON includes a token and complete=false; a resumed final page is complete.
- `pagination-2` All-page traversal and empty first pages follow tokens.
- `pagination-3` Repeated tokens, column changes, query mismatch, bad widths/cells/arrays/column names fail.
- `pagination-4` Changed or missing subsequent result identity fails closed.
- `pagination-5` Existing results remain usable when an older worker omits result identity on every page.

## How to get to it (user POV)

`results --query-id local-query` with `--all`, `--next-token local-page-2`, `--page-size 1`, and `--format json`. Metadata pagination also runs through search.

## Driving it with the local CLI driver

Preconditions: launch and doctor succeeded; ROOT, DRIVER and RUN are the absolute paths from the parent skill.

```sh
python3 "$DRIVER" drive --run "$RUN" --feature pagination
```

Checks literal rows and completion fields, exact requested page size, and MORE_RESULTS diagnostics. A later-page failure leaves partial stdout and an error with partial_output=true; first-page failures have false.

The numbered receipts retain the actual argv, stdout, stderr and exit code. Read them with summary.json and resulting file evidence.

## Gotchas

A partial stdout stream is expected on late failures. Consumers must check the exit code. --out and export provide different file publication guarantees.
