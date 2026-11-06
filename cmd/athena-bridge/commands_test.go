package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestInfoConfiguredFieldsAndFormats(t *testing.T) {
	for _, format := range []string{"json", "csv", "table"} {
		t.Run(format, func(t *testing.T) {
			calls := 0
			c := localWorker(t, func(req request, w http.ResponseWriter) {
				calls++
				if req.Action != "worker_info" {
					t.Errorf("unexpected request: %+v", req)
				}
				io.WriteString(w, `{"version":1,"result":{"worker_id":"test-worker","basis":"configured","default_database":"analytics","databases":["analytics"],"catalog":"AwsDataCatalog","workgroup":"lab","actions":["worker_info","get_results"],"features":["result_identity"],"limits":{"max_page_size":1000},"unknown_private_field":"do not print"}}`)
			})
			var out, stderr bytes.Buffer
			exit := run(context.Background(), []string{"info", "--function", "f", "--format", format}, strings.NewReader(""), &out, &stderr, factory(c))
			if exit != 0 || calls != 1 || stderr.Len() != 0 || !strings.Contains(out.String(), "configured") || strings.Contains(out.String(), "do not print") {
				t.Fatalf("exit=%d calls=%d stdout=%s stderr=%s", exit, calls, &out, &stderr)
			}
			if format == "json" {
				var info workerInfo
				if err := json.Unmarshal(out.Bytes(), &info); err != nil || info.Limits["max_page_size"] != 1000 || !slices.Contains(info.Features, "result_identity") {
					t.Fatalf("info=%+v error=%v", info, err)
				}
			}
		})
	}
}

var searchColumns = []column{{"kind", "varchar"}, {"table_name", "varchar"}, {"column_name", "varchar"}, {"data_type", "varchar"}, {"description", "varchar"}, {"partition_key", "varchar"}}

