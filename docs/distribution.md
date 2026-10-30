# Run Athena Bridge

This package contains the CLI, an agent skill, and an example Lambda worker. The CLI needs no Go installation, Python interpreter, system SQLite, or external SQL files.

## Configure the CLI

Extract the archive for your operating system and architecture. Run these commands from the extracted `athena-bridge` directory:

```sh
./athena-bridge version
./athena-bridge --help
export AWS_PROFILE=athena-bridge
export AWS_REGION=us-east-1
export ATHENA_BRIDGE_FUNCTION=your-worker-name
./athena-bridge info --format json
```

On macOS, an archive downloaded in a browser is quarantined, and macOS refuses to run the unsigned executable. Remove the mark from the extracted directory, then run the commands again:

```sh
xattr -dr com.apple.quarantine .
```

Use an existing AWS SDK credential profile, SSO session, or role. The caller needs permission to invoke the selected Lambda worker. Keep credentials in the SDK credential chain, outside this package.

On Windows, run `.\athena-bridge.exe`. Set configuration in PowerShell:

```powershell
$env:AWS_PROFILE = 'athena-bridge'
$env:AWS_REGION = 'us-east-1'
$env:ATHENA_BRIDGE_FUNCTION = 'your-worker-name'
.\athena-bridge.exe info --format json
```

## Use the CLI with an agent

Load the download's `SKILL.md` in your agent. Keep it beside the executable, or put the executable on `PATH`. The skill covers schema discovery, SQL, exact result values, pagination, exit codes, and recovery.

```sh
./athena-bridge tables --database analytics --all --format json
./athena-bridge query --database analytics --sql 'SELECT 1' --all --format json
```

Every invocation attempts to record local usage metadata in the user's state directory. Set `ATHENA_BRIDGE_USAGE_DB=off` to disable recording. No usage database is bundled.

## Resume a checkpoint export

```sh
./athena-bridge export --query-id QUERY_ID --out results.json --format json \
  --checkpoint results.checkpoint
./athena-bridge export --resume results.checkpoint
```

Destination directories must exist. Export preserves progress across interruption and requires a worker supporting result identity. An existing output file is never replaced.

## Set up a worker

If you already have a compatible worker, supply its function name or ARN. Otherwise, follow `worker/README.md` in the download to adapt the example worker to your AWS environment. The included `docs/lambda-contract.md` defines its requests and responses. Deployment requires separate worker permissions and configuration.

## Fix a failing command

Every failure prints a JSON diagnostic on standard error with a `code`, a `recovery` action, and a nonzero exit code. `docs/troubleshooting.md` explains each code and lists what to include in a bug report.

The package includes `LICENSE` and `THIRD_PARTY_NOTICES.txt`.
