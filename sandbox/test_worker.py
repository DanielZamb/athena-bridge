import importlib.util
import os
from pathlib import Path
import sys
import types
import unittest
from unittest.mock import patch

from fixtures import generate
from oracle import expected


QUERY = "01234567-0123-0123-0123-012345678901"
OTHER_QUERY = "01234567-0123-0123-0123-012345678902"


class FakeConfig:
    def __init__(self, **arguments):
        pass

    def merge(self, other):
        return self


class FakeClientError(Exception):
    def __init__(self, code):
        self.response = {"Error": {"Code": code}}


class Athena:
    def __init__(self):
        self.workgroup = "sandbox"
        self.state = "SUCCEEDED"
        self.receipts = {}

    def start_query_execution(self, **arguments):
        token = arguments["ClientRequestToken"]
        if token in self.receipts and self.receipts[token] != arguments:
            raise FakeClientError("InvalidRequestException")
        self.receipts[token] = arguments
        return {"QueryExecutionId": QUERY}

    def get_query_execution(self, **arguments):
        return {"QueryExecution": {"QueryExecutionId": arguments["QueryExecutionId"], "WorkGroup": self.workgroup,
            "QueryExecutionContext": {"Database": "fixture", "Catalog": "awsdatacatalog"}, "Status": {"State": self.state},
            "ResultConfiguration": {"OutputLocation": f"s3://test-bucket/results/{arguments['QueryExecutionId']}.csv"}}}

    def get_query_results(self, **arguments):
        metadata = [{"Name": "id", "Type": "bigint"}, {"Name": "id", "Type": "decimal", "Precision": 20, "Scale": 4},
            {"Name": "nullable", "Type": "varchar"}, {"Name": "empty", "Type": "varchar"}, {"Name": "literal", "Type": "varchar"}]
        values = [[{"VarCharValue": c["Name"]} for c in metadata],
            [{"VarCharValue": "9007199254740993"}, {"VarCharValue": "1.2300"}, {}, {"VarCharValue": ""}, {"VarCharValue": "\\N"}],
            [{"VarCharValue": "2"}, {"VarCharValue": "2.3400"}]]
        return {"ResultSet": {"ResultSetMetadata": {"ColumnInfo": metadata}, "Rows": [{"Data": row} for row in values]}}


class Glue:
    def get_tables(self, **arguments):
        return {"TableList": [{"Name": "events", "Description": "Synthetic events"}, {"Name": "sessions"}]}

    def get_table(self, **arguments):
        return {"Table": {"StorageDescriptor": {"Columns": [{"Name": "value", "Type": "bigint", "Comment": "Exact value"}]},
            "PartitionKeys": [{"Name": "month", "Type": "string"}]}}


class S3:
    def head_object(self, **arguments):
        return {"ContentLength": 42, "ETag": '"revision-one"'}

    def generate_presigned_url(self, operation, **arguments):
        return "https://test-bucket.s3.us-east-1.amazonaws.com/results/example.csv?temporary=signature"


def load_worker(clients, client_factory=None, environment=None, absent_environment=()):
    modules = {"boto3": types.SimpleNamespace(client=client_factory or (lambda name, **arguments: clients[name])),
        "botocore.config": types.SimpleNamespace(Config=FakeConfig), "botocore.exceptions": types.SimpleNamespace(ClientError=FakeClientError)}
    spec = importlib.util.spec_from_file_location("sandbox_worker_test", Path(__file__).with_name("worker.py"))
    worker = importlib.util.module_from_spec(spec)
    settings = {"DATABASE": "fixture", "WORKGROUP": "sandbox", "BUCKET": "test-bucket", "CURSOR_KEY": "0" * 64,
        "MAX_SCAN_BYTES": "10485760", "RESULT_RETENTION_DAYS": "7"}
    settings.update(environment or {})
    with patch.dict(sys.modules, modules), patch.dict(os.environ, settings):
        for name in absent_environment:
            os.environ.pop(name, None)
        spec.loader.exec_module(worker)
    return worker


