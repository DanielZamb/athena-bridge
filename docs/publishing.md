# Package a release

## Review the release

This repository uses Apache-2.0. Include [LICENSE](../LICENSE) in the public source distribution. Confirm that you have permission to publish the source and bundled fixtures.

Run the local checks from a complete checkout:

```sh
make verify
make smoke
make secrets
git diff --check
```

Review the files you intend to commit, including new files. Secret scanning detects known credential patterns. It cannot establish that every file is appropriate to publish. Check for internal business definitions, account IDs, private hostnames, personal information, query results, and generated policies as well.

## Package a binary download

Build the package for your current operating system and architecture:

```sh
make release
```

To cross-compile, select a target explicitly:

```sh
python3 scripts/package-release.py --os linux --arch amd64
python3 scripts/package-release.py --os darwin --arch arm64
python3 scripts/package-release.py --os windows --arch amd64
```

Supported targets are Linux, macOS, and Windows on `amd64` or `arm64`. Packaging requires Go and Python 3. It builds with CGO disabled, removes debug symbols with `-ldflags='-s -w'`, and keeps the CLI's embedded SQLite support and SQL schemas. No executable packer or runtime extraction is used.

Each archive is named `athena-bridge_VERSION_OS_ARCH.tar.gz`, or `.zip` on Windows. A sibling `.sha256` file records its SHA-256. The version comes from the CLI source. Both files go into `dist/` by default. Use `--output PATH` to choose another directory.

The extracted `athena-bridge` directory contains exactly these files:

```text
athena-bridge               # athena-bridge.exe on Windows
README.md
SKILL.md
LICENSE
THIRD_PARTY_NOTICES.txt
docs/lambda-contract.md
docs/troubleshooting.md
worker/README.md
worker/worker.py
```

The packager selects these files explicitly. It does not include tests, usage databases, sandbox fixtures, developer skills, deployment scripts, or source history. `worker/worker.py` is copied directly from the tested `sandbox/worker.py`. Third-party notices come from the Go standard library and modules linked for the selected target. Fixed archive timestamps and file modes make repeated packaging reproducible with the same source, dependencies, and toolchain.

Inspect an archive's checksum, file list, permissions, and example worker:

```sh
python3 scripts/verify-release.py dist/athena-bridge_0.1.0_linux_amd64.tar.gz
```

For a host-compatible archive, add `--run` to execute help, version, and the local smoke check from its extracted files. With no archive argument, the verifier selects the only binary archive in `dist/`.

Extract the host package into a fresh directory, then verify the executable from outside the source checkout:

```sh
/absolute/path/to/athena-bridge/athena-bridge --help
/absolute/path/to/athena-bridge/athena-bridge version
python3 scripts/verify-local.py --binary /absolute/path/to/athena-bridge/athena-bridge
```

The smoke check uses local servers and fake credentials. It runs the selected executable from a private temporary working directory. Run target binaries on their target operating system. Cross-compilation alone does not verify runtime behavior. Upload the reviewed archive and its checksum as the binary release assets.

The GitHub Actions build job packages and verifies a native download on six runners, one for each supported target. Each job runs the packaged binary on its own platform and uploads its archive and checksum as a workflow artifact. The build jobs use the Go version in `RELEASE_GO_VERSION` in `.github/workflows/ci.yml`, so a rerun produces the same archives. Change that version deliberately, for example to pick up a Go security release.

## Publish a GitHub release

Pushing a `v` tag publishes a release. The release job runs only after the verify, build, and secret-scan jobs pass.

1. Set `const version` in `cmd/athena-bridge/usage.go` to the new version.
2. Add a `## VERSION` section to [CHANGELOG.md](../CHANGELOG.md). It becomes the release notes.
3. Check the tag locally:

   ```sh
   python3 scripts/check-release.py v0.1.0
   ```

4. Commit, then tag and push:

   ```sh
   git tag -a v0.1.0 -m 'Athena Bridge 0.1.0'
   git push origin main v0.1.0
   ```

The release job fails if the tag differs from the CLI version, the changelog has no section for it, or any of the six archives or checksums is missing or wrong. It then records a build provenance attestation for each archive and creates the GitHub release with all twelve files. A version containing a hyphen, such as `0.2.0-rc.1`, becomes a prerelease. To retry a failed release, delete the tag and the release if one was created, fix the problem, and push the tag again.

## Package committed source

Commit the reviewed release files, then run:

```sh
make source
```

The command refuses uncommitted changes and nonignored new files. The archive is `dist/athena-bridge-source.tar.gz`. It contains the committed `HEAD` tree under `athena-bridge/`, without `.git`, other branches, or ignored local files. Extract it into a new directory and check that it builds before distributing it.

## Check the repository before pushing

Scan every ref you intend to publish:

```sh
gitleaks git . --log-opts=--all --redact
```

If a credential enters a commit, revoke or rotate it before removing it from history. Deleting a file from the current revision does not remove earlier versions.

Enable the host's secret scanning and push protection when available. The included GitHub Actions workflow builds the CLI, runs local verification, and scans the public branch history and working files without AWS credentials.
