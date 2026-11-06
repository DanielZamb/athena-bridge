#!/usr/bin/env python3
"""Drive the built CLI at its Lambda HTTP boundary; keep receipts after cleanup."""

import argparse
import csv
import hashlib
import hmac
import http.server
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import shutil
import signal
import sqlite3
import ssl
import subprocess
import sys
import threading
import time
import uuid


ROOT = Path(__file__).resolve().parents[3]
BASE = ROOT / ".scratch/verify-athena-bridge"
FEATURES = ("build-config", "transport", "discovery", "sql", "queries",
            "pagination", "formats", "atomic", "downloads", "checkpoints", "pretty", "usage")
sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("local_smoke", ROOT / "scripts/verify-local.py")
smoke = importlib.util.module_from_spec(spec)
spec.loader.exec_module(smoke)
COLS = [{"name": "value", "type": "bigint"}, {"name": "value", "type": "decimal(30,4)"},
        {"name": "note", "type": "varchar"}]
ROWS = [["9007199254740993", "12345678901234567890.2300", None],
        ["2", "0.0000", ""], ["3", "-1.2300", "\\N"],
        ["4", "1.0000", "comma,quote\"\nline\ttab\\end"]]
TOKEN = "0123456789abcdef0123456789abcdef"
PAYLOAD = b"country,events\nCO,9007199254740993\n"


