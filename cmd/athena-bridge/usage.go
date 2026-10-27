package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	_ "modernc.org/sqlite"
)

//go:embed usage-schema.sql
var usageSchema string

//go:embed usage-v1-migration.sql
var usageV1Migration string

const version = "0.1.0"

type usageRecord struct {
	started                    time.Time
	command, profile, database string
	errorCode                  Code
	format, flags, verb        int
	sqlHash                    []byte
	tabular                    bool
	rows, pages                int64
}

func usageHash(value string) []byte {
	if value == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(value))
	return sum[:8]
}

func (u *usageRecord) sqlIntent(statement string) {
	if len(statement) > 262144 {
		return
	}
	fields := strings.Fields(strings.ToLower(statement))
	u.sqlHash = usageHash(strings.Join(fields, " "))
	if len(fields) > 0 {
		for i, verb := range []string{"select", "with", "explain", "insert", "update", "delete", "create", "alter", "drop", "show", "describe"} {
			if fields[0] == verb {
				u.verb = i + 1
				break
			}
		}
	}
}

func usageCommand(args []string) string {
	if len(args) == 0 {
		return "help"
	}
	switch args[0] {
	case "help", "--help", "-h":
		return "help"
	case "info", "search", "cancel", "tables", "describe", "query", "start", "status", "results", "export", "download", "version":
		return args[0]
	default:
		// Never persist an arbitrary argument in place of a command name.
		return "unknown"
	}
}

func usageTerminal(stream any) bool {
	f, ok := stream.(*os.File)
	return ok && isatty.IsTerminal(f.Fd())
}

func usagePath() (string, error) {
	if path := os.Getenv("ATHENA_BRIDGE_USAGE_DB"); path != "" {
		if path == "off" {
			return "", nil
		}
		return filepath.Abs(path)
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		var err error
		if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
			base, err = os.UserConfigDir()
		} else {
			base, err = os.UserHomeDir()
			base = filepath.Join(base, ".local", "state")
		}
		if err != nil {
			return "", err
		}
	}
	if !filepath.IsAbs(base) {
		return "", errors.New("usage state directory must be absolute")
	}
	return filepath.Join(base, "athena-bridge", "usage.db"), nil
}

func usageURI(path string) url.URL {
	path = filepath.ToSlash(path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return url.URL{Scheme: "file", Path: path}
}

func openUsageDB(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if errors.Is(err, os.ErrExist) {
		f, err = openExportFile(path, os.O_RDWR)
	}
	if err != nil {
		return nil, err
	}
	err = f.Chmod(0600)
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	uri := usageURI(path)
	params := url.Values{"_pragma": {"busy_timeout(25)", "journal_mode(WAL)", "foreign_keys(ON)", "synchronous(NORMAL)"}, "_txlock": {"immediate"}}
	uri.RawQuery = params.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err == nil {
		db.SetMaxOpenConns(1)
	}
	return db, err
}

func usageLabel(ctx context.Context, tx *sql.Tx, kind int, value string) (any, error) {
	if value == "" {
		return nil, nil
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO labels(kind, value) VALUES (?, ?) ON CONFLICT DO NOTHING", kind, value); err != nil {
		return nil, err
	}
	var id int64
	err := tx.QueryRowContext(ctx, "SELECT id FROM labels WHERE kind = ? AND value = ?", kind, value).Scan(&id)
	return id, err
}

func usageOptional(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func (u *usageRecord) write(ctx context.Context, path string, identity usageIdentity, queryID string, exit int, duration time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	db, err := openUsageDB(path)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var schemaVersion int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&schemaVersion); err != nil {
		return err
	}
	switch schemaVersion {
	case 0:
		if _, err := tx.ExecContext(ctx, usageSchema); err != nil {
			return err
		}
	case 1:
		if _, err := tx.ExecContext(ctx, "ALTER TABLE runs RENAME TO runs_v1; ALTER TABLE labels RENAME TO labels_v1;"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, usageSchema); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, usageV1Migration); err != nil {
			return err
		}
	case 2:
	default:
		return errors.New("unsupported usage schema version")
	}
	values := make([]any, 8)
	for i, value := range []string{identity.agent, u.profile, u.database, u.command, string(u.errorCode), version, identity.launcher, identity.model} {
		values[i], err = usageLabel(ctx, tx, i+1, value)
		if err != nil {
			return err
		}
	}
	var sessionID any
	if identity.session != nil {
		if _, err := tx.ExecContext(ctx, "INSERT INTO sessions(fingerprint) VALUES (?) ON CONFLICT DO NOTHING", identity.session); err != nil {
			return err
		}
		var id int64
		if err := tx.QueryRowContext(ctx, "SELECT id FROM sessions WHERE fingerprint = ?", identity.session).Scan(&id); err != nil {
			return err
		}
		sessionID = id
	}
	var rows, pages any
	if u.tabular {
		rows, pages = u.rows, u.pages
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO runs(started_at, duration_ms, agent_id, session_id,
		profile_id, database_id, command_id, error_id, version_id, exit_code, format, flags,
		sql_hash, verb, query_hash, rows_written, pages_written, launcher_id, model_id, effort)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.started.UnixMilli(), duration.Milliseconds(), values[0], sessionID, values[1], values[2], values[3], values[4], values[5],
		exit, usageOptional(u.format), u.flags, u.sqlHash, usageOptional(u.verb), usageHash(queryID), rows, pages, values[6], values[7], usageOptional(identity.effort))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (u *usageRecord) finish(in io.Reader, out io.Writer, queryID string, exit int) {
	duration := time.Since(u.started)
	path, err := usagePath()
	if err != nil || path == "" {
		return
	}
	// Identity lookup and SQL share the original logging budget. Cancellation of
	// the command itself must not prevent recording its failure.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	identity := detectUsageIdentity(os.Getenv, usageTerminal(in) && usageTerminal(out))
	if identity.model == "" && os.Getenv("ATHENA_BRIDGE_MODEL") == "" && (identity.agent != "claude-code" || os.Getenv("ANTHROPIC_MODEL") == "") {
		modelCtx, stop := context.WithTimeout(ctx, 25*time.Millisecond)
		model, effort := usageSessionModel(modelCtx, os.Getenv, identity.agent)
		stop()
		identity.model = model
		if identity.effort == 0 {
			identity.effort = usageEffort(effort)
		}
	}
	// Silent best effort preserves both machine diagnostics and command exit codes.
	_ = u.write(ctx, path, identity, queryID, exit, duration)
}
