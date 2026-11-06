package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/smithy-go"
)

func TestSDKFailurePreservesPrivateCause(t *testing.T) {
	for _, tt := range []struct {
		name  string
		cause error
		code  Code
	}{
		{"denied", &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "private"}, "AUTHORIZATION"},
		{"expired", &smithy.GenericAPIError{Code: "ExpiredTokenException", Message: "private"}, "AUTHENTICATION"},
		{"throttled", &types.TooManyRequestsException{Message: aws.String("private")}, "THROTTLED"},
		{"missing function", &types.ResourceNotFoundException{Message: aws.String("private")}, CodeConfig},
		{"signing", &v4.SigningError{Err: errors.New("private")}, "AUTHENTICATION"},
		{"region", &aws.MissingRegionError{}, CodeConfig},
		{"parameters", smithy.InvalidParamsError{Context: "private"}, CodeConfig},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cause := &smithy.OperationError{ServiceID: "Lambda", OperationName: "Invoke", Err: tt.cause}
			c := &client{api: invokeFunc(func(context.Context, *lambda.InvokeInput) (*lambda.InvokeOutput, error) { return nil, cause })}
			err := c.call(context.Background(), request{Action: "query_status"}, &queryStatus{})
			var original *smithy.OperationError
			var f *Fault
			if !errors.As(err, &original) || original != cause || !errors.Is(err, cause) || !errors.As(err, &f) || f.Code != tt.code || f.Operation != "query_status" || strings.Contains(err.Error(), "private") {
				t.Fatalf("lost classification or cause: fault=%+v error=%v", f, err)
			}
		})
	}
}

