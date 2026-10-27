package main

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func usageTestPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "usage?#.db")
	t.Setenv("ATHENA_BRIDGE_USAGE_DB", path)
	t.Setenv("ATHENA_BRIDGE_AGENT", "test-agent")
	t.Setenv("ATHENA_BRIDGE_SESSION", "test-session-private")
	return path
}

func readUsageDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	uri := usageURI(path)
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestUsageSuccessFailureAndOutputParity(t *testing.T) {
	path := usageTestPath(t)
	t.Setenv("AWS_PROFILE", "environment-profile")
	c := localWorker(t, func(req request, w http.ResponseWriter) {
		reply(w, page{Columns: []column{{"name", "varchar"}}, Rows: [][]*string{{str("exact-value")}}})
	})
	args := []string{"tables", "--function", "local-worker", "--profile", "flag-profile", "--database", "analytics", "--format", "json"}
	var on, off, onErr, offErr bytes.Buffer
	if exit := run(context.Background(), args, nil, &on, &onErr, factory(c)); exit != 0 {
		t.Fatalf("success exit = %d: %s", exit, &onErr)
	}
	t.Setenv("ATHENA_BRIDGE_USAGE_DB", "off")
	if exit := run(context.Background(), args, nil, &off, &offErr, factory(c)); exit != 0 {
		t.Fatalf("logging off exit = %d", exit)
	}
	if !bytes.Equal(on.Bytes(), off.Bytes()) || !bytes.Equal(onErr.Bytes(), offErr.Bytes()) {
		t.Fatalf("logging changed output: on=%q/%q off=%q/%q", &on, &onErr, &off, &offErr)
	}
	t.Setenv("ATHENA_BRIDGE_USAGE_DB", path)
	if exit := run(context.Background(), []string{"not-a-command-private"}, nil, io.Discard, io.Discard, factory(c)); exit != 2 {
		t.Fatalf("argument failure exit = %d", exit)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if exit := run(ctx, args, nil, io.Discard, io.Discard, factory(c)); exit != 3 {
		t.Fatalf("cancellation exit = %d", exit)
	}
	db := readUsageDB(t, path)
	var agent, profile, database, command, cliVersion string
	var exit, format, rows, pages, duration, started int64
	err := db.QueryRow(`SELECT a.value, p.value, d.value, c.value, v.value, r.exit_code,
		r.format, r.rows_written, r.pages_written, r.duration_ms, r.started_at
		FROM runs r JOIN labels a ON a.id=r.agent_id JOIN labels p ON p.id=r.profile_id
		JOIN labels d ON d.id=r.database_id JOIN labels c ON c.id=r.command_id
		JOIN labels v ON v.id=r.version_id WHERE r.id=1`).Scan(&agent, &profile, &database, &command, &cliVersion, &exit, &format, &rows, &pages, &duration, &started)
	if err != nil {
		t.Fatal(err)
	}
	if agent != "test-agent" || profile != "flag-profile" || database != "analytics" || command != "tables" || cliVersion != "0.1.0" || exit != 0 || format != 2 || rows != 1 || pages != 1 || duration < 0 || started < time.Now().Add(-time.Minute).UnixMilli() {
		t.Fatalf("unexpected success metadata: %q %q %q %q %q %d %d %d %d %d %d", agent, profile, database, command, cliVersion, exit, format, rows, pages, duration, started)
	}
	for i, expected := range []struct {
		exit int
		code string
	}{{2, "INVALID_ARGUMENT"}, {3, "CANCELLED"}} {
		var code string
		if err := db.QueryRow("SELECT r.exit_code, e.value FROM runs r JOIN labels e ON e.id=r.error_id WHERE r.id=?", i+2).Scan(&exit, &code); err != nil || int(exit) != expected.exit || code != expected.code {
			t.Fatalf("failure row = %d %q, error=%v", exit, code, err)
		}
	}
	var sessions, count int
	if err := db.QueryRow("SELECT count(*), count(DISTINCT session_id) FROM runs").Scan(&count, &sessions); err != nil || count != 3 || sessions != 1 {
		t.Fatalf("runs/sessions = %d/%d: %v", count, sessions, err)
	}
}

func TestUsagePrivacyAndSQLSources(t *testing.T) {
	path := usageTestPath(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "private-access-marker")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "private-secret-marker")
	t.Setenv("AWS_SESSION_TOKEN", "private-token-marker")
	statement := "SELECT 'sql-private-marker' FROM events"
	file := filepath.Join(t.TempDir(), "query.sql")
	if err := os.WriteFile(file, []byte(statement), 0600); err != nil {
		t.Fatal(err)
	}
	c := localWorker(t, func(req request, w http.ResponseWriter) {
		switch req.Action {
		case "start_query":
			reply(w, started{QueryID: "private-query-id"})
		case "query_status":
			reply(w, queryStatus{QueryID: "private-query-id", State: "SUCCEEDED"})
		case "get_results":
			reply(w, page{QueryID: "private-query-id", Columns: []column{{"value", "varchar"}}, Rows: [][]*string{{str("row-private-marker")}}, NextToken: "cursor-private-marker"})
		}
	})
	for _, source := range [][]string{{"--sql", statement}, {"--sql-file", file}, {"--sql-file", "-"}} {
		args := append([]string{"query", "--function", "worker-private-name", "--request-id", strings.Repeat("a", 32), "--format", "json"}, source...)
		if exit := run(context.Background(), args, strings.NewReader(statement), io.Discard, io.Discard, factory(c)); exit != 0 {
			t.Fatalf("query exit = %d", exit)
		}
	}
	db := readUsageDB(t, path)
	var hashes, verbs, queryHashes int
	if err := db.QueryRow("SELECT count(DISTINCT sql_hash), sum(verb=1), count(DISTINCT query_hash) FROM runs").Scan(&hashes, &verbs, &queryHashes); err != nil || hashes != 1 || verbs != 3 || queryHashes != 1 {
		t.Fatalf("SQL/query grouping = %d/%d/%d: %v", hashes, verbs, queryHashes, err)
	}
	// Check the actual database and every live sidecar, including the WAL.
	paths, err := filepath.Glob(path + "*")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range paths {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, private := range []string{statement, "sql-private-marker", "private-access-marker", "private-secret-marker", "private-token-marker", "test-session-private", "private-query-id", "row-private-marker", "cursor-private-marker", "worker-private-name", file, strings.Repeat("a", 32)} {
			if bytes.Contains(data, []byte(private)) {
				t.Errorf("private value %q found in %s", private, name)
			}
		}
		if runtime.GOOS != "windows" {
			info, _ := os.Stat(name)
			if info.Mode().Perm() != 0600 {
				t.Errorf("%s mode = %o", name, info.Mode().Perm())
			}
		}
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(filepath.Dir(path))
		if info.Mode().Perm() != 0700 {
			t.Errorf("state directory mode = %o", info.Mode().Perm())
		}
	}
}

