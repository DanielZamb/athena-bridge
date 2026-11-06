package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPrettyTableAndDefaultJSON(t *testing.T) {
	for _, pretty := range []bool{false, true} {
		c := responseClient(`{"version":1,"result":{"columns":[{"name":"id","type":"integer"}],"rows":[["1"]]}}`)
		args := []string{"tables", "--function", "f", "--format", "json"}
		if pretty {
			args = append(args, "--pretty")
		}
		var out, stderr bytes.Buffer
		code := run(context.Background(), args, strings.NewReader(""), &out, &stderr, factory(c))
		want := "{\"columns\":[{\"name\":\"id\",\"type\":\"integer\"}],\"rows\":[[\"1\"]],\"next_token\":null,\"complete\":true}\n"
		if pretty {
			want = "┌────┐\n│ id │\n├────┤\n│ 1  │\n└────┘\n1 row · complete\n"
		}
		if code != 0 || out.String() != want || stderr.Len() != 0 {
			t.Fatalf("pretty=%v exit=%d stdout=%q stderr=%s", pretty, code, out.String(), &stderr)
		}
	}
}

func TestPrettyValuesAndExpandedRows(t *testing.T) {
	text := strings.Repeat("long", 35) + " café"
	columns := []column{{"value", "bigint"}, {"value", "varchar"}, {"empty", "varchar"}, {"literal", "varchar"}, {"control", "varchar"}}
	data, _ := json.Marshal(map[string]any{"version": 1, "result": page{Columns: columns, Rows: [][]*string{
		{str("9007199254740993"), nil, str(""), str("NULL"), str(text + "\n\x1b[31m")},
	}}})
	c := responseClient(string(data))
	var out, stderr bytes.Buffer
	code := run(context.Background(), []string{"tables", "--function", "f", "--pretty"}, strings.NewReader(""), &out, &stderr, factory(c))
	for _, want := range []string{"Row 1", "[1] value", "[2] value", "9007199254740993", "NULL", `""`, `"NULL"`, "café", `\n\x1b[31m`, "1 row · complete"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in %q", want, out.String())
		}
	}
	if code != 0 || stderr.Len() != 0 || strings.ContainsRune(out.String(), '\x1b') {
		t.Fatalf("exit=%d stdout=%q stderr=%s", code, out.String(), &stderr)
	}
}

func TestPrettyInfoAndStatus(t *testing.T) {
	cases := []struct {
		command, body string
		wants, absent []string
	}{
		{"info", `{"worker_id":"test-worker","basis":"configured","default_database":"analytics","databases":["analytics"],"catalog":"AwsDataCatalog","workgroup":"lab","actions":["get_results"],"features":[],"limits":{"max_page_size":1000,"max_scan_bytes":536870912000}}`, []string{"Worker", "test-worker", "Workgroup", "536870912000"}, []string{"{\"worker_id\""}},
		{"status", `{"query_id":"q","state":"FAILED","statistics":{"data_scanned_bytes":"90071992547409930000","engine_execution_ms":"0","result_reused":false},"failure":{"sql_line":3,"sql_column":12,"retryable":false}}`, []string{"FAILED", "90071992547409930000", "Engine execution ms", "false", "Sql line", "12"}, []string{"Total execution ms"}},
	}
	for _, test := range cases {
		var out, stderr bytes.Buffer
		args := []string{test.command, "--function", "f", "--pretty"}
		if test.command == "status" {
			args = append(args, "--query-id", "q")
		}
		code := run(context.Background(), args, strings.NewReader(""), &out, &stderr, factory(responseClient(`{"version":1,"result":`+test.body+`}`)))
		for _, want := range test.wants {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%s missing %q: %s", test.command, want, &out)
			}
		}
		for _, absent := range test.absent {
			if strings.Contains(out.String(), absent) {
				t.Errorf("%s contains %q: %s", test.command, absent, &out)
			}
		}
		if code != 0 || stderr.Len() != 0 {
			t.Fatalf("exit=%d stdout=%s stderr=%s", code, &out, &stderr)
		}
	}
}

