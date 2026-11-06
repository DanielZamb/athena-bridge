package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
)

type resultWriter struct {
	out     io.Writer
	format  string
	started bool
	rows    int64
}

func (w *resultWriter) write(p page) error {
	if !w.started {
		switch w.format {
		case "json":
			metadata, _ := json.Marshal(struct {
				QueryID string   `json:"query_id,omitempty"`
				Columns []column `json:"columns"`
			}{p.QueryID, p.Columns})
			if _, err := fmt.Fprintf(w.out, "%s,\"rows\":[", metadata[:len(metadata)-1]); err != nil {
				return err
			}
		case "csv":
			cw := csv.NewWriter(w.out)
			names := make([]string, len(p.Columns))
			for i, c := range p.Columns {
				names[i] = c.Name
			}
			if err := cw.Write(names); err != nil {
				return err
			}
			cw.Flush()
			if err := cw.Error(); err != nil {
				return err
			}
		}
	}
	switch w.format {
	case "json":
		for _, row := range p.Rows {
			if w.rows > 0 {
				if _, err := io.WriteString(w.out, ","); err != nil {
					return err
				}
			}
			data, _ := json.Marshal(row)
			if _, err := w.out.Write(data); err != nil {
				return err
			}
			w.rows++
		}
	case "csv":
		cw := csv.NewWriter(w.out)
		for _, row := range p.Rows {
			values := make([]string, len(row))
			for i, cell := range row {
				if cell == nil {
					values[i] = `\N`
				} else {
					values[i] = strings.ReplaceAll(*cell, `\`, `\\`)
				}
			}
			if len(values) == 1 && values[0] == "" {
				cw.Flush()
				if err := cw.Error(); err != nil {
					return err
				}
				if _, err := io.WriteString(w.out, "\"\"\n"); err != nil {
					return err
				}
			} else if err := cw.Write(values); err != nil {
				return err
			}
			w.rows++
		}
		cw.Flush()
		if err := cw.Error(); err != nil {
			return err
		}
	case "table":
		tw := tabwriter.NewWriter(w.out, 0, 4, 2, ' ', 0)
		if !w.started {
			for _, c := range p.Columns {
				fmt.Fprintf(tw, "%s\t", visible(c.Name))
			}
			fmt.Fprintln(tw)
		}
		for _, row := range p.Rows {
			for _, cell := range row {
				value := "NULL"
				if cell != nil {
					value = visible(*cell)
				}
				fmt.Fprintf(tw, "%s\t", value)
			}
			fmt.Fprintln(tw)
			w.rows++
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	w.started = true
	return nil
}

func (w *resultWriter) finish(token string) error {
	if w.format == "json" {
		tail, _ := json.Marshal(struct {
			NextToken *string `json:"next_token"`
			Complete  bool    `json:"complete"`
		}{NextToken: optional(token), Complete: token == ""})
		_, err := fmt.Fprintf(w.out, "],%s\n", tail[1:])
		return err
	}
	return nil
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func visible(s string) string {
	quoted := strconv.Quote(s)
	return quoted[1 : len(quoted)-1]
}

func atomicOutput(path string, write func(io.Writer) error) error {
	if _, err := os.Lstat(path); err == nil {
		return ioFault("prepare_output", "output already exists", os.ErrExist)
	} else if !errors.Is(err, os.ErrNotExist) {
		return ioFault("prepare_output", "cannot inspect output destination", err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".athena-bridge-*")
	if err != nil {
		return ioFault("create_output", "cannot create output file", err)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := write(&countingWriter{out: f}); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return ioFault("sync_output", "cannot sync output file", err)
	}
	if err := f.Close(); err != nil {
		return ioFault("close_output", "cannot close output file", err)
	}
	if err := os.Link(f.Name(), path); err != nil {
		return ioFault("publish_output", "cannot publish output without overwriting", err)
	}
	return nil
}

func downloadFile(ctx context.Context, d download, out io.Writer, maxBytes int64, hc *http.Client) (int64, error) {
	u, err := url.Parse(d.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return 0, protocolFault("download_result", "worker download requires a valid HTTPS URL without user info or fragment", err)
	}
	if d.Format != "csv" && d.Format != "parquet" && d.Format != "json" && d.Format != "jsonl" {
		return 0, protocolFault("download_result", "worker returned an unsupported download format", nil)
	}
	if d.SizeBytes != nil && *d.SizeBytes < 0 {
		return 0, protocolFault("download_result", "dataset size is invalid", nil)
	}
	if d.SizeBytes != nil && *d.SizeBytes > maxBytes {
		return 0, fault(CodeLimit, "download", "dataset size exceeds --max-bytes", RecoveryArguments, nil)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.URL, nil)
	if err != nil {
		return 0, protocolFault("download", "cannot create dataset download request", err)
	}
	safeClient := *hc
	safeClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := safeClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, fault(CodeTransport, "download", "dataset download interrupted", RecoveryRetry, ctx.Err())
		}
		return 0, fault(CodeTransport, "download", "dataset download failed; check connectivity and link expiry", RecoveryRetry, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fault(CodeTransport, "download", fmt.Sprintf("dataset download returned HTTP %d; request a new link if it expired", resp.StatusCode), RecoveryRetry, nil)
	}
	if resp.ContentLength > maxBytes {
		return 0, fault(CodeLimit, "download", "dataset exceeds --max-bytes", RecoveryArguments, nil)
	}
	n, err := io.Copy(&countingWriter{out: out}, io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return n, fault(CodeTransport, "download", "dataset download interrupted", RecoveryRetry, ctx.Err())
		}
		var writeFailure *Fault
		if errors.As(err, &writeFailure) {
			return n, err
		}
		return n, fault(CodeTransport, "download", "dataset download could not be read completely", RecoveryRetry, err)
	}
	if n > maxBytes {
		return n, fault(CodeLimit, "download", "dataset exceeds --max-bytes", RecoveryArguments, nil)
	}
	if d.SizeBytes != nil && n != *d.SizeBytes {
		return n, protocolFault("download", "dataset size differs from worker metadata", nil)
	}
	return n, nil
}

func writeStatus(out io.Writer, format string, s queryStatus, cancellationRequested *bool) error {
	if format == prettyFormat {
		return writePrettyStatus(out, s, cancellationRequested)
	}
	names := []string{"query_id", "state", "reason"}
	values := []string{s.QueryID, s.State, s.Reason}
	if stats := s.Statistics; stats != nil {
		names = append(names, "data_scanned_bytes", "engine_execution_ms", "total_execution_ms", "queue_ms", "planning_ms", "result_reused")
		for _, value := range []*string{stats.DataScannedBytes, stats.EngineExecutionMS, stats.TotalExecutionMS, stats.QueueMS, stats.PlanningMS} {
			if value == nil {
				values = append(values, "")
			} else {
				values = append(values, *value)
			}
		}
		if stats.ResultReused == nil {
			values = append(values, "")
		} else {
			values = append(values, strconv.FormatBool(*stats.ResultReused))
		}
	}
	if failure := s.Failure; failure != nil {
		names = append(names, "error_category", "error_type", "retryable")
		for _, value := range []*int64{failure.ErrorCategory, failure.ErrorType} {
			if value == nil {
				values = append(values, "")
			} else {
				values = append(values, strconv.FormatInt(*value, 10))
			}
		}
		if failure.Retryable == nil {
			values = append(values, "")
		} else {
			values = append(values, strconv.FormatBool(*failure.Retryable))
		}
	}
	if cancellationRequested != nil {
		names = append(names, "cancellation_requested")
		values = append(values, strconv.FormatBool(*cancellationRequested))
	}
	return writeRecord(out, format, names, values)
}

func writeInfo(out io.Writer, format string, info workerInfo) error {
	if format == prettyFormat {
		return writePrettyInfo(out, info)
	}
	names := []string{"worker_id", "basis", "default_database", "databases", "catalog", "workgroup", "actions", "features", "limits"}
	encoded := func(value any) string { data, _ := json.Marshal(value); return string(data) }
	values := []string{info.WorkerID, info.Basis, info.DefaultDatabase, encoded(info.Databases), info.Catalog, info.Workgroup, encoded(info.Actions), encoded(info.Features), encoded(info.Limits)}
	return writeRecord(out, format, names, values)
}