func TestUsageExecutionFailure(t *testing.T) {
	path := usageTestPath(t)
	c := localWorker(t, func(req request, w http.ResponseWriter) {
		_, _ = io.WriteString(w, `{"version":1,"error":{"code":"NOT_ALLOWED","message":"private-worker-message"}}`)
	})
	args := []string{"results", "--function", "local-worker", "--query-id", "private-query"}
	if exit := run(context.Background(), args, nil, io.Discard, io.Discard, factory(c)); exit != 1 {
		t.Fatalf("execution failure exit = %d", exit)
	}
	var exit int
	var code string
	err := readUsageDB(t, path).QueryRow("SELECT r.exit_code, e.value FROM runs r JOIN labels e ON e.id=r.error_id").Scan(&exit, &code)
	if err != nil || exit != 1 || code != "WORKER_REJECTED" {
		t.Fatalf("execution failure row = %d %q: %v", exit, code, err)
	}
}

func TestUsageUnavailableDoesNotChangeExitOrStreams(t *testing.T) {
	path := usageTestPath(t)
	if exit := run(context.Background(), []string{"version"}, nil, io.Discard, io.Discard, nil); exit != 0 {
		t.Fatal(exit)
	}
	db := readUsageDB(t, path)
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec("ROLLBACK") })
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	unavailablePaths := []string{path, filepath.Join(blocker, "usage.db")}
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		readOnly := filepath.Join(t.TempDir(), "read-only.db")
		if err := os.WriteFile(readOnly, nil, 0400); err != nil {
			t.Fatal(err)
		}
		unavailablePaths = append(unavailablePaths, readOnly)
	}
	for _, unavailable := range unavailablePaths {
		t.Setenv("ATHENA_BRIDGE_USAGE_DB", unavailable)
		for _, args := range [][]string{{"version"}, {"unknown"}} {
			var on, off, onErr, offErr bytes.Buffer
			start := time.Now()
			onExit := run(context.Background(), args, nil, &on, &onErr, nil)
			if time.Since(start) > time.Second {
				t.Fatal("unavailable usage DB blocked the command for more than one second")
			}
			t.Setenv("ATHENA_BRIDGE_USAGE_DB", "off")
			offExit := run(context.Background(), args, nil, &off, &offErr, nil)
			t.Setenv("ATHENA_BRIDGE_USAGE_DB", unavailable)
			if onExit != offExit || !bytes.Equal(on.Bytes(), off.Bytes()) || !bytes.Equal(onErr.Bytes(), offErr.Bytes()) {
				t.Fatalf("unavailable DB changed result: on=%d %q %q; off=%d %q %q", onExit, &on, &onErr, offExit, &off, &offErr)
			}
		}
	}
}

