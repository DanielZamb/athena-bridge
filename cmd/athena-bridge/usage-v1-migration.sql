-- runs/labels have been renamed to *_v1 and the v2 schema created in the same
-- transaction. Preserve IDs, session fingerprints and all other metadata.
INSERT INTO labels SELECT * FROM labels_v1;
INSERT INTO labels(kind, value)
SELECT DISTINCT 7, CASE a.value WHEN 'human' THEN 'terminal' ELSE a.value END
FROM runs_v1 r JOIN labels_v1 a ON a.id = r.agent_id
WHERE a.value IN ('t3-code', 'github-actions', 'gitlab-ci', 'ci', 'human')
ON CONFLICT DO NOTHING;
INSERT INTO labels(kind, value) VALUES (1, 'unknown') ON CONFLICT DO NOTHING;
INSERT INTO runs(id, started_at, duration_ms, agent_id, session_id, profile_id,
    database_id, command_id, error_id, version_id, exit_code, format, flags,
    sql_hash, verb, query_hash, rows_written, pages_written, launcher_id)
SELECT r.id, r.started_at * 1000, r.duration_ms,
    CASE WHEN a.value IN ('t3-code', 'github-actions', 'gitlab-ci', 'ci')
        THEN (SELECT id FROM labels WHERE kind = 1 AND value = 'unknown')
        ELSE r.agent_id END,
    r.session_id, r.profile_id, r.database_id, r.command_id, r.error_id,
    r.version_id, r.exit_code, r.format, r.flags, r.sql_hash, r.verb,
    r.query_hash, r.rows_written, r.pages_written,
    (SELECT id FROM labels WHERE kind = 7 AND value =
        CASE a.value WHEN 'human' THEN 'terminal' ELSE a.value END)
FROM runs_v1 r JOIN labels_v1 a ON a.id = r.agent_id;
DROP TABLE runs_v1;
DROP TABLE labels_v1;
