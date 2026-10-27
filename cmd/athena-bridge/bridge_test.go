package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
)

func localWorker(t *testing.T, handler func(request, http.ResponseWriter)) *client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if req.Version != 1 {
			t.Errorf("protocol version = %d", req.Version)
		}
		w.Header().Set("Content-Type", "application/json")
		handler(req, w)
	}))
	t.Cleanup(server.Close)
	api := lambda.NewFromConfig(aws.Config{
		Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("test-access", "test-secret", "test-session"),
		HTTPClient: server.Client(), RetryMaxAttempts: 1,
	}, func(o *lambda.Options) { o.BaseEndpoint = aws.String(server.URL) })
	return &client{api: api, function: "test-worker"}
}

func reply(w http.ResponseWriter, result any) {
	json.NewEncoder(w).Encode(map[string]any{"version": 1, "result": result})
}

func factory(c *client) func(context.Context, settings) (*client, error) {
	return func(context.Context, settings) (*client, error) { return c, nil }
}

func TestQueryLifecycleAndExactDataset(t *testing.T) {
	var mu sync.Mutex
	var calls []request
	c := localWorker(t, func(req request, w http.ResponseWriter) {
		mu.Lock()
		calls = append(calls, req)
		mu.Unlock()
		switch req.Action {
		case "start_query":
			if req.RequestID != strings.Repeat("a", 32) || req.SQL != "SELECT id FROM events" || req.Database != "analytics" {
				t.Errorf("wrong start request: %+v", req)
			}
			reply(w, started{QueryID: "q-1"})
		case "query_status":
			reply(w, queryStatus{QueryID: "q-1", State: "SUCCEEDED"})
		case "get_results":
			columns := []column{{"id", "bigint"}, {"value", "decimal"}, {"note", "varchar"}}
			if req.NextToken == "" {
				reply(w, page{QueryID: "q-1", Columns: columns, Rows: [][]*string{{str("9007199254740993"), str("1.230000000000000001"), nil}}, NextToken: "page-2"})
			} else if req.NextToken == "page-2" {
				reply(w, page{QueryID: "q-1", Columns: columns, Rows: [][]*string{{str("2"), str("0"), str("")}}})
			} else {
				t.Errorf("unexpected cursor %q", req.NextToken)
			}
		default:
			t.Errorf("unexpected action %s", req.Action)
		}
	})
	var out, diagnostics bytes.Buffer
	code := run(context.Background(), []string{"query", "--function", "test-worker", "--sql-file", "-", "--database", "analytics", "--request-id", strings.Repeat("a", 32), "--all", "--format", "json"}, strings.NewReader("SELECT id FROM events"), &out, &diagnostics, factory(c))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, diagnostics.String())
	}
	expected := `{"query_id":"q-1","columns":[{"name":"id","type":"bigint"},{"name":"value","type":"decimal"},{"name":"note","type":"varchar"}],"rows":[["9007199254740993","1.230000000000000001",null],["2","0",""]],"next_token":null,"complete":true}` + "\n"
	if out.String() != expected {
		t.Errorf("dataset = %s; want %s", out.String(), expected)
	}
	if !strings.Contains(diagnostics.String(), `"query_id":"q-1"`) {
		t.Error("missing query recovery ID")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 4 {
		t.Errorf("calls = %d; want 4", len(calls))
	}
}

