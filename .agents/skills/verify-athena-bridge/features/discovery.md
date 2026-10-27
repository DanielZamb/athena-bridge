# Discover worker and metadata

Read configured capabilities, tables, columns and partition metadata, then forward literal search filters to the worker.

## Sub-features

- `discovery-1` Info renders as JSON, CSV and table; malformed configured info fails.
- `discovery-2` Tables preserve null descriptions; describe preserves partition text.
- `discovery-3` Search all/table/column filters, exact table/database/term forwarding, and empty pages with continuation.

## How to get to it (user POV)

`info`, `tables --database analytics --all`, `describe --table events`, and `search --term CoUnTrY%_ --kind all|table|column --table events --database analytics --all`.

## Driving it with the local CLI driver

Preconditions: launch and doctor succeeded; ROOT, DRIVER and RUN are the absolute paths from the parent skill.

```sh
python3 "$DRIVER" drive --run "$RUN" --feature discovery
```

Checks configured info and exact metadata rows. Search begins with an empty page and still follows its cursor to a matching fixture row. Checks outbound filters unchanged and a complete final page.

The numbered receipts retain the actual argv, stdout, stderr and exit code. Read them with summary.json and resulting file evidence.

## Gotchas

Glue matching, case folding, literal wildcard behavior, authorization, metadata budgets and cursor scoping are worker responsibilities. Local forwarding checks do not prove those behaviors.
