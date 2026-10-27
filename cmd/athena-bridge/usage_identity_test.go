package main

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUsageV1FileMigration(t *testing.T) {
	path := usageTestPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	ddl, err := os.ReadFile("testdata/usage-v1.sql")
	if err != nil {
		t.Fatal(err)
	}
	db := readUsageDB(t, path)
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO labels VALUES
		(1,1,'t3-code'), (2,2,'work-profile'), (3,3,'analytics'), (4,4,'query'),
		(5,5,'QUERY_FAILED'), (6,6,'0.1.0'), (7,1,'codex'), (8,1,'human'),
		(9,1,'github-actions'), (10,1,'claude-code');
		INSERT INTO sessions VALUES (1, X'0102030405060708');
		INSERT INTO runs VALUES (11,1700000000,123,1,1,2,3,4,5,6,1,2,132,X'1112131415161718',1,X'2122232425262728',7,2);
		INSERT INTO runs(id,started_at,duration_ms,agent_id,command_id,version_id,exit_code,flags)
		VALUES (12,1700000001,0,7,4,6,0,0), (13,1700000002,0,8,4,6,0,0),
		(14,1700000003,0,9,4,6,0,0), (15,1700000004,0,10,4,6,0,0);`); err != nil {
		t.Fatal(err)
	}
	var schemaVersion int
	if err := db.QueryRow("PRAGMA user_version").Scan(&schemaVersion); err != nil || schemaVersion != 1 {
		t.Fatalf("fixture version = %d: %v", schemaVersion, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(data, []byte("SQLite format 3\x00")) {
		t.Fatalf("fixture is not an on-disk SQLite file: %v", err)
	}
	before, _ := os.Stat(path)
	for range 2 {
		if exit := run(context.Background(), []string{"version"}, nil, io.Discard, io.Discard, nil); exit != 0 {
			t.Fatal(exit)
		}
	}
	after, _ := os.Stat(path)
	if !os.SameFile(before, after) {
		t.Fatal("migration replaced the database file")
	}
	var started, duration, rows, pages, flags int64
	var agent, launcher, profile, database, command, code, version string
	var session, sqlHash, queryHash []byte
	err = db.QueryRow(`SELECT r.started_at, r.duration_ms, r.rows_written, r.pages_written, r.flags,
		a.value,l.value,p.value,d.value,c.value,e.value,v.value,s.fingerprint,r.sql_hash,r.query_hash
		FROM runs r JOIN labels a ON a.id=r.agent_id JOIN labels l ON l.id=r.launcher_id
		JOIN labels p ON p.id=r.profile_id JOIN labels d ON d.id=r.database_id
		JOIN labels c ON c.id=r.command_id JOIN labels e ON e.id=r.error_id
		JOIN labels v ON v.id=r.version_id JOIN sessions s ON s.id=r.session_id WHERE r.id=11`).Scan(
		&started, &duration, &rows, &pages, &flags, &agent, &launcher, &profile, &database, &command, &code, &version, &session, &sqlHash, &queryHash)
	if err != nil {
		t.Fatal(err)
	}
	if started != 1700000000000 || duration != 123 || rows != 7 || pages != 2 || flags != 132 || agent != "unknown" || launcher != "t3-code" || profile != "work-profile" || database != "analytics" || command != "query" || code != "QUERY_FAILED" || version != "0.1.0" || !bytes.Equal(session, []byte{1, 2, 3, 4, 5, 6, 7, 8}) || !bytes.Equal(sqlHash, []byte{17, 18, 19, 20, 21, 22, 23, 24}) || !bytes.Equal(queryHash, []byte{33, 34, 35, 36, 37, 38, 39, 40}) {
		t.Fatalf("migration changed metadata: %d %d %d %d %d %q %q %q %q %q %q %q %x %x %x", started, duration, rows, pages, flags, agent, launcher, profile, database, command, code, version, session, sqlHash, queryHash)
	}
	for id, expected := range map[int]string{12: "codex/", 13: "human/terminal", 14: "unknown/github-actions", 15: "claude-code/"} {
		var actual string
		if err := db.QueryRow("SELECT a.value || '/' || coalesce(l.value,'') FROM runs r JOIN labels a ON a.id=r.agent_id LEFT JOIN labels l ON l.id=r.launcher_id WHERE r.id=?", id).Scan(&actual); err != nil || actual != expected {
			t.Fatalf("migrated identity %d = %q: %v", id, actual, err)
		}
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM runs WHERE id BETWEEN 11 AND 15 AND model_id IS NULL AND effort IS NULL").Scan(&count); err != nil || count != 5 {
		t.Fatalf("migration invented model/effort: %d: %v", count, err)
	}
	if err := db.QueryRow("PRAGMA user_version").Scan(&schemaVersion); err != nil || schemaVersion != 2 {
		t.Fatalf("migrated version = %d: %v", schemaVersion, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM runs").Scan(&count); err != nil || count != 7 {
		t.Fatalf("migration lost rows: %d: %v", count, err)
	}
	check, err := db.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	if check.Next() {
		t.Fatal("migration broke a foreign key")
	}
}

func TestUsageModelAndEffortPrecedence(t *testing.T) {
	values := map[string]string{"AI_AGENT": "claude-code_2-1-310_agent", "ANTHROPIC_MODEL": "claude-opus-5-5", "CLAUDE_EFFORT": "high"}
	env := func(key string) string { return values[key] }
	i := detectUsageIdentity(env, false)
	if i.agent != "claude-code" || i.model != "claude-opus-5-5" || i.effort != 3 {
		t.Fatalf("configured identity = %+v", i)
	}
	values["ATHENA_BRIDGE_MODEL"], values["ATHENA_BRIDGE_EFFORT"] = "gpt-6.1-sol", "xhigh"
	i = detectUsageIdentity(env, false)
	if i.model != "gpt-6.1-sol" || i.effort != 4 {
		t.Fatalf("overridden identity = %+v", i)
	}
	values["ATHENA_BRIDGE_MODEL"], values["ATHENA_BRIDGE_EFFORT"] = "https://private.invalid/token", "unrecognized"
	i = detectUsageIdentity(env, false)
	if i.model != "" || i.effort != 0 {
		t.Fatalf("invalid overrides were recorded: %+v", i)
	}
}

func TestUsageTranscriptModelPrivacy(t *testing.T) {
	for _, agent := range []string{"claude-code", "codex"} {
		t.Run(agent, func(t *testing.T) {
			path := usageTestPath(t)
			for _, key := range []string{"ATHENA_BRIDGE_MODEL", "ATHENA_BRIDGE_EFFORT", "CLAUDE_EFFORT", "ANTHROPIC_MODEL"} {
				t.Setenv(key, "")
			}
			t.Setenv("ATHENA_BRIDGE_AGENT", agent)
			t.Setenv("__CFBundleIdentifier", "com.t3tools.t3code")
			base := t.TempDir()
			const id = "11111111-1111-4111-8111-111111111111"
			const private = "transcript-private-content-SQL-credentials-marker"
			var transcript, model, record string
			if agent == "claude-code" {
				t.Setenv("CLAUDE_CONFIG_DIR", base)
				t.Setenv("CLAUDE_CODE_SESSION_ID", id)
				cwd, _ := os.Getwd()
				transcript = filepath.Join(base, "projects", usageProjectSlug(cwd), id+".jsonl")
				model = "claude-opus-5-5"
				record = `{"type":"assistant","message":{"model":"claude-opus-5-5","content":"` + private + `"},"reasoning_effort":"max"}`
			} else {
				t.Setenv("CODEX_HOME", base)
				t.Setenv("CODEX_THREAD_ID", id)
				transcript = filepath.Join(base, "sessions", "2026", "10", "26", "rollout-test-"+id+".jsonl")
				model = "gpt-6.1-sol"
				record = `{"type":"turn_context","payload":{"model":"gpt-6.1-sol","content":"` + private + `","effort":"max"}}`
			}
			if err := os.MkdirAll(filepath.Dir(transcript), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(transcript, []byte(record+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			before := time.Now().UnixMilli()
			if exit := run(context.Background(), []string{"version"}, nil, io.Discard, io.Discard, nil); exit != 0 {
				t.Fatal(exit)
			}
			var got, launcher, recordedAgent string
			var started int64
			var effort sql.NullInt64
			db := readUsageDB(t, path)
			err := db.QueryRow(`SELECT m.value,l.value,a.value,r.started_at,r.effort FROM runs r
				JOIN labels m ON m.id=r.model_id AND m.kind=8
				JOIN labels l ON l.id=r.launcher_id AND l.kind=7
				JOIN labels a ON a.id=r.agent_id AND a.kind=1`).Scan(&got, &launcher, &recordedAgent, &started, &effort)
			if err != nil || got != model || launcher != "t3-code" || recordedAgent != agent || started < before || started > time.Now().UnixMilli() {
				t.Fatalf("transcript identity = %q/%q/%q at %d: %v", launcher, recordedAgent, got, started, err)
			}
			// Codex turn context carries the session's effort; Claude's comes only from CLAUDE_EFFORT.
			want := sql.NullInt64{}
			if agent == "codex" {
				want = sql.NullInt64{Int64: 5, Valid: true}
			}
			if effort != want {
				t.Fatalf("transcript effort = %v, want %v", effort, want)
			}
			files, _ := filepath.Glob(path + "*")
			for _, file := range files {
				data, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				for _, secret := range []string{private, transcript, id, record} {
					if bytes.Contains(data, []byte(secret)) {
						t.Fatalf("non-model transcript data reached %s", file)
					}
				}
			}
		})
	}
}

func TestUsageTranscriptWindowsAndDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	filler := func(n int) string {
		return `{"type":"user","message":{"content":"` + strings.Repeat("x", n) + `"}}` + "\n"
	}
	// A model only in the middle, outside both the head and tail windows, is not read.
	write(filler(300<<10) + `{"type":"assistant","message":{"model":"middle-model"}}` + "\n" + filler(70<<10))
	if model, _ := usageTranscriptModel(context.Background(), path, "claude-code"); model != "" {
		t.Fatalf("read a model outside the head and tail windows: %q", model)
	}
	// A long turn pushes the current model out of the tail; the opening record still names it.
	write(`{"type":"turn_context","payload":{"model":"opening-model","effort":"medium"}}` + "\n" + filler(70<<10))
	if model, effort := usageTranscriptModel(context.Background(), path, "codex"); model != "opening-model" || effort != "medium" {
		t.Fatalf("head fallback = %q, %q", model, effort)
	}
	write(`{"type":"assistant","message":{"model":"opening-model"}}` + "\n" + filler(70<<10) +
		`{"type":"assistant","message":{"model":"older-model"}}` + "\n" +
		`{"type":"assistant","message":{"model":"newest-model"}}` + "\n")
	if model, _ := usageTranscriptModel(context.Background(), path, "claude-code"); model != "newest-model" {
		t.Fatalf("latest tail model = %q", model)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if model, _ := usageTranscriptModel(ctx, path, "claude-code"); model != "" {
		t.Fatalf("ignored lookup deadline: %q", model)
	}
	remaining := 0
	if path := findUsageRollout(context.Background(), t.TempDir(), "id", 0, &remaining); path != "" {
		t.Fatal("ignored directory entry budget")
	}
}

func TestUsageCodexV7CalendarDates(t *testing.T) {
	const id = "01941f29-7c00-7000-8000-000000000001" // 2025-01-01 00:00 UTC.
	for _, day := range []string{"2025/01/01", "2024/12/31"} {
		base := t.TempDir()
		path := filepath.Join(base, "sessions", day, "rollout-test-"+id+".jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"type":"turn_context","payload":{"model":"gpt-6.1-sol","effort":"xhigh"}}`+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		values := map[string]string{"CODEX_HOME": base, "CODEX_THREAD_ID": id}
		model, effort := usageSessionModel(context.Background(), func(key string) string { return values[key] }, "codex")
		if model != "gpt-6.1-sol" || effort != "xhigh" {
			t.Fatalf("identity in calendar directory %s = %q, %q", day, model, effort)
		}
	}
}