func TestSDKSignsLambdaInvocation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/2015-03-31/functions/test-worker/invocations" || r.Header.Get("X-Amz-Invocation-Type") != "RequestResponse" {
			t.Errorf("wrong invocation: %s %v", r.URL.Path, r.Header)
		}
		if strings.Contains(string(body), "test-access") || strings.Contains(string(body), "test-secret") || strings.Contains(string(body), "test-session") {
			t.Error("credentials leaked into worker payload")
		}
		if r.Header.Get("X-Amz-Security-Token") != "test-session" {
			t.Error("session token was not applied to signed request")
		}
		auth := r.Header.Get("Authorization")
		match := regexp.MustCompile(`^AWS4-HMAC-SHA256 Credential=test-access/([^,]+), SignedHeaders=([^,]+), Signature=([0-9a-f]{64})$`).FindStringSubmatch(auth)
		if len(match) != 4 {
			t.Errorf("missing SigV4 authorization")
		} else {
			var canonical strings.Builder
			for _, name := range strings.Split(match[2], ";") {
				value := strings.Join(r.Header.Values(name), ",")
				if name == "host" {
					value = r.Host
				} else if name == "content-length" {
					value = fmt.Sprint(r.ContentLength)
				}
				canonical.WriteString(name + ":" + strings.Join(strings.Fields(value), " ") + "\n")
			}
			request := r.Method + "\n" + r.URL.EscapedPath() + "\n" + r.URL.RawQuery + "\n" + canonical.String() + "\n" + match[2] + "\n" + hash(body)
			toSign := "AWS4-HMAC-SHA256\n" + r.Header.Get("X-Amz-Date") + "\n" + match[1] + "\n" + hash([]byte(request))
			key := []byte("AWS4test-secret")
			for _, part := range strings.Split(match[1], "/") {
				key = mac(key, part)
			}
			if hex.EncodeToString(mac(key, toSign)) != match[3] {
				t.Error("SigV4 signature does not match the request and fake secret")
			}
		}
		reply(w, started{QueryID: "q-signed"})
	}))
	defer server.Close()
	api := lambda.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("test-access", "test-secret", "test-session"), HTTPClient: server.Client(), RetryMaxAttempts: 1}, func(o *lambda.Options) { o.BaseEndpoint = aws.String(server.URL) })
	c := client{api: api, function: "test-worker"}
	if _, err := c.start(context.Background(), request{Action: "start_query", RequestID: strings.Repeat("b", 32), SQL: "SELECT 1"}); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerErrorsFailInsteadOfPrintingData(t *testing.T) {
	tests := []struct{ name, body, header, want string }{
		{"worker", `{"version":1,"error":{"code":"DENIED","message":"database is not allowed"}}`, "", "DENIED"},
		{"version", `{"version":2,"result":{"query_id":"q"}}`, "", "protocol version"},
		{"both", `{"version":1,"result":{},"error":{"code":"BAD","message":"bad"}}`, "", "exactly one"},
		{"function", `{"errorMessage":"Athena access denied"}`, "Unhandled", "Lambda function failed"},
		{"malformed", `{"version":1`, "", "invalid worker response"},
		{"trailing", `{"version":1,"result":{}} {}`, "", "one JSON value"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := localWorker(t, func(_ request, w http.ResponseWriter) {
				if test.header != "" {
					w.Header().Set("X-Amz-Function-Error", test.header)
				}
				io.WriteString(w, test.body)
			})
			var out, diagnostics bytes.Buffer
			code := run(context.Background(), []string{"start", "--function", "test-worker", "--sql", "SELECT 1", "--format", "json"}, strings.NewReader(""), &out, &diagnostics, factory(c))
			if code != 1 || out.Len() != 0 || !strings.Contains(diagnostics.String(), test.want) {
				t.Fatalf("exit=%d stdout=%s stderr=%s", code, &out, &diagnostics)
			}
		})
	}
}

func TestPaginationFailureDoesNotPublishPartialFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "results.csv")
	c := localWorker(t, func(req request, w http.ResponseWriter) {
		reply(w, page{QueryID: req.QueryID, Columns: []column{{"id", "bigint"}}, Rows: [][]*string{{str("1")}}, NextToken: "repeated"})
	})
	var out, diagnostics bytes.Buffer
	code := run(context.Background(), []string{"results", "--function", "test-worker", "--query-id", "q-1", "--all", "--out", path, "--format", "csv"}, strings.NewReader(""), &out, &diagnostics, factory(c))
	if code != 1 || !strings.Contains(diagnostics.String(), "repeated a pagination token") {
		t.Fatalf("exit=%d stderr=%s", code, &diagnostics)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("partial final file exists")
	}
	files, _ := os.ReadDir(filepath.Dir(path))
	if len(files) != 0 {
		t.Error("partial temporary file remains")
	}
}

func TestUsageAndExistingOutputDoNotInvokeAWS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.csv")
	if err := os.WriteFile(path, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"results", "--function", "f", "--query-id", "q", "--page-size", "0"},
		{"query", "--function", "f", "--sql", "SELECT 1", "--out", path},
		{"query", "--function", "f", "--sql", "SELECT 1", "--request-id", "short"},
		{"results", "--function", "f", "--query-id", "q", "--format", "bad"},
		{"start", "--function", "f"},
	} {
		var out, diagnostics bytes.Buffer
		code := run(context.Background(), args, strings.NewReader(""), &out, &diagnostics, func(context.Context, settings) (*client, error) {
			t.Error("invalid invocation loaded AWS configuration")
			return nil, fmt.Errorf("should not be reached")
		})
		if code == 0 || out.Len() != 0 {
			t.Errorf("invalid args succeeded: %v", args)
		}
	}
	data, _ := os.ReadFile(path)
	if string(data) != "keep me" {
		t.Error("existing output was overwritten")
	}
}

