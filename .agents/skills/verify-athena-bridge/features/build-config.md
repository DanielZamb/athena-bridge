# Build and configure the CLI

Build one real binary, discover commands, and select the worker and AWS settings without touching AWS.

## Sub-features

- `build-config-1` Build and version/help, including every command help path.
- `build-config-2` Invalid flags, required fields, sizes, durations and positionals exit 2 before a request.
- `build-config-3` Named profile, missing profile/region, and explicit function/region/profile overrides.

## How to get to it (user POV)

`make build`, `athena-bridge version`, `athena-bridge --help`, and command flags `--function`, `--region`, `--profile`, `--format`, `--timeout`.

## Driving it with the local CLI driver

Preconditions: launch and doctor succeeded; ROOT, DRIVER and RUN are the absolute paths from the parent skill.

```sh
python3 "$DRIVER" drive --run "$RUN" --feature build-config
```

Runs actual help/version commands and invalid argument arrays. Invokes `tables --function flag-worker --region us-west-2 --profile local-proof` and observes the selected invocation path and signing region. Missing configuration exits 1; no function exits 2.

The numbered receipts retain the actual argv, stdout, stderr and exit code. Read them with summary.json and resulting file evidence.

## Gotchas

Launch builds into the run directory. The recipe does not verify install locations or cross-platform binaries.
