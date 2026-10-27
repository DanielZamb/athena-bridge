package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
)

type invokeFunc func(context.Context, *lambda.InvokeInput) (*lambda.InvokeOutput, error)

func (f invokeFunc) Invoke(ctx context.Context, in *lambda.InvokeInput, _ ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
	return f(ctx, in)
}

func responseClient(body string) *client {
	return &client{api: invokeFunc(func(context.Context, *lambda.InvokeInput) (*lambda.InvokeOutput, error) {
		return &lambda.InvokeOutput{Payload: []byte(body)}, nil
	})}
}

func readDiagnostics(t *testing.T, data string) []diagnosticEvent {
	t.Helper()
	var events []diagnosticEvent
	for _, line := range strings.Split(strings.TrimSpace(data), "\n") {
		if line == "" {
			continue
		}
		var event diagnosticEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("stderr is not JSONL: %q: %v", line, err)
		}
		var fields map[string]json.RawMessage
		json.Unmarshal([]byte(line), &fields)
		for _, field := range []string{"version", "type", "code", "operation", "message", "exit_code", "recovery", "partial_output", "published"} {
			if _, ok := fields[field]; !ok {
				t.Fatalf("missing %s in %s", field, line)
			}
		}
		if event.Version != 1 || event.Operation == "" || event.Code == "" || event.Message == "" || event.Recovery == "" {
			t.Fatalf("invalid diagnostic: %+v", event)
		}
		events = append(events, event)
	}
	return events
}

func TestFailureMatrix(t *testing.T) {
	secret := "https://private.invalid/path?token=secret SELECT secret_sql"
	base := []string{"results", "--function", "f", "--query-id", "q", "--format", "json"}
	status := func(state string) *client {
		return responseClient(`{"version":1,"result":{"query_id":"q","state":"` + state + `","reason":"` + secret + `"}}`)
	}
	tests := []struct {
		name      string
		args      []string
		client    *client
		configErr error
		writer    io.Writer
		want      Code
		exit      int
	}{
		{name: "parse", args: []string{"results", "--timeout", secret}, want: CodeUsage, exit: 2},
		{name: "unknown command", args: []string{secret}, want: CodeUsage, exit: 2},
		{name: "validation", args: []string{"results", "--function", "f"}, want: CodeUsage, exit: 2},
		{name: "configuration", configErr: errors.New(secret), want: CodeConfig, exit: 1},
		{name: "transport", client: &client{api: invokeFunc(func(context.Context, *lambda.InvokeInput) (*lambda.InvokeOutput, error) {
			return nil, &url.Error{Op: "Post", URL: secret, Err: errors.New(secret)}
		})}, want: CodeTransport, exit: 1},
		{name: "function", client: &client{api: invokeFunc(func(context.Context, *lambda.InvokeInput) (*lambda.InvokeOutput, error) {
			return &lambda.InvokeOutput{FunctionError: aws.String("Unhandled"), Payload: []byte(secret)}, nil
		})}, want: CodeFunction, exit: 1},
		{name: "worker", client: responseClient(`{"version":1,"error":{"code":"NOT_READY","message":"` + secret + `"}}`), want: CodeNotReady, exit: 1},
		{name: "worker unsafe code", client: responseClient(`{"version":1,"error":{"code":"` + secret + `","message":"private"}}`), want: CodeWorker, exit: 1},
		{name: "malformed", client: responseClient(secret), want: CodeProtocol, exit: 1},
		{name: "version", client: responseClient(`{"version":2,"result":{}}`), want: CodeProtocol, exit: 1},
		{name: "envelope", client: responseClient(`{"version":1,"result":{},"error":{"code":"BAD","message":"private"}}`), want: CodeProtocol, exit: 1},
		{name: "result", client: responseClient(`{"version":1,"result":{"columns":[],"rows":[[1]]}}`), want: CodeProtocol, exit: 1},
		{name: "query failed", args: append(append([]string{}, base...), "--wait"), client: status("FAILED"), want: CodeQueryFailed, exit: 1},
		{name: "query cancelled", args: append(append([]string{}, base...), "--wait"), client: status("CANCELLED"), want: CodeQueryCancelled, exit: 1},
		{name: "cancellation", client: &client{api: invokeFunc(func(context.Context, *lambda.InvokeInput) (*lambda.InvokeOutput, error) {
			return nil, fmt.Errorf("%s: %w", secret, context.Canceled)
		})}, want: CodeCancelled, exit: 3},
		{name: "deadline", client: &client{api: invokeFunc(func(context.Context, *lambda.InvokeInput) (*lambda.InvokeOutput, error) {
			return nil, fmt.Errorf("%s: %w", secret, context.DeadlineExceeded)
		})}, want: CodeDeadline, exit: 3},
		{name: "input", args: []string{"start", "--function", "f", "--sql-file", filepath.Join(t.TempDir(), "absent.sql")}, want: CodeLocalIO, exit: 1},
		{name: "output", client: responseClient(`{"version":1,"result":{"query_id":"q","columns":[],"rows":[]}}`), writer: brokenWriter{}, want: CodeLocalIO, exit: 1},
		{name: "help output", args: []string{"results", "--help"}, writer: brokenWriter{}, want: CodeLocalIO, exit: 1},
		{name: "version output", args: []string{"version"}, writer: brokenWriter{}, want: CodeLocalIO, exit: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := tt.args
			if args == nil {
				args = base
			}
			var out, stderr bytes.Buffer
			writer := tt.writer
			if writer == nil {
				writer = &out
			}
			code := run(context.Background(), args, strings.NewReader(""), writer, &stderr, func(context.Context, settings) (*client, error) {
				if tt.client == nil && tt.configErr == nil {
					t.Fatal("unexpected AWS configuration")
				}
				return tt.client, tt.configErr
			})
			events := readDiagnostics(t, stderr.String())
			if len(events) == 0 {
				t.Fatal("missing diagnostic")
			}
			event := events[len(events)-1]
			if code != tt.exit || event.ExitCode != tt.exit || event.Type != "error" || event.Code != tt.want || event.PartialOutput || event.Published {
				t.Fatalf("exit=%d event=%+v", code, event)
			}
			if strings.Contains(stderr.String(), "private.invalid") || strings.Contains(stderr.String(), "secret_sql") || strings.Contains(stderr.String(), "token=secret") {
				t.Fatalf("private cause leaked: %s", &stderr)
			}
		})
	}
}

