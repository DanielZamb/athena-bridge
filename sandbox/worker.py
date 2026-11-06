import base64
import hashlib
import hmac
import json
import os
import re
import time
from functools import cache
from urllib.parse import urlparse

import boto3
from botocore.config import Config
from botocore.exceptions import ClientError


AWS_CONFIG = Config(connect_timeout=3, read_timeout=8, retries={"max_attempts": 1})
DATABASE = os.environ["DATABASE"]
WORKGROUP = os.environ["WORKGROUP"]
BUCKET = os.environ["BUCKET"]
CURSOR_KEY = os.environ["CURSOR_KEY"].encode()
RESULT_PREFIX = "results/"
MAX_RESPONSE = 4_000_000
MAX_SQL_BYTES = 262144
CURSOR_TTL_SECONDS = 3600
MAX_SCAN_BYTES = int(os.environ["MAX_SCAN_BYTES"])
RESULT_RETENTION_DAYS = int(os.environ["RESULT_RETENTION_DAYS"]) if "RESULT_RETENTION_DAYS" in os.environ else None
TABLES_PER_CALL = 100
WORKER_ID = os.environ.get("WORKER_ID", "athena-bridge-sandbox")
ACTIONS = ["worker_info", "list_tables", "describe_table", "search_metadata", "start_query",
    "query_status", "cancel_query", "get_results", "download_result"]


@cache
def client(service):
    config = AWS_CONFIG.merge(Config(signature_version="s3v4")) if service == "s3" else AWS_CONFIG
    return boto3.client(service, config=config)


def worker_info():
    limits = {"default_page_size": 200, "max_page_size": 1000,
        "max_response_bytes": MAX_RESPONSE, "metadata_tables_per_call": TABLES_PER_CALL,
        "max_scan_bytes": MAX_SCAN_BYTES, "max_sql_bytes": MAX_SQL_BYTES,
        "cursor_ttl_seconds": CURSOR_TTL_SECONDS}
    if RESULT_RETENTION_DAYS is not None:
        limits["result_retention_days"] = RESULT_RETENTION_DAYS
    return {"worker_id": WORKER_ID, "basis": "configured", "default_database": DATABASE,
        "databases": [DATABASE], "catalog": "AwsDataCatalog", "workgroup": WORKGROUP,
        "actions": ACTIONS, "features": ["result_identity"],
        "limits": limits}


class Rejected(Exception):
    def __init__(self, code, message):
        self.code, self.message = code, message


def reject(code, message):
    raise Rejected(code, message)


def cursor_encode(scope, state):
    payload = json.dumps({"scope": scope, "state": state, "expires": int(time.time()) + CURSOR_TTL_SECONDS}, separators=(",", ":")).encode()
    signature = hmac.new(CURSOR_KEY, payload, hashlib.sha256).digest()
    return base64.urlsafe_b64encode(signature + payload).decode()


def cursor_decode(token, scope):
    if not isinstance(token, str) or len(token) > 16384:
        reject("INVALID_CURSOR", "Invalid continuation token.")
    try:
        raw = base64.b64decode(token, altchars=b"-_", validate=True)
        signature, payload = raw[:32], raw[32:]
        expected = hmac.new(CURSOR_KEY, payload, hashlib.sha256).digest()
        if not hmac.compare_digest(signature, expected):
            raise ValueError()
        decoded = json.loads(payload)
        if decoded["scope"] != scope or decoded["expires"] < time.time():
            raise ValueError()
        return decoded["state"]
    except (ValueError, KeyError, TypeError):
        reject("INVALID_CURSOR", "Expired, mismatched, or invalid continuation token.")