def dump(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def environment(resources):
    env = {k: v for k, v in os.environ.items()
           if not k.startswith(("AWS_", "ATHENA_BRIDGE_"))
           and k not in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy")}
    env.update(AWS_PROFILE="local-proof", AWS_SHARED_CREDENTIALS_FILE=str(resources / "credentials"),
               AWS_CONFIG_FILE=str(resources / "config"), AWS_EC2_METADATA_DISABLED="true",
               AWS_MAX_ATTEMPTS="1", ATHENA_BRIDGE_FUNCTION="local-worker", ATHENA_BRIDGE_USAGE_DB="off", NO_PROXY="*")
    return env


def load_run(path):
    run = Path(path).resolve()
    if run.parent != BASE.resolve():
        raise ValueError(f"Run must be an immediate child of {BASE}")
    state = json.loads((run / "state.json").read_text())
    if state["root"] != str(ROOT) or state["run"] != str(run):
        raise ValueError("Run belongs to a different checkout")
    return run, state


def launch(path):
    run = Path(path).resolve() if path else BASE / (time.strftime("%Y%m%dT%H%M%S") + "-" + uuid.uuid4().hex[:8])
    if run.parent != BASE.resolve():
        raise ValueError(f"Run must be an immediate child of {BASE}")
    run.mkdir(parents=True, exist_ok=False)
    resources = run / "resources"
    resources.mkdir(mode=0o700)
    state = {"root": str(ROOT), "run": str(run), "cleaned": False,
             "discovery_link_owned": ".claude/skills/verify-athena-bridge"}
    dump(run / "state.json", state)
    try:
        (resources / "credentials").write_text("[local-proof]\naws_access_key_id=local-access\naws_secret_access_key=local-secret\n")
        (resources / "credentials").chmod(0o600)
        (resources / "config").write_text("[profile local-proof]\nregion=us-east-1\n")
        result = subprocess.run(["go", "build", "-o", str(resources / "athena-bridge"), "./cmd/athena-bridge"],
                                cwd=ROOT, capture_output=True, text=True, timeout=120)
        dump(run / "build.json", {"argv": ["go", "build", "-o", str(resources / "athena-bridge"), "./cmd/athena-bridge"],
                                  "stdout": result.stdout, "stderr": result.stderr, "exit_code": result.returncode})
        if result.returncode:
            raise RuntimeError("Build failed; see build.json")
        state["binary_sha256"] = digest(resources / "athena-bridge")
        state["source_revision"] = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
        state["source_status"] = subprocess.check_output(["git", "status", "--short"], cwd=ROOT, text=True)
        dump(run / "state.json", state)
    except BaseException:
        cleanup(run)
        raise
    print(run)


def doctor(run):
    run, state = load_run(run)
    resources = run / "resources"
    assert not state["cleaned"], "Run was cleaned; launch a fresh run"
    assert digest(resources / "athena-bridge") == state["binary_sha256"], "Binary changed"
    assert (ROOT / ".claude/skills/verify-athena-bridge").resolve() == Path(__file__).resolve().parent
    result = subprocess.run([str(resources / "athena-bridge"), "version"], env=environment(resources),
                            capture_output=True, text=True, timeout=5)
    assert result.returncode == 0 and result.stdout.startswith("athena-bridge ") and not result.stderr
    assert (resources / "credentials").stat().st_mode & 0o777 == 0o600
    assert shutil.which("openssl"), "Local HTTPS verification needs openssl; do not install packages automatically"
    proof = {"binary_sha256": state["binary_sha256"], "version": result.stdout.strip(),
             "link_resolves_in_checkout": True,
             "aws_contacted": False}
    print(json.dumps(proof))
    return proof


def cleanup(run):
    run, state = load_run(run)
    resources = run / "resources"
    if resources.is_symlink():
        raise ValueError("Refusing a symlink resource directory")
    if resources.exists():
        shutil.rmtree(resources)
    state["cleaned"] = True
    dump(run / "state.json", state)
    dump(run / "cleanup.json", {"resources_removed": not resources.exists(),
                                 "receipts_retained": True, "processes": "drive closes and joins its own servers",
                                 "discovery_link": "retained as a project deliverable"})
    print(f"Cleaned {resources}; receipts retained in {run}")


class Download(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        self.server.requests.append({"path": self.path.split("?")[0],
                                     "authorization_present": bool(self.headers.get("Authorization")),
                                     "session_token_present": bool(self.headers.get("X-Amz-Security-Token"))})
        mode = self.server.mode
        code = 302 if mode == "redirect" else 403 if mode == "expired" else 200
        self.send_response(code)
        if mode == "redirect":
            self.send_header("Location", self.server.url + "/redirect-target")
        self.send_header("Content-Length", str(len(PAYLOAD) + (10 if mode == "truncated" else 0)))
        self.end_headers()
        self.wfile.write(PAYLOAD)


class Harness:
    def __init__(self, run):
        self.run, self.state = load_run(run)
        self.evidence = self.run / ("drive-" + uuid.uuid4().hex[:8])
        self.evidence.mkdir()
        self.resources = self.run / "resources" / self.evidence.name
        self.resources.mkdir(mode=0o700)
        for name in ("athena-bridge", "credentials", "config"):
            shutil.copy2(self.run / "resources" / name, self.resources / name)
        assert digest(self.resources / "athena-bridge") == self.state["binary_sha256"]
        self.env = environment(self.resources)
        self.counter = 0
        self.requests = []
        self.starts = {}
        self.calls = []
        self.status_seen = threading.Event()
        self.export_blocked = threading.Event()
        self.export_release = threading.Event()
        self.mode = "normal"
        self.poll = 0
        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), smoke.Worker)
        self.server.respond = self.respond
        self.server.validate = self.validate
        self.server.region = "us-east-1"
        self.server.function = "local-worker"
        self.server.RequestHandlerClass.failures = []
        self.env["AWS_ENDPOINT_URL_LAMBDA"] = f"http://127.0.0.1:{self.server.server_port}"
        self.threads = []
        self.download = None

    def __enter__(self):
        self.start_server(self.server)
        return self

    def start_server(self, server):
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.threads.append((server, thread))

    def __exit__(self, *args):
        for server, thread in reversed(self.threads):
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)
        dump(self.evidence / "worker-requests.json", self.requests)
        dump(self.evidence / "server.json", {"handler_failures": self.server.RequestHandlerClass.failures,
                                            "all_threads_stopped": all(not t.is_alive() for _, t in self.threads)})

    def validate(self, handler, body):
        # Verify the real SDK's signature using only disposable fake credentials.
        auth = handler.headers["Authorization"]
        fields = dict(part.split("=", 1) for part in auth.split(" ", 1)[1].split(", "))
        credential = fields["Credential"].split("/")
        names = fields["SignedHeaders"].split(";")
        headers = "".join(name + ":" + " ".join(handler.headers[name].split()) + "\n" for name in names)
        canonical = "\n".join(("POST", handler.path, "", headers, fields["SignedHeaders"], hashlib.sha256(body).hexdigest()))
        string = "\n".join(("AWS4-HMAC-SHA256", handler.headers["X-Amz-Date"], "/".join(credential[1:]),
                              hashlib.sha256(canonical.encode()).hexdigest()))
        key = b"AWS4local-secret"
        for part in credential[1:]:
            key = hmac.new(key, part.encode(), hashlib.sha256).digest()
        assert hmac.compare_digest(hmac.new(key, string.encode(), hashlib.sha256).hexdigest(), fields["Signature"])
        assert handler.headers.get("X-Amz-Invocation-Type") == "RequestResponse"
        assert handler.headers.get("X-Amz-Log-Type") == "None"
        req = json.loads(body)
        self.requests.append({"request": req, "signature_valid": True, "region": credential[2],
                              "function": self.server.function})

    def respond(self, req):
        action = req["action"]
        mode = self.mode
        if mode == "bad-json":
            return b'{"version":1} trailing'
        if mode == "bad-version":
            return {"version": 2, "result": {}}
        if mode == "both-envelope":
            return {"version": 1, "result": {}, "error": {"code": "NO", "message": "No"}}
        if mode == "worker-reject":
            return {"version": 1, "error": {"code": "NOT_ALLOWED", "message": "SELECT private https://secret.invalid/?signature=hidden"}}
        if mode.startswith("worker-code-"):
            return {"version": 1, "error": {"code": mode.removeprefix("worker-code-"),
                    "message": "SELECT private https://secret.invalid/?signature=hidden"}}
        if mode == "sdk-error":
            return {"message": "SELECT private https://secret.invalid/?signature=hidden"}
        if action == "start_query":
            previous = self.starts.setdefault(req["request_id"], req)
            if previous != req:
                return {"version": 1, "error": {"code": "ID_REUSED", "message": "Use identical parameters"}}
            result = {"query_id": "local-query"}
        elif action in ("query_status", "cancel_query"):
            state = "SUCCEEDED"
            if mode.startswith("state-"):
                state = mode[6:]
            if mode == "progress":
                state = ("QUEUED", "RUNNING", "SUCCEEDED")[min(self.poll, 2)]
                self.poll += 1
            result = {"query_id": req["query_id"], "state": state}
            if mode == "statistics":
                result.update(statistics={"data_scanned_bytes": "9007199254740993", "queue_ms": "0", "result_reused": False},
                              failure={"error_category": 0, "error_type": 1, "retryable": False})
            if mode == "bad-statistics":
                result["statistics"] = {"queue_ms": "-1"}
            if mode == "bad-status-id":
                result["query_id"] = "other-query"
            if action == "cancel_query":
                result["cancellation_requested"] = state in ("QUEUED", "RUNNING")
            self.status_seen.set()
        elif action == "worker_info":
            result = {"worker_id": "local-worker", "basis": "configured", "default_database": "analytics",
                      "databases": ["analytics"], "catalog": "AwsDataCatalog", "workgroup": "local",
                      "actions": ["worker_info", "search_metadata", "cancel_query", "get_results"],
                      "features": ["result_identity"], "limits": {"page_size": 1000}}
            if mode == "bad-info":
                del result["basis"]
        elif action == "download_result":
            url = self.download.url + "/data?signature=never-log-this"
            if mode == "http-url":
                url = url.replace("https:", "http:")
            result = {"url": url, "format": "csv", "size_bytes": len(PAYLOAD)}
            if mode == "size-mismatch":
                result["size_bytes"] += 1
            if mode == "unknown-size":
                del result["size_bytes"]
            if mode == "bad-download-format":
                result["format"] = "zip"
        elif action in ("list_tables", "describe_table", "search_metadata", "get_results"):
            second = bool(req.get("next_token"))
            if mode == "export-block" and second:
                self.export_blocked.set()
                assert self.export_release.wait(10), "Export interruption was not released"
            if mode in ("expired-cursor", "expired-changed") and second and not self.expired:
                self.expired = True
                return {"version": 1, "error": {"code": "EXPIRED_CURSOR", "message": "Synthetic expired cursor"}}
            result = {"columns": COLS, "rows": ROWS[2:] if second else ROWS[:2],
                      "next_token": None if second else "local-page-2"}
            if action == "get_results":
                result.update(query_id=req["query_id"], result_id="local-result-v1")
            elif action == "list_tables":
                result.update(columns=[{"name": n, "type": "varchar"} for n in ("table_name", "table_type", "description")],
                              rows=[["events", "EXTERNAL_TABLE", None]], next_token=None)
            elif action == "describe_table":
                result.update(columns=[{"name": n, "type": "varchar"} for n in ("column_name", "data_type", "description", "partition_key")],
                              rows=[["country", "varchar", "Country code", "true"]], next_token=None)
            else:
                result.update(columns=[{"name": n, "type": "varchar"} for n in ("kind", "table_name", "column_name", "data_type", "description", "partition_key")],
                              rows=[["column", "events", "country", "varchar", "Country code", "true"]] if second else [])
            if mode == "empty-first" and not second:
                result["rows"] = []
            if mode == "repeat" and second:
                result["next_token"] = "local-page-2"
            if mode == "changed-columns" and second:
                result["columns"] = [{"name": "different", "type": c["type"]} for c in COLS]
            if mode == "changed-result" and second:
                result["result_id"] = "local-result-v2"
            if mode == "missing-result" and second:
                result.pop("result_id", None)
            if mode == "no-result":
                result.pop("result_id", None)
            if mode == "expired-changed" and not second:
                result["rows"] = [["changed", "12345678901234567890.2300", None], ROWS[1]]
            if mode == "wrong-query":
                result["query_id"] = "other-query"
            if mode == "row-width":
                result["rows"] = [["one"]]
            if mode == "number-cell":
                result["rows"] = [[1, "2", None]]
            if mode == "null-rows":
                result["rows"] = None
            if mode == "bad-column":
                result["columns"] = [{"name": "", "type": "varchar"}]
        else:
            raise AssertionError(f"Unexpected action {action}")
        return {"version": 1, "result": result}

    def record(self, args, result, expected, before, human=False):
        self.counter += 1
        prefix = self.evidence / f"{self.counter:03}"
        prefix.with_suffix(".stdout").write_text(result.stdout)
        prefix.with_suffix(".stderr").write_text(result.stderr)
        dump(prefix.with_suffix(".json"), {"argv": [str(self.resources / "athena-bridge"), *args],
                                           "exit_code": result.returncode, "expected_exit_code": expected,
                                           "worker_requests": self.requests[before:],
                                           "diagnostic_format": "human" if human else "jsonl"})
        events = [] if human else [json.loads(line) for line in result.stderr.splitlines()]
        for event in events:
            assert event["version"] == 1
            assert all(k in event for k in ("type", "code", "operation", "message", "exit_code", "recovery", "partial_output", "published"))
        self.calls.append(str(prefix))
        assert result.returncode == expected, (args, result.returncode, result.stderr)
        assert not self.server.RequestHandlerClass.failures, self.server.RequestHandlerClass.failures
        assert "local-secret" not in result.stdout + result.stderr
        assert "never-log-this" not in result.stdout + result.stderr
        assert "hidden" not in result.stderr
        if expected > 0:
            if human:
                assert "ERROR [" in result.stderr
                assert re.search(rf"Exit code\s+{expected}\b", result.stderr)
            else:
                assert events[-1]["type"] == "error" and events[-1]["exit_code"] == expected
        if human:
            assert all(char not in result.stdout + result.stderr for char in ("\x1b", "\x00", "\r", "\u202e"))
        return result, events

    def call(self, *args, expected=0, stdin=None, env=None, interrupt=False, human=False):
        before = len(self.requests)
        argv = [str(self.resources / "athena-bridge"), *args]
        if interrupt:
            self.status_seen.clear()
            process = subprocess.Popen(argv, env=env or self.env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            try:
                assert self.status_seen.wait(5), "CLI never reached the local status endpoint"
                # Let the response drain before interrupting the long polling timer.
                time.sleep(0.1)
                process.send_signal(signal.SIGINT)
                stdout, stderr = process.communicate(timeout=5)
                result = subprocess.CompletedProcess(argv, process.returncode, stdout, stderr)
            finally:
                if process.poll() is None:
                    process.kill()
                    process.communicate()
        else:
            started = time.perf_counter_ns()
            result = subprocess.run(argv, input=stdin, env=env or self.env, capture_output=True, text=True, timeout=15)
            result.elapsed_ms = (time.perf_counter_ns() - started) / 1_000_000
        return self.record(list(args), result, expected, before, human=human)

    def data(self, *args, **kwargs):
        return json.loads(self.call(*args, "--format", "json", **kwargs)[0].stdout)

    def results(self, *args, **kwargs):
        return self.call("results", "--query-id", "local-query", *args, **kwargs)

    def usage(self):
        path = self.resources / "usage" / "usage.db"
        env = self.env.copy()
        thread = "11111111-1111-4111-8111-111111111111"
        env.update(ATHENA_BRIDGE_USAGE_DB=str(path), AI_AGENT="codex_0-0_agent",
                   __CFBundleIdentifier="com.t3tools.t3code", CODEX_HOME=str(self.resources / "codex"),
                   CODEX_THREAD_ID=thread, ATHENA_BRIDGE_EFFORT="xhigh")
        transcript = self.resources / "codex/sessions/2026/10/26" / f"rollout-fixture-{thread}.jsonl"
        transcript.parent.mkdir(parents=True)
        transcript.write_text(json.dumps({"private": "transcript-private-marker" * 4000}) + "\n" +
                              json.dumps({"type": "turn_context", "payload": {"model": "gpt-6.1-sol", "private": "transcript-private-marker"}}) + "\n")
        transcript.chmod(0o600)
        statement = "SELECT 'usage-private-sql' FROM events"
        request_id = "7" * 32
        sql_args = ("--database", "analytics", "--sql", statement, "--request-id", request_id, "--format", "json")
        cases = (("version",), ("tables", "--database", "analytics", "--format", "json"),
                 ("describe", "--database", "analytics", "--table", "events", "--format", "json"),
                 ("start", *sql_args), ("query", *sql_args, "--all"),
                 ("results", "--query-id", "local-query", "--all", "--format", "json"),
                 ("status", "--query-id", "local-query", "--format", "json"),
                 ("tables", "--database", "analytics", "--pretty"), ("unknown",),
                 ("tables", "--database", "analytics", "--format", "csv"))
        baseline = self.call(*cases[1])[0]
        timings = []
        for i in range(100):
            env["ATHENA_BRIDGE_SESSION"] = f"private-session-{i // 10}"
            args = cases[i % len(cases)]
            pair = {}
            for enabled in ((False, True) if i % 2 == 0 else (True, False)):
                selected = env.copy()
                if not enabled:
                    selected["ATHENA_BRIDGE_USAGE_DB"] = "off"
                pair[enabled] = self.call(*args, env=selected, expected=2 if args[0] == "unknown" else 0,
                                          human="--pretty" in args)[0]
            result = pair[True]
            assert (result.stdout, result.stderr) == (pair[False].stdout, pair[False].stderr), "Usage logging changed streams"
            timings.append({"command": args[0], "exit_code": result.returncode,
                            "on_ms": result.elapsed_ms, "off_ms": pair[False].elapsed_ms,
                            "added_ms": result.elapsed_ms - pair[False].elapsed_ms})
            if i == 1:
                assert (result.stdout, result.stderr) == (baseline.stdout, baseline.stderr), "Usage logging changed streams"
        with sqlite3.connect(path) as db:
            assert db.execute("PRAGMA user_version").fetchone() == (2,)
            assert db.execute("PRAGMA journal_mode").fetchone() == ("wal",)
            assert db.execute("SELECT count(*) FROM pragma_table_list WHERE strict=1").fetchone() == (3,)
            assert db.execute("SELECT count(*), count(DISTINCT session_id), sum(exit_code=2) FROM runs").fetchone() == (100, 10, 10)
            assert db.execute("SELECT DISTINCT a.value,l.value,m.value,r.effort FROM runs r JOIN labels a ON a.id=r.agent_id AND a.kind=1 JOIN labels l ON l.id=r.launcher_id AND l.kind=7 JOIN labels m ON m.id=r.model_id AND m.kind=8").fetchall() == [("codex", "t3-code", "gpt-6.1-sol", 4)]
            assert db.execute("SELECT min(started_at) > 1000000000000 FROM runs").fetchone() == (1,)
            assert db.execute("SELECT count(DISTINCT sql_hash), min(length(sql_hash)), max(length(sql_hash)) FROM runs WHERE sql_hash IS NOT NULL").fetchone() == (1, 8, 8)
            assert db.execute("SELECT DISTINCT e.value FROM runs r JOIN labels e ON e.id=r.error_id").fetchall() == [("INVALID_ARGUMENT",)]
            assert db.execute("PRAGMA foreign_key_check").fetchall() == []
            payload, cells, allocated = db.execute("SELECT sum(payload), sum(ncell), sum(pgsize) FROM dbstat WHERE name='runs' AND pagetype='leaf'").fetchone()
            page_size = db.execute("PRAGMA page_size").fetchone()[0]
            total_bytes = page_size * db.execute("PRAGMA page_count").fetchone()[0]
            dump(self.evidence / "usage-metrics.json", {
                "runs": 100, "session_count": 10, "record_payload_bytes_per_run": payload / cells,
                "live_cell_bytes_per_run": payload / cells + 4,
                "live_cell_overhead": "2-byte cell pointer + 1-byte payload length + 1-byte rowid for IDs 1-100",
                "runs_leaf_allocated_bytes": allocated, "page_size": page_size, "database_bytes": total_bytes,
                "amortized_database_bytes_per_run": total_bytes / 100,
                "includes": "100 actual CLI invocations across 10 command/option cases, including 10 failures; dictionaries and fixed schema pages included only in database_bytes"})
            with sqlite3.connect(self.evidence / "usage.db") as snapshot:
                db.backup(snapshot)
            def percentiles(key):
                values = sorted(sample[key] for sample in timings)
                return {"p50_ms": (values[49] + values[50]) / 2, "p95_ms": values[94],
                        "min_ms": values[0], "max_ms": values[-1]}
            dump(self.evidence / "usage-latency.json", {
                "pairs": 100, "on": percentiles("on_ms"), "off": percentiles("off_ms"),
                "added": percentiles("added_ms"), "samples": timings,
                "cpu_count": os.cpu_count(), "load_average": os.getloadavg() if hasattr(os, "getloadavg") else None,
                "logged_rows": 100, "expected_argument_failures_per_side": 10, "unexpected_failures": 0,
                "method": "one local run, alternating on/off pair order; process wall time excludes evidence writes; added is signed on-minus-off per pair; 64 KiB transcript tail reads and WAL commits included; first row initializes schema; filesystem cache warms during the run"})
            db.execute("BEGIN IMMEDIATE")
            try:
                self.call("version", env=env)
                self.call("unknown", env=env, expected=2)
                assert db.execute("SELECT count(*) FROM runs").fetchone() == (100,), "Locked log changed"
            finally:
                db.rollback()
        processes = []
        try:
            for _ in range(4):
                processes.append(subprocess.Popen([str(self.resources / "athena-bridge"), "version"], env=env,
                                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True))
            for process in processes:
                stdout, stderr = process.communicate(timeout=5)
                self.record(["version"], subprocess.CompletedProcess(process.args, process.returncode, stdout, stderr), 0, len(self.requests))
        finally:
            for process in processes:
                if process.poll() is None:
                    process.kill()
                    process.communicate()
        with sqlite3.connect(path) as db:
            assert db.execute("SELECT count(*) FROM runs").fetchone() == (104,), "Concurrent invocations lost rows"
        blocker = self.resources / "not-a-directory"
        blocker.write_bytes(b"")
        env["ATHENA_BRIDGE_USAGE_DB"] = str(blocker / "usage.db")
        self.call("version", env=env)
        self.call("unknown", env=env, expected=2)
        empty = self.resources / "empty-usage.db"
        empty.write_bytes(b"")
        env["ATHENA_BRIDGE_USAGE_DB"] = str(empty)
        self.call("version", env=env)
        with sqlite3.connect(empty) as db:
            assert db.execute("PRAGMA user_version").fetchone() == (2,)
            assert db.execute("SELECT count(*) FROM runs").fetchone() == (1,)
        legacy = self.resources / "v1-usage.db"
        with sqlite3.connect(legacy) as db:
            db.executescript((ROOT / "cmd/athena-bridge/testdata/usage-v1.sql").read_text())
            db.executescript("INSERT INTO labels VALUES (1,1,'codex'),(2,4,'version'),(3,6,'0.1.0');"
                             "INSERT INTO runs(id,started_at,duration_ms,agent_id,command_id,version_id,exit_code,flags) VALUES (1,1700000000,7,1,2,3,0,0);")
        env["ATHENA_BRIDGE_USAGE_DB"] = str(legacy)
        self.call("version", env=env)
        with sqlite3.connect(legacy) as db:
            assert db.execute("PRAGMA user_version").fetchone() == (2,)
            assert db.execute("SELECT started_at,duration_ms,launcher_id,model_id,effort FROM runs WHERE id=1").fetchone() == (1700000000000, 7, None, None, None)
            assert db.execute("PRAGMA foreign_key_check").fetchall() == []
        for file in path.parent.glob("usage.db*"):
            data = file.read_bytes()
            for private in (statement, "usage-private-sql", "local-access", "local-secret", "private-session-", request_id,
                            "local-query", "9007199254740993", "local-page-2", "transcript-private-marker", thread, str(transcript)):
                assert private.encode() not in data, f"Private value in {file.name}"
            if os.name != "nt":
                assert file.stat().st_mode & 0o777 == 0o600
        if os.name != "nt":
            assert path.parent.stat().st_mode & 0o777 == 0o700

    def build_config(self):
        assert "Commands:" in self.call("--help")[0].stdout
        assert self.call("version")[0].stdout.startswith("athena-bridge ")
        for command in ("tables", "describe", "query", "start", "status", "results", "download", "info", "search", "cancel", "export"):
            assert "Usage:" in self.call(command, "--help")[0].stdout
        before = len(self.requests)
        for args in (("unknown",), ("tables", "--format", "xml"), ("tables", "--page-size", "0"),
                     ("tables", "--page-size", "1001"), ("tables", "--timeout", "0s"),
                     ("tables", "position"), ("tables", "--unknown"), ("describe",), ("status",),
                     ("results",), ("cancel",), ("download",), ("search", "--term", "x", "--kind", "bad"),
                     ("results", "--query-id", "local-query", "--poll-interval", "0s")):
            assert self.call(*args, expected=2)[1][-1]["code"] == "INVALID_ARGUMENT"
        assert len(self.requests) == before
        env = self.env.copy()
        env.pop("ATHENA_BRIDGE_FUNCTION")
        self.call("tables", expected=2, env=env)
        self.call("tables", "--profile", "missing-local-profile", expected=1)
        self.server.function, self.server.region = "flag-worker", "us-west-2"
        self.call("tables", "--function", "flag-worker", "--region", "us-west-2", "--profile", "local-proof")
        self.server.function, self.server.region = "local-worker", "us-east-1"
        empty = self.resources / "no-region"
        empty.write_text("")
        env = self.env.copy()
        env["AWS_CONFIG_FILE"] = str(empty)
        assert self.call("tables", expected=1, env=env)[1][-1]["code"] == "CONFIGURATION"

    def transport(self):
        self.call("tables")
        env = self.env.copy()
        env.pop("AWS_PROFILE")
        env.update(AWS_ACCESS_KEY_ID="local-access", AWS_SECRET_ACCESS_KEY="local-secret", AWS_SESSION_TOKEN="fake-session", AWS_REGION="us-east-1")
        self.call("tables", env=env)
        assert self.requests[-1]["signature_valid"]
        for mode in ("bad-json", "bad-version", "both-envelope", "worker-reject"):
            self.mode = mode
            _, events = self.call("tables", expected=1)
            assert events[-1]["code"] == ("WORKER_REJECTED" if mode == "worker-reject" else "PROTOCOL")
        for worker_code, code, recovery in (("NOT_READY", "NOT_READY", "wait_query"),
                                            ("RESULT_CHANGED", "RESULT_CHANGED", "inspect_query"),
                                            ("NOT_ALLOWED", "WORKER_REJECTED", "none"),
                                            ("INVALID_REQUEST", "WORKER_REJECTED", "fix_arguments"),
                                            ("CURSOR_EXPIRED", "WORKER_REJECTED", "check_worker"),
                                            ("UNKNOWN", "WORKER_REJECTED", "check_worker")):
            self.mode = "worker-code-" + worker_code
            result, events = self.results("--format", "json", expected=1)
            assert not result.stdout and len(events) == 1
            assert events[-1]["code"] == code and events[-1]["worker_code"] == worker_code
            assert events[-1]["recovery"] == recovery and events[-1]["query_id"] == "local-query"
            assert events[-1]["operation"] == "get_results"
            assert events[-1]["partial_output"] is False and events[-1]["published"] is False
        self.mode = "normal"
        self.server.response_headers = {"X-Amz-Function-Error": "Unhandled"}
        assert self.call("tables", expected=1)[1][-1]["code"] == "WORKER_FUNCTION"
        self.server.response_headers = {"Location": self.env["AWS_ENDPOINT_URL_LAMBDA"] + "/must-not-follow"}
        self.server.response_status = 302
        before = len(self.requests)
        assert self.call("tables", expected=1)[1][-1]["code"] == "TRANSPORT"
        assert len(self.requests) == before + 1
        self.server.response_status, self.server.response_headers = 200, {}
        self.mode = "sdk-error"
        for sdk_code, status, code, recovery in (
                ("AccessDeniedException", 403, "AUTHORIZATION", "check_configuration"),
                ("ExpiredTokenException", 403, "AUTHENTICATION", "check_configuration"),
                ("InvalidSignatureException", 403, "AUTHENTICATION", "check_configuration"),
                ("TooManyRequestsException", 429, "THROTTLED", "retry_operation"),
                ("ResourceNotFoundException", 404, "CONFIGURATION", "check_configuration"),
                ("InvalidParameterValueException", 400, "CONFIGURATION", "check_configuration")):
            self.server.response_status = status
            self.server.response_headers = {"X-Amzn-ErrorType": sdk_code}
            result, events = self.results("--format", "json", expected=1)
            assert not result.stdout and len(events) == 1
            assert events[-1]["code"] == code and events[-1]["recovery"] == recovery
            assert events[-1]["query_id"] == "local-query" and "worker_code" not in events[-1]
            assert events[-1]["partial_output"] is False and events[-1]["published"] is False
        self.server.response_status = 429
        self.server.response_headers = {"X-Amzn-ErrorType": "TooManyRequestsException"}
        result, events = self.call("start", "--sql", "SELECT 1", "--request-id", TOKEN, expected=1)
        assert not result.stdout and len(events) == 2
        assert events[-1]["code"] == "THROTTLED" and events[-1]["recovery"] == "retry_same_request"
        assert all(event["request_id"] == TOKEN and "query_id" not in event for event in events)
        self.mode = "normal"
        self.server.response_status, self.server.response_headers = 200, {}

    def discovery(self):
        for fmt in ("json", "csv", "table"):
            result, _ = self.call("info", "--format", fmt)
            assert "local-worker" in result.stdout and "configured" in result.stdout
        assert self.data("info")["limits"] == {"page_size": 1000}
        assert self.data("tables", "--database", "analytics", "--all")["rows"] == [["events", "EXTERNAL_TABLE", None]]
        assert self.data("describe", "--table", "events")["rows"] == [["country", "varchar", "Country code", "true"]]
        for kind in ("all", "table", "column"):
            result = self.data("search", "--term", "CoUnTrY%_", "--kind", kind, "--table", "events", "--database", "analytics", "--all")
            assert result["complete"] and result["rows"] == [["column", "events", "country", "varchar", "Country code", "true"]]
            assert self.requests[-1]["request"]["term"] == "CoUnTrY%_"
            assert self.requests[-1]["request"]["kind"] == kind
        self.mode = "bad-info"
        assert self.call("info", expected=1)[1][-1]["code"] == "PROTOCOL"

    def sql(self):
        sql = "SELECT 'synthetic'\n"
        file = self.resources / "input.sql"
        file.write_text(sql)
        for args, stdin in ((("--sql", sql), None), (("--sql-file", str(file)), None), (("--sql-file", "-"), sql)):
            for command in ("start", "query"):
                result, events = self.call(command, *args, "--request-id", TOKEN, "--format", "json", stdin=stdin)
                assert json.loads(result.stdout)["query_id"] == "local-query"
                assert events[0]["code"] == "START_REQUEST" and events[0]["request_id"] == TOKEN
                assert events[1]["code"] == "QUERY_STARTED"
                assert sql.strip() not in result.stderr
        self.call("start", "--sql", "SELECT 2", "--request-id", TOKEN, expected=1)
        _, events = self.call("start", "--sql", "SELECT 3", "--format", "json")
        assert len(events[0]["request_id"]) == 32
        for args in ((), ("--sql", " "), ("--sql", "x", "--sql-file", "-"), ("--sql", "x", "--request-id", "short")):
            self.call("start", *args, expected=2)
        self.call("start", "--sql-file", "-", stdin="x" * 262145, expected=2)
        self.call("start", "--sql-file", str(self.resources / "absent.sql"), expected=1)
        self.server.response_headers = {"X-Amz-Function-Error": "Unhandled"}
        _, events = self.call("start", "--sql", "SELECT 1", "--request-id", TOKEN, expected=1)
        assert events[-1]["recovery"] == "retry_same_request" and events[-1]["request_id"] == TOKEN
        self.server.response_headers = {}
        self.mode = "worker-reject"
        _, events = self.call("start", "--sql", "SELECT 1", "--request-id", TOKEN, expected=1)
        assert events[-1]["code"] == "WORKER_REJECTED" and events[-1]["worker_code"] == "NOT_ALLOWED"
        assert events[-1]["recovery"] == "none" and events[-1]["request_id"] == TOKEN
        assert "query_id" not in events[-1]

    def queries(self):
        for state in ("QUEUED", "RUNNING", "SUCCEEDED", "FAILED", "CANCELLED"):
            self.mode = "state-" + state
            assert self.data("status", "--query-id", "local-query")["state"] == state
            if state in ("FAILED", "CANCELLED"):
                for command in ("query", "results"):
                    args = ("--sql", "SELECT 1") if command == "query" else ("--query-id", "local-query", "--wait")
                    _, events = self.call(command, *args, expected=1)
                    assert events[-1]["code"] == "QUERY_" + state
        for command in ("query", "results"):
            self.mode, self.poll = "progress", 0
            args = ("--sql", "SELECT 1") if command == "query" else ("--query-id", "local-query", "--wait")
            self.call(command, *args, "--poll-interval", "1ms", "--format", "json")
            assert self.poll == 3
        self.mode = "normal"
        assert "statistics" not in self.data("status", "--query-id", "local-query")
        self.mode = "statistics"
        data = self.data("status", "--query-id", "local-query")
        assert data["statistics"] == {"data_scanned_bytes": "9007199254740993", "queue_ms": "0", "result_reused": False}
        assert data["failure"] == {"error_category": 0, "error_type": 1, "retryable": False}
        for fmt in ("csv", "table"):
            text = self.call("status", "--query-id", "local-query", "--format", fmt)[0].stdout
            assert "9007199254740993" in text and "false" in text
        for state, requested in (("RUNNING", True), ("SUCCEEDED", False), ("FAILED", False), ("CANCELLED", False)):
            self.mode = "state-" + state
            data = self.data("cancel", "--query-id", "local-query")
            assert data["cancellation_requested"] is requested and data["state"] == state
        for fmt in ("csv", "table"):
            assert "cancellation_requested" in self.call("cancel", "--query-id", "local-query", "--format", fmt)[0].stdout
        for mode in ("state-UNKNOWN", "bad-statistics", "bad-status-id"):
            self.mode = mode
            assert self.call("status", "--query-id", "local-query", expected=1)[1][-1]["code"] == "PROTOCOL"
        self.mode = "state-RUNNING"
        for interrupt in (False, True):
            args = ("--timeout", "100ms") if not interrupt else ()
            _, events = self.call("results", "--query-id", "local-query", "--wait", "--poll-interval", "10s", *args,
                                  expected=3, interrupt=interrupt)
            assert events[-1]["code"] == ("CANCELLED" if interrupt else "DEADLINE")
            assert events[-1]["recovery"] == "inspect_query"
        assert not any(r["request"]["action"] == "cancel_query" for r in self.requests[-2:])

    def pagination(self):
        first = json.loads(self.results("--format", "json")[0].stdout)
        assert first["rows"] == ROWS[:2] and first["next_token"] == "local-page-2" and not first["complete"]
        second = json.loads(self.results("--next-token", first["next_token"], "--page-size", "1", "--format", "json")[0].stdout)
        assert second["rows"] == ROWS[2:] and second["complete"]
        assert self.requests[-1]["request"]["page_size"] == 1
        assert json.loads(self.results("--all", "--format", "json")[0].stdout)["rows"] == ROWS
        self.mode = "no-result"
        assert json.loads(self.results("--all", "--format", "json")[0].stdout)["rows"] == ROWS
        self.mode = "empty-first"
        assert json.loads(self.results("--all", "--format", "json")[0].stdout)["rows"] == ROWS[2:]
        for mode in ("repeat", "changed-columns", "changed-result", "missing-result", "wrong-query", "row-width", "number-cell", "null-rows", "bad-column"):
            self.mode = mode
            _, events = self.results("--all", "--format", "json", expected=1)
            assert events[-1]["code"] == ("RESULT_CHANGED" if mode in ("changed-result", "missing-result") else "PROTOCOL")
            assert events[-1]["partial_output"] is (mode in ("repeat", "changed-columns", "changed-result", "missing-result"))

    def pretty(self):
        info = self.call("info", "--pretty", human=True)[0]
        assert "Worker capabilities" in info.stdout and "local-worker" in info.stdout
        for args in (("tables",), ("describe", "--table", "events"),
                     ("search", "--term", "CoUnTrY%_", "--kind", "column", "--table", "events", "--all")):
            result = self.call(*args, "--pretty", human=True)[0]
            assert "complete" in result.stdout or "more results available" in result.stdout
        result = self.results("--all", "--pretty", human=True)[0]
        assert all(value in result.stdout for value in ("9007199254740993", "12345678901234567890.2300", "NULL", '""', "4 rows · complete"))
        assert "┌" in result.stdout and "│" in result.stdout
        partial = self.results("--pretty", human=True)[0]
        assert "more results available" in partial.stdout and '"local-page-2"' in partial.stderr
        request_id = uuid.uuid4().hex
        start = self.call("start", "--sql", "SELECT 'pretty-test'", "--request-id", request_id,
                          "--pretty", human=True)[0]
        assert request_id in start.stderr and "local-query" in start.stdout
        self.mode = "statistics"
        status = self.call("status", "--query-id", "local-query", "--pretty", human=True)[0]
        assert "9007199254740993" in status.stdout and "false" in status.stdout
        self.mode = "state-RUNNING"
        cancel = self.call("cancel", "--query-id", "local-query", "--pretty", human=True)[0]
        assert "Cancellation requested" in cancel.stdout and "CANCELLATION_REQUESTED" in cancel.stderr
        self.mode = "normal"
        for fmt in ("json", "csv"):
            expected = self.results("--all", "--format", fmt)[0].stdout
            destination = self.resources / ("pretty-file." + fmt)
            result = self.results("--all", "--format", fmt, "--out", str(destination), "--pretty", human=True)[0]
            assert result.stdout == "" and destination.read_text() == expected
        export = self.resources / "pretty-export.csv"
        result = self.call("export", "--query-id", "local-query", "--format", "csv", "--out", str(export), "--pretty", human=True)[0]
        assert "Export" in result.stdout and "Published" in result.stdout
        assert export.read_text() == (self.resources / "pretty-file.csv").read_text()
        self.call("export", "--resume", str(export) + ".checkpoint.json", "--pretty", human=True)
        self.call("tables", "--unknown", "--pretty", expected=2, human=True)
        self.mode = "worker-reject"
        rejected = self.call("start", "--sql", "SELECT 1", "--pretty", expected=1, human=True)[0]
        assert "NOT_ALLOWED" in rejected.stderr
        self.mode = "normal"
        assert json.loads(self.results("--all", "--format", "json", "--pretty=false")[0].stdout)["rows"] == ROWS

    def formats(self):
        data = json.loads(self.results("--all", "--format", "json")[0].stdout)
        assert data == {"query_id": "local-query", "columns": COLS, "rows": ROWS, "next_token": None, "complete": True}
        result = self.results("--all", "--format", "csv")[0]
        rows = list(csv.reader(io.StringIO(result.stdout)))
        assert rows == [["value", "value", "note"],
                        ["9007199254740993", "12345678901234567890.2300", "\\N"],
                        ["2", "0.0000", ""], ["3", "-1.2300", "\\\\N"],
                        ["4", "1.0000", "comma,quote\"\nline\ttab\\\\end"]]
        text = self.results("--all", "--format", "table")[0].stdout
        assert "9007199254740993" in text and "12345678901234567890.2300" in text and "NULL" in text
        assert "line\\ttab" in text and '\\nline' in text and text.count("value") == 2

    def atomic(self):
        path = self.resources / "atomic.csv"
        _, events = self.results("--all", "--format", "csv", "--out", str(path))
        before = path.read_bytes()
        assert before.startswith(b"value,value,note\n")
        assert path.stat().st_mode & 0o777 == 0o600
        assert events[-1]["code"] == "OUTPUT_PUBLISHED" and events[-1]["published"]
        self.results("--out", str(path), expected=1)
        assert path.read_bytes() == before
        for mode in ("repeat", "changed-columns", "changed-result"):
            self.mode = mode
            missing = self.resources / (mode + ".json")
            self.results("--all", "--format", "json", "--out", str(missing), expected=1)
            assert not missing.exists() and not list(self.resources.glob(".athena-bridge-*"))
        self.mode = "normal"
        self.results("--out", str(self.resources / "missing/child.csv"), expected=1)
        partial = self.resources / "partial.json"
        self.results("--format", "json", "--out", str(partial))
        assert not json.loads(partial.read_text())["complete"]
        link = self.resources / "destination-link"
        link.symlink_to(path)
        self.results("--out", str(link), expected=1)
        assert path.read_bytes() == before
        dump(self.evidence / "published-files.json", {"atomic.csv": {"sha256": digest(path), "mode": "0600"},
                                                     "overwrite_preserved": True, "temporary_files_removed": True,
                                                     "partial_page": json.loads(partial.read_text())})
        shutil.copyfile(path, self.evidence / "atomic.csv")

    def downloads(self):
        cert, key = self.resources / "tls.pem", self.resources / "tls.key"
        config = self.resources / "tls.cnf"
        config.write_text("[req]\ndistinguished_name=dn\nx509_extensions=ext\nprompt=no\n[dn]\nCN=127.0.0.1\n[ext]\nsubjectAltName=IP:127.0.0.1\nbasicConstraints=critical,CA:TRUE\n")
        subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-config", str(config),
                        "-keyout", str(key), "-out", str(cert)], check=True, capture_output=True, timeout=15)
        key.chmod(0o600)
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Download)
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(cert, key)
        server.socket = context.wrap_socket(server.socket, server_side=True)
        server.url = f"https://127.0.0.1:{server.server_port}"
        server.mode, server.requests = "normal", []
        self.download = server
        self.start_server(server)
        self.env["SSL_CERT_FILE"] = str(cert)
        self.env.update(AWS_ACCESS_KEY_ID="local-access", AWS_SECRET_ACCESS_KEY="local-secret", AWS_SESSION_TOKEN="fake-session")
        native_trust = sys.platform in ("darwin", "win32")
        cases = (("untrusted-tls", "http-url", "bad-download-format", "max-bytes") if native_trust else
                 ("normal", "redirect", "expired", "truncated", "size-mismatch", "http-url", "bad-download-format", "max-bytes", "unknown-size"))
        for case in cases:
            self.mode = case if case in ("size-mismatch", "http-url", "bad-download-format", "unknown-size") else "normal"
            server.mode = case
            path = self.resources / ("download-" + case + ".csv")
            args = ("--max-bytes", "4") if case in ("max-bytes", "unknown-size") else ()
            result, events = self.call("download", "--query-id", "local-query", "--out", str(path), "--format", "json", *args,
                                       expected=0 if case == "normal" else 1)
            if case == "normal":
                assert path.read_bytes() == PAYLOAD
                assert json.loads(result.stdout)["bytes"] == len(PAYLOAD)
                assert events[-1]["published"] and path.stat().st_mode & 0o777 == 0o600
                shutil.copyfile(path, self.evidence / "download.csv")
            else:
                assert not path.exists() and not events[-1]["published"]
                assert not list(self.resources.glob(".athena-bridge-*"))
                expected_code = {"http-url": "PROTOCOL", "bad-download-format": "PROTOCOL", "max-bytes": "OUTPUT_LIMIT",
                                 "unknown-size": "OUTPUT_LIMIT", "size-mismatch": "PROTOCOL"}.get(case, "TRANSPORT")
                assert events[-1]["code"] == expected_code
        assert not any(r["authorization_present"] or r["session_token_present"] for r in server.requests)
        assert not any(r["path"] == "/redirect-target" for r in server.requests)
        dump(self.evidence / "download-requests.json", server.requests)
        if native_trust:
            assert server.requests == [], "Untrusted TLS must fail before an HTTP request"
            return "native system TLS trust: untrusted TLS/HTTP/format/declared-size rejection passed; successful HTTPS, redirect/expiry/truncation/actual-byte limit and no credential forwarding need the Linux SSL_CERT_FILE recipe"

    def checkpoints(self):
        start = len(self.requests)
        proofs = []
        for fmt in ("json", "csv"):
            reference = self.resources / ("reference." + fmt)
            reference_cp = Path(str(reference) + ".checkpoint.json")
            receipt = json.loads(self.call("export", "--query-id", "local-query", "--out", str(reference), "--format", fmt)[0].stdout)
            assert receipt["published"] and receipt["rows"] == 4 and receipt["sha256"] == digest(reference)
            assert reference.stat().st_mode & 0o777 == 0o600
            assert reference_cp.exists() and reference_cp.stat().st_mode & 0o777 == 0o600
            if fmt == "json":
                assert json.loads(reference.read_text())["rows"] == ROWS
            else:
                assert list(csv.reader(io.StringIO(reference.read_text())))[1][2] == "\\N"
            for case in ("suffix", "expired", "changed", "corrupt", "replay-changed"):
                destination = self.resources / (case + "." + fmt)
                checkpoint = self.resources / (case + "-" + fmt + ".checkpoint.json")
                self.interrupt_export(destination, checkpoint, fmt)
                checkpoint_before = checkpoint.read_text()
                committed = json.loads(checkpoint_before)
                data = Path(committed["data"])
                before = data.read_bytes()
                assert committed["phase"] == "writing" and committed["rows"] == 2
                assert committed["offset"] == len(before) and committed["sha256"] == digest(data)
                assert committed["result_id"] == "local-result-v1" and not destination.exists()
                dump(self.evidence / (case + "-" + fmt + "-interrupted.json"), committed)
                if case == "suffix":
                    with data.open("ab") as file:
                        file.write(b"uncommitted suffix")
                elif case == "corrupt":
                    data.write_bytes(b"X" + before[1:])
                self.mode = {"expired": "expired-cursor", "changed": "changed-result", "replay-changed": "expired-changed"}.get(case, "normal")
                self.expired = False
                success = case in ("suffix", "expired")
                result, events = self.call("export", "--resume", str(checkpoint), "--profile", "local-proof", "--timeout", "5s",
                                           expected=0 if success else 1)
                if success:
                    assert destination.read_bytes() == reference.read_bytes()
                    assert json.loads(result.stdout)["sha256"] == digest(reference)
                    assert json.loads(checkpoint.read_text())["phase"] == "published"
                    assert not data.exists()
                    request_count = len(self.requests)
                    self.call("export", "--resume", str(checkpoint))
                    assert len(self.requests) == request_count and destination.read_bytes() == reference.read_bytes()
                    shutil.copyfile(destination, self.evidence / (case + "." + fmt))
                else:
                    assert events[-1]["code"] == ("LOCAL_IO" if case == "corrupt" else "RESULT_CHANGED")
                    assert not destination.exists() and checkpoint.read_text() == checkpoint_before
                    assert data.read_bytes() == (b"X" + before[1:] if case == "corrupt" else before)
                proofs.append({"format": fmt, "case": case, "byte_identical": success, "failed_closed": not success})
            self.mode = "normal"
            before = reference.read_bytes()
            self.call("export", "--query-id", "local-query", "--out", str(reference), "--format", fmt, expected=1)
            assert reference.read_bytes() == before
        self.mode = "no-result"
        absent = self.resources / "identity-required.json"
        assert self.call("export", "--query-id", "local-query", "--out", str(absent), "--format", "json", expected=1)[1][-1]["code"] == "RESULT_CHANGED"
        assert not absent.exists()
        self.call("export", "--query-id", "local-query", "--out", str(self.resources / "bad-export"), expected=2)
        self.call("export", "--resume", str(reference_cp), "--format", "json", expected=2)
        assert all(r["request"]["action"] == "get_results" for r in self.requests[start:]), "Export must never submit SQL"
        dump(self.evidence / "export-recovery.json", proofs)

    def interrupt_export(self, destination, checkpoint, fmt):
        self.mode = "export-block"
        self.export_blocked.clear()
        self.export_release.clear()
        args = ["export", "--query-id", "local-query", "--out", str(destination), "--format", fmt, "--checkpoint", str(checkpoint)]
        before = len(self.requests)
        process = subprocess.Popen([str(self.resources / "athena-bridge"), *args], env=self.env,
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            assert self.export_blocked.wait(5), "Export never durably committed its first page"
            # A second actual CLI process must fail while the first owns the OS lock.
            _, events = self.call("export", "--resume", str(checkpoint), expected=1)
            assert events[-1]["operation"] == "export_lock"
            process.kill()
            stdout, stderr = process.communicate(timeout=5)
            self.record(args, subprocess.CompletedProcess(args, process.returncode, stdout, stderr), -signal.SIGKILL, before)
        finally:
            if process.poll() is None:
                process.kill()
                process.communicate()
            self.export_release.set()


def drive(run, features):
    doctor(run)
    summary = {}
    with Harness(run) as harness:
        try:
            for feature in features:
                harness.mode, harness.poll = "normal", 0
                pending = getattr(harness, feature.replace("-", "_"))()
                summary[feature] = {"status": "pending" if pending else "passed", "detail": pending,
                                    "commands_so_far": harness.counter}
                print(f"{feature}: {summary[feature]['status']}", flush=True)
        except BaseException as error:
            summary[feature] = {"status": "failed", "error": str(error)}
            raise
        finally:
            dump(harness.evidence / "summary.json", summary)
            print(f"Evidence: {harness.evidence}", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("launch", "doctor", "drive", "evidence", "cleanup"))
    parser.add_argument("--run", help=f"absolute immediate child of {BASE}")
    parser.add_argument("--feature", action="append", choices=FEATURES, help="repeat to select; default all")
    args = parser.parse_args()
    if args.action == "launch":
        launch(args.run)
        return
    if not args.run:
        parser.error("--run is required outside launch")
    run, _ = load_run(args.run)
    if args.action == "doctor":
        doctor(run)
    elif args.action == "drive":
        drive(run, args.feature or FEATURES)
    elif args.action == "cleanup":
        cleanup(run)
    else:
        print(json.dumps({"run": str(run), "summaries": [str(p) for p in sorted(run.glob("drive-*/summary.json"))],
                          "cleanup": (run / "cleanup.json").exists()}))


if __name__ == "__main__":
    main()