func TestFaultPreservesTypedCauses(t *testing.T) {
	cause := &url.Error{Op: "Post", URL: "https://private.invalid/?token=secret", Err: context.DeadlineExceeded}
	c := &client{api: invokeFunc(func(context.Context, *lambda.InvokeInput) (*lambda.InvokeOutput, error) { return nil, cause })}
	err := c.call(context.Background(), request{Action: "query_status"}, &queryStatus{})
	var f *Fault
	var transport *url.Error
	if !errors.As(err, &f) || !errors.As(err, &transport) || transport != cause || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, CodeTransport) {
		t.Fatalf("lost typed cause: %v", err)
	}
	if strings.Contains(err.Error(), "private.invalid") {
		t.Fatalf("unsafe public error: %v", err)
	}
	c = responseClient(`{"version":1,"error":{"code":"CURSOR_EXPIRED","message":"private"}}`)
	err = c.call(context.Background(), request{Action: "get_results"}, &page{})
	var worker *workerError
	if !errors.Is(err, CodeWorker) || !errors.As(err, &worker) || worker.Code != "CURSOR_EXPIRED" {
		t.Fatalf("lost worker rejection: %v", err)
	}
}

func TestKnownWorkerFailureRecovery(t *testing.T) {
	for _, tt := range []struct {
		workerCode string
		code       Code
		recovery   Recovery
	}{
		{"NOT_READY", "NOT_READY", "wait_query"},
		{"RESULT_CHANGED", CodeResultChanged, RecoveryInspect},
		{"NOT_ALLOWED", CodeWorker, RecoveryNone},
		{"INVALID_REQUEST", CodeWorker, RecoveryArguments},
		{"CURSOR_EXPIRED", CodeWorker, RecoveryWorker},
		{"UNKNOWN", CodeWorker, RecoveryWorker},
	} {
		t.Run(tt.workerCode, func(t *testing.T) {
			c := responseClient(`{"version":1,"error":{"code":"` + tt.workerCode + `","message":"private SQL and policy"}}`)
			err := c.call(context.Background(), request{Action: "get_results", QueryID: "q"}, &page{})
			var worker *workerError
			if !errors.As(err, &worker) || worker.Code != tt.workerCode || !errors.Is(err, tt.code) {
				t.Fatalf("lost worker cause or code: %v", err)
			}
			var out, stderr bytes.Buffer
			exit := run(context.Background(), []string{"results", "--function", "f", "--query-id", "q"}, strings.NewReader(""), &out, &stderr, factory(c))
			events := readDiagnostics(t, stderr.String())
			if exit != 1 || out.Len() != 0 || len(events) != 1 || events[0].Code != tt.code || events[0].Recovery != tt.recovery || events[0].WorkerCode != tt.workerCode || events[0].QueryID != "q" || strings.Contains(stderr.String(), "private") {
				t.Fatalf("exit=%d stdout=%s events=%+v", exit, &out, events)
			}
		})
	}
}