func TestCLIInvokeFailuresAndRetryIdentity(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "athena-bridge")
	if data, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v: %s", err, data)
	}
	config := filepath.Join(t.TempDir(), "empty-config")
	if err := os.WriteFile(config, nil, 0600); err != nil {
		t.Fatal(err)
	}
	command := func(ctx context.Context, endpoint string, attempts int, extraEnv []string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, binary, args...)
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "AWS_") && !strings.HasPrefix(entry, "ATHENA_BRIDGE_FUNCTION=") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, "AWS_ACCESS_KEY_ID=local-access", "AWS_SECRET_ACCESS_KEY=local-secret", "AWS_REGION=us-east-1", "AWS_EC2_METADATA_DISABLED=true", "AWS_CONFIG_FILE="+config, "AWS_SHARED_CREDENTIALS_FILE="+config, "AWS_ENDPOINT_URL_LAMBDA="+endpoint, fmt.Sprintf("AWS_MAX_ATTEMPTS=%d", attempts))
		cmd.Env = append(cmd.Env, extraEnv...)
		return cmd
	}
	call := func(t *testing.T, endpoint string, attempts int, extraEnv []string, args ...string) (int, string, []diagnosticEvent) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := command(ctx, endpoint, attempts, extraEnv, args...)
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		exit := 0
		if err := cmd.Run(); err != nil {
			var failed *exec.ExitError
			if !errors.As(err, &failed) || ctx.Err() != nil {
				t.Fatalf("run CLI: %v: %s", err, &stderr)
			}
			exit = failed.ExitCode()
		}
		for _, secret := range []string{"private", "local-access", "local-secret", endpoint} {
			if strings.Contains(stderr.String(), secret) {
				t.Fatalf("private cause leaked: %s", &stderr)
			}
		}
		return exit, out.String(), readDiagnostics(t, stderr.String())
	}
	t.Run("interrupted response body", func(t *testing.T) {
		var received atomic.Bool
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			received.Store(true)
			w.Header().Set("Content-Length", "4096")
			fmt.Fprint(w, `{"version":1,"result":`)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		defer server.Close()
		exit, out, events := call(t, server.URL, 1, nil, "results", "--function", "worker", "--query-id", "q", "--format", "json", "--timeout", "500ms")
		if !received.Load() || exit != 3 || out != "" || len(events) != 1 || events[0].Code != CodeDeadline || events[0].PartialOutput || events[0].Published {
			t.Fatalf("interrupted body: received=%v exit=%d out=%s events=%+v", received.Load(), exit, out, events)
		}
	})
	for _, format := range []string{"json", "csv"} {
		t.Run("checkpoint integrity/"+format, func(t *testing.T) {
			second := make(chan struct{}, 1)
			var resume atomic.Bool
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var req request
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				if req.Action != "get_results" || req.QueryID != "q" {
					t.Errorf("unexpected request: %+v", req)
				}
				if req.NextToken == "" {
					reply(w, page{QueryID: "q", ResultID: "r", Columns: exportColumns, Rows: exportRows()[:2], NextToken: "second"})
				} else if !resume.Load() {
					second <- struct{}{}
					<-r.Context().Done()
				} else {
					reply(w, page{QueryID: "q", ResultID: "r", Columns: exportColumns, Rows: exportRows()[2:]})
				}
			}))
			defer server.Close()
			destination := filepath.Join(t.TempDir(), "result")
			checkpoint := destination + ".checkpoint.json"
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			child := command(ctx, server.URL, 1, nil, "export", "--function", "worker", "--query-id", "q", "--out", destination, "--format", format)
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
			select {
			case <-second:
			case <-ctx.Done():
				t.Fatal("export did not commit its first page")
			}
			if err := child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := child.Wait(); err == nil {
				t.Fatal("killed export succeeded")
			}
			s := readExportState(t, checkpoint)
			if s.Rows != 2 || s.Phase != "writing" {
				t.Fatalf("interrupted checkpoint: %+v", s)
			}
			original := readExportBytes(t, checkpoint)
			f, err := os.OpenFile(s.Data, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.WriteString("uncommitted suffix")
			if closeErr := f.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				t.Fatal(err)
			}
			data := readExportBytes(t, s.Data)
			resume.Store(true)
			mutations := map[string]any{
				"version": 1, "encoding": "other", "function": "other", "region": "us-west-2",
				"query_id": "other", "result_id": "other", "destination": destination + ".other",
				"data": s.Data + ".other", "format": map[string]string{"json": "csv", "csv": "json"}[format],
				"page_size": 100, "columns": []column{{"other", "varchar"}}, "offset": s.Offset - 1,
				"rows": 0, "sha256": strings.Repeat("0", 64), "next_token": "other", "phase": "published",
				"started": false, "integrity": "",
				"cycle.anchor": "other", "cycle.power": 16, "cycle.length": 3,
				"columns.name": "other", "columns.type": "other",
			}
			for field, value := range mutations {
				t.Run(field, func(t *testing.T) {
					var fields map[string]any
					if err := json.Unmarshal(original, &fields); err != nil {
						t.Fatal(err)
					}
					switch field {
					case "cycle.anchor", "cycle.power", "cycle.length":
						fields["cycle"].(map[string]any)[strings.TrimPrefix(field, "cycle.")] = value
					case "columns.name", "columns.type":
						fields["columns"].([]any)[0].(map[string]any)[strings.TrimPrefix(field, "columns.")] = value
					default:
						fields[field] = value
					}
					if field == "version" {
						delete(fields, "integrity")
					}
					changed, err := json.Marshal(fields)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(checkpoint, changed, 0600); err != nil {
						t.Fatal(err)
					}
					beforeCalls := calls.Load()
					exit, out, events := call(t, server.URL, 1, nil, "export", "--resume", checkpoint)
					if exit != 1 || out != "" || len(events) != 1 || events[0].Code != CodeLocalIO || events[0].Published {
						t.Fatalf("metadata corruption accepted: exit=%d out=%s events=%+v", exit, out, events)
					}
					if calls.Load() != beforeCalls {
						t.Fatal("corrupt metadata fetched worker results")
					}
					if field == "version" && events[0].Message != "unsupported export checkpoint version; expected 2" {
						t.Fatalf("legacy checkpoint: %+v", events[0])
					}
					if !bytes.Equal(readExportBytes(t, s.Data), data) || !bytes.Equal(readExportBytes(t, checkpoint), changed) {
						t.Fatal("rejected checkpoint mutated export state")
					}
					if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("corrupt checkpoint published")
					}
					if _, err := os.Stat(destination + ".other"); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("corrupt checkpoint published alternate destination")
					}
				})
			}
			if err := os.WriteFile(checkpoint, original, 0600); err != nil {
				t.Fatal(err)
			}
			exit, _, events := call(t, server.URL, 1, nil, "export", "--resume", checkpoint)
			if exit != 0 || !bytes.Equal(readExportBytes(t, destination), expectedExport(t, format)) {
				t.Fatalf("exact crash recovery failed: exit=%d events=%+v", exit, events)
			}
		})
	}
	for _, tt := range []struct {
		name, sdkCode, code string
		status              int
		recovery            Recovery
	}{
		{"denied", "AccessDeniedException", "AUTHORIZATION", 403, RecoveryConfig},
		{"expired", "ExpiredTokenException", "AUTHENTICATION", 403, RecoveryConfig},
		{"signature", "InvalidSignatureException", "AUTHENTICATION", 403, RecoveryConfig},
		{"throttled", "TooManyRequestsException", "THROTTLED", 429, RecoveryRetry},
		{"missing function", "ResourceNotFoundException", "CONFIGURATION", 404, RecoveryConfig},
		{"invalid parameter", "InvalidParameterValueException", "CONFIGURATION", 400, RecoveryConfig},
	} {
		for _, command := range []string{"results", "start", "query"} {
			t.Run(tt.name+"/"+command, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("X-Amzn-ErrorType", tt.sdkCode)
					w.WriteHeader(tt.status)
					fmt.Fprint(w, `{"message":"private SQL and URL"}`)
				}))
				defer server.Close()
				args := []string{command, "--function", "worker", "--format", "json"}
				if command == "results" {
					args = append(args, "--query-id", "q")
				} else {
					args = append(args, "--sql", "SELECT 1")
				}
				exit, out, events := call(t, server.URL, 1, nil, args...)
				last := events[len(events)-1]
				wantRecovery := tt.recovery
				if command != "results" && tt.code == "THROTTLED" {
					wantRecovery = RecoverySameStart
				}
				if exit != 1 || out != "" || last.Code != Code(tt.code) || last.Recovery != wantRecovery || last.ExitCode != 1 {
					t.Fatalf("exit=%d stdout=%s last=%+v", exit, out, last)
				}
				if command != "results" && (len(events) != 2 || last.RequestID == "" || last.RequestID != events[0].RequestID) {
					t.Fatalf("lost request identity: %+v", events)
				}
			})
		}
	}
	t.Run("configuration and missing credentials", func(t *testing.T) {
		for _, env := range [][]string{{"AWS_REGION="}, {"AWS_ACCESS_KEY_ID=", "AWS_SECRET_ACCESS_KEY="}} {
			want := CodeConfig
			if len(env) == 2 {
				want = "AUTHENTICATION"
			}
			exit, out, events := call(t, "http://127.0.0.1:1", 1, env, "start", "--function", "worker", "--sql", "SELECT 1")
			last := events[len(events)-1]
			if exit != 1 || out != "" || last.Code != want || last.Recovery != RecoveryConfig {
				t.Fatalf("exit=%d stdout=%s last=%+v", exit, out, last)
			}
		}
	})
	for _, attempts := range []int{1, 2} {
		t.Run(fmt.Sprintf("uncertain start attempts=%d", attempts), func(t *testing.T) {
			var mu sync.Mutex
			var requests []request
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				var req request
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				requests = append(requests, req)
				if len(requests) == 1 {
					w.Header().Set("X-Amzn-ErrorType", "ServiceException")
					w.WriteHeader(500)
					fmt.Fprint(w, `{"message":"private lost response after submission"}`)
					return
				}
				reply(w, started{QueryID: "q-once"})
			}))
			defer server.Close()
			args := []string{"start", "--function", "worker", "--sql", "SELECT 1", "--database", "analytics", "--format", "json"}
			exit, out, events := call(t, server.URL, attempts, nil, args...)
			if attempts == 1 {
				last := events[len(events)-1]
				if exit != 1 || out != "" || last.Code != CodeTransport || last.Recovery != RecoverySameStart || last.RequestID != events[0].RequestID {
					t.Fatalf("unsafe uncertain retry: exit=%d last=%+v", exit, last)
				}
				exit, out, _ = call(t, server.URL, 1, nil, append(args, "--request-id", last.RequestID)...)
			}
			if exit != 0 || out != "{\"query_id\":\"q-once\"}\n" {
				t.Fatalf("retry failed: exit=%d stdout=%s", exit, out)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(requests) != 2 || requests[0] != requests[1] || requests[0].RequestID == "" || requests[0].SQL != "SELECT 1" || requests[0].Database != "analytics" {
				t.Fatalf("retry changed the submission: %+v", requests)
			}
		})
	}
}