func TestPrettyErrorsAndFlagValues(t *testing.T) {
	var out, stderr bytes.Buffer
	code := run(context.Background(), []string{"tables", "--unknown", "--pretty"}, strings.NewReader(""), &out, &stderr, nil)
	if code != 2 || !strings.Contains(stderr.String(), "ERROR") || !strings.Contains(stderr.String(), "INVALID_ARGUMENT") || strings.HasPrefix(stderr.String(), "{") {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, &out, &stderr)
	}
	for _, value := range []string{"--pretty", "--pretty=true"} {
		out.Reset()
		stderr.Reset()
		c := localWorker(t, func(req request, w http.ResponseWriter) {
			if req.SQL != value {
				t.Errorf("SQL changed: %q", req.SQL)
			}
			reply(w, started{QueryID: "q"})
		})
		code = run(context.Background(), []string{"start", "--function", "f", "--format", "json", "--sql", value}, strings.NewReader(""), &out, &stderr, factory(c))
		if code != 0 || out.String() != "{\"query_id\":\"q\"}\n" {
			t.Fatalf("exit=%d stdout=%s stderr=%s", code, &out, &stderr)
		}
		readDiagnostics(t, stderr.String())
	}
	out.Reset()
	stderr.Reset()
	code = run(context.Background(), []string{"results", "--function", "f", "--query-id", "q", "--pretty"}, strings.NewReader(""), &out, &stderr, factory(responseClient(`{"version":1,"error":{"code":"NOT_ALLOWED","message":"https://private.invalid/secret"}}`)))
	if code != 1 || !strings.Contains(stderr.String(), "NOT_ALLOWED") || !strings.Contains(stderr.String(), "Query ID") || strings.Contains(stderr.String(), "private.invalid") {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, &out, &stderr)
	}
}

func TestPrettyLeavesFilesExactAndTokensRecoverable(t *testing.T) {
	for _, format := range []string{"json", "csv"} {
		var files [][]byte
		for _, pretty := range []bool{false, true} {
			c := responseClient(`{"version":1,"result":{"query_id":"q","columns":[{"name":"x","type":"varchar"}],"rows":[[null],[""],["\\N"]],"next_token":"opaque-token"}}`)
			path := filepath.Join(t.TempDir(), "results."+format)
			args := []string{"results", "--function", "f", "--query-id", "q", "--format", format, "--out", path}
			if pretty {
				args = append(args, "--pretty")
			}
			var out, stderr bytes.Buffer
			code := run(context.Background(), args, strings.NewReader(""), &out, &stderr, factory(c))
			if code != 0 || out.Len() != 0 || !strings.Contains(stderr.String(), "opaque-token") {
				t.Fatalf("exit=%d stdout=%s stderr=%s", code, &out, &stderr)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, data)
		}
		if !bytes.Equal(files[0], files[1]) {
			t.Fatalf("%s file changed: %q != %q", format, files[0], files[1])
		}
	}
}

func TestPrettyExportReceiptAndResume(t *testing.T) {
	c := exportClient(t, func(req request, w http.ResponseWriter) {
		reply(w, page{QueryID: "q", ResultID: "revision", Columns: exportColumns, Rows: exportRows()})
	})
	path := filepath.Join(t.TempDir(), "rows.csv")
	args := append([]string{"export"}, startExportArgs(path, "csv")...)
	args = append(args, "--pretty")
	for _, invocation := range [][]string{args, {"export", "--resume", path + ".checkpoint.json", "--pretty"}} {
		var out, stderr bytes.Buffer
		code := run(context.Background(), invocation, strings.NewReader(""), &out, &stderr, factory(c))
		if code != 0 || !strings.Contains(out.String(), "Export") || !strings.Contains(out.String(), "Published") || strings.HasPrefix(out.String(), "{") {
			t.Fatalf("exit=%d stdout=%s stderr=%s", code, &out, &stderr)
		}
		if !bytes.Equal(readExportBytes(t, path), expectedExport(t, "csv")) {
			t.Fatal("pretty modified export bytes")
		}
	}
}