func TestStartFailureRetainsSafeRetryIdentity(t *testing.T) {
	for _, cause := range []error{errors.New("lost response"), context.Canceled, context.DeadlineExceeded,
		fault(CodeAuthentication, "authenticate", "cannot retrieve credentials", RecoveryConfig, context.Canceled),
		fault(CodeAuthentication, "authenticate", "cannot retrieve credentials", RecoveryConfig, context.DeadlineExceeded)} {
		c := &client{api: invokeFunc(func(context.Context, *lambda.InvokeInput) (*lambda.InvokeOutput, error) { return nil, cause })}
		var out, stderr bytes.Buffer
		id := strings.Repeat("r", 32)
		exit := run(context.Background(), []string{"start", "--function", "f", "--sql", "SELECT private_sql", "--request-id", id}, strings.NewReader(""), &out, &stderr, factory(c))
		events := readDiagnostics(t, stderr.String())
		if len(events) != 2 || events[0].Code != "START_REQUEST" {
			t.Fatalf("missing start notice: %+v", events)
		}
		last := events[len(events)-1]
		wantExit := 1
		if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
			wantExit = 3
		}
		if exit != wantExit || last.ExitCode != wantExit || last.RequestID != id || last.QueryID != "" || last.Recovery != RecoverySameStart || strings.Contains(stderr.String(), "private_sql") {
			t.Fatalf("unsafe retry: exit=%d events=%+v", exit, events)
		}
	}
}

func TestOutputStateDiagnostics(t *testing.T) {
	for _, file := range []bool{false, true} {
		t.Run(fmt.Sprint("file=", file), func(t *testing.T) {
			calls := 0
			c := localWorker(t, func(req request, w http.ResponseWriter) {
				calls++
				if calls == 1 {
					reply(w, page{QueryID: "q", Columns: []column{{"n", "bigint"}}, Rows: [][]*string{{str("1")}}, NextToken: "next"})
				} else {
					io.WriteString(w, "invalid")
				}
			})
			args := []string{"results", "--function", "f", "--query-id", "q", "--all", "--format", "json"}
			path := filepath.Join(t.TempDir(), "output.json")
			if file {
				args = append(args, "--out", path)
			}
			var out, stderr bytes.Buffer
			exit := run(context.Background(), args, strings.NewReader(""), &out, &stderr, factory(c))
			events := readDiagnostics(t, stderr.String())
			last := events[len(events)-1]
			if exit != 1 || last.PartialOutput != !file || last.Published {
				t.Fatalf("exit=%d last=%+v", exit, last)
			}
			if file {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("published partial file: %v", err)
				}
			}
		})
	}
}

func TestNoticeOutputFailureStopsSubmission(t *testing.T) {
	calls := 0
	c := &client{api: invokeFunc(func(context.Context, *lambda.InvokeInput) (*lambda.InvokeOutput, error) { calls++; return nil, nil })}
	exit := run(context.Background(), []string{"start", "--function", "f", "--sql", "SELECT 1"}, strings.NewReader(""), io.Discard, brokenWriter{}, factory(c))
	if exit != 1 || calls != 0 {
		t.Fatalf("exit=%d starts=%d", exit, calls)
	}
}
