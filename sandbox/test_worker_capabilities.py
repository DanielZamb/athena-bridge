import datetime
import json
import unittest
from unittest.mock import Mock, patch

from stack import template
from test_worker import Athena, FakeClientError, Glue, OTHER_QUERY, QUERY, S3, load_worker


class WorkerCapabilityTests(unittest.TestCase):
    def setUp(self):
        self.athena, self.glue, self.s3 = Athena(), Glue(), S3()
        self.clients = {"athena": self.athena, "glue": self.glue, "s3": self.s3}
        self.worker = load_worker(self.clients)

    def invoke(self, action, **arguments):
        return self.worker.handler(dict(version=1, action=action, **arguments), None)

    def execution(self):
        return self.athena.get_query_execution(QueryExecutionId=QUERY)["QueryExecution"]

    def test_info_has_only_configured_fields_without_creating_any_aws_client(self):
        factory = Mock(side_effect=AssertionError("No downstream or IAM client is allowed"))
        worker = load_worker(self.clients, client_factory=factory)
        response = worker.handler({"version": 1, "action": "worker_info"}, None)
        self.assertEqual(response, {"version": 1, "result": {
            "worker_id": "athena-bridge-sandbox", "basis": "configured", "default_database": "fixture",
            "databases": ["fixture"], "catalog": "AwsDataCatalog", "workgroup": "sandbox",
            "actions": ["worker_info", "list_tables", "describe_table", "search_metadata", "start_query",
                "query_status", "cancel_query", "get_results", "download_result"],
            "features": ["result_identity"], "limits": {"default_page_size": 200, "max_page_size": 1000,
                "max_response_bytes": 4000000, "metadata_tables_per_call": 100,
                "max_scan_bytes": 10485760, "max_sql_bytes": 262144,
                "cursor_ttl_seconds": 3600, "result_retention_days": 7}}})
        factory.assert_not_called()
        serialized = json.dumps(response)
        for secret in ["test-bucket", "CURSOR_KEY", "BUCKET", "role", "policy", "s3://"]:
            self.assertNotIn(secret, serialized)

    def test_info_uses_stack_scan_and_retention_configuration_without_clients(self):
        resources = template("athena-bridge-lab-example")["Resources"]
        variables = resources["Worker"]["Properties"]["Environment"]["Variables"]
        factory = Mock(side_effect=AssertionError("No downstream client is allowed"))
        worker = load_worker(self.clients, client_factory=factory, environment={
            key: variables[key] for key in ["MAX_SCAN_BYTES", "RESULT_RETENTION_DAYS"]})
        limits = worker.handler({"version": 1, "action": "worker_info"}, None)["result"]["limits"]
        self.assertEqual(limits["max_scan_bytes"], resources["Workgroup"]["Properties"][
            "WorkGroupConfiguration"]["BytesScannedCutoffPerQuery"])
        self.assertEqual(limits["result_retention_days"], resources["Bucket"]["Properties"][
            "LifecycleConfiguration"]["Rules"][0]["ExpirationInDays"])
        changed = load_worker(self.clients, client_factory=factory, environment={
            "MAX_SCAN_BYTES": "20971520", "RESULT_RETENTION_DAYS": "3"})
        changed_limits = changed.handler({"version": 1, "action": "worker_info"}, None)["result"]["limits"]
        self.assertEqual(changed_limits["max_scan_bytes"], 20971520)
        self.assertEqual(changed_limits["result_retention_days"], 3)
        factory.assert_not_called()

    def test_info_omits_unknown_retention_without_creating_clients(self):
        factory = Mock(side_effect=AssertionError("No downstream client is allowed"))
        worker = load_worker(self.clients, client_factory=factory,
            absent_environment=("RESULT_RETENTION_DAYS",))
        response = worker.handler({"version": 1, "action": "worker_info"}, None)
        self.assertEqual(response["version"], 1)
        self.assertEqual(response["result"]["default_database"], "fixture")
        self.assertNotIn("result_retention_days", response["result"]["limits"])
        self.assertEqual(response["result"]["limits"]["max_scan_bytes"], 10485760)
        factory.assert_not_called()

    def test_advertised_sql_byte_limit_is_enforced_before_athena(self):
        limit = self.invoke("worker_info")["result"]["limits"]["max_sql_bytes"]
        sql = "SELECT '" + "é" * ((limit - 10) // 2) + "' "
        self.assertEqual(len(sql.encode()), limit)
        self.assertEqual(self.invoke("start_query", sql=sql, request_id="x" * 32)["result"], {"query_id": QUERY})
        with patch.object(self.athena, "start_query_execution", side_effect=AssertionError("Oversized SQL reached Athena")) as start:
            self.assertEqual(self.invoke("start_query", sql=sql + "é", request_id="y" * 32)["error"]["code"], "INVALID_REQUEST")
            start.assert_not_called()

    def test_advertised_cursor_ttl_is_enforced(self):
        ttl = self.invoke("worker_info")["result"]["limits"]["cursor_ttl_seconds"]
        with patch.object(self.worker.time, "time", return_value=1000):
            first = self.invoke("list_tables", page_size=1)["result"]
        with patch.object(self.worker.time, "time", return_value=1000 + ttl - 1):
            self.assertEqual(self.invoke("list_tables", next_token=first["next_token"])["result"]["rows"],
                [["sessions", "EXTERNAL_TABLE", None]])
        with patch.object(self.worker.time, "time", return_value=1000 + ttl + 1):
            self.assertEqual(self.invoke("list_tables", next_token=first["next_token"])["error"]["code"], "INVALID_CURSOR")

    def test_search_empty_page_continues_and_limits_each_invocation_to_one_glue_page(self):
        pages = {None: {"TableList": [{"Name": "unrelated"}], "NextToken": "native-two"},
            "native-two": {"TableList": [{"Name": "events", "Description": "Exact VALUE"}]}}
        with patch.object(self.glue, "get_tables", side_effect=lambda **args: pages[args.get("NextToken")]) as get:
            first = self.invoke("search_metadata", term="value", kind="table")["result"]
            self.assertEqual(first["rows"], [])
            self.assertTrue(first["next_token"])
            self.assertEqual(get.call_args.kwargs, {"DatabaseName": "fixture", "MaxResults": 100})
            self.assertEqual(get.call_count, 1)
            second = self.invoke("search_metadata", term="VALUE", kind="TABLE", next_token=first["next_token"])["result"]
            self.assertEqual(second["rows"], [["table", "events", None, None, "Exact VALUE", None]])
            self.assertIsNone(second["next_token"])
            self.assertEqual(get.call_count, 2)
            self.assertEqual(get.call_args.kwargs["NextToken"], "native-two")
            self.assertEqual(second["columns"], [{"name": name, "type": "varchar"} for name in
                ["kind", "table_name", "column_name", "data_type", "description", "partition_key"]])

    def test_search_exact_table_matches_comments_names_and_partition_columns_literally(self):
        definition = {"Name": "events", "Description": "Contains [x] literal text",
            "StorageDescriptor": {"Columns": [{"Name": "value", "Type": "bigint", "Comment": "[X] value"}]},
            "PartitionKeys": [{"Name": "month", "Type": "string", "Comment": "[x] month"}]}
        with patch.object(self.glue, "get_tables", side_effect=AssertionError("Exact table must use GetTable")), patch.object(
                self.glue, "get_table", return_value={"Table": definition}) as get:
            first = self.invoke("search_metadata", term="[X]", table="EVENTS", page_size=1)["result"]
            self.assertEqual(first["rows"], [["table", "events", None, None, "Contains [x] literal text", None]])
            second = self.invoke("search_metadata", term="[x]", table="events", page_size=1, next_token=first["next_token"])["result"]
            self.assertEqual(second["rows"], [["column", "events", "value", "bigint", "[X] value", "false"]])
            third = self.invoke("search_metadata", term="[x]", table="events", page_size=1, next_token=second["next_token"])["result"]
            self.assertEqual(third["rows"], [["column", "events", "month", "string", "[x] month", "true"]])
            self.assertIsNone(third["next_token"])
            self.assertEqual(get.call_args.kwargs, {"DatabaseName": "fixture", "Name": "events"})
            names = self.invoke("search_metadata", term="MONTH", kind="column", table="events")["result"]
            self.assertEqual(names["rows"], third["rows"])
            for changes in [{"term": "other"}, {"kind": "column"}, {"table": "sessions"}, {"database": "other"}]:
                response = self.invoke("search_metadata", **dict({"term": "[x]", "table": "events", "next_token": first["next_token"]}, **changes))
                self.assertIn(response["error"]["code"], ["INVALID_CURSOR", "NOT_ALLOWED"])
            self.assertEqual(self.invoke("list_tables", next_token=first["next_token"])["error"]["code"], "INVALID_CURSOR")

    def test_list_tables_keeps_native_page_and_local_offset_without_scanning_the_catalog(self):
        pages = {None: {"TableList": [{"Name": "a"}, {"Name": "b"}], "NextToken": "native-two"},
            "native-two": {"TableList": [{"Name": "c"}]}}
        with patch.object(self.glue, "get_tables", side_effect=lambda **args: pages[args.get("NextToken")]) as get:
            token, rows = None, []
            for index in range(3):
                result = self.invoke("list_tables", page_size=1, next_token=token)["result"]
                rows.extend(result["rows"])
                token = result["next_token"]
                self.assertEqual(get.call_count, index + 1)
            self.assertEqual(rows, [["a", "EXTERNAL_TABLE", None], ["b", "EXTERNAL_TABLE", None], ["c", "EXTERNAL_TABLE", None]])
            self.assertIsNone(token)
            self.assertEqual([call.kwargs.get("NextToken") for call in get.call_args_list], [None, None, "native-two"])

    def test_invalid_search_filters_never_call_glue(self):
        for arguments in [{}, {"term": ""}, {"term": 1}, {"term": "x", "kind": "invalid"},
                {"term": "x", "table": "../other"}, {"term": "x", "page_size": True}, {"term": "x", "page_size": 1001}]:
            self.assertEqual(self.invoke("search_metadata", **arguments)["error"]["code"], "INVALID_REQUEST")
        self.assertEqual(self.worker.client.cache_info().currsize, 0)

    def test_statistics_distinguish_unknown_from_zero_and_preserve_large_integers(self):
        self.assertEqual(self.invoke("query_status", query_id=QUERY)["result"], {"query_id": QUERY, "state": "SUCCEEDED"})
        execution = self.execution()
        execution["Statistics"] = {"DataScannedInBytes": 9007199254740993, "EngineExecutionTimeInMillis": 0,
            "TotalExecutionTimeInMillis": 23, "QueryQueueTimeInMillis": 0, "QueryPlanningTimeInMillis": 2,
            "ResultReuseInformation": {"ReusedPreviousResult": False}}
        with patch.object(self.athena, "get_query_execution", return_value={"QueryExecution": execution}):
            self.assertEqual(self.invoke("query_status", query_id=QUERY)["result"]["statistics"], {
                "data_scanned_bytes": "9007199254740993", "engine_execution_ms": "0", "total_execution_ms": "23",
                "queue_ms": "0", "planning_ms": "2", "result_reused": False})
            execution["Statistics"] = {"DataScannedInBytes": 0}
            self.assertEqual(self.invoke("query_status", query_id=QUERY)["result"]["statistics"], {"data_scanned_bytes": "0"})

    def test_failure_exposes_only_sanitized_athena_error_fields(self):
        execution = self.execution()
        execution["Status"] = {"State": "FAILED", "StateChangeReason": "SQL secret s3://test-bucket",
            "AthenaError": {"ErrorCategory": 2, "ErrorType": 1006, "Retryable": False, "ErrorMessage": "private SQL"}}
        with patch.object(self.athena, "get_query_execution", return_value={"QueryExecution": execution}):
            self.assertEqual(self.invoke("query_status", query_id=QUERY)["result"], {"query_id": QUERY, "state": "FAILED",
                "reason": "Check SQL syntax and column references using the describe command.",
                "failure": {"error_category": 2, "error_type": 1006, "retryable": False}})

    def test_failure_guidance_uses_error_types_and_categories_without_raw_text(self):
        cases = [
            (2, 1004, False, "Check table and column definitions with the describe command before correcting SQL."),
            (2, 1110, False, "Check database and table names with the tables and describe commands."),
            (2, 1500, False, "Ask the worker administrator to check data access permissions before retrying."),
            (2, 1501, False, "Ask the worker administrator to check data access permissions before retrying."),
            (1, 0, False, "Reduce the data scanned or simplify joins and aggregations before retrying."),
            (1, 206, False, "Reduce the query scope before retrying after the Athena timeout."),
            (1, 1401, True, "Athena throttled the query. Retry after a delay and reduce concurrent queries."),
            (1, 9999, True, "Athena reported a system error. Retry after a delay."),
            (1, 9999, False, "Athena reported a system error. Contact the worker administrator with the query ID."),
            (2, 9998, False, "Check SQL syntax, identifiers, and data types with metadata commands before resubmitting."),
            (3, 9998, False, "Contact the worker administrator with the query ID to check query scope and configuration."),
        ]
        for category, kind, retryable, reason in cases:
            with self.subTest(category=category, kind=kind, retryable=retryable):
                execution = self.execution()
                execution["Status"] = {"State": "FAILED", "StateChangeReason": "SELECT private_sql https://private.invalid",
                    "AthenaError": {"ErrorCategory": category, "ErrorType": kind, "Retryable": retryable,
                        "ErrorMessage": "AccessDenied arn:aws:iam::123:role/private"}}
                with patch.object(self.athena, "get_query_execution", return_value={"QueryExecution": execution}):
                    self.assertEqual(self.invoke("query_status", query_id=QUERY)["result"], {
                        "query_id": QUERY, "state": "FAILED", "reason": reason,
                        "failure": {"error_category": category, "error_type": kind, "retryable": retryable}})

    def test_failure_extracts_only_bounded_sql_coordinates(self):
        for source in ["StateChangeReason", "ErrorMessage"]:
            for text, location in [("line 3:12: SELECT secret https://private.invalid", (3, 12)),
                    ("SYNTAX_ERROR: line 7:4: IAM AccessDenied arn:aws:iam::123:role/private", (7, 4)),
                    ("SELECT 'line 3:12: secret'", None), ("https://private.invalid/line 3:12", None),
                    ("line 0:2: secret", None), ("line 3:1234567890: secret", None)]:
                with self.subTest(source=source, text=text):
                    execution = self.execution()
                    failure = {"ErrorCategory": 2, "ErrorType": 1006, "Retryable": False}
                    execution["Status"] = {"State": "FAILED", "AthenaError": failure}
                    (failure if source == "ErrorMessage" else execution["Status"])[source] = text
                    with patch.object(self.athena, "get_query_execution", return_value={"QueryExecution": execution}):
                        result = self.invoke("query_status", query_id=QUERY)["result"]
                    reason = "Check SQL syntax and column references using the describe command."
                    expected_failure = {"error_category": 2, "error_type": 1006, "retryable": False}
                    if location:
                        expected_failure.update(sql_line=location[0], sql_column=location[1])
                        reason += f" SQL line {location[0]}, column {location[1]}."
                    self.assertEqual(result, {"query_id": QUERY, "state": "FAILED", "reason": reason,
                        "failure": expected_failure})

    def test_failure_without_structured_error_still_has_safe_recovery_guidance(self):
        execution = self.execution()
        execution["Status"] = {"State": "FAILED", "StateChangeReason": "SELECT secret s3://private"}
        with patch.object(self.athena, "get_query_execution", return_value={"QueryExecution": execution}):
            self.assertEqual(self.invoke("query_status", query_id=QUERY)["result"], {"query_id": QUERY, "state": "FAILED",
                "reason": "Contact the worker administrator with the query ID to check query scope and configuration."})

    def test_parser_and_scan_limit_guidance_survives_missing_athena_error(self):
        cases = [
            ("FAILED", "line 1:15: mismatched input 'private_sql' https://private.invalid",
                "Check SQL syntax and column references using the describe command. SQL line 1, column 15.",
                {"sql_line": 1, "sql_column": 15}),
            ("FAILED", "SYNTAX_ERROR: line 2:6: Column 'private' cannot be resolved",
                "Check SQL syntax and column references using the describe command. SQL line 2, column 6.",
                {"sql_line": 2, "sql_column": 6}),
            ("CANCELLED", "Query cancelled due to exceeding bytes scanned limit",
                "Athena scan limit exceeded. Add partition filters or reduce the query scope before resubmitting.", None),
            ("CANCELLED", "Query was cancelled because it exceeded the bytes scanned limit. s3://private",
                "Athena scan limit exceeded. Add partition filters or reduce the query scope before resubmitting.", None),
            ("FAILED", "Bytes scanned limit was exceeded",
                "Athena scan limit exceeded. Add partition filters or reduce the query scope before resubmitting.", None),
            ("CANCELLED", "User asked to stop SELECT private_sql",
                "Athena query was cancelled. Resubmit only if results are still needed.", None),
        ]
        for state, raw_reason, reason, failure in cases:
            with self.subTest(state=state, raw_reason=raw_reason):
                execution = self.execution()
                execution["Status"] = {"State": state, "StateChangeReason": raw_reason}
                with patch.object(self.athena, "get_query_execution", return_value={"QueryExecution": execution}):
                    result = self.invoke("query_status", query_id=QUERY)["result"]
                expected = {"query_id": QUERY, "state": state, "reason": reason}
                if failure:
                    expected["failure"] = failure
                self.assertEqual(result, expected)

    def test_cancel_authorizes_before_stop_and_reports_observed_state_after_acceptance(self):
        self.athena.state = "RUNNING"
        with patch.object(self.athena, "stop_query_execution", create=True) as stop, patch.object(
                self.athena, "get_query_execution", wraps=self.athena.get_query_execution) as get:
            self.assertEqual(self.invoke("cancel_query", query_id=QUERY)["result"], {
                "query_id": QUERY, "state": "RUNNING", "cancellation_requested": True})
            stop.assert_called_once_with(QueryExecutionId=QUERY)
            self.assertEqual(get.call_count, 2)
            for state in ["SUCCEEDED", "FAILED", "CANCELLED"]:
                self.athena.state = state
                result = self.invoke("cancel_query", query_id=QUERY)["result"]
                self.assertEqual(result["state"], state)
                self.assertIs(result["cancellation_requested"], False)
            stop.assert_called_once()
            self.athena.workgroup = "other"
            self.assertEqual(self.invoke("cancel_query", query_id=QUERY)["error"]["code"], "NOT_ALLOWED")
            stop.assert_called_once()

    def test_cancel_returns_the_newly_observed_cancelled_state(self):
        self.athena.state = "QUEUED"
        with patch.object(self.athena, "stop_query_execution", create=True,
                side_effect=lambda **args: setattr(self.athena, "state", "CANCELLED")):
            self.assertEqual(self.invoke("cancel_query", query_id=QUERY)["result"], {
                "query_id": QUERY, "state": "CANCELLED", "reason": "Athena query was cancelled. Resubmit only if results are still needed.",
                "cancellation_requested": True})

    def test_denied_cancellation_returns_sanitized_error_without_claiming_acceptance(self):
        self.athena.state = "RUNNING"
        with patch.object(self.athena, "stop_query_execution", create=True, side_effect=FakeClientError("AccessDeniedException")) as stop:
            self.assertEqual(self.invoke("cancel_query", query_id=QUERY), {"version": 1, "error": {
                "code": "AccessDeniedException", "message": "The sandbox worker is not allowed to access this resource."}})
            stop.assert_called_once_with(QueryExecutionId=QUERY)

    def test_result_identity_is_opaque_stable_scoped_and_checked_before_reading(self):
        first = self.invoke("get_results", query_id=QUERY, page_size=1)["result"]
        identity = first["result_id"]
        self.assertRegex(identity, r"^[0-9a-f]{64}$")
        second = self.invoke("get_results", query_id=QUERY, expected_result_id=identity, next_token=first["next_token"])["result"]
        self.assertEqual(second["result_id"], identity)
        self.assertNotEqual(self.invoke("get_results", query_id=OTHER_QUERY)["result"]["result_id"], identity)
        with patch.object(self.athena, "get_query_results", side_effect=AssertionError("Read must not happen")) as get:
            self.assertEqual(self.invoke("get_results", query_id=QUERY, expected_result_id="wrong")["error"]["code"], "RESULT_CHANGED")
            get.assert_not_called()
        with patch.object(self.s3, "head_object", return_value={"ContentLength": 42, "ETag": '"replacement"'}):
            self.assertEqual(self.invoke("get_results", query_id=QUERY, next_token=first["next_token"])["error"]["code"], "RESULT_CHANGED")

    def test_result_identity_survives_native_cursor_transition_without_dropping_rows(self):
        metadata = {"ColumnInfo": [{"Name": "id", "Type": "bigint"}]}
        pages = {None: {"ResultSet": {"ResultSetMetadata": metadata,
            "Rows": [{"Data": [{"VarCharValue": "id"}]}, {"Data": [{"VarCharValue": "1"}]}]}, "NextToken": "next"},
            "next": {"ResultSet": {"ResultSetMetadata": metadata, "Rows": [{"Data": [{"VarCharValue": "2"}]}]}}}
        with patch.object(self.athena, "get_query_results", side_effect=lambda **args: pages[args.get("NextToken")]):
            first = self.invoke("get_results", query_id=QUERY)["result"]
            second = self.invoke("get_results", query_id=QUERY, next_token=first["next_token"],
                expected_result_id=first["result_id"])["result"]
            self.assertEqual(first["rows"] + second["rows"], [["1"], ["2"]])
            self.assertEqual(second["columns"], first["columns"])
            self.assertEqual(second["result_id"], first["result_id"])
            self.assertIsNone(second["next_token"])
            with patch.object(self.s3, "head_object", return_value={"ContentLength": 42, "ETag": '"replacement"'}):
                self.assertEqual(self.invoke("get_results", query_id=QUERY, next_token=first["next_token"])["error"]["code"], "RESULT_CHANGED")

    def test_result_revision_change_during_page_read_returns_no_rows(self):
        identity = self.invoke("get_results", query_id=QUERY)["result"]["result_id"]
        for expected in [{}, {"expected_result_id": identity}]:
            with patch.object(self.s3, "head_object", side_effect=[{"ContentLength": 42, "ETag": '"revision-one"'},
                    {"ContentLength": 42, "ETag": '"revision-two"'}]):
                self.assertEqual(self.invoke("get_results", query_id=QUERY, **expected), {"version": 1, "error": {
                    "code": "RESULT_CHANGED", "message": "The query result revision changed."}})

    def test_identity_includes_version_timestamp_and_length_and_fails_when_revision_unknown(self):
        original = {"ContentLength": 42, "ETag": '"one"', "VersionId": "one",
            "LastModified": datetime.datetime(2026, 10, 23, tzinfo=datetime.timezone.utc)}
        with patch.object(self.s3, "head_object", return_value=original):
            identity = self.invoke("get_results", query_id=QUERY)["result"]["result_id"]
        for change in [{"VersionId": "two"}, {"ContentLength": 43},
                {"LastModified": datetime.datetime(2026, 10, 24, tzinfo=datetime.timezone.utc)}]:
            with patch.object(self.s3, "head_object", return_value=dict(original, **change)):
                self.assertEqual(self.invoke("get_results", query_id=QUERY, expected_result_id=identity)["error"]["code"], "RESULT_CHANGED")
        with patch.object(self.s3, "head_object", return_value={"ContentLength": 42}):
            self.assertEqual(self.invoke("get_results", query_id=QUERY)["error"]["code"], "RESULT_IDENTITY_UNAVAILABLE")

    def test_result_authorization_precedes_s3_revision_read(self):
        self.athena.workgroup = "other"
        with patch.object(self.s3, "head_object", side_effect=AssertionError("Unauthorized result")) as head:
            self.assertEqual(self.invoke("get_results", query_id=QUERY)["error"]["code"], "NOT_ALLOWED")
            head.assert_not_called()


if __name__ == "__main__":
    unittest.main()
