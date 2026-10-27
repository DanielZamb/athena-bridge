# Athena Bridge CLI verification map

Use the parent skill's launch, doctor, drive, evidence, and cleanup commands. Each row selects an independent recipe with `--feature ID`; omitting selection runs all rows. Evidence lives in the selected absolute run directory after cleanup. A passed recipe covers the assertions listed in its feature file, not every possible worker behavior.

| ID | User entry points | Local proof |
| --- | --- | --- |
| [build-config](build-config.md) | build, version, help, every command's flags, profile/region/function | Build and argument/configuration precedence |
| [transport](transport.md) | all signed worker invocations | Real SDK SigV4, credential chain, envelope and transport failures |
| [discovery](discovery.md) | info, tables, describe, search | Configured metadata and literal search argument forwarding |
| [sql](sql.md) | start, query, SQL text/file/stdin, request IDs | Exact input, stable retry token and isolated worker idempotency fixture |
| [queries](queries.md) | query, status, results --wait, cancel | All states, statistics, cancellation acceptance, deadline and Ctrl-C |
| [pagination](pagination.md) | results, metadata pagination, --all, --next-token | Complete/partial traversal and response-integrity rejection |
| [formats](formats.md) | --format json/csv/table | Exact numbers, nulls, duplicate names, quoting and controls |
| [atomic](atomic.md) | tabular --out | Private, no-overwrite publication and failure cleanup |
| [downloads](downloads.md) | download | Real local HTTPS, unchanged bytes, rejected redirects/expiry/limits |
| [checkpoints](checkpoints.md) | export, export --resume | Real process crashes, byte-identical JSON/CSV recovery, locks, cursor replay and corruption rejection |
| [pretty](pretty.md) | --pretty on discovery, results, status, errors and receipts | Human presentation, exact files, tokens, export/resume and unchanged machine defaults |
| [usage](usage.md) | every invocation; ATHENA_BRIDGE_USAGE_DB | Millisecond SQLite rows, launcher/agent/model, transcript privacy, v1 migration, paired latency, stream parity, locks and concurrent processes |

## Baseline and reporting

Each drive owns OS-assigned loopback ports and a disposable named profile. Its binary is built from this checkout and checked by hash. Python uses the existing local Lambda HTTP handler and actual CLI subprocesses. No test contacts AWS. No packages or separate agents are required.

Read numbered command receipts together with stdout, stderr, resulting files and `summary.json`. `server.json` must report no handler failures and stopped threads. Cleanup removes temporary resources and retains those receipts. A failure exits nonzero; platform-specific pending checks remain explicit in the summary.

Doctor checks only the checkout-local link's resolution for registration.

## Uncovered checks

- Native macOS/Windows cannot trust a temporary HTTPS certificate through SSL_CERT_FILE. Successful local HTTPS, redirect/expiry/truncation/actual-byte-limit and credential-forwarding checks require the Linux recipe; native runs prove TLS rejection and pre-network checks. Existing Go tests exercise trusted local TLS independently.
- The export recipe exercises real crashes in the writing phase and idempotent published resumes. Exact finishing/sealed crash windows, power loss, directory sync failures and replay with different page boundaries remain separate fault checks.
- Local fixtures do not prove worker-side SQL policy, literal/case-insensitive Glue matching, scope authorization, Athena cancellation completion, real S3 identity or cursor expiry. Those require separately authorized worker or live-sandbox checks.
- SDK SSO/role/credential-process chains, real service retries, cross-platform installation, disk-full/fsync/hard-link faults, an output-publication race, broken downstream pipes, and individual 30-second invocation expiry are not driven by these local recipes. Repository Go tests cover some of these boundaries; do not infer end-to-end proof from that.