func TestPrettyStopsOnOutputFailure(t *testing.T) {
	calls := 0
	c := localWorker(t, func(req request, w http.ResponseWriter) {
		calls++
		reply(w, page{Columns: []column{{"x", "integer"}}, Rows: [][]*string{{str("1")}}, NextToken: "next"})
	})
	var stderr bytes.Buffer
	code := run(context.Background(), []string{"tables", "--function", "f", "--pretty", "--all"}, strings.NewReader(""), brokenWriter{}, &stderr, factory(c))
	if code != 1 || calls != 1 || !strings.Contains(stderr.String(), "LOCAL_IO") {
		t.Fatalf("exit=%d calls=%d stderr=%s", code, calls, &stderr)
	}
}

func TestPrettyPaginationAndEmptyPages(t *testing.T) {
	calls := 0
	c := localWorker(t, func(req request, w http.ResponseWriter) {
		calls++
		p := page{Columns: []column{{"id", "integer"}}, Rows: [][]*string{}, NextToken: "second"}
		if calls == 2 {
			p.Rows, p.NextToken = [][]*string{{str("1")}}, "third"
		} else if calls == 3 {
			p.Rows, p.NextToken = [][]*string{{str("2")}}, ""
		}
		reply(w, p)
	})
	var out, stderr bytes.Buffer
	code := run(context.Background(), []string{"tables", "--function", "f", "--pretty", "--all"}, strings.NewReader(""), &out, &stderr, factory(c))
	if code != 0 || calls != 3 || !strings.Contains(out.String(), "(no rows on this page)") || !strings.HasSuffix(out.String(), "2 rows · complete\n") {
		t.Fatalf("exit=%d calls=%d stdout=%s stderr=%s", code, calls, &out, &stderr)
	}
}

func TestPrettyTokenIsReversibleAndCancellationKeepsExitThree(t *testing.T) {
	token := "opaque\"\\\n\x1b\u202e"
	var stderr bytes.Buffer
	d := &diagnostics{out: &stderr, pretty: true}
	if err := d.emit(diagnosticEvent{Type: "notice", Code: "MORE_RESULTS", Operation: "get_results", Message: "more rows", Recovery: RecoveryRetry, NextToken: token}); err != nil {
		t.Fatal(err)
	}
	_, encoded, found := strings.Cut(stderr.String(), "Next token (Go string)  ")
	decoded, err := strconv.Unquote(strings.TrimSpace(encoded))
	if !found || err != nil || decoded != token || strings.ContainsRune(stderr.String(), '\x1b') || strings.ContainsRune(stderr.String(), '\u202e') {
		t.Fatalf("token lost: %q: %v", stderr.String(), err)
	}
	var out bytes.Buffer
	stderr.Reset()
	code := run(context.Background(), []string{"tables", "--function", "f", "--pretty"}, strings.NewReader(""), &out, &stderr, func(context.Context, settings) (*client, error) {
		return nil, errors.Join(errors.New("private"), context.Canceled)
	})
	if code != 3 || !strings.Contains(stderr.String(), "CANCELLED") || strings.Contains(stderr.String(), "private") {
		t.Fatalf("exit=%d stderr=%s", code, &stderr)
	}
}

func TestPrettyFalseKeepsMachineOutput(t *testing.T) {
	var out, stderr bytes.Buffer
	code := run(context.Background(), []string{"tables", "--function", "f", "--format", "json", "--pretty=false"}, strings.NewReader(""), &out, &stderr, factory(responseClient(`{"version":1,"result":{"columns":[],"rows":[]}}`)))
	if code != 0 || !json.Valid(out.Bytes()) || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, &out, &stderr)
	}
}