func TestUsageMigrationAndFutureVersion(t *testing.T) {
	path := usageTestPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	run(context.Background(), nil, nil, io.Discard, io.Discard, nil)
	db := readUsageDB(t, path)
	var schemaVersion, strict, count int
	var journal string
	for query, target := range map[string]any{"PRAGMA user_version": &schemaVersion, "PRAGMA journal_mode": &journal, "SELECT count(*) FROM pragma_table_list WHERE strict=1": &strict, "SELECT count(*) FROM runs": &count} {
		if err := db.QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if schemaVersion != 2 || journal != "wal" || strict != 3 || count != 1 {
		t.Fatalf("migration = version %d journal %q strict %d rows %d", schemaVersion, journal, strict, count)
	}
	if _, err := db.Exec("PRAGMA user_version=99"); err != nil {
		t.Fatal(err)
	}
	if exit := run(context.Background(), []string{"version"}, nil, io.Discard, io.Discard, nil); exit != 0 {
		t.Fatal(exit)
	}
	if err := db.QueryRow("SELECT count(*) FROM runs").Scan(&count); err != nil || count != 1 {
		t.Fatalf("future schema was changed: rows=%d: %v", count, err)
	}
}

func TestUsageAgentPrecedence(t *testing.T) {
	for _, test := range []struct {
		name, want, launcher string
		env                  map[string]string
		terminal             bool
	}{
		{"override", "my-agent", "t3-code", map[string]string{"ATHENA_BRIDGE_AGENT": "my-agent", "CODEX_INTERNAL_ORIGINATOR_OVERRIDE": "T3 Code", "CLAUDECODE": "1", "CODEX_THREAD_ID": "thread"}, false},
		{"t3 claude", "claude-code", "t3-code", map[string]string{"__CFBundleIdentifier": "com.t3tools.t3code", "CLAUDECODE": "1"}, false},
		{"t3 codex", "codex", "t3-code", map[string]string{"T3CODE_CLI_PATH": "/private/bin/t3", "CODEX_THREAD_ID": "thread"}, false},
		{"ai agent wins", "cursor", "t3-code", map[string]string{"AI_AGENT": "cursor_1-0_agent", "CLAUDECODE": "1", "T3CODE_CLI_PATH": "t3"}, false},
		{"claude", "claude-code", "", map[string]string{"CLAUDECODE": "1", "CODEX_CI": "1", "CURSOR_AGENT": "1"}, false},
		{"codex", "codex", "ci", map[string]string{"CODEX_THREAD_ID": "thread", "CI": "1"}, false},
		{"cursor", "cursor", "ci", map[string]string{"CURSOR_AGENT": "1", "CI": "1"}, false},
		{"github", "unknown", "github-actions", map[string]string{"GITHUB_ACTIONS": "true", "CI": "1"}, false},
		{"gitlab", "unknown", "gitlab-ci", map[string]string{"GITLAB_CI": "true", "CI": "1"}, false},
		{"ci", "unknown", "ci", map[string]string{"CI": "true"}, true},
		{"sdk is not agent", "unknown", "", map[string]string{"CLAUDE_AGENT_SDK_VERSION": "0.3.276", "TERM_PROGRAM": "Cursor", "CI": "false"}, false},
		{"terminal", "human", "terminal", nil, true},
		{"launcher override", "human", "custom-terminal", map[string]string{"ATHENA_BRIDGE_LAUNCHER": "custom-terminal", "T3CODE_CLI_PATH": "t3"}, true},
		{"t3 in ci", "unknown", "t3-code", map[string]string{"CI": "true", "T3CODE_CLI_PATH": "t3"}, true},
		{"pipe", "unknown", "", nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			identity := detectUsageIdentity(func(key string) string { return test.env[key] }, test.terminal)
			if identity.agent != test.want || identity.launcher != test.launcher {
				t.Fatalf("identity = %q/%q, want %q/%q", identity.agent, identity.launcher, test.want, test.launcher)
			}
		})
	}
	values := map[string]string{"CODEX_THREAD_ID": "stable-thread", "CODEX_SESSION_ID": "transient-session"}
	env := func(key string) string { return values[key] }
	a := detectUsageIdentity(env, false).session
	values["CODEX_SESSION_ID"] = "different-session"
	b := detectUsageIdentity(env, false).session
	values["ATHENA_BRIDGE_AGENT"] = "my-agent"
	if session := detectUsageIdentity(env, false).session; len(session) != 8 {
		t.Fatal("agent override discarded the available thread identity")
	}
	delete(values, "ATHENA_BRIDGE_AGENT")
	values["ATHENA_BRIDGE_SESSION"] = "explicit-session"
	c := detectUsageIdentity(env, false).session
	if len(a) != 8 || !bytes.Equal(a, b) || bytes.Equal(a, c) || bytes.Contains(a, []byte("stable-thread")) {
		t.Fatalf("session precedence failed: %x %x %x", a, b, c)
	}
}