func TestSearchPaginatesEmptyPagesAndKeepsLiteralFilters(t *testing.T) {
	calls := 0
	c := localWorker(t, func(req request, w http.ResponseWriter) {
		calls++
		if req.Action != "search_metadata" || req.Database != "analytics" || req.Term != "A_%'s" || req.Kind != "column" || req.Table != "events" || req.PageSize != 2 {
			t.Errorf("unexpected search: %+v", req)
		}
		if calls == 1 {
			if req.NextToken != "resume" {
				t.Errorf("initial token=%q", req.NextToken)
			}
			reply(w, page{Columns: searchColumns, Rows: [][]*string{}, NextToken: "second"})
		} else {
			if req.NextToken != "second" {
				t.Errorf("next token=%q", req.NextToken)
			}
			reply(w, page{Columns: searchColumns, Rows: [][]*string{{str("column"), str("events"), str("day"), str("date"), nil, str("true")}}})
		}
	})
	var out, stderr bytes.Buffer
	exit := run(context.Background(), []string{"search", "--function", "f", "--database", "analytics", "--term", "A_%'s", "--kind", "column", "--table", "events", "--page-size", "2", "--next-token", "resume", "--all", "--format", "json"}, strings.NewReader(""), &out, &stderr, factory(c))
	if exit != 0 || calls != 2 || stderr.Len() != 0 {
		t.Fatalf("exit=%d calls=%d stdout=%s stderr=%s", exit, calls, &out, &stderr)
	}
	var result struct {
		Columns  []column
		Rows     [][]*string
		Complete bool
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || !result.Complete || !slices.Equal(result.Columns, searchColumns) || len(result.Rows) != 1 || result.Rows[0][4] != nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestSearchSinglePageNoticeAndPublication(t *testing.T) {
	for _, format := range []string{"json", "csv", "table"} {
		t.Run(format, func(t *testing.T) {
			c := localWorker(t, func(req request, w http.ResponseWriter) {
				if req.Kind != "all" || req.NextToken != "" {
					t.Errorf("unexpected defaults: %+v", req)
				}
				reply(w, page{Columns: searchColumns, Rows: [][]*string{}, NextToken: "cursor-2"})
			})
			path := filepath.Join(t.TempDir(), "search."+format)
			var out, stderr bytes.Buffer
			exit := run(context.Background(), []string{"search", "--function", "f", "--term", "event", "--format", format, "--out", path}, strings.NewReader(""), &out, &stderr, factory(c))
			events := readDiagnostics(t, stderr.String())
			if exit != 0 || out.Len() != 0 || len(events) != 2 || events[0].Code != "MORE_RESULTS" || events[0].NextToken != "cursor-2" || !events[0].PartialOutput || events[1].Code != "OUTPUT_PUBLISHED" || !events[1].Published || !events[1].PartialOutput || strings.Contains(stderr.String(), "private.invalid") {
				t.Fatalf("exit=%d stdout=%s events=%+v", exit, &out, events)
			}
			data, err := os.ReadFile(path)
			if err != nil || len(data) == 0 {
				t.Fatalf("empty output: %s %v", data, err)
			}
			if format == "json" && !strings.Contains(string(data), `"complete":false`) {
				t.Fatalf("lost partial marker: %s", data)
			}
		})
	}
}

func TestCapabilityArgumentsFailBeforeConfiguration(t *testing.T) {
	for _, args := range [][]string{
		{"search"}, {"search", "--term", ""}, {"search", "--term", "x", "--kind", "regex"}, {"search", "--term", "x", "--page-size", "1001"}, {"cancel"},
	} {
		var out, stderr bytes.Buffer
		args = append(args, "--function", "f")
		exit := run(context.Background(), args, strings.NewReader(""), &out, &stderr, func(context.Context, settings) (*client, error) {
			t.Fatal("invalid arguments reached configuration")
			return nil, nil
		})
		events := readDiagnostics(t, stderr.String())
		if exit != 2 || out.Len() != 0 || len(events) != 1 || events[0].Code != CodeUsage {
			t.Fatalf("args=%v exit=%d events=%+v", args, exit, events)
		}
	}
}

func TestCancelReportsAcceptanceAndObservedState(t *testing.T) {
	for _, state := range []string{"QUEUED", "RUNNING", "SUCCEEDED", "FAILED", "CANCELLED"} {
		for _, format := range []string{"json", "csv", "table"} {
			t.Run(state+"/"+format, func(t *testing.T) {
				requested := state == "RUNNING" || state == "QUEUED"
				calls := 0
				c := localWorker(t, func(req request, w http.ResponseWriter) {
					calls++
					if req.Action != "cancel_query" || req.QueryID != "q" {
						t.Errorf("unexpected cancel: %+v", req)
					}
					reply(w, cancelled{queryStatus: queryStatus{QueryID: "q", State: state}, CancellationRequested: &requested})
				})
				var out, stderr bytes.Buffer
				exit := run(context.Background(), []string{"cancel", "--function", "f", "--query-id", "q", "--format", format}, strings.NewReader(""), &out, &stderr, factory(c))
				events := readDiagnostics(t, stderr.String())
				wantNotices := 0
				if requested {
					wantNotices = 1
				}
				if exit != 0 || calls != 1 || !strings.Contains(out.String(), state) || len(events) != wantNotices {
					t.Fatalf("exit=%d calls=%d stdout=%s events=%+v", exit, calls, &out, events)
				}
				if format == "json" {
					var result cancelled
					if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.CancellationRequested == nil || *result.CancellationRequested != requested || result.State != state {
						t.Fatalf("result=%+v err=%v", result, err)
					}
				}
			})
		}
	}
}

func TestStatusOptionalStatisticsKeepExactZeroAndUnknown(t *testing.T) {
	for _, state := range []string{"FAILED", "CANCELLED", "SUCCEEDED"} {
		for _, format := range []string{"json", "csv", "table"} {
			t.Run(state+"/"+format, func(t *testing.T) {
				c := responseClient(`{"version":1,"result":{"query_id":"q","state":"` + state + `","statistics":{"data_scanned_bytes":"90071992547409930000","engine_execution_ms":"0","result_reused":false},"failure":{"error_category":1,"error_type":1001,"retryable":false}}}`)
				var out, stderr bytes.Buffer
				exit := run(context.Background(), []string{"status", "--function", "f", "--query-id", "q", "--format", format}, strings.NewReader(""), &out, &stderr, factory(c))
				if exit != 0 || stderr.Len() != 0 || !strings.Contains(out.String(), "90071992547409930000") {
					t.Fatalf("exit=%d stdout=%s stderr=%s", exit, &out, &stderr)
				}
				if format == "json" {
					var result queryStatus
					if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Statistics.EngineExecutionMS == nil || *result.Statistics.EngineExecutionMS != "0" || result.Statistics.TotalExecutionMS != nil || result.Statistics.ResultReused == nil || *result.Statistics.ResultReused {
						t.Fatalf("status=%+v err=%v", result, err)
					}
				} else if format == "csv" {
					rows, err := csv.NewReader(strings.NewReader(out.String())).ReadAll()
					if err != nil || rows[1][4] != "0" || rows[1][5] != "" || rows[1][8] != "false" {
						t.Fatalf("rows=%v err=%v", rows, err)
					}
				}
			})
		}
	}
	// Older workers omit the additive fields and retain the original output.
	c := responseClient(`{"version":1,"result":{"query_id":"q","state":"RUNNING"}}`)
	var out, stderr bytes.Buffer
	exit := run(context.Background(), []string{"status", "--function", "f", "--query-id", "q", "--format", "json"}, strings.NewReader(""), &out, &stderr, factory(c))
	if exit != 0 || out.String() != "{\"query_id\":\"q\",\"state\":\"RUNNING\"}\n" {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, &out, &stderr)
	}
}

func TestMalformedCapabilityResultsFailClosed(t *testing.T) {
	for _, tt := range []struct{ command, body string }{
		{"info", `{"worker_id":"w","basis":"inferred","databases":[],"actions":[],"features":[],"limits":{}}`},
		{"info", `{"worker_id":"w","basis":"configured","databases":[],"actions":[],"features":[],"limits":{"page_size":-1}}`},
		{"cancel", `{"query_id":"q","state":"RUNNING"}`},
		{"cancel", `{"query_id":"other","state":"RUNNING","cancellation_requested":true}`},
		{"status", `{"query_id":"q","state":"RUNNING","statistics":{"data_scanned_bytes":12}}`},
		{"status", `{"query_id":"q","state":"RUNNING","statistics":{"engine_execution_ms":"-1"}}`},
		{"status", `{"query_id":"q","state":"RUNNING","statistics":{"planning_ms":""}}`},
		{"status", `{"query_id":"q","state":"RUNNING","failure":{"error_type":-1}}`},
	} {
		t.Run(tt.command+tt.body, func(t *testing.T) {
			args := []string{tt.command, "--function", "f", "--format", "json"}
			if tt.command != "info" {
				args = append(args, "--query-id", "q")
			}
			var out, stderr bytes.Buffer
			exit := run(context.Background(), args, strings.NewReader(""), &out, &stderr, factory(responseClient(`{"version":1,"result":`+tt.body+`}`)))
			events := readDiagnostics(t, stderr.String())
			if exit != 1 || out.Len() != 0 || len(events) != 1 || events[0].Code != CodeProtocol {
				t.Fatalf("exit=%d stdout=%s events=%+v", exit, &out, events)
			}
		})
	}
}

func TestStatusFailureSQLCoordinates(t *testing.T) {
	for _, command := range []string{"status", "cancel"} {
		for _, coords := range []string{`"sql_line":2,"sql_column":17`, `"sql_line":1`, `"sql_column":1`} {
			t.Run(command+"/"+coords, func(t *testing.T) {
				body := `{"version":1,"result":{"query_id":"q","state":"FAILED","cancellation_requested":false,"failure":{` + coords + `}}}`
				var out, stderr bytes.Buffer
				exit := run(context.Background(), []string{command, "--function", "f", "--query-id", "q", "--format", "json"}, strings.NewReader(""), &out, &stderr, factory(responseClient(body)))
				if exit != 0 || stderr.Len() != 0 || !strings.Contains(out.String(), `"failure":{`+coords+`}`) {
					t.Fatalf("lost SQL coordinates: exit=%d stdout=%s stderr=%s", exit, &out, &stderr)
				}
			})
		}
		for _, field := range []string{"sql_line", "sql_column"} {
			for _, value := range []string{"0", "-1", "1.5", `"2"`, "true", "9223372036854775808"} {
				t.Run(command+"/"+field+"/"+value, func(t *testing.T) {
					body := `{"version":1,"result":{"query_id":"q","state":"FAILED","cancellation_requested":false,"failure":{"` + field + `":` + value + `}}}`
					var out, stderr bytes.Buffer
					exit := run(context.Background(), []string{command, "--function", "f", "--query-id", "q", "--format", "json"}, strings.NewReader(""), &out, &stderr, factory(responseClient(body)))
					events := readDiagnostics(t, stderr.String())
					if exit != 1 || out.Len() != 0 || len(events) != 1 || events[0].Code != CodeProtocol {
						t.Fatalf("invalid SQL coordinates accepted: exit=%d stdout=%s events=%+v", exit, &out, events)
					}
				})
			}
		}
	}
}

func TestValidatedFetchPageForExport(t *testing.T) {
	columns := []column{{"n", "bigint"}}
	for _, tt := range []struct {
		name, resultID, queryID string
		columns                 []column
		want                    Code
	}{
		{"same", "revision-1", "q", columns, ""},
		{"changed result", "revision-2", "q", columns, CodeResultChanged},
		{"missing result", "", "q", columns, CodeResultChanged},
		{"wrong query", "revision-1", "other", columns, CodeProtocol},
		{"changed schema", "revision-1", "q", []column{{"n", "varchar"}}, CodeProtocol},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := localWorker(t, func(req request, w http.ResponseWriter) {
				if req.ExpectedResultID != "revision-1" || req.QueryID != "q" || req.NextToken != "cursor" {
					t.Errorf("unexpected fetch: %+v", req)
				}
				reply(w, page{QueryID: tt.queryID, ResultID: tt.resultID, Columns: tt.columns, Rows: [][]*string{{nil}}})
			})
			p, err := c.fetchPage(context.Background(), request{Action: "get_results", QueryID: "q", ExpectedResultID: "revision-1", NextToken: "cursor", PageSize: 100}, columns)
			if tt.want == "" {
				if err != nil || p.Rows[0][0] != nil {
					t.Fatalf("page=%+v err=%v", p, err)
				}
			} else if !errors.Is(err, tt.want) {
				t.Fatalf("error=%v want=%v", err, tt.want)
			}
		})
	}
}

