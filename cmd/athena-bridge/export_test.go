package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var exportColumns = []column{{"value", "bigint"}, {"value", "varchar"}, {"amount", "decimal(30,18)"}}

func exportRows() [][]*string {
	return [][]*string{
		{str("9007199254740993"), nil, str("1.230000000000000001")},
		{str("2"), str(""), nil},
		{str("3"), str("\\N,\"quoted\"\nnext\\line"), str("0.000000000000000001")},
		{str("4"), str("end"), str("-100.0000")},
	}
}
func exportClient(t *testing.T, handler func(request, http.ResponseWriter)) *client {
	c := localWorker(t, func(req request, w http.ResponseWriter) {
		if req.Action != "get_results" || req.QueryID != "q" || req.SQL != "" || req.RequestID != "" || req.PageSize != 200 {
			t.Errorf("export must only retrieve existing rows: %+v", req)
		}
		handler(req, w)
	})
	c.region = "us-east-1"
	return c
}
func invokeExport(t *testing.T, c *client, args ...string) (int, string, []diagnosticEvent) {
	t.Helper()
	var out, stderr bytes.Buffer
	code := run(context.Background(), append([]string{"export"}, args...), strings.NewReader(""), &out, &stderr, factory(c))
	return code, out.String(), readDiagnostics(t, stderr.String())
}
func startExportArgs(destination, format string) []string {
	return []string{"--function", "test-worker", "--query-id", "q", "--out", destination, "--format", format}
}
func readExportState(t *testing.T, path string) exportState {
	t.Helper()
	s, err := loadExport(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func expectedExport(t *testing.T, format string) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := resultWriter{out: &out, format: format}
	if err := writer.write(page{QueryID: "q", Columns: exportColumns, Rows: exportRows()}); err != nil {
		t.Fatal(err)
	}
	if err := writer.finish(""); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func rejectExport(w http.ResponseWriter, code string) {
	_ = json.NewEncoder(w).Encode(map[string]any{"version": 1, "error": workerError{Code: code, Message: "safe worker failure"}})
}
func canonicalExportTestPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := exportPath(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func readExportBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestExportInterruptedResumeMatchesUninterrupted(t *testing.T) {
	for _, format := range []string{"json", "csv"} {
		t.Run(format, func(t *testing.T) {
			var interrupt atomic.Bool
			interrupt.Store(true)
			c := exportClient(t, func(req request, w http.ResponseWriter) {
				if req.NextToken == "" {
					if req.ExpectedResultID != "" {
						t.Errorf("first identity=%q", req.ExpectedResultID)
					}
					reply(w, page{QueryID: "q", ResultID: "revision-1", Columns: exportColumns, Rows: exportRows()[:2], NextToken: "second"})
				} else {
					if req.NextToken != "second" || req.ExpectedResultID != "revision-1" {
						t.Errorf("continuation=%+v", req)
					}
					if interrupt.Load() {
						rejectExport(w, "TEMPORARY")
						return
					}
					reply(w, page{QueryID: "q", ResultID: "revision-1", Columns: exportColumns, Rows: exportRows()[2:]})
				}
			})
			dir := t.TempDir()
			destination := filepath.Join(dir, "data."+format)
			checkpoint := destination + ".checkpoint.json"
			code, out, events := invokeExport(t, c, startExportArgs(destination, format)...)
			if code != 1 || out != "" || len(events) != 1 || !events[0].PartialOutput || events[0].Published || events[0].QueryID != "q" {
				t.Fatalf("code=%d out=%s events=%+v", code, out, events)
			}
			if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("partial destination: %v", err)
			}
			s := readExportState(t, checkpoint)
			if s.Rows != 2 || s.Offset == 0 || s.Cursor != "second" || s.ResultID != "revision-1" || s.Region != c.region || s.Function != c.function || s.Phase != "writing" {
				t.Fatalf("checkpoint=%+v", s)
			}
			for _, path := range []string{checkpoint, s.Data, checkpoint + ".lock"} {
				info, err := os.Stat(path)
				if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
					t.Fatalf("private file %s: %v %v", path, info, err)
				}
			}
			f, err := os.OpenFile(s.Data, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.WriteString(f, "uncommitted interrupted write")
			_ = f.Close()
			interrupt.Store(false)
			code, out, events = invokeExport(t, c, "--resume", checkpoint)
			if code != 0 || len(events) != 1 || !events[0].Published || events[0].PartialOutput {
				t.Fatalf("code=%d out=%s events=%+v", code, out, events)
			}
			if got := readExportBytes(t, destination); !bytes.Equal(got, expectedExport(t, format)) {
				t.Fatalf("resumed bytes=%s", got)
			}
			var receipt exportReceipt
			if err := json.Unmarshal([]byte(out), &receipt); err != nil || !receipt.Published || receipt.Rows != 4 || receipt.Path != s.Destination || receipt.Checkpoint != canonicalExportTestPath(t, checkpoint) || receipt.Bytes != int64(len(expectedExport(t, format))) {
				t.Fatalf("receipt=%+v err=%v", receipt, err)
			}
			s = readExportState(t, checkpoint)
			if s.Phase != "published" || receipt.SHA256 != s.Hash {
				t.Fatalf("published state=%+v", s)
			}
			if _, err := os.Stat(s.Data); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("retained data after publication: %v", err)
			}
			// A repeated resume recovers the same receipt without fetching rows.
			code, again, _ := invokeExport(t, c, "--resume", checkpoint)
			if code != 0 || again != out {
				t.Fatalf("receipt retry code=%d got=%s want=%s", code, again, out)
			}
			// A separate uninterrupted export must produce identical bytes.
			destination2 := filepath.Join(dir, "uninterrupted")
			code, _, events = invokeExport(t, c, startExportArgs(destination2, format)...)
			if code != 0 {
				t.Fatalf("uninterrupted events=%+v", events)
			}
			if !bytes.Equal(readExportBytes(t, destination), readExportBytes(t, destination2)) {
				t.Fatal("interrupted export differs from uninterrupted export")
			}
		})
	}
}

func TestExportExpiredCursorReplaysPrefixAcrossNewPageBoundaries(t *testing.T) {
	for _, format := range []string{"json", "csv"} {
		for _, changed := range []bool{false, true} {
			t.Run(format+"/changed="+map[bool]string{true: "yes", false: "no"}[changed], func(t *testing.T) {
				var resumed atomic.Bool
				c := exportClient(t, func(req request, w http.ResponseWriter) {
					rows := exportRows()
					if !resumed.Load() {
						if req.NextToken == "" {
							reply(w, page{QueryID: "q", ResultID: "r", Columns: exportColumns, Rows: rows[:2], NextToken: "old"})
						} else {
							rejectExport(w, "TEMPORARY")
						}
						return
					}
					if req.ExpectedResultID != "r" {
						t.Errorf("replay lost expected identity: %+v", req)
					}
					switch req.NextToken {
					case "old":
						rejectExport(w, "EXPIRED_CURSOR")
					case "":
						if changed {
							rows[0][0] = str("CHANGED")
						}
						reply(w, page{QueryID: "q", ResultID: "r", Columns: exportColumns, Rows: rows[:1], NextToken: "fresh"})
					case "fresh":
						reply(w, page{QueryID: "q", ResultID: "r", Columns: exportColumns, Rows: rows[1:], NextToken: "empty"})
					case "empty":
						reply(w, page{QueryID: "q", ResultID: "r", Columns: exportColumns, Rows: [][]*string{}})
					default:
						t.Errorf("unexpected replay cursor: %s", req.NextToken)
					}
				})
				destination := filepath.Join(t.TempDir(), "result")
				checkpoint := destination + ".checkpoint.json"
				code, _, _ := invokeExport(t, c, startExportArgs(destination, format)...)
				if code != 1 {
					t.Fatal("expected interruption")
				}
				before := readExportState(t, checkpoint)
				resumed.Store(true)
				code, _, events := invokeExport(t, c, "--resume", checkpoint)
				if changed {
					if code != 1 || events[len(events)-1].Code != CodeResultChanged {
						t.Fatalf("changed prefix code=%d events=%+v", code, events)
					}
					after := readExportState(t, checkpoint)
					if before.Offset != after.Offset || before.Hash != after.Hash {
						t.Fatal("changed replay committed progress")
					}
				} else if code != 0 || !bytes.Equal(readExportBytes(t, destination), expectedExport(t, format)) {
					t.Fatalf("replay code=%d events=%+v", code, events)
				}
			})
		}
	}
}

func TestExportCorruptionAndResultChangesFailClosed(t *testing.T) {
	for _, problem := range []string{"local", "identity", "missing_identity", "schema", "short", "target"} {
		t.Run(problem, func(t *testing.T) {
			var resumed atomic.Bool
			var calls atomic.Int64
			c := exportClient(t, func(req request, w http.ResponseWriter) {
				calls.Add(1)
				if req.NextToken == "" {
					reply(w, page{QueryID: "q", ResultID: "r", Columns: exportColumns, Rows: exportRows()[:2], NextToken: "second"})
					return
				}
				if !resumed.Load() {
					rejectExport(w, "TEMPORARY")
					return
				}
				p := page{QueryID: "q", ResultID: "r", Columns: exportColumns, Rows: exportRows()[2:]}
				switch problem {
				case "identity":
					p.ResultID = "r2"
				case "missing_identity":
					p.ResultID = ""
				case "schema":
					p.Columns = []column{{"other", "bigint"}, {"value", "varchar"}, {"amount", "decimal(30,18)"}}
				}
				reply(w, p)
			})
			destination := filepath.Join(t.TempDir(), "result")
			checkpoint := destination + ".checkpoint.json"
			code, _, _ := invokeExport(t, c, startExportArgs(destination, "json")...)
			if code != 1 {
				t.Fatal("expected interruption")
			}
			s := readExportState(t, checkpoint)
			originalState := readExportBytes(t, checkpoint)
			switch problem {
			case "local":
				f, err := os.OpenFile(s.Data, os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = f.WriteAt([]byte("X"), 0)
				_ = f.Close()
			case "short":
				if err := os.Truncate(s.Data, s.Offset-1); err != nil {
					t.Fatal(err)
				}
			case "target":
				c.region = "other-region"
			}
			previousCalls := calls.Load()
			resumed.Store(true)
			code, out, events := invokeExport(t, c, "--resume", checkpoint)
			if code != 1 || out != "" || len(events) != 1 {
				t.Fatalf("code=%d out=%s events=%+v", code, out, events)
			}
			if (problem == "local" || problem == "short" || problem == "target") && calls.Load() != previousCalls {
				t.Fatal("invalid local state fetched remote results")
			}
			if !bytes.Equal(originalState, readExportBytes(t, checkpoint)) {
				t.Fatal("failure replaced durable checkpoint")
			}
			if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed export published")
			}
		})
	}
}

func completedExport(t *testing.T, format string) (*client, string, exportState) {
	t.Helper()
	c := exportClient(t, func(req request, w http.ResponseWriter) {
		reply(w, page{QueryID: "q", ResultID: "r", Columns: exportColumns, Rows: exportRows()})
	})
	destination := filepath.Join(t.TempDir(), "result")
	code, _, events := invokeExport(t, c, startExportArgs(destination, format)...)
	if code != 0 {
		t.Fatalf("export code=%d events=%+v", code, events)
	}
	checkpoint := destination + ".checkpoint.json"
	return c, checkpoint, readExportState(t, checkpoint)
}

func TestExportFinalizationAndPublicationRecovery(t *testing.T) {
	for _, format := range []string{"json", "csv"} {
		for _, phase := range []string{"finishing", "sealed", "linked", "published"} {
			t.Run(format+"/"+phase, func(t *testing.T) {
				c, path, s := completedExport(t, format)
				if phase != "published" {
					if err := os.Rename(s.Destination, s.Data); err != nil {
						t.Fatal(err)
					}
					s.Phase = "sealed"
					if phase == "finishing" {
						data := readExportBytes(t, s.Data)
						if format == "json" {
							data = data[:bytes.LastIndex(data, []byte(`],"next_token"`))]
						}
						s.Offset = int64(len(data))
						digest := sha256.Sum256(data)
						s.Hash = hex.EncodeToString(digest[:])
						s.Phase = "finishing"
						f, err := os.OpenFile(s.Data, os.O_APPEND|os.O_WRONLY, 0600)
						if err != nil {
							t.Fatal(err)
						}
						_, _ = io.WriteString(f, "torn trailer")
						_ = f.Close()
					}
					if phase == "linked" {
						if err := os.Link(s.Data, s.Destination); err != nil {
							t.Fatal(err)
						}
					}
					if err := saveExport(path, &s); err != nil {
						t.Fatal(err)
					}
				}
				code, _, events := invokeExport(t, c, "--resume", path)
				if code != 0 || len(events) != 1 || !events[0].Published {
					t.Fatalf("recovery code=%d events=%+v", code, events)
				}
				if !bytes.Equal(readExportBytes(t, s.Destination), expectedExport(t, format)) {
					t.Fatal("finalization duplicated or lost bytes")
				}
				code, _, events = invokeExport(t, c, "--resume", path)
				if code != 0 {
					t.Fatalf("second recovery code=%d events=%+v", code, events)
				}
			})
		}
	}
}

func TestExportNeverAdoptsCompetingDestination(t *testing.T) {
	for _, sameBytes := range []bool{false, true} {
		t.Run(map[bool]string{true: "identical", false: "different"}[sameBytes], func(t *testing.T) {
			c, path, s := completedExport(t, "json")
			if err := os.Rename(s.Destination, s.Data); err != nil {
				t.Fatal(err)
			}
			s.Phase = "sealed"
			if err := saveExport(path, &s); err != nil {
				t.Fatal(err)
			}
			competitor := []byte("someone else's output")
			if sameBytes {
				competitor = expectedExport(t, "json")
			}
			if err := os.WriteFile(s.Destination, competitor, 0600); err != nil {
				t.Fatal(err)
			}
			code, _, events := invokeExport(t, c, "--resume", path)
			if code != 1 || events[0].Published || !bytes.Equal(readExportBytes(t, s.Destination), competitor) {
				t.Fatalf("competing destination code=%d events=%+v", code, events)
			}
			if readExportState(t, path).Phase != "sealed" {
				t.Fatal("competing file marked published")
			}
		})
	}
}

func TestExportPublicationRaceKeepsProgress(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "result")
	c := exportClient(t, func(req request, w http.ResponseWriter) {
		if err := os.WriteFile(destination, []byte("winner"), 0600); err != nil {
			t.Error(err)
		}
		reply(w, page{QueryID: "q", ResultID: "r", Columns: exportColumns, Rows: exportRows()})
	})
	code, _, events := invokeExport(t, c, startExportArgs(destination, "json")...)
	if code != 1 || events[0].Published || string(readExportBytes(t, destination)) != "winner" {
		t.Fatalf("race code=%d events=%+v", code, events)
	}
	path := destination + ".checkpoint.json"
	if readExportState(t, path).Phase != "sealed" {
		t.Fatal("publication failure lost sealed progress")
	}
	if err := os.Remove(destination); err != nil {
		t.Fatal(err)
	}
	code, _, events = invokeExport(t, c, "--resume", path)
	if code != 0 || !bytes.Equal(readExportBytes(t, destination), expectedExport(t, "json")) {
		t.Fatalf("retry code=%d events=%+v", code, events)
	}
}

