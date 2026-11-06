package main

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

const prettyFormat = "pretty"

type rowSink interface {
	write(page) error
	finish(string) error
}

type property struct {
	name, value string
}

type prettyRows struct {
	out     io.Writer
	rows    int64
	started bool
}

func prettyRequested(args []string, flags *flag.FlagSet) bool {
	pretty := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" || !strings.HasPrefix(arg, "-") || arg == "-" {
			break
		}
		name, value, assigned := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if name == "pretty" {
			if !assigned {
				pretty = true
			} else if enabled, err := strconv.ParseBool(value); err == nil {
				pretty = enabled
			}
		}
		if option := flags.Lookup(name); option != nil && !assigned {
			boolean, ok := option.Value.(interface{ IsBoolFlag() bool })
			if !ok || !boolean.IsBoolFlag() {
				i++
			}
		}
	}
	return pretty
}

func prettyCell(cell *string) string {
	if cell == nil {
		return "NULL"
	}
	if *cell == "" || *cell == "NULL" || strings.TrimSpace(*cell) != *cell {
		return strconv.Quote(*cell)
	}
	return visible(*cell)
}

func asciiText(value string) bool {
	for _, char := range value {
		if char > 127 {
			return false
		}
	}
	return true
}

func (w *prettyRows) write(p page) error {
	if !w.started && p.QueryID != "" {
		if _, err := fmt.Fprintf(w.out, "Query %s\n\n", visible(p.QueryID)); err != nil {
			return err
		}
	}
	w.started = true
	if len(p.Rows) == 0 {
		_, err := fmt.Fprintln(w.out, "(no rows on this page)")
		return err
	}
	if len(p.Columns) == 0 {
		w.rows += int64(len(p.Rows))
		_, err := fmt.Fprintf(w.out, "(%d rows with no columns)\n", len(p.Rows))
		return err
	}
	widths := make([]int, len(p.Columns))
	expanded := false
	for i, col := range p.Columns {
		name := visible(col.Name)
		widths[i] = len(name)
		expanded = expanded || !asciiText(name)
	}
	for _, row := range p.Rows {
		for i, cell := range row {
			value := prettyCell(cell)
			widths[i] = max(widths[i], len(value))
			expanded = expanded || !asciiText(value) || widths[i] > 48
		}
	}
	width := 1
	for _, size := range widths {
		width += size + 3
	}
	if expanded || width > 120 {
		for _, row := range p.Rows {
			fields := make([]property, len(p.Columns))
			for i, col := range p.Columns {
				fields[i] = property{visible(fmt.Sprintf("[%d] %s", i+1, col.Name)), prettyCell(row[i])}
			}
			if err := writePrettyFields(w.out, fmt.Sprintf("Row %d", w.rows+1), fields); err != nil {
				return err
			}
			w.rows++
		}
		return nil
	}
	border := func(left, middle, right string) error {
		var line strings.Builder
		line.WriteString(left)
		for i, size := range widths {
			if i > 0 {
				line.WriteString(middle)
			}
			line.WriteString(strings.Repeat("─", size+2))
		}
		line.WriteString(right + "\n")
		_, err := io.WriteString(w.out, line.String())
		return err
	}
	line := func(values []string) error {
		var text strings.Builder
		text.WriteString("│")
		for i, value := range values {
			text.WriteString(" " + value + strings.Repeat(" ", widths[i]-len(value)) + " │")
		}
		text.WriteByte('\n')
		_, err := io.WriteString(w.out, text.String())
		return err
	}
	if err := border("┌", "┬", "┐"); err != nil {
		return err
	}
	names := make([]string, len(p.Columns))
	for i, col := range p.Columns {
		names[i] = visible(col.Name)
	}
	if err := line(names); err != nil {
		return err
	}
	if err := border("├", "┼", "┤"); err != nil {
		return err
	}
	for _, row := range p.Rows {
		values := make([]string, len(row))
		for i, cell := range row {
			values[i] = prettyCell(cell)
		}
		if err := line(values); err != nil {
			return err
		}
		w.rows++
	}
	return border("└", "┴", "┘")
}

func (w *prettyRows) finish(token string) error {
	noun, state := "rows", "complete"
	if w.rows == 1 {
		noun = "row"
	}
	if token != "" {
		state = "more results available"
	}
	_, err := fmt.Fprintf(w.out, "%d %s · %s\n", w.rows, noun, state)
	return err
}

func prettyLabel(value string) string {
	label := []rune(strings.ReplaceAll(value, "_", " "))
	if len(label) != 0 {
		label[0] = unicode.ToUpper(label[0])
	}
	return string(label)
}

func writePrettyRecord(out io.Writer, title string, fields []property) error {
	for i, field := range fields {
		fields[i] = property{visible(field.name), visible(field.value)}
	}
	return writePrettyFields(out, visible(title), fields)
}