class WorkerTests(unittest.TestCase):
    def setUp(self):
        self.athena = Athena()
        self.worker = load_worker({"athena": self.athena, "glue": Glue(), "s3": S3()})

    def invoke(self, action, **arguments):
        return self.worker.handler(dict(version=1, action=action, **arguments), None)

    def test_pagination_preserves_header_exact_cells_nulls_and_duplicate_columns(self):
        first = self.invoke("get_results", query_id=QUERY, page_size=1)["result"]
        self.assertEqual(first["rows"], [["9007199254740993", "1.2300", None, "", "\\N"]])
        self.assertEqual(first["columns"][:2], [{"name": "id", "type": "bigint"}, {"name": "id", "type": "decimal(20,4)"}])
        second = self.invoke("get_results", query_id=QUERY, page_size=1, next_token=first["next_token"])["result"]
        self.assertEqual(second["rows"], [["2", "2.3400", None, None, None]])
        self.assertIsNone(second["next_token"])
        self.assertEqual(second["columns"], first["columns"])

    def test_cursor_is_bound_to_query_and_detects_tampering(self):
        token = self.invoke("get_results", query_id=QUERY, page_size=1)["result"]["next_token"]
        self.assertEqual(self.invoke("get_results", query_id=OTHER_QUERY, next_token=token)["error"]["code"], "INVALID_CURSOR")
        tampered = ("Y" if token[0] == "X" else "X") + token[1:]
        self.assertEqual(self.invoke("get_results", query_id=QUERY, next_token=tampered)["error"]["code"], "INVALID_CURSOR")

    def test_database_workgroup_and_unfinished_query_rejections(self):
        self.assertEqual(self.invoke("list_tables", database="unrelated")["error"]["code"], "NOT_ALLOWED")
        self.athena.workgroup = "unrelated"
        self.assertEqual(self.invoke("query_status", query_id=QUERY)["error"]["code"], "NOT_ALLOWED")
        self.athena.workgroup, self.athena.state = "sandbox", "RUNNING"
        self.assertEqual(self.invoke("get_results", query_id=QUERY)["error"]["code"], "NOT_READY")

    def test_metadata_has_text_partition_flags_and_scoped_pagination(self):
        first = self.invoke("list_tables", page_size=1)["result"]
        self.assertEqual(first["rows"], [["events", "EXTERNAL_TABLE", "Synthetic events"]])
        second = self.invoke("list_tables", page_size=1, next_token=first["next_token"])["result"]
        self.assertEqual(second["rows"], [["sessions", "EXTERNAL_TABLE", None]])
        described = self.invoke("describe_table", table="events")["result"]
        self.assertEqual(described["rows"], [["value", "bigint", "Exact value", "false"], ["month", "string", None, "true"]])

    def test_idempotent_request_token_and_mutation_rejection(self):
        arguments = {"request_id": "stable-request-token-01234567890123", "sql": "WITH x AS (SELECT 1) SELECT * FROM x"}
        self.assertEqual(self.invoke("start_query", **arguments)["result"], {"query_id": QUERY})
        self.assertEqual(self.invoke("start_query", **arguments)["result"], {"query_id": QUERY})
        arguments["sql"] = "SELECT 2"
        self.assertEqual(self.invoke("start_query", **arguments)["error"]["code"], "InvalidRequestException")
        for sql in ["CREATE TABLE bad AS SELECT 1", "SELECT 1; DROP TABLE events", "WITH x AS (SELECT 1) INSERT INTO events SELECT * FROM x"]:
            self.assertEqual(self.invoke("start_query", request_id="x" * 32, sql=sql)["error"]["code"], "NOT_ALLOWED")

    def test_download_returns_fresh_file_metadata(self):
        result = self.invoke("download_result", query_id=QUERY)["result"]
        self.assertEqual({"format": result["format"], "size_bytes": result["size_bytes"]}, {"format": "csv", "size_bytes": 42})
        self.assertTrue(result["url"].startswith("https://"))


class FixtureTests(unittest.TestCase):
    def test_independent_oracle_matches_the_named_edge_cases(self):
        self.assertEqual(expected()["consecutive_user_count"], 488)

    def test_fixture_has_unique_keys_and_boundary_event_attributed_to_september(self):
        tables = generate()
        sessions = {(row["account_no"], row["user_id"], row["session_id"]): row for row in tables["ac_us_sessions"]}
        self.assertEqual(len(sessions), len(tables["ac_us_sessions"]))
        self.assertEqual(sum((event["account_no"], event["user_id"], event["session_id"]) not in sessions for event in tables["ac_us_client_events"]), 3)
        self.assertTrue(any(user["user_id"] == "inactive" for user in tables["ac_us_users"]))
        self.assertFalse(any(session["user_id"] == "inactive" for session in sessions.values()))
        boundary = [event for event in tables["ac_us_client_events"] if event["event_time"] == "2026-10-01 00:05:00"]
        self.assertEqual(len(boundary), 2)
        self.assertEqual({sessions[(event["account_no"], event["user_id"], event["session_id"])]["reporting_month"] for event in boundary}, {"2026-09-01"})


if __name__ == "__main__":
    unittest.main()
