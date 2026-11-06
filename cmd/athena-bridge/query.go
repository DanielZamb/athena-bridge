package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
)

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{32,128}$`)
var quantityPattern = regexp.MustCompile(`^[0-9]+$`)

func requestID(value string) (string, error) {
	if value != "" {
		if !requestIDPattern.MatchString(value) {
			return "", fault(CodeUsage, "start_query", "--request-id must contain 32 to 128 letters, digits, dots, underscores, colons, or hyphens", RecoveryArguments, nil)
		}
		return value, nil
	}
	var valueBytes [16]byte
	if _, err := rand.Read(valueBytes[:]); err != nil {
		return "", fault(CodeInternal, "start_query", "cannot generate request ID", RecoveryRetry, err)
	}
	return hex.EncodeToString(valueBytes[:]), nil
}

func (c *client) start(ctx context.Context, req request) (started, error) {
	var result started
	if err := c.call(ctx, req, &result); err != nil {
		return result, err
	}
	if strings.TrimSpace(result.QueryID) == "" {
		return result, protocolFault(req.Action, "worker start result requires query_id", nil)
	}
	return result, nil
}

func validateStatus(s queryStatus, id, operation string) error {
	if s.QueryID != id {
		return protocolFault(operation, "worker returned a different query_id", nil)
	}
	switch s.State {
	case "QUEUED", "RUNNING", "SUCCEEDED", "FAILED", "CANCELLED":
	default:
		return protocolFault(operation, "unknown worker query state", nil)
	}
	if stats := s.Statistics; stats != nil {
		for _, value := range []*string{stats.DataScannedBytes, stats.EngineExecutionMS, stats.TotalExecutionMS, stats.QueueMS, stats.PlanningMS} {
			if value != nil && !quantityPattern.MatchString(*value) {
				return protocolFault(operation, "worker statistics require nonnegative decimal strings", nil)
			}
		}
	}
	if f := s.Failure; f != nil {
		if (f.ErrorCategory != nil && *f.ErrorCategory < 0) || (f.ErrorType != nil && *f.ErrorType < 0) {
			return protocolFault(operation, "worker failure categories must be nonnegative", nil)
		}
		if (f.SQLLine != nil && *f.SQLLine <= 0) || (f.SQLColumn != nil && *f.SQLColumn <= 0) {
			return protocolFault(operation, "worker SQL coordinates must be positive integers", nil)
		}
	}
	return nil
}

func (c *client) status(ctx context.Context, id string) (queryStatus, error) {
	var s queryStatus
	if err := c.call(ctx, request{Action: "query_status", QueryID: id}, &s); err != nil {
		return s, err
	}
	return s, validateStatus(s, id, "query_status")
}

func (c *client) cancel(ctx context.Context, id string) (cancelled, error) {
	var s cancelled
	if err := c.call(ctx, request{Action: "cancel_query", QueryID: id}, &s); err != nil {
		return s, err
	}
	if s.CancellationRequested == nil {
		return s, protocolFault("cancel_query", "worker cancel result requires cancellation_requested", nil)
	}
	return s, validateStatus(s.queryStatus, id, "cancel_query")
}

func (c *client) info(ctx context.Context) (workerInfo, error) {
	var info workerInfo
	if err := c.call(ctx, request{Action: "worker_info"}, &info); err != nil {
		return info, err
	}
	if strings.TrimSpace(info.WorkerID) == "" || info.Basis != "configured" || info.Databases == nil || info.Actions == nil || info.Features == nil || info.Limits == nil {
		return info, protocolFault("worker_info", "worker info requires configured identity, databases, actions, features and limits", nil)
	}
	for _, limit := range info.Limits {
		if limit < 0 {
			return info, protocolFault("worker_info", "worker limits must be nonnegative", nil)
		}
	}
	return info, nil
}

func (c *client) wait(ctx context.Context, id string, interval time.Duration) error {
	for {
		s, err := c.status(ctx, id)
		if err != nil {
			return err
		}
		switch s.State {
		case "SUCCEEDED":
			return nil
		case "FAILED":
			return fault(CodeQueryFailed, "wait_query", "query failed; inspect its status", RecoveryInspect, nil)
		case "CANCELLED":
			return fault(CodeQueryCancelled, "wait_query", "query was cancelled; inspect its status", RecoveryInspect, nil)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fault(CodeTransport, "wait_query", "query wait interrupted", RecoveryInspect, ctx.Err())
		case <-timer.C:
		}
	}
}

// fetchPage validates a single response. expectedColumns == nil accepts the first
// page's schema; a non-nil empty slice requires an empty schema. Export owns its
// durable cursor and result identity, and passes both back on each fetch.
func (c *client) fetchPage(ctx context.Context, req request, expectedColumns []column) (page, error) {
	var p page
	if err := c.call(ctx, req, &p); err != nil {
		return p, err
	}
	if err := validatePage(p); err != nil {
		return p, err
	}
	if p.QueryID != req.QueryID {
		return p, protocolFault(req.Action, "worker results returned a different query_id", nil)
	}
	if req.ExpectedResultID != "" && p.ResultID != req.ExpectedResultID {
		return p, fault(CodeResultChanged, req.Action, "worker result identity changed or is missing", RecoveryInspect, nil)
	}
	if expectedColumns != nil && !slices.Equal(expectedColumns, p.Columns) {
		return p, protocolFault(req.Action, "worker changed column metadata between pages", nil)
	}
	return p, nil
}

func (c *client) pages(ctx context.Context, req request, all bool, format string, out io.Writer, d *diagnostics) error {
	var w rowSink = &resultWriter{out: out, format: format}
	if format == prettyFormat {
		w = &prettyRows{out: out}
	}
	seen := map[string]bool{req.NextToken: true}
	var columns []column
	for {
		p, err := c.fetchPage(ctx, req, columns)
		if err != nil {
			return err
		}
		if p.NextToken != "" && seen[p.NextToken] {
			return protocolFault(req.Action, "worker repeated a pagination token", nil)
		}
		columns = p.Columns
		if req.ExpectedResultID == "" {
			req.ExpectedResultID = p.ResultID
		}
		if err := w.write(p); err != nil {
			return err
		}
		d.usage.rows += int64(len(p.Rows))
		d.usage.pages++
		if p.NextToken == "" || !all {
			if err := w.finish(p.NextToken); err != nil {
				return err
			}
			if p.NextToken != "" {
				d.partialOutput = true
				return d.emit(diagnosticEvent{Type: "notice", Code: "MORE_RESULTS", Message: "more rows are available; use --all, or continue with --next-token", Recovery: RecoveryRetry, NextToken: p.NextToken})
			}
			return nil
		}
		seen[p.NextToken] = true
		req.NextToken = p.NextToken
	}
}

// definiteStartRejection is limited to explicit configuration, authentication,
// validation and authorization refusals.
// Other failures can follow a successful submission and must retain the token.
func definiteStartRejection(err error) bool {
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) &&
		(errors.Is(err, CodeAuthorization) || errors.Is(err, CodeAuthentication) || errors.Is(err, CodeConfig)) {
		return true
	}
	var rejection *workerError
	return errors.As(err, &rejection) && (rejection.Code == "NOT_ALLOWED" || rejection.Code == "INVALID_REQUEST")
}