func TestInvalidOutputParentDoesNotInvokeAWS(t *testing.T) {
	commands := [][]string{
		{"tables"},
		{"describe", "--table", "events"},
		{"query", "--sql", "SELECT 1"},
		{"results", "--query-id", "q"},
		{"download", "--query-id", "q"},
	}
	for _, command := range commands {
		for _, kind := range []string{"missing", "not-directory"} {
			t.Run(command[0]+"/"+kind, func(t *testing.T) {
				dir := t.TempDir()
				parent := filepath.Join(dir, "parent")
				if kind == "not-directory" {
					if err := os.WriteFile(parent, []byte("keep me"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				args := append(append([]string{}, command...), "--function", "f", "--out", filepath.Join(parent, "output.csv"))
				var out, diagnostics bytes.Buffer
				code := run(context.Background(), args, strings.NewReader(""), &out, &diagnostics, func(context.Context, settings) (*client, error) {
					t.Error("invalid output parent loaded AWS configuration")
					return nil, fmt.Errorf("should not be reached")
				})
				if code != 1 || out.Len() != 0 || !strings.Contains(diagnostics.String(), `"code":"LOCAL_IO"`) {
					t.Errorf("exit=%d stdout=%s stderr=%s", code, &out, &diagnostics)
				}
				if kind == "not-directory" {
					data, err := os.ReadFile(parent)
					if err != nil || string(data) != "keep me" {
						t.Errorf("parent file changed: %q, %v", data, err)
					}
				} else if _, err := os.Stat(parent); !os.IsNotExist(err) {
					t.Errorf("missing parent was created: %v", err)
				}
			})
		}
	}
}

func TestStartRejectionDoesNotSuggestAnUncertainRetry(t *testing.T) {
	for _, command := range []string{"query", "start"} {
		for _, errorCode := range []string{"NOT_ALLOWED", "INVALID_REQUEST", "WORKER_ERROR"} {
			t.Run(command+"/"+errorCode, func(t *testing.T) {
				calls := 0
				c := localWorker(t, func(req request, w http.ResponseWriter) {
					calls++
					if req.Action != "start_query" {
						t.Errorf("unexpected action %s", req.Action)
					}
					json.NewEncoder(w).Encode(envelope{Version: 1, Error: &workerError{Code: errorCode, Message: "request failed"}})
				})
				var out, diagnostics bytes.Buffer
				code := run(context.Background(), []string{command, "--function", "f", "--sql", "SELECT 1"}, strings.NewReader(""), &out, &diagnostics, factory(c))
				if code != 1 || out.Len() != 0 || calls != 1 || !strings.Contains(diagnostics.String(), errorCode) {
					t.Errorf("exit=%d calls=%d stdout=%s stderr=%s", code, calls, &out, &diagnostics)
				}
				uncertain := errorCode == "WORKER_ERROR"
				if strings.Contains(diagnostics.String(), "retry with the same --request-id") != uncertain {
					t.Errorf("wrong retry guidance for %s: %s", errorCode, &diagnostics)
				}
			})
		}
	}
}

func TestDownloadStreamingCancellationExitsThree(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := streamingDownload(t)
	previous := http.DefaultClient
	http.DefaultClient = interruptDownloadAfterRead(server.Client(), cancel)
	t.Cleanup(func() { http.DefaultClient = previous })
	c := localWorker(t, func(req request, w http.ResponseWriter) {
		if req.Action != "download_result" || req.QueryID != "q" {
			t.Errorf("unexpected download request: %+v", req)
		}
		reply(w, download{URL: server.URL + "/file?token=fake-bearer", Format: "csv"})
	})
	dir := t.TempDir()
	var out, diagnostics bytes.Buffer
	code := run(ctx, []string{"download", "--function", "f", "--query-id", "q", "--out", filepath.Join(dir, "output.csv")}, strings.NewReader(""), &out, &diagnostics, factory(c))
	if ctx.Err() != context.Canceled {
		t.Fatal("download did not reach the first body read")
	}
	if code != 3 || out.Len() != 0 || !strings.Contains(diagnostics.String(), "context canceled") {
		t.Errorf("exit=%d stdout=%s stderr=%s", code, &out, &diagnostics)
	}
	if strings.Contains(diagnostics.String(), "fake-bearer") || strings.Contains(diagnostics.String(), server.URL) {
		t.Errorf("diagnostics exposed signed URL: %s", &diagnostics)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Errorf("canceled download left files: %v, %v", files, err)
	}
}

func TestQueryFailureAndUnknownState(t *testing.T) {
	for _, state := range []string{"FAILED", "CANCELLED", "surprise"} {
		t.Run(state, func(t *testing.T) {
			c := localWorker(t, func(req request, w http.ResponseWriter) {
				if req.Action == "start_query" {
					reply(w, started{QueryID: "q-failed"})
				} else {
					reply(w, queryStatus{QueryID: "q-failed", State: state, Reason: "test reason"})
				}
			})
			var out, diagnostics bytes.Buffer
			code := run(context.Background(), []string{"query", "--function", "f", "--sql", "SELECT 1"}, strings.NewReader(""), &out, &diagnostics, factory(c))
			if code != 1 || out.Len() != 0 || !strings.Contains(diagnostics.String(), "q-failed") {
				t.Fatalf("exit=%d stdout=%s stderr=%s", code, &out, &diagnostics)
			}
		})
	}
}

func TestQueryPollsWithoutResubmission(t *testing.T) {
	var mu sync.Mutex
	starts, statuses := 0, 0
	c := localWorker(t, func(req request, w http.ResponseWriter) {
		mu.Lock()
		defer mu.Unlock()
		switch req.Action {
		case "start_query":
			starts++
			reply(w, started{QueryID: "q-poll"})
		case "query_status":
			statuses++
			state := "RUNNING"
			if statuses > 1 {
				state = "SUCCEEDED"
			}
			reply(w, queryStatus{QueryID: "q-poll", State: state})
		case "get_results":
			reply(w, page{QueryID: "q-poll", Columns: []column{{"answer", "integer"}}, Rows: [][]*string{{str("42")}}})
		}
	})
	var out, diagnostics bytes.Buffer
	code := run(context.Background(), []string{"query", "--function", "f", "--sql", "SELECT 42", "--poll-interval", "1ms", "--format", "json"}, strings.NewReader(""), &out, &diagnostics, factory(c))
	if code != 0 || !strings.Contains(out.String(), `"rows":[["42"]]`) {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, &out, &diagnostics)
	}
	mu.Lock()
	defer mu.Unlock()
	if starts != 1 || statuses != 2 {
		t.Fatalf("started %d times and polled %d times", starts, statuses)
	}
}

func TestCancellationRetainsRecoveryID(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := localWorker(t, func(req request, w http.ResponseWriter) {
		if req.Action == "start_query" {
			reply(w, started{QueryID: "q-cancelled-client"})
		} else {
			reply(w, queryStatus{QueryID: "q-cancelled-client", State: "RUNNING"})
			cancel()
		}
	})
	var out, diagnostics bytes.Buffer
	code := run(ctx, []string{"query", "--function", "f", "--sql", "SELECT 1"}, strings.NewReader(""), &out, &diagnostics, factory(c))
	if code != 3 || out.Len() != 0 || !strings.Contains(diagnostics.String(), `"query_id":"q-cancelled-client"`) {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, &out, &diagnostics)
	}
}

func TestInvalidPagesFail(t *testing.T) {
	for _, body := range []string{
		`{"version":1,"result":{"query_id":"q","columns":[{"name":"n","type":"bigint"}],"rows":[[9007199254740993]]}}`,
		`{"version":1,"result":{"query_id":"q","columns":[{"name":"n","type":"bigint"}],"rows":[[]]}}`,
		`{"version":1,"result":{"query_id":"other","columns":[],"rows":[]}}`,
		`{"version":1,"result":{"query_id":"q","columns":[],"rows":null}}`,
	} {
		c := localWorker(t, func(_ request, w http.ResponseWriter) { io.WriteString(w, body) })
		var out, diagnostics bytes.Buffer
		code := run(context.Background(), []string{"results", "--function", "f", "--query-id", "q", "--format", "json"}, strings.NewReader(""), &out, &diagnostics, factory(c))
		if code != 1 || out.Len() != 0 {
			t.Fatalf("invalid page succeeded: %s, exit=%d, stdout=%s", body, code, &out)
		}
	}
}

func str(s string) *string { return &s }

func hash(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func mac(key []byte, text string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(text))
	return h.Sum(nil)
}
