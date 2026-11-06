package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
)

type Code string

const (
	CodeUsage          Code = "INVALID_ARGUMENT"
	CodeConfig         Code = "CONFIGURATION"
	CodeTransport      Code = "TRANSPORT"
	CodeAuthorization  Code = "AUTHORIZATION"
	CodeAuthentication Code = "AUTHENTICATION"
	CodeThrottled      Code = "THROTTLED"
	CodeNotReady       Code = "NOT_READY"
	CodeWorker         Code = "WORKER_REJECTED"
	CodeFunction       Code = "WORKER_FUNCTION"
	CodeProtocol       Code = "PROTOCOL"
	CodeQueryFailed    Code = "QUERY_FAILED"
	CodeQueryCancelled Code = "QUERY_CANCELLED"
	CodeLocalIO        Code = "LOCAL_IO"
	CodeCancelled      Code = "CANCELLED"
	CodeDeadline       Code = "DEADLINE"
	CodeResultChanged  Code = "RESULT_CHANGED"
	CodeInternal       Code = "INTERNAL"
	CodeLimit          Code = "OUTPUT_LIMIT"
)

func (c Code) Error() string { return string(c) }

type Recovery string

const (
	RecoveryNone      Recovery = "none"
	RecoveryArguments Recovery = "fix_arguments"
	RecoveryConfig    Recovery = "check_configuration"
	RecoveryRetry     Recovery = "retry_operation"
	RecoverySameStart Recovery = "retry_same_request"
	RecoveryInspect   Recovery = "inspect_query"
	RecoveryWait      Recovery = "wait_query"
	RecoveryOutput    Recovery = "check_output"
	RecoveryWorker    Recovery = "check_worker"
)

// Fault keeps its cause private. Only the explicit public fields enter diagnostics.
type Fault struct {
	Code      Code
	Operation string
	Message   string
	Recovery  Recovery
	cause     error
}

func (f *Fault) Error() string        { return f.Message }
func (f *Fault) Unwrap() error        { return f.cause }
func (f *Fault) Is(target error) bool { return target == f.Code }

func fault(code Code, operation, message string, recovery Recovery, cause error) *Fault {
	return &Fault{Code: code, Operation: operation, Message: message, Recovery: recovery, cause: cause}
}

func protocolFault(operation, message string, cause error) *Fault {
	return fault(CodeProtocol, operation, message, RecoveryWorker, cause)
}

func ioFault(operation, message string, cause error) *Fault {
	return fault(CodeLocalIO, operation, message, RecoveryOutput, cause)
}

type diagnosticEvent struct {
	Version       int      `json:"version"`
	Type          string   `json:"type"`
	Code          Code     `json:"code"`
	Operation     string   `json:"operation"`
	Message       string   `json:"message"`
	RequestID     string   `json:"request_id,omitempty"`
	QueryID       string   `json:"query_id,omitempty"`
	WorkerCode    string   `json:"worker_code,omitempty"`
	NextToken     string   `json:"next_token,omitempty"`
	ExitCode      int      `json:"exit_code"`
	Recovery      Recovery `json:"recovery"`
	PartialOutput bool     `json:"partial_output"`
	Published     bool     `json:"published"`
}

type diagnostics struct {
	usage         usageRecord
	out           io.Writer
	pretty        bool
	operation     string
	requestID     string
	queryID       string
	startPending  bool
	partialOutput bool
	published     bool
}

var diagnosticID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,256}$`)

func safeID(id string) string {
	if diagnosticID.MatchString(id) {
		return id
	}
	return ""
}

func (d *diagnostics) emit(event diagnosticEvent) error {
	event.Version = 1
	event.RequestID = safeID(d.requestID)
	event.QueryID = safeID(d.queryID)
	event.PartialOutput = d.partialOutput
	event.Published = d.published
	if event.Operation == "" {
		event.Operation = d.operation
	}
	var err error
	if d.pretty {
		err = writePrettyDiagnostic(d.out, event)
	} else {
		err = json.NewEncoder(d.out).Encode(event)
	}
	if err != nil {
		return ioFault("diagnostics", "cannot write diagnostics", err)
	}
	return nil
}

func (d *diagnostics) notice(code Code, message string, recovery Recovery) error {
	return d.emit(diagnosticEvent{Type: "notice", Code: code, Message: message, Recovery: recovery})
}

func (d *diagnostics) failure(err error) int {
	f := fault(CodeInternal, d.operation, "command failed", RecoveryNone, err)
	var typed *Fault
	if errors.As(err, &typed) {
		f = typed
	}
	code, message, recovery := f.Code, f.Message, f.Recovery
	exit := 1
	if errors.Is(err, context.Canceled) {
		code, message = CodeCancelled, "context canceled"
		exit = 3
	} else if errors.Is(err, context.DeadlineExceeded) {
		code, message = CodeDeadline, "command deadline exceeded"
		exit = 3
	} else if code == CodeUsage {
		exit = 2
	}
	d.usage.errorCode = code
	if d.startPending {
		recovery = RecoverySameStart
		message += "; retry with the same --request-id and SQL"
	} else if d.queryID != "" && (code == CodeCancelled || code == CodeDeadline) {
		recovery = RecoveryInspect
	}
	var worker *workerError
	workerCode := ""
	if errors.As(err, &worker) && diagnosticID.MatchString(worker.Code) {
		workerCode = worker.Code
	}
	// A failed diagnostics writer cannot report its own failure on that writer.
	_ = d.emit(diagnosticEvent{Type: "error", Code: code, Operation: f.Operation,
		Message: message, WorkerCode: workerCode, ExitCode: exit, Recovery: recovery})
	return exit
}

type countingWriter struct {
	out io.Writer
	n   int64
	err error
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.out.Write(p)
	w.n += int64(n)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.err = ioFault("write_output", "cannot write output", err)
		return n, w.err
	}
	return n, nil
}