func TestResultsPinIdentityAcrossPages(t *testing.T) {
	c := localWorker(t, func(req request, w http.ResponseWriter) {
		if req.NextToken == "" {
			reply(w, page{QueryID: "q", ResultID: "revision-1", Columns: []column{}, Rows: [][]*string{}, NextToken: "next"})
		} else {
			if req.ExpectedResultID != "revision-1" {
				t.Errorf("unbound next request: %+v", req)
			}
			reply(w, page{QueryID: "q", ResultID: "revision-2", Columns: []column{}, Rows: [][]*string{}})
		}
	})
	var out, stderr bytes.Buffer
	exit := run(context.Background(), []string{"results", "--function", "f", "--query-id", "q", "--all", "--format", "json"}, strings.NewReader(""), &out, &stderr, factory(c))
	events := readDiagnostics(t, stderr.String())
	if exit != 1 || len(events) != 1 || events[0].Code != CodeResultChanged || !events[0].PartialOutput {
		t.Fatalf("exit=%d events=%+v", exit, events)
	}
}

func TestDownloadPublicationSurvivesMetadataWriteFailure(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "complete\n") }))
	defer server.Close()
	previous := http.DefaultClient
	http.DefaultClient = server.Client()
	t.Cleanup(func() { http.DefaultClient = previous })
	c := localWorker(t, func(_ request, w http.ResponseWriter) {
		reply(w, download{URL: server.URL + "/file?token=secret", Format: "csv"})
	})
	path := filepath.Join(t.TempDir(), "download.csv")
	var stderr bytes.Buffer
	exit := run(context.Background(), []string{"download", "--function", "f", "--query-id", "q", "--out", path, "--format", "json"}, strings.NewReader(""), brokenWriter{}, &stderr, factory(c))
	events := readDiagnostics(t, stderr.String())
	data, err := os.ReadFile(path)
	if exit != 1 || err != nil || string(data) != "complete\n" || len(events) != 2 || events[0].Code != "OUTPUT_PUBLISHED" || !events[0].Published || events[1].Code != CodeLocalIO || !events[1].Published || events[1].PartialOutput {
		t.Fatalf("exit=%d data=%q err=%v events=%+v", exit, data, err, events)
	}
	if strings.Contains(stderr.String(), server.URL) || strings.Contains(stderr.String(), "token=secret") {
		t.Fatalf("leaked URL: %s", &stderr)
	}
}