def page(columns, rows, size, scope, state=None, query_id=None):
    offset = state.get("offset", 0) if state else 0
    end = min(offset + size, len(rows))
    result = {"columns": columns, "rows": rows[offset:end], "next_token": None}
    if query_id:
        result["query_id"] = query_id
    while len(json.dumps(result).encode()) > MAX_RESPONSE and end > offset:
        end -= max(1, (end - offset) // 2)
        result["rows"] = rows[offset:end]
    if end == offset and offset < len(rows):
        reject("ROW_TOO_LARGE", "One row exceeds the worker response limit.")
    if end < len(rows):
        continuation = dict(state or {}, offset=end)
    elif state and state.get("following"):
        continuation = dict(state, native=state["following"], offset=0, first=False)
    else:
        continuation = None
    if continuation is not None:
        continuation.pop("following", None)
        result["next_token"] = cursor_encode(scope, continuation)
    return result


def authorize_query(query_id):
    if not isinstance(query_id, str) or not re.fullmatch(r"[a-f0-9-]{36}", query_id):
        reject("NOT_ALLOWED", "This query is not available in the sandbox.")
    execution = client("athena").get_query_execution(QueryExecutionId=query_id)["QueryExecution"]
    context = execution.get("QueryExecutionContext", {})
    output = execution.get("ResultConfiguration", {}).get("OutputLocation", "")
    if execution.get("WorkGroup") != WORKGROUP or context.get("Database") != DATABASE or context.get("Catalog", "AwsDataCatalog").lower() != "awsdatacatalog":
        reject("NOT_ALLOWED", "This query is not available in the sandbox.")
    if output and not output.startswith(f"s3://{BUCKET}/{RESULT_PREFIX}"):
        reject("NOT_ALLOWED", "This result is outside the sandbox.")
    return execution


def require_success(execution):
    state = execution["Status"]["State"]
    if state != "SUCCEEDED":
        reject("NOT_READY" if state in ("QUEUED", "RUNNING") else "QUERY_FAILED", "Query results are not available.")


def start(event):
    request_id, sql = event.get("request_id"), event.get("sql")
    if not isinstance(request_id, str) or not re.fullmatch(r"[A-Za-z0-9._:-]{32,128}", request_id):
        reject("INVALID_REQUEST", "A valid request_id is required.")
    if not isinstance(sql, str) or not 0 < len(sql.encode()) <= MAX_SQL_BYTES:
        reject("INVALID_REQUEST", "A SQL statement is required.")
    stripped = re.sub(r"'(?:''|[^'])*'|\"(?:\"\"|[^\"])*\"|/\*.*?\*/|--[^\n]*", " ", sql, flags=re.S).strip()
    stripped = stripped.rstrip("; \t\r\n")
    if not re.match(r"^(SELECT|WITH|EXPLAIN)\b", stripped, flags=re.I):
        reject("NOT_ALLOWED", "Only SELECT, WITH, and EXPLAIN queries are allowed in this sandbox.")
    forbidden = {"INSERT", "UPDATE", "DELETE", "MERGE", "CREATE", "ALTER", "DROP", "TRUNCATE", "UNLOAD", "GRANT", "REVOKE", "EXECUTE", "PREPARE", "CALL", "VACUUM", "OPTIMIZE"}
    if ";" in stripped or forbidden.intersection(re.findall(r"[A-Za-z_]+", stripped.upper())):
        reject("NOT_ALLOWED", "Only one read-only analytical statement is allowed.")
    response = client("athena").start_query_execution(
        QueryString=sql, ClientRequestToken=request_id,
        QueryExecutionContext={"Database": DATABASE, "Catalog": "AwsDataCatalog"},
        WorkGroup=WORKGROUP,
        ResultConfiguration={"OutputLocation": f"s3://{BUCKET}/{RESULT_PREFIX}", "EncryptionConfiguration": {"EncryptionOption": "SSE_S3"}},
    )
    return {"query_id": response["QueryExecutionId"]}


def table_definition(table):
    if not isinstance(table, str) or not re.fullmatch(r"[a-z0-9_]+", table):
        reject("INVALID_REQUEST", "A valid table name is required.")
    return client("glue").get_table(DatabaseName=DATABASE, Name=table)["Table"]


def table_page(state):
    arguments = {"DatabaseName": DATABASE, "MaxResults": TABLES_PER_CALL}
    if state and state.get("native"):
        arguments["NextToken"] = state["native"]
    response = client("glue").get_tables(**arguments)
    return response.get("TableList", []), dict(state or {}, following=response.get("NextToken"))


def table_columns(definition):
    return [(column, "false") for column in definition.get("StorageDescriptor", {}).get("Columns", [])] + [
        (column, "true") for column in definition.get("PartitionKeys", [])]


def metadata(event, size, scope, state):
    columns = [{"name": name, "type": "varchar"} for name in (
        ["table_name", "table_type", "description"] if event["action"] == "list_tables"
        else ["column_name", "data_type", "description", "partition_key"]
    )]
    if event["action"] == "list_tables":
        tables, state = table_page(state)
        rows = [[t["Name"], t.get("TableType", "EXTERNAL_TABLE"), t.get("Description")] for t in tables]
    else:
        definition = table_definition(event.get("table"))
        rows = [[c["Name"], c["Type"], c.get("Comment"), partition] for c, partition in table_columns(definition)]
    return page(columns, rows, size, scope, state)


def search_filters(event):
    term, kind, table = event.get("term"), event.get("kind", "all"), event.get("table")
    if not isinstance(term, str) or not 0 < len(term) <= 1024:
        reject("INVALID_REQUEST", "A search term of 1 to 1024 characters is required.")
    if not isinstance(kind, str) or kind.lower() not in ("all", "table", "column"):
        reject("INVALID_REQUEST", "kind must be all, table, or column.")
    if table is not None:
        if not isinstance(table, str) or not re.fullmatch(r"[A-Za-z0-9_]+", table):
            reject("INVALID_REQUEST", "A valid table name is required.")
        table = table.lower()
    return {"term": term.casefold(), "kind": kind.lower(), "table": table}


def search_metadata(filters, size, scope, state):
    columns = [{"name": name, "type": "varchar"} for name in
        ["kind", "table_name", "column_name", "data_type", "description", "partition_key"]]
    if filters["table"]:
        tables = [table_definition(filters["table"])]
    else:
        tables, state = table_page(state)
    rows = []
    term, kind = filters["term"], filters["kind"]
    for table in tables:
        if kind in ("all", "table") and any(term in (value or "").casefold() for value in [table["Name"], table.get("Description")]):
            rows.append(["table", table["Name"], None, None, table.get("Description"), None])
        if kind in ("all", "column"):
            for column, partition in table_columns(table):
                if any(term in (value or "").casefold() for value in [column["Name"], column.get("Comment")]):
                    rows.append(["column", table["Name"], column["Name"], column["Type"], column.get("Comment"), partition])
    return page(columns, rows, size, scope, state)


def query_failure_reason(status, failure):
    messages = [value for value in [status.get("AthenaError", {}).get("ErrorMessage"),
        status.get("StateChangeReason")] if isinstance(value, str)]
    location = next((match for text in messages if (match := re.match(
        r"^(?:[A-Z_]+:\s*)?line ([1-9][0-9]{0,8}):([1-9][0-9]{0,8})(?::|\b)", text))), None)
    if location:
        failure.update(sql_line=int(location[1]), sql_column=int(location[2]))
    kind, category = failure.get("error_type"), failure.get("error_category")
    if any(re.match(r"^(?:Query\b.*\bexceed(?:ed|ing|s)?\b.*\b(?:bytes scanned|scan) limit\b|"
            r"Bytes scanned limit\b.*\bexceed(?:ed|ing|s)?\b)", text, flags=re.I) for text in messages):
        reason = "Athena scan limit exceeded. Add partition filters or reduce the query scope before resubmitting."
    elif status["State"] == "CANCELLED":
        reason = "Athena query was cancelled. Resubmit only if results are still needed."
    elif kind == 1006 or kind is None and (location or any(text.startswith("SYNTAX_ERROR:") for text in messages)):
        reason = "Check SQL syntax and column references using the describe command."
    elif kind == 1004:
        reason = "Check table and column definitions with the describe command before correcting SQL."
    elif kind == 1110:
        reason = "Check database and table names with the tables and describe commands."
    elif kind in (1500, 1501):
        reason = "Ask the worker administrator to check data access permissions before retrying."
    elif kind in range(9):
        reason = "Reduce the data scanned or simplify joins and aggregations before retrying."
    elif kind in (206, 210):
        reason = "Reduce the query scope before retrying after the Athena timeout."
    elif kind in (1400, 1401, 1403, 1404, 1405, 1406):
        reason = "Athena throttled the query. Retry after a delay and reduce concurrent queries."
    elif category == 1:
        reason = "Athena reported a system error. " + ("Retry after a delay." if failure.get("retryable") else
            "Contact the worker administrator with the query ID.")
    elif category == 2:
        reason = "Check SQL syntax, identifiers, and data types with metadata commands before resubmitting."
    else:
        reason = "Contact the worker administrator with the query ID to check query scope and configuration."
    if location:
        reason += f" SQL line {failure['sql_line']}, column {failure['sql_column']}."
    return reason


def query_status(query_id, execution):
    status = execution["Status"]
    result = {"query_id": query_id, "state": status["State"]}
    statistics = execution.get("Statistics", {})
    quantities = {}
    for source, target in [("DataScannedInBytes", "data_scanned_bytes"),
            ("EngineExecutionTimeInMillis", "engine_execution_ms"), ("TotalExecutionTimeInMillis", "total_execution_ms"),
            ("QueryQueueTimeInMillis", "queue_ms"), ("QueryPlanningTimeInMillis", "planning_ms")]:
        value = statistics.get(source)
        if type(value) is int and value >= 0:
            quantities[target] = str(value)
    reused = statistics.get("ResultReuseInformation", {}).get("ReusedPreviousResult")
    if type(reused) is bool:
        quantities["result_reused"] = reused
    if quantities:
        result["statistics"] = quantities
    failure = status.get("AthenaError", {})
    sanitized = {}
    for source, target in [("ErrorCategory", "error_category"), ("ErrorType", "error_type")]:
        value = failure.get(source)
        if type(value) is int and value >= 0:
            sanitized[target] = value
    if type(failure.get("Retryable")) is bool:
        sanitized["retryable"] = failure["Retryable"]
    if status["State"] in ("FAILED", "CANCELLED"):
        result["reason"] = query_failure_reason(status, sanitized)
    if sanitized:
        result["failure"] = sanitized
    return result


def cancel(event):
    query_id = event.get("query_id")
    execution = authorize_query(query_id)
    requested = execution["Status"]["State"] not in ("SUCCEEDED", "FAILED", "CANCELLED")
    if requested:
        client("athena").stop_query_execution(QueryExecutionId=query_id)
        execution = authorize_query(query_id)
    return dict(query_status(query_id, execution), cancellation_requested=requested)


def result_key(event, execution, code):
    location = urlparse(execution.get("ResultConfiguration", {}).get("OutputLocation", ""))
    key = location.path.lstrip("/")
    if location.scheme != "s3" or location.netloc != BUCKET or key != f"{RESULT_PREFIX}{event['query_id']}.csv":
        reject(code, "This query has no supported CSV result file.")
    return key


def result_identity(event, execution):
    key = result_key(event, execution, "RESULT_IDENTITY_UNAVAILABLE")
    revision = client("s3").head_object(Bucket=BUCKET, Key=key)
    if not revision.get("ETag") and revision.get("VersionId") in (None, "", "null"):
        reject("RESULT_IDENTITY_UNAVAILABLE", "The result revision is unavailable.")
    modified = revision.get("LastModified")
    identity = {"query_id": event["query_id"], "database": DATABASE, "catalog": "AwsDataCatalog",
        "workgroup": WORKGROUP, "bucket": BUCKET, "key": key, "version": revision.get("VersionId"),
        "etag": revision.get("ETag"), "size": revision.get("ContentLength"),
        "modified": modified.isoformat() if modified is not None else None}
    return hmac.new(CURSOR_KEY, json.dumps(identity, sort_keys=True, separators=(",", ":")).encode(), hashlib.sha256).hexdigest()


def results(event, size, scope, state):
    execution = authorize_query(event.get("query_id"))
    require_success(execution)
    result_id = result_identity(event, execution)
    if event.get("expected_result_id", result_id) != result_id or state and state.get("result_id", result_id) != result_id:
        reject("RESULT_CHANGED", "The query result revision changed.")
    state = dict(state or {"native": None, "offset": 0, "first": True}, result_id=result_id)
    arguments = {"QueryExecutionId": event["query_id"], "MaxResults": 1000}
    if state.get("native"):
        arguments["NextToken"] = state["native"]
    response = client("athena").get_query_results(**arguments)
    metadata = response["ResultSet"]["ResultSetMetadata"]["ColumnInfo"]
    columns = []
    for c in metadata:
        kind = c["Type"]
        if kind == "decimal":
            kind = f"decimal({c['Precision']},{c['Scale']})"
        columns.append({"name": c.get("Label") or c["Name"], "type": kind})
    raw_rows = response["ResultSet"].get("Rows", [])
    if state.get("first"):
        raw_rows = raw_rows[1:]
    rows = []
    for row in raw_rows:
        values = [cell.get("VarCharValue") for cell in row.get("Data", [])]
        if len(values) > len(columns):
            reject("INVALID_RESULT", "Athena returned an invalid row width.")
        rows.append(values + [None] * (len(columns) - len(values)))
    state = dict(state, following=response.get("NextToken"))
    result = page(columns, rows, size, scope, state, event["query_id"])
    if result_identity(event, execution) != result_id:
        reject("RESULT_CHANGED", "The query result revision changed.")
    result["result_id"] = result_id
    return result


def download(event):
    execution = authorize_query(event.get("query_id"))
    require_success(execution)
    key = result_key(event, execution, "DOWNLOAD_UNAVAILABLE")
    size = client("s3").head_object(Bucket=BUCKET, Key=key)["ContentLength"]
    url = client("s3").generate_presigned_url("get_object", Params={"Bucket": BUCKET, "Key": key}, ExpiresIn=300)
    return {"url": url, "format": "csv", "size_bytes": size}


def handler(event, context):
    try:
        if not isinstance(event, dict) or event.get("version") != 1:
            reject("INVALID_REQUEST", "Expected a version-one request object.")
        if event.get("database") not in (None, "", DATABASE):
            reject("NOT_ALLOWED", "This database is not available in the sandbox.")
        action = event.get("action")
        if action == "worker_info":
            result = worker_info()
        elif action == "start_query":
            result = start(event)
        elif action == "query_status":
            result = query_status(event.get("query_id"), authorize_query(event.get("query_id")))
        elif action == "cancel_query":
            result = cancel(event)
        elif action in ("list_tables", "describe_table", "search_metadata", "get_results"):
            size = event.get("page_size", 200)
            if type(size) is not int or not 1 <= size <= 1000:
                reject("INVALID_REQUEST", "page_size must be between 1 and 1000.")
            scope = {"action": action, "database": DATABASE, "table": event.get("table"), "query_id": event.get("query_id")}
            if action == "search_metadata":
                filters = search_filters(event)
                scope.update(filters)
            if action == "get_results" and "expected_result_id" in event:
                if not isinstance(event["expected_result_id"], str) or not event["expected_result_id"]:
                    reject("INVALID_REQUEST", "expected_result_id must be nonempty text.")
            state = cursor_decode(event["next_token"], scope) if event.get("next_token") else None
            if action == "get_results":
                result = results(event, size, scope, state)
            elif action == "search_metadata":
                result = search_metadata(filters, size, scope, state)
            else:
                result = metadata(event, size, scope, state)
        elif action == "download_result":
            result = download(event)
        else:
            reject("INVALID_REQUEST", "Unknown worker action.")
        return {"version": 1, "result": result}
    except Rejected as error:
        return {"version": 1, "error": {"code": error.code, "message": error.message}}
    except ClientError as error:
        code = error.response.get("Error", {}).get("Code", "AWS_ERROR")
        messages = {
            "InvalidRequestException": "Athena rejected the request. Check SQL, identifiers, and request-id reuse.",
            "EntityNotFoundException": "The requested table does not exist.",
            "AccessDeniedException": "The sandbox worker is not allowed to access this resource.",
        }
        return {"version": 1, "error": {"code": code, "message": messages.get(code, "An AWS operation failed in the sandbox worker.")}}
    except Exception:
        return {"version": 1, "error": {"code": "WORKER_ERROR", "message": "The sandbox worker could not complete the request."}}