func TestExportLockReleasesAfterProcessDeath(t *testing.T) {
	if path := os.Getenv("ATHENA_EXPORT_TEST_LOCK"); path != "" {
		lock, err := lockExport(path)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		_, _ = io.WriteString(os.Stdout, "locked\n")
		select {}
	}
	path := filepath.Join(t.TempDir(), "checkpoint.lock")
	child := exec.Command(os.Args[0], "-test.run=^TestExportLockReleasesAfterProcessDeath$")
	child.Env = append(os.Environ(), "ATHENA_EXPORT_TEST_LOCK="+path)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	ready := make(chan error, 1)
	go func() {
		data := make([]byte, 7)
		_, err := io.ReadFull(stdout, data)
		if err == nil && string(data) != "locked\n" {
			err = errors.New("child did not acquire lock")
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("lock child timed out")
	}
	if lock, err := lockExport(path); err == nil {
		lock.Close()
		t.Fatal("concurrent process acquired lock")
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	lock, err := lockExport(path)
	if err != nil {
		t.Fatalf("dead process retained lock: %v", err)
	}
	_ = lock.Close()
	if lock, err := lockExport(path); err != nil {
		t.Fatal(err)
	} else {
		_ = lock.Close()
	}
}

func TestExportConcurrentResumeRejectedBeforeWorker(t *testing.T) {
	c, path, s := completedExport(t, "csv")
	lock, err := lockExport(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	code, out, events := invokeExport(t, c, "--resume", path)
	if code != 1 || out != "" || len(events) != 1 || events[0].Operation != "export_lock" {
		t.Fatalf("lock code=%d events=%+v", code, events)
	}
	if !bytes.Equal(readExportBytes(t, s.Destination), expectedExport(t, "csv")) {
		t.Fatal("locked attempt changed destination")
	}
}

func TestExportRejectsArgumentsBeforeConfiguration(t *testing.T) {
	for _, args := range [][]string{
		{}, {"--query-id", "q", "--out", "x"}, {"--out", "x", "--format", "json"},
		{"--query-id", "q", "--format", "csv"}, {"--query-id", "q", "--out", "x", "--format", "table"},
		{"--resume", "x", "--query-id", "q"}, {"--resume", "x", "--out", "y"}, {"--resume", "x", "--checkpoint", "y"},
		{"--resume", "x", "--format", "csv"}, {"--resume", "x", "--region", "r"}, {"--resume", "x", "--timeout", "0"},
		{"--sql", "SELECT 1"}, {"--all"}, {"--next-token", "cursor"},
	} {
		var out, stderr bytes.Buffer
		code := run(context.Background(), append([]string{"export", "--function", "f"}, args...), strings.NewReader(""), &out, &stderr, func(context.Context, settings) (*client, error) {
			t.Fatal("invalid arguments configured client")
			return nil, nil
		})
		events := readDiagnostics(t, stderr.String())
		if code != 2 || out.Len() != 0 || len(events) != 1 || events[0].Code != CodeUsage {
			t.Fatalf("args=%v code=%d events=%+v", args, code, events)
		}
	}
}

func TestExportRequiresIdentityOnFirstPage(t *testing.T) {
	c := exportClient(t, func(req request, w http.ResponseWriter) {
		reply(w, page{QueryID: "q", Columns: exportColumns, Rows: exportRows()})
	})
	destination := filepath.Join(t.TempDir(), "result")
	code, out, events := invokeExport(t, c, startExportArgs(destination, "json")...)
	if code != 1 || out != "" || events[0].Code != CodeResultChanged {
		t.Fatalf("code=%d events=%+v", code, events)
	}
	s := readExportState(t, destination+".checkpoint.json")
	if s.Offset != 0 || s.Started {
		t.Fatal("identity-less bytes committed")
	}
}

func TestExportCheckpointExplicitPathAndStoredTarget(t *testing.T) {
	c := exportClient(t, func(req request, w http.ResponseWriter) {
		reply(w, page{QueryID: "q", ResultID: "r", Columns: exportColumns, Rows: exportRows()})
	})
	dir := t.TempDir()
	destination := filepath.Join(dir, "result")
	checkpoint := filepath.Join(dir, "progress")
	code, _, events := invokeExport(t, c, append(startExportArgs(destination, "csv"), "--checkpoint", checkpoint)...)
	if code != 0 {
		t.Fatalf("code=%d events=%+v", code, events)
	}
	t.Setenv("ATHENA_BRIDGE_FUNCTION", "unrelated-environment-worker")
	var out, stderr bytes.Buffer
	code = run(context.Background(), []string{"export", "--resume", checkpoint, "--profile", "other-credentials"}, strings.NewReader(""), &out, &stderr, func(_ context.Context, s settings) (*client, error) {
		if s.Function != c.function || s.Region != c.region || s.Profile != "other-credentials" {
			t.Errorf("resume target=%+v", s)
		}
		return c, nil
	})
	if code != 0 {
		t.Fatalf("resume=%d stderr=%s", code, &stderr)
	}
}

func TestExportEmptyPagesAndCycleDetection(t *testing.T) {
	for _, format := range []string{"json", "csv"} {
		for _, cycle := range []bool{false, true} {
			t.Run(format+"/cycle="+map[bool]string{true: "yes", false: "no"}[cycle], func(t *testing.T) {
				c := exportClient(t, func(req request, w http.ResponseWriter) {
					next := ""
					switch req.NextToken {
					case "":
						next = "one"
					case "one":
						next = "two"
					case "two":
						if cycle {
							next = "one"
						}
					}
					reply(w, page{QueryID: "q", ResultID: "r", Columns: []column{}, Rows: [][]*string{}, NextToken: next})
				})
				destination := filepath.Join(t.TempDir(), "empty")
				code, _, events := invokeExport(t, c, startExportArgs(destination, format)...)
				if cycle {
					if code != 1 || events[len(events)-1].Code != CodeProtocol {
						t.Fatalf("cycle code=%d events=%+v", code, events)
					}
				} else {
					if code != 0 {
						t.Fatalf("empty export code=%d events=%+v", code, events)
					}
					expected := "\n"
					if format == "json" {
						expected = "{\"query_id\":\"q\",\"columns\":[],\"rows\":[],\"next_token\":null,\"complete\":true}\n"
					}
					if got := string(readExportBytes(t, destination)); got != expected {
						t.Fatalf("empty bytes=%q want=%q", got, expected)
					}
				}
			})
		}
	}
}

func TestExportCancellationRetainsCommittedPage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var interrupted atomic.Bool
	interrupted.Store(true)
	c := exportClient(t, func(req request, w http.ResponseWriter) {
		if req.NextToken == "" {
			reply(w, page{QueryID: "q", ResultID: "r", Columns: exportColumns, Rows: exportRows()[:2], NextToken: "second"})
			return
		}
		if interrupted.Load() {
			cancel()
			rejectExport(w, "TEMPORARY")
			return
		}
		reply(w, page{QueryID: "q", ResultID: "r", Columns: exportColumns, Rows: exportRows()[2:]})
	})
	destination := filepath.Join(t.TempDir(), "result")
	var out, stderr bytes.Buffer
	code := run(ctx, append([]string{"export"}, startExportArgs(destination, "json")...), strings.NewReader(""), &out, &stderr, factory(c))
	events := readDiagnostics(t, stderr.String())
	if code != 3 || out.Len() != 0 || len(events) != 1 || events[0].Code != CodeCancelled || !events[0].PartialOutput || events[0].QueryID != "q" {
		t.Fatalf("cancellation code=%d events=%+v", code, events)
	}
	path := destination + ".checkpoint.json"
	if readExportState(t, path).Rows != 2 {
		t.Fatal("cancellation lost committed page")
	}
	interrupted.Store(false)
	code, _, events = invokeExport(t, c, "--resume", path)
	if code != 0 || !bytes.Equal(readExportBytes(t, destination), expectedExport(t, "json")) {
		t.Fatalf("resume code=%d events=%+v", code, events)
	}
}

func TestExportCorruptCheckpointAndUnsafeFilesRejected(t *testing.T) {
	for _, problem := range []string{"invalid-json", "version", "format", "phase", "path", "schema", "checkpoint-symlink", "data-symlink", "lock-symlink"} {
		t.Run(problem, func(t *testing.T) {
			c, path, s := completedExport(t, "json")
			switch problem {
			case "invalid-json":
				if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "version":
				s.Version = 99
			case "format":
				s.Format = "table"
			case "phase":
				s.Phase = "unknown"
			case "path":
				s.Data = path + ".lock"
			case "schema":
				s.Columns[0].Type = ""
			case "checkpoint-symlink":
				if err := os.Rename(path, path+".actual"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".actual", path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			case "data-symlink":
				if err := os.Rename(s.Destination, s.Destination+".actual"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(s.Destination+".actual", s.Destination); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			case "lock-symlink":
				if err := os.Remove(path + ".lock"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(s.Destination, path+".lock"); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			switch problem {
			case "version", "format", "phase", "path", "schema":
				if err := saveExport(path, &s); err != nil {
					t.Fatal(err)
				}
			}
			code, out, events := invokeExport(t, c, "--resume", path)
			if code != 1 || out != "" || len(events) != 1 || events[0].Code != CodeLocalIO {
				t.Fatalf("unsafe checkpoint code=%d events=%+v", code, events)
			}
		})
	}
}

func TestExportReceiptFailureReportsPublicationAndCanResume(t *testing.T) {
	c := exportClient(t, func(req request, w http.ResponseWriter) {
		reply(w, page{QueryID: "q", ResultID: "r", Columns: exportColumns, Rows: exportRows()})
	})
	destination := filepath.Join(t.TempDir(), "result")
	var stderr bytes.Buffer
	code := run(context.Background(), append([]string{"export"}, startExportArgs(destination, "csv")...), strings.NewReader(""), exportFailWriter{}, &stderr, factory(c))
	events := readDiagnostics(t, stderr.String())
	if code != 1 || len(events) != 2 || !events[1].Published || events[1].Code != CodeLocalIO {
		t.Fatalf("receipt failure code=%d events=%+v", code, events)
	}
	code, _, events = invokeExport(t, c, "--resume", destination+".checkpoint.json")
	if code != 0 || !bytes.Equal(readExportBytes(t, destination), expectedExport(t, "csv")) {
		t.Fatalf("receipt retry code=%d events=%+v", code, events)
	}
}

type exportFailWriter struct{}

func (exportFailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestExportResumeConfigurationFailureReportsRetainedState(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(map[bool]string{false: "partial", true: "published"}[published], func(t *testing.T) {
			_, path, s := completedExport(t, "json")
			if !published {
				if err := os.Rename(s.Destination, s.Data); err != nil {
					t.Fatal(err)
				}
				s.Phase = "sealed"
				if err := saveExport(path, &s); err != nil {
					t.Fatal(err)
				}
			}
			before := readExportBytes(t, path)
			var out, stderr bytes.Buffer
			called := false
			code := run(context.Background(), []string{"export", "--resume", path}, strings.NewReader(""), &out, &stderr, func(context.Context, settings) (*client, error) {
				called = true
				return nil, errors.New("configuration failed")
			})
			events := readDiagnostics(t, stderr.String())
			if !called || code != 1 || out.Len() != 0 || len(events) != 1 || events[0].Code != CodeConfig || events[0].QueryID != "q" || events[0].PartialOutput != !published || events[0].Published != published {
				t.Fatalf("early configuration failure: code=%d events=%+v", code, events)
			}
			if !bytes.Equal(before, readExportBytes(t, path)) {
				t.Fatal("configuration failure changed checkpoint")
			}
		})
	}
}