func TestClientStoresResolvedRegion(t *testing.T) {
	t.Setenv("AWS_REGION", "us-west-2")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "absent-config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "absent-credentials"))
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	for _, region := range []string{"", "eu-west-1"} {
		c, err := newClient(context.Background(), settings{Function: "f", Region: region})
		want := region
		if want == "" {
			want = "us-west-2"
		}
		if err != nil || c.region != want || c.function != "f" {
			t.Fatalf("region=%q client=%+v err=%v", region, c, err)
		}
	}
}

func TestOutputPublicationRaceHasTypedFault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output.json")
	c := localWorker(t, func(req request, w http.ResponseWriter) {
		if err := os.WriteFile(path, []byte("racing writer"), 0600); err != nil {
			t.Error(err)
		}
		reply(w, page{QueryID: "q", Columns: []column{}, Rows: [][]*string{}})
	})
	var out, stderr bytes.Buffer
	exit := run(context.Background(), []string{"results", "--function", "f", "--query-id", "q", "--out", path}, strings.NewReader(""), &out, &stderr, factory(c))
	events := readDiagnostics(t, stderr.String())
	if exit != 1 || out.Len() != 0 || len(events) != 1 || events[0].Code != CodeLocalIO || events[0].Operation != "publish_output" || events[0].Published || events[0].PartialOutput {
		t.Fatalf("exit=%d events=%+v", exit, events)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "racing writer" {
		t.Fatalf("overwritten racing writer: %q %v", data, err)
	}
}

