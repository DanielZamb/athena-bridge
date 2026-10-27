# Publish a source release

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
