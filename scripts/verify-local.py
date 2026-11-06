"""Exercise the built CLI using a temporary AWS profile and local Lambda endpoint."""

import argparse
import csv
import http.server
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading


ROOT = Path(__file__).resolve().parents[1]
COLUMNS = [
    {"name": "country", "type": "varchar"},
    {"name": "events", "type": "bigint"},
    {"name": "note", "type": "varchar"},
]


class Worker(http.server.BaseHTTPRequestHandler):
    failures = []
    starts = {}

    def log_message(self, *args):
        pass

    def do_POST(self):
        try:
            function = getattr(self.server, "function", "local-worker")
            region = getattr(self.server, "region", "us-east-1")
            assert self.path == f"/2015-03-31/functions/{function}/invocations"
            authorization = self.headers.get("Authorization", "")
            assert authorization.startswith("AWS4-HMAC-SHA256 Credential=local-access/")
            assert f"/{region}/lambda/aws4_request" in authorization
            body = self.rfile.read(int(self.headers["Content-Length"]))
            assert b"local-secret" not in body and b"local-access" not in body
            request = json.loads(body)
            assert request["version"] == 1
            if hasattr(self.server, "validate"):
                self.server.validate(self, body)
            action = request["action"]
            if hasattr(self.server, "respond"):
                response = self.server.respond(request)
            elif action == "start_query":
                previous = self.starts.setdefault(request["request_id"], request)
                if previous != request:
                    response = {"version": 1, "error": {"code": "ID_REUSED", "message": "Use identical query parameters."}}
                else:
                    response = {"version": 1, "result": {"query_id": "local-query"}}
            elif action == "query_status":
                response = {"version": 1, "result": {"query_id": request["query_id"], "state": "SUCCEEDED"}}
            elif action == "get_results":
                assert request["query_id"] == "local-query"
                more = not request.get("next_token")
                rows = [["CO", "9007199254740993", None]] if more else [["MX", "2", ""]]
                response = {"version": 1, "result": {"query_id": "local-query", "columns": COLUMNS, "rows": rows,
                                                       "next_token": "local-page-2" if more else None}}
            elif action == "list_tables":
                response = {"version": 1, "result": {"columns": [{"name": "table_name", "type": "varchar"}],
                                                       "rows": [["events"]], "next_token": None}}
            elif action == "describe_table":
                assert request["table"] == "events"
                response = {"version": 1, "result": {"columns": [{"name": "column_name", "type": "varchar"}],
                                                       "rows": [["country"], ["events"]], "next_token": None}}
            else:
                raise AssertionError("unexpected action")
            data = response if isinstance(response, bytes) else json.dumps(response).encode()
            self.send_response(getattr(self.server, "response_status", 200))
            for name, value in getattr(self.server, "response_headers", {}).items():
                self.send_header(name, value)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        except (BrokenPipeError, ConnectionResetError):
            pass  # A cancelled CLI may close its local invocation socket.
        except Exception as error:
            self.failures.append(str(error))
            self.send_error(500)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path,
        default=ROOT / "bin" / ("athena-bridge.exe" if os.name == "nt" else "athena-bridge"))
    binary = parser.parse_args().binary.resolve()
    if not binary.exists():
        raise SystemExit("Run make build first.")
    with tempfile.TemporaryDirectory(prefix="athena-bridge-proof-") as directory:
        temporary = Path(directory)
        credentials = temporary / "credentials"
        credentials.write_text("[local-proof]\naws_access_key_id=local-access\naws_secret_access_key=local-secret\n")
        credentials.chmod(0o600)
        config = temporary / "config"
        config.write_text("[profile local-proof]\nregion=us-east-1\n")
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Worker)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            environment = {key: value for key, value in os.environ.items() if not key.startswith("AWS_")}
            environment.update({"AWS_PROFILE": "local-proof", "AWS_SHARED_CREDENTIALS_FILE": str(credentials),
                                "AWS_CONFIG_FILE": str(config), "AWS_EC2_METADATA_DISABLED": "true",
                                "AWS_ENDPOINT_URL_LAMBDA": f"http://127.0.0.1:{server.server_port}",
                                "ATHENA_BRIDGE_FUNCTION": "local-worker", "ATHENA_BRIDGE_USAGE_DB": "off"})

            def call(*arguments, success=True):
                result = subprocess.run([str(binary), *arguments], cwd=temporary, env=environment,
                    capture_output=True, text=True, timeout=15)
                if success and result.returncode:
                    raise AssertionError(result.stderr)
                if not success and result.returncode == 0:
                    raise AssertionError("Expected a nonzero exit")
                return result

            tables = json.loads(call("tables", "--database", "analytics", "--format", "json").stdout)
            assert tables["rows"] == [["events"]]
            schema = json.loads(call("describe", "--table", "events", "--format", "json").stdout)
            assert schema["rows"] == [["country"], ["events"]]
            token = "0123456789abcdef0123456789abcdef"
            result = call("query", "--sql", "SELECT * FROM events", "--request-id", token, "--all", "--format", "json")
            dataset = json.loads(result.stdout)
            assert dataset["rows"] == [["CO", "9007199254740993", None], ["MX", "2", ""]]
            assert dataset["complete"] is True and dataset["next_token"] is None
            events = [json.loads(line) for line in result.stderr.splitlines()]
            assert any(event["version"] == 1 and event["code"] == "QUERY_STARTED"
                       and event["query_id"] == "local-query" for event in events)
            started = json.loads(call("start", "--sql", "SELECT * FROM events", "--request-id", token, "--format", "json").stdout)
            assert started["query_id"] == "local-query"
            call("start", "--sql", "SELECT 2", "--request-id", token, success=False)
            path = temporary / "results.csv"
            call("results", "--query-id", "local-query", "--all", "--format", "csv", "--out", str(path))
            before = path.read_bytes()
            rows = list(csv.reader(io.StringIO(before.decode())))
            assert rows == [["country", "events", "note"], ["CO", "9007199254740993", "\\N"], ["MX", "2", ""]]
            call("results", "--query-id", "local-query", "--out", str(path), success=False)
            assert path.read_bytes() == before
            assert not Worker.failures, Worker.failures
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)
    print("Local CLI proof passed: named-profile credentials, signed invocation, metadata, query, paginated JSON/CSV, and overwrite protection.")


if __name__ == "__main__":
    main()