func TestFetchPageRejectsNullRowsWithEmptySchema(t *testing.T) {
	c := responseClient(`{"version":1,"result":{"query_id":"q","columns":[],"rows":[null]}}`)
	_, err := c.fetchPage(context.Background(), request{Action: "get_results", QueryID: "q"}, nil)
	if !errors.Is(err, CodeProtocol) {
		t.Fatal(fmt.Errorf("null row accepted: %w", err))
	}
}

func TestPartialTextOutputExposesContinuationToken(t *testing.T) {
	for _, format := range []string{"csv", "table"} {
		t.Run(format, func(t *testing.T) {
			calls := 0
			c := localWorker(t, func(req request, w http.ResponseWriter) {
				calls++
				if req.NextToken == "" {
					reply(w, page{QueryID: "q", Columns: []column{{"n", "bigint"}}, Rows: [][]*string{{str("1")}}, NextToken: "opaque+/cursor=="})
				} else {
					if req.NextToken != "opaque+/cursor==" {
						t.Errorf("wrong continuation token: %q", req.NextToken)
					}
					reply(w, page{QueryID: "q", Columns: []column{{"n", "bigint"}}, Rows: [][]*string{{str("2")}}})
				}
			})
			args := []string{"results", "--function", "f", "--query-id", "q", "--format", format}
			var out, stderr bytes.Buffer
			exit := run(context.Background(), args, strings.NewReader(""), &out, &stderr, factory(c))
			events := readDiagnostics(t, stderr.String())
			if exit != 0 || len(events) != 1 || events[0].Code != "MORE_RESULTS" || events[0].NextToken != "opaque+/cursor==" || !events[0].PartialOutput {
				t.Fatalf("exit=%d events=%+v", exit, events)
			}
			args = append(args, "--next-token", events[0].NextToken)
			out.Reset()
			stderr.Reset()
			exit = run(context.Background(), args, strings.NewReader(""), &out, &stderr, factory(c))
			if exit != 0 || calls != 2 || stderr.Len() != 0 || !strings.Contains(out.String(), "2") {
				t.Fatalf("exit=%d calls=%d stdout=%s stderr=%s", exit, calls, &out, &stderr)
			}
		})
	}
}
