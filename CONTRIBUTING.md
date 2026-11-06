# Contribute to Athena Bridge

The CLI uses Go 1.24 or later. Read [README.md](README.md) and the [Lambda worker contract](docs/lambda-contract.md) before changing commands or protocol fields.

## Build and run local checks

From the repository root:

```sh
make build
./bin/athena-bridge --help
make verify
make smoke
```

Building requires only Go. Verification also needs Make, Python 3, and a C compiler for `go test -race`. Python tests use the standard library. The tests and smoke check use loopback servers and fake credentials. They must not contact AWS. Go may download module dependencies on the first run.

Without Make, run these commands directly:

```sh
go build -trimpath -o bin/ ./cmd/athena-bridge
go test -race ./...
go vet ./...
python3 -m unittest discover -s sandbox -p 'test_*.py'
python3 scripts/verify-local.py
```

On Windows, invoke the binary as `.\bin\athena-bridge.exe`. Use `python` instead of `python3` if that is your Python executable.

To cross-compile without a C compiler on macOS or Linux:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o bin/linux-amd64/ ./cmd/athena-bridge
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -o bin/windows-amd64/ ./cmd/athena-bridge
```

Cross-compilation checks the build. Run the binary on its target operating system to verify runtime behavior.

## Check for secrets

Install [Gitleaks](https://github.com/gitleaks/gitleaks#installing) 8.30.1 or later, then run:

```sh
make secrets
```

This scans tracked files, nonignored new files, and the current branch's complete Git history. A shallow checkout must fetch its full history first. Reports redact matched values. The configuration allows one fixed request ID only in the two local verification fixtures. It does not exclude test directories or Git commits.

Keep AWS credentials in the SDK credential chain, outside the repository. Use fictional values and reserved example domains in tests and documentation. Keep account-specific policies, manifests, downloaded data, checkpoints, usage databases, and local agent installations out of commits. Generate account-specific IAM policies only when you opt into the live sandbox.

The optional sandbox scripts create billable AWS resources. They are separate from local checks and CI. Follow the [sandbox instructions](README.md#test-against-a-disposable-aws-sandbox) only when you intend to use AWS.

## Change CLI behavior

Preserve exact numeric text, SQL nulls, column order, and nonzero failure exit codes. Keep results on stdout and diagnostics on stderr. The CLI signs Lambda requests and coordinates local output. The worker owns Athena permissions, query policy, and result-file access.

Run `make fmt`, `make verify`, and `make smoke` before submitting a behavior change. Include the behavior you changed and the checks you ran in the pull request. Describe user-visible changes in [CHANGELOG.md](CHANGELOG.md) under the next version. Remove SQL, credentials, result data, signed URLs, and local account details from issue reports.

See [Package a release](docs/publishing.md) for compact binary downloads and source releases.
