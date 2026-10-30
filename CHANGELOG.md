# Changelog

Each release section supplies the GitHub release notes. The release job fails when the section for the tagged version is missing.

## 0.1.0

First public release.

- Signed AWS Lambda invocation through the AWS SDK credential chain, with no credential flags or files of its own.
- Schema discovery with `info`, `search`, `tables`, and `describe`.
- Query execution with `query`, `start`, `status`, `results`, and `cancel`, including safe retry of an uncertain start with `--request-id`.
- Table, JSON, and CSV output that preserves exact numeric text, SQL nulls, duplicate column names, and column order. `--pretty` selects human-readable output.
- Pagination with `--all` or opaque `--next-token` continuation.
- Resumable checkpoint exports with `export` and native result files with `download`. Neither replaces an existing destination.
- Versioned JSON Lines diagnostics on standard error, with stable codes, recovery actions, and exit codes 0 to 3.
- Optional local usage log in SQLite. Set `ATHENA_BRIDGE_USAGE_DB=off` to disable it.
- Example Python Lambda worker and the version-one worker contract.
- Binary downloads for macOS, Linux, and Windows on amd64 and arm64, with an agent skill, a troubleshooting guide, SHA-256 checksums, and build provenance attestations.