func writePrettyFields(out io.Writer, title string, fields []property) error {
	var text strings.Builder
	text.WriteString(title + "\n" + strings.Repeat("─", 40) + "\n")
	width := 0
	for _, field := range fields {
		width = min(32, max(width, len(field.name)))
	}
	for _, field := range fields {
		text.WriteString("  " + field.name + strings.Repeat(" ", max(0, width-len(field.name))) + "  " + field.value + "\n")
	}
	text.WriteByte('\n')
	_, err := io.WriteString(out, text.String())
	return err
}

func writePrettyInfo(out io.Writer, info workerInfo) error {
	fields := []property{{"Worker", info.WorkerID}, {"Scope basis", info.Basis},
		{"Default database", info.DefaultDatabase}, {"Databases", strings.Join(info.Databases, ", ")},
		{"Catalog", info.Catalog}, {"Workgroup", info.Workgroup}}
	for _, action := range info.Actions {
		fields = append(fields, property{"Action", action})
	}
	for _, feature := range info.Features {
		fields = append(fields, property{"Feature", feature})
	}
	names := make([]string, 0, len(info.Limits))
	for name := range info.Limits {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := strconv.FormatInt(info.Limits[name], 10)
		if strings.HasSuffix(name, "_bytes") {
			value = prettyBytes(info.Limits[name])
		}
		fields = append(fields, property{prettyLabel(name), value})
	}
	return writePrettyRecord(out, "Worker capabilities", fields)
}

func prettyBytes(value int64) string {
	for _, unit := range []struct {
		name  string
		bytes int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if value >= unit.bytes && value%unit.bytes == 0 {
			return fmt.Sprintf("%d %s (%d bytes)", value/unit.bytes, unit.name, value)
		}
	}
	return strconv.FormatInt(value, 10) + " bytes"
}

func writePrettyStatus(out io.Writer, s queryStatus, cancellation *bool) error {
	fields := []property{{"Query ID", s.QueryID}, {"State", s.State}}
	if s.Reason != "" {
		fields = append(fields, property{"Reason", s.Reason})
	}
	if stats := s.Statistics; stats != nil {
		for _, field := range []struct {
			name  string
			value *string
		}{{"Data scanned bytes", stats.DataScannedBytes}, {"Engine execution ms", stats.EngineExecutionMS},
			{"Total execution ms", stats.TotalExecutionMS}, {"Queue ms", stats.QueueMS}, {"Planning ms", stats.PlanningMS}} {
			if field.value != nil {
				fields = append(fields, property{field.name, *field.value})
			}
		}
		if stats.ResultReused != nil {
			fields = append(fields, property{"Result reused", strconv.FormatBool(*stats.ResultReused)})
		}
	}
	if failure := s.Failure; failure != nil {
		for _, field := range []struct {
			name  string
			value *int64
		}{{"Error category", failure.ErrorCategory}, {"Error type", failure.ErrorType},
			{"Sql line", failure.SQLLine}, {"Sql column", failure.SQLColumn}} {
			if field.value != nil {
				fields = append(fields, property{field.name, strconv.FormatInt(*field.value, 10)})
			}
		}
		if failure.Retryable != nil {
			fields = append(fields, property{"Retryable", strconv.FormatBool(*failure.Retryable)})
		}
	}
	if cancellation != nil {
		fields = append(fields, property{"Cancellation requested", strconv.FormatBool(*cancellation)})
	}
	return writePrettyRecord(out, "Query status", fields)
}

func writePrettyDiagnostic(out io.Writer, event diagnosticEvent) error {
	fields := []property{{"Message", event.Message}}
	for _, field := range []property{{"Request ID", event.RequestID}, {"Query ID", event.QueryID},
		{"Worker code", event.WorkerCode}} {
		if field.value != "" {
			fields = append(fields, field)
		}
	}
	if event.Type == "error" {
		fields = append(fields, property{"Exit code", strconv.Itoa(event.ExitCode)},
			property{"Partial output", strconv.FormatBool(event.PartialOutput)}, property{"Published", strconv.FormatBool(event.Published)})
	}
	if event.Recovery != RecoveryNone {
		fields = append(fields, property{"Recovery", string(event.Recovery)})
	}
	if err := writePrettyRecord(out, strings.ToUpper(event.Type)+" ["+string(event.Code)+"] "+event.Operation, fields); err != nil {
		return err
	}
	if event.NextToken != "" {
		_, err := fmt.Fprintf(out, "  Next token (Go string)  %s\n\n", strconv.QuoteToASCII(event.NextToken))
		return err
	}
	return nil
}

func writePrettyExport(out io.Writer, receipt exportReceipt) error {
	return writePrettyRecord(out, "Export", []property{{"Query ID", receipt.QueryID}, {"Result ID", receipt.ResultID},
		{"Path", receipt.Path}, {"Checkpoint", receipt.Checkpoint}, {"Format", receipt.Format},
		{"Rows", strconv.FormatInt(receipt.Rows, 10)}, {"Bytes", strconv.FormatInt(receipt.Bytes, 10)},
		{"SHA256", receipt.SHA256}, {"Published", strconv.FormatBool(receipt.Published)}})
}
