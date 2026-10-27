---
name: verify-athena-bridge
description: Verify every Athena Bridge CLI subsystem through built CLI subprocesses and isolated local Lambda HTTP and download HTTPS servers. Use after command, configuration, diagnostic, pagination, output, or recovery changes.
---

# Verify Athena Bridge

Read `AGENTS.md`, `README.md`, `docs/lambda-contract.md`, and [the feature index](features/README.md). This skill drives the real Go binary. It replaces only the external Lambda and result-file servers with local fixtures. It uses fake credentials through the AWS SDK credential chain and never calls AWS. Do not run sandbox scripts for this recipe.

## Launch

Prerequisites are the repository's Go toolchain, Python 3, and `openssl` for the local HTTPS certificate. No Python packages, AWS CLI, real credentials, browser, Cursor, or delegated agents are needed. Do not install missing prerequisites automatically.

From the selected checkout, resolve absolute paths before running:

```sh
ROOT="$(git rev-parse --show-toplevel)"
DRIVER="$ROOT/.agents/skills/verify-athena-bridge/drive.py"
RUN="$ROOT/.scratch/verify-athena-bridge/agent-$(date +%Y%m%dT%H%M%S)-$$"
python3 "$DRIVER" launch --run "$RUN"
```

Launch builds `RUN/resources/athena-bridge` and creates a private disposable named AWS profile. Readiness is a zero exit and the absolute run path on stdout. A build failure removes resources and retains the build receipt. The CLI is short lived; each drive starts its own loopback servers on OS-assigned ports, then shuts them down and joins their threads. No background server survives a drive.

Every run has a separate binary, profile, TLS certificate, files, and ports. Two runs may use the same checkout with different run paths. Do not reuse a run directory or clean another run. Build commands record the source revision, dirty paths, and binary hash; after Go changes, launch again.

## Doctor

```sh
python3 "$DRIVER" doctor --run "$RUN" > "$RUN/doctor.json"
```

This read-only check validates ownership, the built binary hash, its actual `version` output, private profile permissions, prerequisites, and the checkout-local discovery link. Run it whenever a drive looks wrong. It does not rebuild or contact a worker. After cleanup, doctor deliberately fails; launch a fresh run.

The registered skill source is `.agents/skills/verify-athena-bridge/SKILL.md`. The checkout-local `.claude/skills/verify-athena-bridge` link resolves through `../../.agents/skills/verify-athena-bridge`. Preserve existing entries. Doctor checks only link resolution for skill registration.

## Drive

```sh
python3 "$DRIVER" drive --run "$RUN"
python3 "$DRIVER" drive --run "$RUN" --feature pagination --feature atomic
```

Selections are `build-config`, `transport`, `discovery`, `sql`, `queries`, `pagination`, `formats`, `atomic`, `downloads`, `checkpoints`, `pretty`, and `usage`. Each selection starts from its own baseline behavior, so it also works alone. Read the matching feature file for user entry points, observable assertions, and gaps. `checkpoints` drives the integrated export command, kills actual export subprocesses after a durable page, then verifies byte-identical JSON and CSV recovery, exclusive locks, expired cursor replay and failure preservation.

The subprocess environment strips inherited AWS and Athena Bridge variables and proxy settings, disables instance metadata, supplies only fake profiles or fake environment credentials, and points Lambda to loopback. On Linux, downloads trust the run's generated local test certificate through `SSL_CERT_FILE`. Native macOS/Windows uses system TLS trust, so the driver proves certificate rejection and pre-network checks there and explicitly reports the remaining HTTPS matrix pending. It never changes system trust. The shared HTTP handler comes from `scripts/verify-local.py`. No production code is imported into Python. Fixtures describe external responses; assertions check literal user-visible values, exit codes, diagnostics, signed requests, and filesystem effects.

## Evidence

The subprocess environment sets `ATHENA_BRIDGE_USAGE_DB=off`. Only the `usage` recipe overrides it with paths inside its private run resources and retains a metadata-only sample database.

```sh
python3 "$DRIVER" evidence --run "$RUN"
```

Each drive creates a fresh `RUN/drive-ID/` with numbered argument-array/exit-code/request receipts, separate stdout and stderr files, a feature summary, observed worker requests, and server teardown status. Atomic-output, export and successful download selections also retain the actual published files. Interrupted exports retain checkpoint snapshots and recovery results; SIGKILL receipts have a negative signal exit code and may have empty stderr. `state.json` binds the run to the checkout and built binary; `build.json` records build output. Evidence is under ignored `.scratch`, not committed.

Require the action and resulting state. Successful exit alone is insufficient. Verify exact cell values, headers and order, null versus empty text, continuation metadata, final exit codes, output contents and permissions, and absence of incomplete publications. Default-mode error receipts must contain versioned JSONL diagnostics with the correct recovery and publication state. The explicit pretty recipe checks readable diagnostics while comparing file encodings with their machine-format baselines. SDK signatures are recomputed using the disposable secret, while evidence retains only the validation result and credential-free requests. Authorization headers, keys, TLS private keys, and signed URLs are never copied into receipts.

An external fixture proves CLI behavior at the documented network boundary. It does not prove Glue search matching, Athena idempotency enforcement, IAM policy, worker result authorization, S3 retention, or production AWS connectivity. No dry-run claim substitutes for these observations. Pending checks remain visible in the index and summary; do not report them as passed.

## Cleanup

Run after success and every failed iteration:

```sh
python3 "$DRIVER" cleanup --run "$RUN"
python3 "$DRIVER" evidence --run "$RUN"
test ! -e "$RUN/resources"
test -f "$RUN/build.json"
```

Cleanup is idempotent. It removes only this run's resources, including the built binary, fake profile, SQL input, TLS keys, temporary outputs and any interrupted export files. It preserves receipts, file evidence, state, and `cleanup.json`. It never kills by process name. Drives stop the precise server instances they started, even after assertion failure. Ctrl-C unwinds the same cleanup path; after a hard kill, run cleanup to remove files. The project discovery link is retained, not treated as a temporary run resource.

## Helpers

The executable helper is `drive.py`; its supported commands appear above. `python3 "$DRIVER" --help` lists actions and feature selections. `make verify` runs repository checks; `make smoke` runs the built binary against a local worker with structured stderr assertions. Run both after verification-helper changes.

Use `/maintain-verification-skill` when commands or the contract change. Update the feature map and runtime checks together; keep unresolved integration checks explicit.
