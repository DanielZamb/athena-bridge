#!/usr/bin/env python3
"""Opt-in live AWS checks. Local tests never invoke this script."""

import argparse
import csv
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import re
import signal
import subprocess
import sys
import tempfile
import time
import uuid


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "sandbox"))


def validate_diagnostics(stderr, exit_code):
    events = [json.loads(line) for line in stderr.splitlines()]
    recoveries = {"none", "fix_arguments", "check_configuration", "retry_operation",
        "retry_same_request", "inspect_query", "wait_query", "check_output", "check_worker"}
    for event in events:
        assert event["version"] == 1 and event["type"] in ("notice", "error"), event
        for key in ("code", "operation", "message"):
            assert isinstance(event[key], str) and event[key], event
        assert event["recovery"] in recoveries, event
        assert type(event["partial_output"]) is bool and type(event["published"]) is bool, event
        assert type(event["exit_code"]) is int, event
        for key in ("request_id", "query_id", "worker_code"):
            if key in event:
                assert re.fullmatch(r"[A-Za-z0-9._:-]{1,256}", event[key]), event
        assert not any(text in event["message"] for text in ("https://", "s3://", "arn:aws:", "SELECT ")), event
    errors = [event for event in events if event["type"] == "error"]
    if exit_code:
        assert len(errors) == 1 and events[-1] == errors[0], events
        assert errors[0]["exit_code"] == exit_code, events
    else:
        assert not errors and all(event["exit_code"] == 0 for event in events), events
    return events


def validate_status(status, query_id):
    assert status["query_id"] == query_id, status
    assert status["state"] in ("QUEUED", "RUNNING", "SUCCEEDED", "FAILED", "CANCELLED"), status
    for key, value in status.get("statistics", {}).items():
        if key == "result_reused":
            assert type(value) is bool, status
        elif key in ("data_scanned_bytes", "engine_execution_ms", "total_execution_ms", "queue_ms", "planning_ms"):
            assert isinstance(value, str) and re.fullmatch(r"[0-9]+", value), status
    for key, value in status.get("failure", {}).items():
        if key == "retryable":
            assert type(value) is bool, status
        elif key in ("error_category", "error_type", "sql_line", "sql_column"):
            assert type(value) is int and value >= (1 if key.startswith("sql_") else 0), status
    assert not any(text in status.get("reason", "") for text in ("s3://", "arn:aws:", "https://", "SELECT ")), status


def interrupt_export(process, checkpoint):
    """Interrupt after checkpoint progress without depending on its private schema."""
    previous = None
    deadline = time.monotonic() + 90
    while process.poll() is None and time.monotonic() < deadline:
        if checkpoint.exists():
            current = checkpoint.read_bytes()
            if previous and current and current != previous:
                process.send_signal(signal.SIGINT)
                return
            if current:
                previous = current
        time.sleep(0.05)
    raise AssertionError("Export did not advance a checkpoint while running; interruption was not exercised.")


def validate_export_receipt(receipt, destination, query_id, rows, file_format, checkpoint=None):
    assert receipt["query_id"] == query_id and receipt["result_id"], receipt
    assert receipt["path"] == str(destination.resolve()) and receipt["format"] == file_format, receipt
    assert receipt["rows"] == rows and receipt["published"] is True, receipt
    data = destination.read_bytes()
    assert receipt["bytes"] == len(data) and receipt["sha256"] == hashlib.sha256(data).hexdigest(), receipt
    checkpoint = checkpoint or Path(str(destination) + ".checkpoint.json")
    assert receipt["checkpoint"] == str(checkpoint.resolve()) and checkpoint.is_file(), receipt


