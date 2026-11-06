# Render exact JSON, CSV and tables

Keep ordered duplicate columns, large integers, decimals, SQL nulls, empty strings, backslashes and control characters.

## Sub-features

- `formats-1` One complete JSON object preserves cells as text/null and duplicate column order.
- `formats-2` CSV headers, null marker, literal backslash escaping and quoted multiline cells.
- `formats-3` Table output preserves numeric text and escapes line/tab controls.

## How to get to it (user POV)

`results --query-id local-query --all --format json|csv|table`.

## Driving it with the local CLI driver

Preconditions: launch and doctor succeeded; ROOT, DRIVER and RUN are the absolute paths from the parent skill.

```sh
python3 "$DRIVER" drive --run "$RUN" --feature formats
```

Compares JSON against literal full rows including 9007199254740993 and 12345678901234567890.2300. Reads emitted CSV with Python csv and compares literal expected fields, including distinct null, empty and literal backslash-N. Inspects table headers, values and escaped controls.

The numbered receipts retain the actual argv, stdout, stderr and exit code. Read them with summary.json and resulting file evidence.

## Gotchas

Native downloaded files keep the worker file encoding. CLI CSV null conventions apply only to rendered tabular output. The driver imports no Go encoder or expected-value generator.