func TestUsageSQLNormalizationAndPath(t *testing.T) {
	for path, want := range map[string]string{
		"/tmp/usage?#.db":        "file:///tmp/usage%3F%23.db",
		"C:/Users/name/usage.db": "file:///C:/Users/name/usage.db",
	} {
		uri := usageURI(path)
		if uri.String() != want {
			t.Fatalf("usage URI = %q, want %q", uri.String(), want)
		}
	}
	a, b, c := usageRecord{}, usageRecord{}, usageRecord{}
	a.sqlIntent(" SELECT\n 1 ")
	b.sqlIntent("select 1")
	c.sqlIntent("select 2")
	if len(a.sqlHash) != 8 || !bytes.Equal(a.sqlHash, b.sqlHash) || bytes.Equal(a.sqlHash, c.sqlHash) || a.verb != 1 {
		t.Fatalf("SQL fingerprint mismatch: %x %x %x", a.sqlHash, b.sqlHash, c.sqlHash)
	}
	t.Setenv("ATHENA_BRIDGE_USAGE_DB", "")
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	path, err := usagePath()
	if err != nil || path != filepath.Join(base, "athena-bridge", "usage.db") {
		t.Fatalf("XDG path = %q: %v", path, err)
	}
	t.Setenv("ATHENA_BRIDGE_USAGE_DB", "off")
	if path, err := usagePath(); err != nil || path != "" {
		t.Fatalf("disabled path = %q: %v", path, err)
	}
}
