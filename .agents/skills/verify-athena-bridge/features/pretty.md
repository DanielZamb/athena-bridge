# Read CLI output manually

## User entry points

Add `--pretty` to discovery, query, status, cancellation, download, and export commands. Resume accepts `--pretty` as a presentation override. The flag selects readable console output and diagnostics. Existing formats and JSONL diagnostics remain the default.

## Drive locally

Run the parent skill's launch and doctor steps, then:

```sh
python3 "$DRIVER" drive --run "$RUN" --feature pretty
```

The recipe invokes the built CLI against an isolated HTTP Lambda fixture with fake credentials. It checks bordered rows, exact large numbers and decimals, null versus empty text, capabilities, metadata, search, status, cancellation acceptance, request and query IDs, pagination tokens, readable errors, unchanged JSON and CSV files, export receipts, completed-checkpoint resume, and explicit `--pretty=false`.

Each receipt records its expected diagnostic format. Default-mode commands still require versioned JSONL on every stderr line. Pretty-mode commands require readable error codes and the expected exit code, and reject terminal control characters. Published files are compared byte for byte with the corresponding default format.

## Limits

Go regression tests cover expanded Unicode and wide rows, duplicate columns, safe control escaping, argument values resembling flags, empty intermediate pages, reversible tokens, cancelled contexts, and failed output writers. This recipe does not prove terminal font rendering or remote worker behavior. No AWS resources or system terminal settings change.
