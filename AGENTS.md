# Athena Bridge

This repository contains a Go CLI that invokes a user-managed AWS Lambda worker. Read `README.md` and `docs/lambda-contract.md` before changing commands or the worker protocol.

The CLI owns AWS request signing, query coordination, and local output. The Lambda owns Athena access, query policy, and access to result files. Keep credentials in the AWS SDK credential chain. Never put credentials in requests, examples, logs, or the repository.

Run `make verify` after behavior changes. Tests use local HTTP servers and fake credentials and must not contact AWS. Preserve exact numbers, SQL nulls, column order, and nonzero exit codes for failures. Keep standard output suitable for piping and diagnostics on standard error.