def check_fixture_source(expected_hashes):
    from fixtures import write
    with tempfile.TemporaryDirectory(prefix="athena-source-fixtures-") as directory:
        write(directory)
        actual = {path.stem: hashlib.sha256(path.read_bytes()).hexdigest() for path in Path(directory).glob("*.jsonl")}
    if actual != expected_hashes:
        raise AssertionError("Deployed fixture hashes differ from fixtures regenerated from this source revision.")
    return actual


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest", default=".scratch/cloud-lab/manifest.json")
    parser.add_argument("--check-export", action="store_true",
        help="also check JSON and CSV export, interruption, and checkpoint resume")
    args = parser.parse_args()
    manifest_path = Path(args.manifest).resolve()
    manifest = json.loads(manifest_path.read_text())
    outputs = manifest["outputs"]
    directory = manifest_path.parent / ("verification-" + uuid.uuid4().hex[:8])
    directory.mkdir()
    checks = []
    receipts = []
    source_commit = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    source_status = subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT, text=True)
    assert not source_status, "Commit tracked changes before binding live verification to a source revision."
    binary = ROOT / "bin/athena-bridge"
    report = {"account": manifest["account"], "stack": manifest["stack_name"], "state": "running", "checks": checks,
        "check_export": args.check_export,
        "source_commit": source_commit, "binary_sha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
        "worker_sha256": hashlib.sha256((ROOT / "sandbox/worker.py").read_bytes()).hexdigest(),
        "template_sha256": manifest["template_hash"], "fixture_sha256": manifest["fixture_hashes"], "receipts": receipts}

    def checkpoint(check):
        checks.append(check)
        (directory / "report.json").write_text(json.dumps(report, indent=2) + "\n")

    def cli(command, *arguments, exit_code=0, error_code=None, worker_code=None, recovery=None,
            output_format="json", interrupt_checkpoint=None):
        argv = [str(binary), command, "--profile", manifest["profile"]]
        resuming = command == "export" and "--resume" in arguments
        if not resuming:
            argv.extend(["--region", manifest["region"], "--function", outputs["Function"]])
            if output_format is not None:
                argv.extend(["--format", output_format])
        argv.extend(arguments)
        stem = f"{len(receipts) + 1:03d}-{command}"
        # Files also preserve diagnostics if a probe times out or interruption fails.
        with (directory / (stem + ".out")).open("w+") as stdout, (directory / (stem + ".err")).open("w+") as stderr:
            with subprocess.Popen(argv, stdout=stdout, stderr=stderr, text=True, cwd=ROOT) as process:
                try:
                    if interrupt_checkpoint is not None:
                        interrupt_export(process, interrupt_checkpoint)
                    process.wait(timeout=150)
                except BaseException:
                    process.kill()
                    process.wait()
                    raise
            stdout.seek(0)
            stderr.seek(0)
            output, diagnostics = stdout.read(), stderr.read()
        receipts.append({"argv": argv, "exit_code": process.returncode, "expected_exit_code": exit_code,
            "stdout": stem + ".out", "stderr": stem + ".err", "timestamp": datetime.now(timezone.utc).isoformat()})
        (directory / "report.json").write_text(json.dumps(report, indent=2) + "\n")
        if process.returncode != exit_code:
            raise AssertionError(f"{command} returned {process.returncode}, expected {exit_code}. {diagnostics}")
        events = validate_diagnostics(diagnostics, exit_code)
        if exit_code:
            for key, expected in (("code", error_code), ("worker_code", worker_code), ("recovery", recovery)):
                if expected is not None:
                    assert events[-1][key] == expected, events
            assert not output, "A failed probe unexpectedly wrote to stdout."
            if interrupt_checkpoint is not None:
                assert events[-1]["partial_output"] is True and events[-1]["published"] is False, events
        for flag, field in (("--query-id", "query_id"), ("--request-id", "request_id")):
            if flag in arguments:
                identity = arguments[arguments.index(flag) + 1]
                assert all(event.get(field) == identity for event in events), events
        result = json.loads(output) if output.strip() and (command == "export" or output_format in ("json", None)) else None
        if command in ("query", "start") and not exit_code:
            assert [event["code"] for event in events[:2]] == ["START_REQUEST", "QUERY_STARTED"], events
            assert events[0]["request_id"] == events[1]["request_id"], events
            assert events[1]["query_id"] == result["query_id"], events
        if command == "export" and not exit_code:
            assert events[-1]["code"] == "OUTPUT_PUBLISHED" and events[-1]["published"] is True, events
            assert events[-1]["partial_output"] is False and events[-1]["query_id"] == result["query_id"], events
        return result

    report["source_fixture_sha256"] = check_fixture_source(manifest["fixture_hashes"])
    for table, expected_hash in manifest["fixture_hashes"].items():
        local = manifest_path.parent / "fixtures" / (table + ".jsonl")
        assert hashlib.sha256(local.read_bytes()).hexdigest() == expected_hash, table
        remote = directory / (table + ".jsonl")
        process = subprocess.run(["aws", "--profile", manifest["profile"], "--region", manifest["region"], "s3api", "get-object",
            "--bucket", outputs["Bucket"], "--key", f"data/{table}/rows.jsonl", str(remote)], capture_output=True, text=True, timeout=60)
        assert process.returncode == 0, process.stderr
        assert hashlib.sha256(remote.read_bytes()).hexdigest() == expected_hash, table
    from cloud import Aws
    from stack import MAX_SCAN_BYTES, RESULT_RETENTION_DAYS, template
    body = json.dumps(template(manifest["stack_name"]), sort_keys=True)
    assert hashlib.sha256(body.encode()).hexdigest() == manifest["template_hash"]
    aws = Aws(manifest["profile"], manifest["region"], manifest["account"])
    aws.verify_account()
    deployed = aws.call("cloudformation", "get-template", StackName=manifest["stack_id"], TemplateStage="Original")["TemplateBody"]
    deployed = json.loads(deployed) if isinstance(deployed, str) else deployed
    assert deployed == json.loads(body), "The deployed template differs from this source revision."
    checkpoint("Source revision, worker/template hashes, and every deployed fixture object are bound to this verification")

    tables = cli("tables", "--database", outputs["Database"], "--all", "--page-size", "1")
    assert len(tables["rows"]) == 5 and tables["complete"]
    described = cli("describe", "--database", outputs["Database"], "--table", "ac_us_client_events", "--all", "--page-size", "1")
    assert [row[0] for row in described["rows"]] == ["event_id", "session_id", "account_no", "user_id", "event_time", "event_name", "latency_ms"]
    checkpoint("Metadata discovery and page-size-one pagination")

    info = cli("info")
    assert set(info) == {"worker_id", "basis", "default_database", "databases", "catalog", "workgroup", "actions", "features", "limits"}, info
    assert info["basis"] == "configured" and info["worker_id"] == manifest["stack_name"] + "-worker", info
    assert info["databases"] == [outputs["Database"]] and info["default_database"] == outputs["Database"], info
    assert info["catalog"] == "AwsDataCatalog" and info["workgroup"] == outputs["Workgroup"], info
    assert set(info["actions"]) == {"worker_info", "list_tables", "describe_table", "search_metadata", "start_query",
        "query_status", "cancel_query", "get_results", "download_result"}, info
    assert info["features"] == ["result_identity"], info
    assert info["limits"] == {"default_page_size": 200, "max_page_size": 1000, "max_response_bytes": 4000000,
        "metadata_tables_per_call": 100, "max_scan_bytes": MAX_SCAN_BYTES, "max_sql_bytes": 262144,
        "cursor_ttl_seconds": 3600, "result_retention_days": RESULT_RETENTION_DAYS}, info
    assert not any(value in json.dumps(info) for value in (outputs["Bucket"], "arn:aws:iam:", "s3://")), info
    checkpoint("Configured worker identity, scope, actions and limits expose only the agreed fields; zero-IAM behavior is covered locally")

    from fixtures import SCHEMAS, TABLE_COMMENTS
    search_args = ("--database", outputs["Database"], "--term", "SESSION", "--kind", "column", "--table", "AC_US_SESSIONS", "--page-size", "1")
    search_first = cli("search", *search_args)
    assert search_first["columns"] == [{"name": name, "type": "varchar"} for name in
        ("kind", "table_name", "column_name", "data_type", "description", "partition_key")]
    assert search_first["next_token"] and not search_first["complete"], search_first
    search_rest = cli("search", *search_args, "--next-token", search_first["next_token"], "--all")
    expected_search = [["column", "ac_us_sessions", name, kind, comment, "false"]
        for name, kind, comment in SCHEMAS["ac_us_sessions"] if "session" in name.casefold() or "session" in comment.casefold()]
    assert search_first["rows"] + search_rest["rows"] == expected_search and search_rest["complete"], search_rest
    assert search_rest["columns"] == search_first["columns"]
    search_tables = cli("search", "--term", "synthetic", "--kind", "table", "--all", "--page-size", "1")
    assert sorted(row[1] for row in search_tables["rows"]) == sorted(TABLE_COMMENTS) and search_tables["complete"]
    literal = cli("search", "--term", "%", "--kind", "all", "--all")
    assert literal["rows"] == [] and literal["complete"], literal
    cli("search", "--term", "different", "--kind", "column", "--table", "ac_us_sessions",
        "--next-token", search_first["next_token"], exit_code=1, error_code="WORKER_REJECTED", worker_code="INVALID_CURSOR")
    cli("search", "--term", "session", "--kind", "invalid", output_format="csv", exit_code=2,
        error_code="INVALID_ARGUMENT", recovery="fix_arguments")
    checkpoint("Literal case-insensitive metadata names/comments, exact table, pagination, filter-bound cursors and JSONL errors with CSV stdout mode")

    sql = "SELECT CAST('9007199254740993' AS bigint) AS exact_integer, CAST('1.2300' AS decimal(20,4)) AS exact_decimal, CAST(NULL AS varchar) AS nullable, '' AS empty, '\\N' AS literal UNION ALL SELECT 2, CAST('2.3400' AS decimal(20,4)), 'present', '', 'x' ORDER BY exact_integer DESC"
    token = uuid.uuid4().hex
    first_start = cli("start", "--sql", sql, "--request-id", token)
    assert cli("start", "--sql", sql, "--request-id", token) == first_start
    cli("start", "--sql", "SELECT 2", "--request-id", token, exit_code=1,
        error_code="WORKER_REJECTED", worker_code="InvalidRequestException", recovery="retry_same_request")
    query_id = first_start["query_id"]
    first = cli("results", "--query-id", query_id, "--wait", "--page-size", "1")
    assert first["rows"] == [["9007199254740993", "1.2300", None, "", "\\N"]], first
    second = cli("results", "--query-id", query_id, "--page-size", "1", "--next-token", first["next_token"])
    assert second["rows"] == [["2", "2.3400", "present", "", "x"]] and second["complete"], second
    all_rows = cli("results", "--query-id", query_id, "--all", "--page-size", "1")
    assert all_rows["rows"] == first["rows"] + second["rows"]
    status = cli("status", "--query-id", query_id)
    validate_status(status, query_id)
    assert status["state"] == "SUCCEEDED" and status["statistics"]["data_scanned_bytes"] == "0", status
    checkpoint("Idempotency, start/status/results, exact integers/decimals/null/empty/backslash cells")
    checkpoint("Status statistics preserve exact decimal strings and known zero; optional values retain their types")

    terminal = cli("cancel", "--query-id", query_id)
    validate_status(terminal, query_id)
    assert terminal["state"] == "SUCCEEDED" and terminal["cancellation_requested"] is False, terminal
    accepted = False
    for _ in range(3):
        cancel_id = cli("start", "--sql", "SELECT count(*) FROM ac_us_sessions a CROSS JOIN ac_us_sessions b")["query_id"]
        cancellation = cli("cancel", "--query-id", cancel_id)
        validate_status(cancellation, cancel_id)
        assert type(cancellation["cancellation_requested"]) is bool, cancellation
        if not cancellation["cancellation_requested"]:
            assert cancellation["state"] in ("SUCCEEDED", "FAILED", "CANCELLED"), cancellation
            continue
        accepted = True
        # Stop acceptance need not return CANCELLED, and a terminal race is valid.
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            observed = cli("status", "--query-id", cancel_id)
            validate_status(observed, cancel_id)
            if observed["state"] in ("SUCCEEDED", "FAILED", "CANCELLED"):
                break
            time.sleep(1)
        else:
            raise AssertionError("Accepted cancellation did not reach a terminal state within 60 seconds.")
        again = cli("cancel", "--query-id", cancel_id)
        assert again["cancellation_requested"] is False and again["state"] == observed["state"], again
        report.update(cancellation_query_id=cancel_id, cancellation_observed_state=observed["state"])
        break
    assert accepted, "All three queries finished before cancellation; the stop-acceptance path was not exercised."
    cli("cancel", "--query-id", "not-a-query", exit_code=1, error_code="WORKER_REJECTED", worker_code="NOT_ALLOWED", recovery="none")
    checkpoint("CLI cancellation authorizes scope, distinguishes acceptance from completion, and handles terminal no-op cancellation")

    destination = directory / "native.csv"
    cli("download", "--query-id", query_id, "--out", str(destination))
    native = directory / "s3-original.csv"
    process = subprocess.run(["aws", "--profile", manifest["profile"], "--region", manifest["region"], "s3api", "get-object", "--bucket", outputs["Bucket"],
        "--key", f"results/{query_id}.csv", str(native)], capture_output=True, text=True, timeout=60)
    assert process.returncode == 0, process.stderr
    assert destination.read_bytes() == native.read_bytes()
    with destination.open(newline="") as handle:
        assert list(csv.reader(handle))[1] == ["9007199254740993", "1.2300", "", "", "\\N"]
    checkpoint("Presigned HTTPS download equals the complete S3 object byte for byte")
    before = destination.read_bytes()
    cli("download", "--query-id", query_id, "--out", str(destination), output_format="table", exit_code=1,
        error_code="LOCAL_IO", recovery="check_output")
    assert destination.read_bytes() == before
    checkpoint("No-overwrite download failure preserves the destination and emits JSONL with table stdout mode")

    columns = ["account_no", "user_id", "session_id", "session_start", "reporting_month", "product"]
    sql = "SELECT " + ", ".join(columns) + " FROM ac_us_sessions ORDER BY account_no, user_id, session_id"
    sessions = [json.loads(line) for line in (directory / "ac_us_sessions.jsonl").read_text().splitlines()]
    expected = [[row[column] for column in columns] for row in sorted(sessions, key=lambda row: (row["account_no"], row["user_id"], row["session_id"]))]
    assert len(expected) > 1000, "Fixture must cross Athena's native 1000-row page boundary."
    large_id = cli("start", "--sql", sql)["query_id"]
    first_large = cli("results", "--query-id", large_id, "--wait", "--page-size", "997")
    assert first_large["rows"] == expected[:997] and not first_large["complete"]
    rest = cli("results", "--query-id", large_id, "--page-size", "997", "--next-token", first_large["next_token"], "--all")
    assert rest["rows"] == expected[997:] and rest["complete"]
    assert rest["columns"] == first_large["columns"]
    large_all = cli("results", "--query-id", large_id, "--page-size", "1000", "--all")
    assert large_all["rows"] == expected and large_all["complete"]
    large_csv = directory / "sessions.csv"
    cli("download", "--query-id", large_id, "--out", str(large_csv))
    with large_csv.open(newline="") as handle:
        assert list(csv.reader(handle)) == [columns] + expected
    report.update(native_boundary_query_id=large_id, native_boundary_rows=len(expected))
    checkpoint("Every session matches raw fixture rows across native Athena pages, explicit resume, --all, and complete download")

    count = cli("query", "--sql", "SELECT count(*) AS total FROM ac_us_sessions")
    assert int(count["rows"][0][0]) > 0
    cli("results", "--query-id", count["query_id"], "--next-token", first["next_token"], exit_code=1,
        error_code="WORKER_REJECTED", worker_code="INVALID_CURSOR")
    cli("tables", "--database", "unrelated_database", exit_code=1, error_code="WORKER_REJECTED", worker_code="NOT_ALLOWED", recovery="none")
    cli("query", "--sql", "CREATE TABLE forbidden AS SELECT 1", exit_code=1,
        error_code="WORKER_REJECTED", worker_code="NOT_ALLOWED", recovery="none")
    failed_id = cli("start", "--sql", "SELECT 1 / 0 AS invalid")["query_id"]
    cli("results", "--query-id", failed_id, "--wait", exit_code=1, error_code="QUERY_FAILED", recovery="inspect_query")
    failed_status = cli("status", "--query-id", failed_id)
    validate_status(failed_status, failed_id)
    assert failed_status["state"] == "FAILED" and failed_status["reason"], failed_status
    assert "1 / 0" not in failed_status["reason"], failed_status
    assert "failure" in failed_status, failed_status
    checkpoint("Real Athena SQL, query-scoped cursors, denied writes/databases, and nonzero failed-query exits")
    checkpoint("Every CLI stderr line is a versioned diagnostic with typed recovery and known request/query identity")

    if args.check_export:
        # Resume takes only the checkpoint, profile and optional timeout override.
        export_json = directory / "export-sessions.json"
        receipt = cli("export", "--query-id", large_id, "--out", str(export_json))
        validate_export_receipt(receipt, export_json, large_id, len(expected), "json")
        exported = json.loads(export_json.read_text())
        assert exported["query_id"] == large_id and exported["columns"] == large_all["columns"], exported
        assert exported["rows"] == expected and exported["complete"] and exported["next_token"] is None
        cli("export", "--query-id", large_id, "--out", str(export_json), exit_code=1,
            error_code="LOCAL_IO", recovery="check_output")
        assert hashlib.sha256(export_json.read_bytes()).hexdigest() == receipt["sha256"]

        export_csv = directory / "export-exact.csv"
        csv_receipt = cli("export", "--query-id", query_id, "--out", str(export_csv), output_format="csv")
        validate_export_receipt(csv_receipt, export_csv, query_id, len(all_rows["rows"]), "csv")
        with export_csv.open(newline="") as handle:
            csv_rows = list(csv.reader(handle))
        assert csv_rows[0] == [column["name"] for column in all_rows["columns"]]
        decoded = [[None if cell == "\\N" else cell.replace("\\\\", "\\") for cell in row] for row in csv_rows[1:]]
        assert decoded == all_rows["rows"], decoded
        checkpoint("Checkpoint API fresh JSON/CSV exports match all rows, column order and exact/null/empty/backslash cells")

        resumed_json = directory / "resumed-sessions.json"
        progress = directory / "sessions.checkpoint"
        cli("export", "--query-id", large_id, "--out", str(resumed_json), "--checkpoint", str(progress),
            exit_code=3, error_code="CANCELLED", interrupt_checkpoint=progress)
        assert progress.is_file() and not resumed_json.exists(), "Interrupted export lost progress or published early."
        resumed = cli("export", "--resume", str(progress), "--timeout", "2m")
        validate_export_receipt(resumed, resumed_json, large_id, len(expected), "json", progress)
        assert resumed["result_id"] == receipt["result_id"], resumed
        assert json.loads(resumed_json.read_text()) == exported
        sealed = resumed_json.read_bytes()
        repeated = cli("export", "--resume", str(progress))
        assert repeated == resumed, repeated
        assert resumed_json.read_bytes() == sealed, "Repeated resume changed a published result."
        checkpoint("SIGINT retains an advancing checkpoint; resume retrieves the same query without duplicates and repeats publication safely")
    report.update(state="passed", probe_query_id=query_id, failed_query_id=failed_id,
        native_sha256=hashlib.sha256(destination.read_bytes()).hexdigest())
    (directory / "report.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report, indent=2))


if __name__ == "__main__":
    main()
