package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const usage = `athena-bridge invokes an Athena worker through signed AWS Lambda requests.

Usage: athena-bridge <command> [flags]

Commands:
  info       Read configured worker capabilities
  search     Search table and column names and descriptions
  cancel     Request cancellation of an existing query
  tables     List tables in a database
  describe   Describe a table's columns and partitions
  query      Start a query, wait, and display its results
  start      Start a query and return its query ID
  status     Read a query's state
  results    Retrieve an existing query's results
  export     Export all rows with durable resume checkpoints
  download   Save a complete dataset through an expiring HTTPS link
  version    Print the CLI version

Common flags (after the command):
  --function NAME   Lambda name, ARN, or alias; or ATHENA_BRIDGE_FUNCTION
  --profile NAME    AWS shared profile; defaults to the SDK credential chain
  --region REGION   AWS region; defaults to AWS environment/profile settings
  --format FORMAT   table (default), json, or csv
  --pretty          Human-readable console output and diagnostics
  --timeout 2m      Overall command deadline

Examples:
  athena-bridge tables --database analytics --format json
  athena-bridge describe --database analytics --table events
  athena-bridge query --database analytics --sql-file query.sql --format json
  athena-bridge results --query-id ID --all --format csv --out data/results.csv
  athena-bridge export --query-id ID --out data/results.csv --format csv
  athena-bridge export --resume data/results.csv.checkpoint.json
  athena-bridge download --query-id ID --out data/results.csv

Run a command with --help for its flags. No command deploys AWS resources.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, newClient)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, in io.Reader, out, diagnosticsOut io.Writer, factory func(context.Context, settings) (*client, error)) (code int) {
	d := &diagnostics{out: diagnosticsOut, operation: "command"}
	d.usage = usageRecord{started: time.Now(), command: usageCommand(args), profile: os.Getenv("AWS_PROFILE")}
	if d.usage.profile == "" {
		d.usage.profile = os.Getenv("AWS_DEFAULT_PROFILE")
	}
	defer func() { d.usage.finish(in, out, d.queryID, code) }()
	output := &countingWriter{out: out}
	err := execute(ctx, args, in, output, d, factory)
	if err == nil || errors.Is(err, flag.ErrHelp) {
		if output.err == nil {
			return 0
		}
		err = output.err
	}
	d.partialOutput = d.partialOutput || output.n > 0
	return d.failure(err)
}

func execute(ctx context.Context, args []string, in io.Reader, out io.Writer, d *diagnostics, factory func(context.Context, settings) (*client, error)) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := io.WriteString(out, usage)
		return err
	}
	command := args[0]
	if command == "version" {
		_, err := fmt.Fprintln(out, "athena-bridge "+version)
		return err
	}
	switch command {
	case "tables", "describe", "query", "start", "status", "results", "download", "info", "search", "cancel", "export":
	default:
		return fault(CodeUsage, "command", "unknown command; run athena-bridge --help", RecoveryArguments, nil)
	}
	d.operation = command
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	var s settings
	f.StringVar(&s.Function, "function", os.Getenv("ATHENA_BRIDGE_FUNCTION"), "Lambda name, ARN, or alias")
	f.StringVar(&s.Region, "region", "", "AWS region; otherwise use SDK environment/profile settings")
	f.StringVar(&s.Profile, "profile", "", "AWS shared profile; otherwise use the SDK credential chain")
	format := f.String("format", "table", "output: table, json, or csv")
	pretty := f.Bool("pretty", false, "readable console output and diagnostics; --format still controls files")
	timeout := f.Duration("timeout", 2*time.Minute, "overall command deadline")
	var database, table, sql, sqlFile, id, token, outFile, retryID, term, kind string
	var checkpoint, resume string
	var all, wait bool
	var pageSize int
	var interval time.Duration
	var maxBytes int64
	if command == "tables" || command == "describe" || command == "query" || command == "start" || command == "search" {
		f.StringVar(&database, "database", "", "Athena database; otherwise the worker's configured default")
	}
	if command == "describe" {
		f.StringVar(&table, "table", "", "table to describe (required)")
	} else if command == "search" {
		f.StringVar(&table, "table", "", "restrict search to an exact table name")
	}
	if command == "search" {
		f.StringVar(&term, "term", "", "literal name or description text (required)")
		f.StringVar(&kind, "kind", "all", "search all, table, or column metadata")
	}
	if command == "query" || command == "start" {
		f.StringVar(&sql, "sql", "", "SQL statement; use --sql-file to avoid shell history")
		f.StringVar(&sqlFile, "sql-file", "", "read SQL from a file, or - for standard input")
		f.StringVar(&retryID, "request-id", "", "stable 32-128 character ID; reuse with identical SQL after an uncertain start")
	}
	if command == "query" || command == "results" {
		f.DurationVar(&interval, "poll-interval", time.Second, "interval between status requests")
	}
	if command == "results" {
		f.BoolVar(&wait, "wait", false, "wait for completion before retrieving results")
	}
	if command == "results" || command == "status" || command == "download" || command == "cancel" {
		f.StringVar(&id, "query-id", "", "existing query ID (required)")
	}
	if command == "tables" || command == "describe" || command == "query" || command == "results" || command == "search" {
		f.BoolVar(&all, "all", false, "retrieve all result pages")
		f.IntVar(&pageSize, "page-size", 200, "rows per worker page, from 1 to 1000")
		f.StringVar(&token, "next-token", "", "opaque token from a previous page")
		f.StringVar(&outFile, "out", "", "save output to a new file; never overwrite")
	}
	if command == "export" {
		f.Lookup("format").Usage = "file encoding: json or csv (required); stdout is a JSON receipt"
		f.StringVar(&id, "query-id", "", "existing query ID")
		f.StringVar(&outFile, "out", "", "destination file; never overwrite")
		f.StringVar(&checkpoint, "checkpoint", "", "checkpoint path; defaults to OUT.checkpoint.json")
		f.StringVar(&resume, "resume", "", "resume a saved export checkpoint")
	}
	if command == "download" {
		f.Lookup("format").Usage = "receipt output: table, json, or csv; downloaded file stays unchanged"
		f.StringVar(&outFile, "out", "", "destination file (required); never overwrite")
		f.Int64Var(&maxBytes, "max-bytes", 1<<30, "maximum download size in bytes")
	}
	f.Usage = func() {}
	d.pretty = prettyRequested(args[1:], f)
	parseErr := f.Parse(args[1:])
	if s.Profile != "" {
		d.usage.profile = s.Profile
	}
	d.usage.database = database
	d.usage.format = map[string]int{"table": 1, "json": 2, "csv": 3}[*format]
	for bit, set := range []bool{outFile != "", *pretty, all, wait, resume != "", sqlFile != "", token != "", retryID != ""} {
		if set {
			d.usage.flags |= 1 << bit
		}
	}
	d.usage.tabular = command == "tables" || command == "describe" || command == "query" || command == "results" || command == "search" || command == "export"
	if command == "export" {
		d.usage.flags |= 1 | 4
	}
	d.usage.sqlIntent(sql)
	d.queryID = id
	d.requestID = retryID
	if err := parseErr; err != nil {
		if errors.Is(err, flag.ErrHelp) {
			if _, writeErr := fmt.Fprintf(out, "Usage: athena-bridge %s [flags]\n\n", command); writeErr != nil {
				return writeErr
			}
			f.SetOutput(out)
			f.PrintDefaults()
			return err
		}
		return fault(CodeUsage, command, "cannot parse flags; run the command with --help", RecoveryArguments, err)
	}
	d.pretty = *pretty
	bad := func(message string) error { return fault(CodeUsage, command, message, RecoveryArguments, nil) }
	if f.NArg() != 0 {
		return bad("unexpected positional arguments; flags must follow the command")
	}
	if command == "export" {
		if *timeout <= 0 {
			return bad("--timeout must be positive")
		}
		if resume != "" {
			conflict := false
			f.Visit(func(flag *flag.Flag) {
				if flag.Name != "resume" && flag.Name != "profile" && flag.Name != "timeout" && flag.Name != "pretty" {
					conflict = true
				}
			})
			if conflict {
				return bad("--resume accepts only --profile, --timeout, and --pretty overrides")
			}
		} else if strings.TrimSpace(id) == "" || outFile == "" || (*format != "json" && *format != "csv") || strings.TrimSpace(s.Function) == "" {
			return bad("export requires --query-id, --out, --format json|csv, and a configured function")
		}
		ctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		return runExport(ctx, s, id, outFile, *format, checkpoint, resume, out, d, factory)
	}
	if strings.TrimSpace(s.Function) == "" {
		return bad("set --function or ATHENA_BRIDGE_FUNCTION")
	}
	if *format != "table" && *format != "json" && *format != "csv" {
		return bad("--format must be table, json, or csv")
	}
	consoleFormat := *format
	if d.pretty {
		consoleFormat = prettyFormat
	}
	if *timeout <= 0 {
		return bad("--timeout must be positive")
	}
	paged := command == "tables" || command == "describe" || command == "query" || command == "results" || command == "search"
	if paged && (pageSize < 1 || pageSize > 1000) {
		return bad("--page-size must be between 1 and 1000")
	}
	if (command == "query" || command == "results") && interval <= 0 {
		return bad("--poll-interval must be positive")
	}
	if (command == "status" || command == "results" || command == "download" || command == "cancel") && strings.TrimSpace(id) == "" {
		return bad("--query-id is required")
	}
	if command == "describe" && strings.TrimSpace(table) == "" {
		return bad("--table is required")
	}
	if command == "search" && (strings.TrimSpace(term) == "" || (kind != "all" && kind != "table" && kind != "column")) {
		return bad("search requires --term and --kind all, table, or column")
	}
	if command == "download" && (outFile == "" || maxBytes <= 0 || maxBytes == math.MaxInt64) {
		return bad("download requires --out and a positive --max-bytes smaller than the maximum int64")
	}
	if outFile != "" {
		parent := filepath.Dir(outFile)
		info, err := os.Stat(parent)
		if err != nil {
			return ioFault("prepare_output", "cannot access output directory", err)
		}
		if !info.IsDir() {
			return ioFault("prepare_output", "output parent is not a directory", nil)
		}
		if _, err := os.Lstat(outFile); err == nil {
			return ioFault("prepare_output", "output already exists", os.ErrExist)
		} else if !errors.Is(err, os.ErrNotExist) {
			return ioFault("prepare_output", "cannot inspect output destination", err)
		}
	}
	if command == "query" || command == "start" {
		if (sql == "") == (sqlFile == "") {
			return bad("provide exactly one of --sql or --sql-file")
		}
		if sqlFile != "" {
			reader := in
			if sqlFile != "-" {
				file, err := os.Open(sqlFile)
				if err != nil {
					return ioFault("read_sql", "cannot open SQL file", err)
				}
				defer file.Close()
				reader = file
			}
			data, err := io.ReadAll(io.LimitReader(reader, 262145))
			if err != nil {
				return ioFault("read_sql", "cannot read SQL", err)
			}
			sql = string(data)
			d.usage.sqlIntent(sql)
		}
		if strings.TrimSpace(sql) == "" || len(sql) > 262144 {
			return bad("SQL must be nonempty and no larger than 256 KiB")
		}
		var err error
		retryID, err = requestID(retryID)
		if err != nil {
			return err
		}
		d.requestID = retryID
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	c, err := factory(ctx, s)
	if err != nil {
		var typed *Fault
		if errors.As(err, &typed) {
			return err
		}
		return fault(CodeConfig, "configure", "cannot configure AWS client", RecoveryConfig, err)
	}
	if command == "query" || command == "start" {
		d.requestID = retryID
		if err := d.notice("START_REQUEST", "submitting query with a stable request ID", RecoverySameStart); err != nil {
			return err
		}
		d.startPending = true
		q, err := c.start(ctx, request{Action: "start_query", RequestID: retryID, Database: database, SQL: sql})
		if err != nil {
			if definiteStartRejection(err) {
				d.startPending = false
			}
			return err
		}
		d.startPending = false
		id = q.QueryID
		d.queryID = id
		if err := d.notice("QUERY_STARTED", "query accepted; retrieve it with status or results", RecoveryInspect); err != nil {
			return err
		}
		if command == "start" {
			if consoleFormat == "json" {
				return json.NewEncoder(out).Encode(q)
			}
			return writeRecord(out, consoleFormat, []string{"query_id"}, []string{id})
		}
		if err := c.wait(ctx, id, interval); err != nil {
			return err
		}
	}
	if command == "info" {
		info, err := c.info(ctx)
		if err != nil {
			return err
		}
		if consoleFormat == "json" {
			return json.NewEncoder(out).Encode(info)
		}
		return writeInfo(out, consoleFormat, info)
	}
	if command == "status" || command == "cancel" {
		if command == "cancel" {
			status, err := c.cancel(ctx, id)
			if err != nil {
				return err
			}
			if *status.CancellationRequested {
				if err := d.notice("CANCELLATION_REQUESTED", "cancellation requested; the observed state may still be running", RecoveryInspect); err != nil {
					return err
				}
			}
			if consoleFormat == "json" {
				return json.NewEncoder(out).Encode(status)
			}
			return writeStatus(out, consoleFormat, status.queryStatus, status.CancellationRequested)
		}
		status, err := c.status(ctx, id)
		if err != nil {
			return err
		}
		if consoleFormat == "json" {
			return json.NewEncoder(out).Encode(status)
		}
		return writeStatus(out, consoleFormat, status, nil)
	}
	if command == "download" {
		var dataset download
		var size int64
		err := atomicOutput(outFile, func(w io.Writer) error {
			if err := c.call(ctx, request{Action: "download_result", QueryID: id}, &dataset); err != nil {
				return err
			}
			var err error
			size, err = downloadFile(ctx, dataset, w, maxBytes, http.DefaultClient)
			return err
		})
		if err != nil {
			return err
		}
		d.published = true
		if err := d.notice("OUTPUT_PUBLISHED", "output file published", RecoveryNone); err != nil {
			return err
		}
		if consoleFormat == "json" {
			return json.NewEncoder(out).Encode(struct {
				Path   string `json:"path"`
				Format string `json:"format"`
				Bytes  int64  `json:"bytes"`
			}{outFile, dataset.Format, size})
		}
		return writeRecord(out, consoleFormat, []string{"path", "format", "bytes"}, []string{outFile, dataset.Format, fmt.Sprint(size)})
	}
	if command == "results" && wait {
		if err := c.wait(ctx, id, interval); err != nil {
			return err
		}
	}
	action := "get_results"
	if command == "tables" {
		action = "list_tables"
	} else if command == "describe" {
		action = "describe_table"
	} else if command == "search" {
		action = "search_metadata"
	}
	req := request{Action: action, Database: database, Table: table, QueryID: id, PageSize: pageSize, NextToken: token, Term: term, Kind: kind}
	outputFormat := consoleFormat
	if outFile != "" {
		outputFormat = *format
	}
	write := func(w io.Writer) error { return c.pages(ctx, req, all, outputFormat, w, d) }
	if outFile != "" {
		if err := atomicOutput(outFile, write); err != nil {
			return err
		}
		d.published = true
		return d.notice("OUTPUT_PUBLISHED", "output file published", RecoveryNone)
	}
	return write(out)
}

func writeRecord(out io.Writer, format string, names, values []string) error {
	if format == prettyFormat {
		fields := make([]property, len(names))
		for i, name := range names {
			fields[i] = property{prettyLabel(name), values[i]}
		}
		return writePrettyRecord(out, "Result", fields)
	}
	p := page{Columns: make([]column, len(names)), Rows: make([][]*string, 1)}
	p.Rows[0] = make([]*string, len(values))
	for i := range names {
		p.Columns[i] = column{Name: names[i], Type: "varchar"}
		p.Rows[0][i] = &values[i]
	}
	w := resultWriter{out: out, format: format}
	if err := w.write(p); err != nil {
		return err
	}
	return w.finish("")
}
