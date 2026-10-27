-- Version 1. All timestamps are UTC Unix seconds; durations are milliseconds.
-- label kinds: 1 agent, 2 profile, 3 database, 4 command, 5 error code, 6 CLI version.
-- format: NULL unknown/not applicable, 1 table, 2 json, 3 csv.
-- verb: NULL unknown, 1 SELECT, 2 WITH, 3 EXPLAIN, 4 INSERT, 5 UPDATE,
--       6 DELETE, 7 CREATE, 8 ALTER, 9 DROP, 10 SHOW, 11 DESCRIBE.
-- flags: 1 out, 2 pretty, 4 all, 8 wait, 16 resume, 32 SQL file/stdin,
--        64 continuation supplied, 128 explicit request ID.
-- Typical run: ~45 bytes including record header, rowid and cell pointer;
-- session + SQL + query hashes and populated counters bring it to ~65 bytes.
-- NULLs cost one header byte and no payload. Small dictionary IDs cost 1-2
-- payload bytes. Sessions store one 8-byte hash plus their unique index once.
-- Page slack, dictionaries and SQLite's fixed schema pages are additional.
CREATE TABLE labels (
    id INTEGER PRIMARY KEY,
    kind INTEGER NOT NULL CHECK (kind BETWEEN 1 AND 6),
    value TEXT NOT NULL CHECK (length(value) > 0),
    UNIQUE (kind, value)
) STRICT;
CREATE TABLE sessions (
    id INTEGER PRIMARY KEY,
    fingerprint BLOB NOT NULL UNIQUE CHECK (length(fingerprint) = 8)
) STRICT;
CREATE TABLE runs (
    id INTEGER PRIMARY KEY,
    started_at INTEGER NOT NULL,
    duration_ms INTEGER NOT NULL,
    agent_id INTEGER NOT NULL REFERENCES labels(id),
    session_id INTEGER REFERENCES sessions(id),
    profile_id INTEGER REFERENCES labels(id),
    database_id INTEGER REFERENCES labels(id),
    command_id INTEGER NOT NULL REFERENCES labels(id),
    error_id INTEGER REFERENCES labels(id),
    version_id INTEGER NOT NULL REFERENCES labels(id),
    exit_code INTEGER NOT NULL,
    format INTEGER CHECK (format BETWEEN 1 AND 3),
    flags INTEGER NOT NULL,
    sql_hash BLOB CHECK (length(sql_hash) = 8),
    verb INTEGER CHECK (verb BETWEEN 1 AND 11),
    query_hash BLOB CHECK (length(query_hash) = 8),
    rows_written INTEGER,
    pages_written INTEGER
) STRICT;
PRAGMA user_version = 1;
