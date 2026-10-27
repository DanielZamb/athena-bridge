package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	hashpkg "hash"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Each offset describes synced encoder bytes. finishing excludes the trailer;
// sealed includes it. The source inode survives until publication is recorded.
type exportState struct {
	Version     int        `json:"version"`
	Integrity   string     `json:"integrity"`
	Encoding    string     `json:"encoding"`
	Function    string     `json:"function"`
	Region      string     `json:"region"`
	QueryID     string     `json:"query_id"`
	ResultID    string     `json:"result_id"`
	Destination string     `json:"destination"`
	Data        string     `json:"data"`
	Format      string     `json:"format"`
	PageSize    int        `json:"page_size"`
	Columns     []column   `json:"columns"`
	Offset      int64      `json:"offset"`
	Rows        int64      `json:"rows"`
	Hash        string     `json:"sha256"`
	Cursor      string     `json:"next_token"`
	Phase       string     `json:"phase"`
	Started     bool       `json:"started"`
	Cycle       tokenCycle `json:"cycle"`
}

type exportReceipt struct {
	QueryID    string `json:"query_id"`
	ResultID   string `json:"result_id"`
	Path       string `json:"path"`
	Checkpoint string `json:"checkpoint"`
	Format     string `json:"format"`
	Bytes      int64  `json:"bytes"`
	Rows       int64  `json:"rows"`
	SHA256     string `json:"sha256"`
	Published  bool   `json:"published"`
}

// Brent's cycle detector keeps pagination memory independent of the page count.
type tokenCycle struct {
	Anchor string `json:"anchor"`
	Power  uint64 `json:"power"`
	Length uint64 `json:"length"`
}

func (c *tokenCycle) advance(token string) error {
	if token == "" {
		return nil
	}
	c.Length++
	if token == c.Anchor {
		return protocolFault("get_results", "worker repeated a pagination token", nil)
	}
	if c.Length >= c.Power {
		c.Anchor, c.Length = token, 0
		if c.Power == 0 {
			c.Power = 1
		} else if c.Power < 1<<63 {
			c.Power *= 2
		}
	}
	return nil
}

func exportPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", ioFault("export", "cannot resolve export path", err)
	}
	// Resolve directory aliases so cooperating exporters share the same lock.
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", ioFault("export", "cannot resolve export directory", err)
	}
	return filepath.Join(parent, filepath.Base(absolute)), nil
}

func loadExport(path string) (exportState, error) {
	var s exportState
	f, err := openExportFile(path, os.O_RDONLY)
	if err != nil {
		return s, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 8<<20+1))
	if err != nil {
		return s, ioFault("checkpoint", "cannot read checkpoint", err)
	}
	if len(data) > 8<<20 || decodeJSON(data, &s) != nil {
		return s, ioFault("checkpoint", "invalid export checkpoint", nil)
	}
	if s.Version != 2 {
		return s, ioFault("checkpoint", "unsupported export checkpoint version; expected 2", nil)
	}
	integrity, err := exportIntegrity(s)
	if err != nil || s.Integrity != integrity {
		return s, ioFault("checkpoint", "export checkpoint metadata checksum mismatch", err)
	}
	digest, hashErr := hex.DecodeString(s.Hash)
	validPhase := s.Phase == "writing" || s.Phase == "finishing" || s.Phase == "sealed" || s.Phase == "published"
	if s.Encoding != "result-writer-v1" || s.Function == "" || s.Region == "" || s.QueryID == "" ||
		(s.Format != "json" && s.Format != "csv") || s.PageSize < 1 || s.PageSize > 1000 || s.Offset < 0 || s.Rows < 0 ||
		hashErr != nil || len(digest) != sha256.Size || !validPhase || !filepath.IsAbs(s.Data) || !filepath.IsAbs(s.Destination) ||
		filepath.Dir(s.Data) != filepath.Dir(s.Destination) || s.Data == s.Destination || s.Data == path || s.Data == path+".lock" || s.Destination == path || s.Destination == path+".lock" ||
		filepath.Clean(s.Data) != s.Data || filepath.Clean(s.Destination) != s.Destination || !strings.HasPrefix(filepath.Base(s.Data), ".athena-export-") ||
		(s.Started && (s.ResultID == "" || s.Columns == nil)) || (!s.Started && (s.Offset != 0 || s.Rows != 0 || s.Phase != "writing" || s.Cursor != "" || s.ResultID != "" || s.Columns != nil)) ||
		(s.Started && s.Phase == "writing" && s.Cursor == "") || (s.Phase != "writing" && (!s.Started || s.Cursor != "")) {
		return s, ioFault("checkpoint", "invalid export checkpoint", nil)
	}
	if s.Started {
		if err := validatePage(page{Columns: s.Columns, Rows: [][]*string{}}); err != nil {
			return s, ioFault("checkpoint", "invalid checkpoint schema", err)
		}
	}
	return s, nil
}

func openExportFile(path string, flags int) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, ioFault("export", "cannot inspect export file", err)
	}
	if !before.Mode().IsRegular() {
		return nil, ioFault("export", "export file must be a regular file", nil)
	}
	f, err := os.OpenFile(path, flags, 0600)
	if err != nil {
		return nil, ioFault("export", "cannot open export file", err)
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		f.Close()
		return nil, ioFault("export", "export file changed while opening", err)
	}
	return f, nil
}

// This unkeyed checksum detects corruption of every state field, including the
// data digest. It does not authenticate checkpoints against same-user changes.
func exportIntegrity(s exportState) (string, error) {
	s.Integrity = ""
	data, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func saveExport(path string, s *exportState) error {
	integrity, err := exportIntegrity(*s)
	if err != nil {
		return ioFault("checkpoint", "cannot checksum checkpoint metadata", err)
	}
	s.Integrity = integrity
	f, err := os.CreateTemp(filepath.Dir(path), ".athena-checkpoint-*")
	if err != nil {
		return ioFault("checkpoint", "cannot create checkpoint", err)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = json.NewEncoder(f).Encode(s); err == nil {
		var info os.FileInfo
		info, err = f.Stat()
		if err == nil && info.Size() > 8<<20 {
			err = fault(CodeLimit, "checkpoint", "checkpoint exceeds the 8 MiB limit", RecoveryWorker, nil)
		}
		if err == nil {
			err = f.Sync()
		}
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = replaceExportState(f.Name(), path)
	}
	if err == nil {
		err = syncExportDir(filepath.Dir(path))
	}
	if err != nil {
		return ioFault("checkpoint", "cannot commit checkpoint", err)
	}
	return nil
}

func verifyExport(ctx context.Context, f *os.File, s *exportState, exact bool) (hashpkg.Hash, error) {
	h := sha256.New()
	info, err := f.Stat()
	if err != nil {
		return nil, ioFault("export", "cannot inspect export bytes", err)
	}
	if info.Size() < s.Offset || (exact && info.Size() != s.Offset) {
		return nil, ioFault("export", "export byte count differs from checkpoint", nil)
	}
	if _, err = io.CopyN(h, exportReader{ctx, f}, s.Offset); err != nil {
		return nil, ioFault("export", "cannot verify export bytes", err)
	}
	if hex.EncodeToString(h.Sum(nil)) != s.Hash {
		return nil, ioFault("export", "export checksum differs from checkpoint", nil)
	}
	return h, nil
}

func (c *client) export(ctx context.Context, path string, s *exportState, d *diagnostics) (exportReceipt, error) {
	var receipt exportReceipt
	d.queryID = s.QueryID
	d.published = s.Phase == "published"
	d.partialOutput = s.Offset > 0 && !d.published
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	if c.function != s.Function || c.region != s.Region {
		return receipt, fault(CodeConfig, "export", "checkpoint worker target differs from configured target", RecoveryConfig, nil)
	}
	dataPath := s.Data
	if s.Phase == "published" {
		dataPath = s.Destination
	}
	flags := os.O_RDWR
	if s.Phase == "published" {
		flags = os.O_RDONLY
	}
	f, err := openExportFile(dataPath, flags)
	if err != nil {
		return receipt, err
	}
	defer f.Close()
	h, err := verifyExport(ctx, f, s, s.Phase == "sealed" || s.Phase == "published")
	if err != nil {
		return receipt, err
	}
	if s.Phase == "published" {
		d.published, d.partialOutput = true, false
	} else {
		if err := f.Truncate(s.Offset); err != nil {
			return receipt, ioFault("export", "cannot discard uncommitted export bytes", err)
		}
		writer := resultWriter{out: io.MultiWriter(&countingWriter{out: f}, h), format: s.Format, started: s.Started, rows: s.Rows}
		if s.Phase == "writing" {
			req := request{Action: "get_results", QueryID: s.QueryID, PageSize: s.PageSize, NextToken: s.Cursor, ExpectedResultID: s.ResultID}
			// Only one expired-cursor replay per invocation, so an invalid worker cannot loop.
			replayed := false
			for {
				if err := ctx.Err(); err != nil {
					return receipt, err
				}
				p, err := c.fetchPage(ctx, req, s.Columns)
				if err != nil && req.NextToken != "" && !replayed && expiredExportCursor(err) {
					p, err = c.replayExport(ctx, s)
					replayed = true
					s.Cycle = tokenCycle{}
				}
				if err != nil {
					return receipt, err
				}
				if p.ResultID == "" {
					return receipt, fault(CodeResultChanged, "export", "export requires a stable worker result_id", RecoveryWorker, nil)
				}
				if err := s.Cycle.advance(p.NextToken); err != nil {
					return receipt, err
				}
				if err := writer.write(p); err != nil {
					return receipt, err
				}
				d.usage.rows += int64(len(p.Rows))
				d.usage.pages++
				s.Columns, s.ResultID, s.Started, s.Rows, s.Cursor = p.Columns, p.ResultID, true, writer.rows, p.NextToken
				if p.NextToken == "" {
					s.Phase = "finishing"
				}
				if err := commitExportBytes(f, h, path, s); err != nil {
					return receipt, err
				}
				d.partialOutput = true
				if s.Phase == "finishing" {
					break
				}
				req.NextToken, req.ExpectedResultID = s.Cursor, s.ResultID
			}
		}
		if s.Phase == "finishing" {
			if err := writer.finish(""); err != nil {
				return receipt, ioFault("export", "cannot finish export", err)
			}
			s.Phase = "sealed"
			if err := commitExportBytes(f, h, path, s); err != nil {
				return receipt, err
			}
		}
		if err := f.Close(); err != nil {
			return receipt, ioFault("export", "cannot close export data", err)
		}
		if err := os.Link(s.Data, s.Destination); err != nil {
			source, sourceErr := os.Stat(s.Data)
			dest, destErr := os.Lstat(s.Destination)
			if sourceErr != nil || destErr != nil || !dest.Mode().IsRegular() || !os.SameFile(source, dest) {
				return receipt, ioFault("publish_export", "cannot publish export without overwriting", err)
			}
		}
		d.published, d.partialOutput = true, false
		if err := syncExportDir(filepath.Dir(s.Destination)); err != nil {
			return receipt, ioFault("publish_export", "cannot sync published export", err)
		}
		s.Phase = "published"
		if err := saveExport(path, s); err != nil {
			return receipt, err
		}
	}
	// A retained published checkpoint makes retries after a lost receipt idempotent.
	source, sourceErr := os.Lstat(s.Data)
	destination, destErr := os.Lstat(s.Destination)
	if sourceErr == nil && destErr == nil && os.SameFile(source, destination) {
		_ = os.Remove(s.Data)
	}
	receipt = exportReceipt{s.QueryID, s.ResultID, s.Destination, path, s.Format, s.Offset, s.Rows, s.Hash, true}
	return receipt, d.notice("OUTPUT_PUBLISHED", "export file published", RecoveryNone)
}

func commitExportBytes(f *os.File, h hashpkg.Hash, path string, s *exportState) error {
	if err := f.Sync(); err != nil {
		return ioFault("export", "cannot sync export data", err)
	}
	offset, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return ioFault("export", "cannot read export position", err)
	}
	s.Offset, s.Hash = offset, hex.EncodeToString(h.Sum(nil))
	return saveExport(path, s)
}

func expiredExportCursor(err error) bool {
	var worker *workerError
	if !errors.As(err, &worker) {
		return false
	}
	switch worker.Code {
	case "INVALID_CURSOR", "EXPIRED_CURSOR", "CURSOR_EXPIRED", "INVALID_TOKEN", "EXPIRED_TOKEN", "TOKEN_EXPIRED":
		return true
	}
	return false
}

// Replay compares encoder bytes for the delivered logical row prefix, independent
// of new page boundaries. The returned page contains only undelivered rows.
func (c *client) replayExport(ctx context.Context, s *exportState) (page, error) {
	h := sha256.New()
	count := &countingWriter{out: h}
	writer := resultWriter{out: count, format: s.Format}
	req := request{Action: "get_results", QueryID: s.QueryID, PageSize: s.PageSize, ExpectedResultID: s.ResultID}
	remaining := s.Rows
	cycle := tokenCycle{}
	for {
		if err := ctx.Err(); err != nil {
			return page{}, err
		}
		p, err := c.fetchPage(ctx, req, s.Columns)
		if err != nil {
			return page{}, err
		}
		if err := cycle.advance(p.NextToken); err != nil {
			return page{}, err
		}
		n := int64(len(p.Rows))
		if n > remaining {
			n = remaining
		}
		prefix := p
		prefix.Rows = p.Rows[:n]
		if err := writer.write(prefix); err != nil {
			return page{}, err
		}
		remaining -= n
		if remaining == 0 {
			if count.n != s.Offset || hex.EncodeToString(h.Sum(nil)) != s.Hash {
				return page{}, fault(CodeResultChanged, "export", "replayed result differs from delivered prefix", RecoveryInspect, nil)
			}
			p.Rows = p.Rows[n:]
			return p, nil
		}
		if p.NextToken == "" {
			return page{}, fault(CodeResultChanged, "export", "replayed result is shorter than delivered prefix", RecoveryInspect, nil)
		}
		req.NextToken = p.NextToken
	}
}

func runExport(ctx context.Context, target settings, id, destination, format, checkpoint, resume string, out io.Writer, d *diagnostics, factory func(context.Context, settings) (*client, error)) error {
	if resume != "" {
		checkpoint = resume
	} else if checkpoint == "" {
		checkpoint = destination + ".checkpoint.json"
	}
	path, err := exportPath(checkpoint)
	if err != nil {
		return err
	}
	lock, err := lockExport(path + ".lock")
	if err != nil {
		return ioFault("export_lock", "cannot acquire exclusive export lock", err)
	}
	defer lock.Close()
	var s exportState
	if resume != "" {
		s, err = loadExport(path)
		if err != nil {
			return err
		}
		d.queryID = s.QueryID
		d.usage.format = map[string]int{"json": 2, "csv": 3}[s.Format]
		d.usage.flags |= 1 | 4
		d.published = s.Phase == "published"
		d.partialOutput = s.Offset > 0 && !d.published
		target.Function, target.Region = s.Function, s.Region
	} else {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return ioFault("checkpoint", "checkpoint already exists or cannot be inspected; use --resume", err)
		}
	}
	c, err := factory(ctx, target)
	if err != nil {
		var typed *Fault
		if errors.As(err, &typed) {
			return err
		}
		return fault(CodeConfig, "configure", "cannot configure AWS client", RecoveryConfig, err)
	}
	if resume == "" {
		if c.function == "" || c.region == "" {
			return fault(CodeConfig, "export", "export requires a resolved worker function and region", RecoveryConfig, nil)
		}
		destination, err = exportPath(destination)
		if err != nil {
			return err
		}
		if destination == path || destination == path+".lock" {
			return fault(CodeUsage, "export", "output and checkpoint paths must be distinct", RecoveryArguments, nil)
		}
		if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
			return ioFault("export", "output already exists or cannot be inspected", err)
		}
		f, err := os.CreateTemp(filepath.Dir(destination), ".athena-export-*")
		if err != nil {
			return ioFault("export", "cannot create export data", err)
		}
		s = exportState{Version: 2, Encoding: "result-writer-v1", Function: c.function, Region: c.region, QueryID: id, Destination: destination, Data: f.Name(), Format: format, PageSize: 200, Hash: hex.EncodeToString(sha256.New().Sum(nil)), Phase: "writing"}
		err = f.Sync()
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err == nil {
			err = syncExportDir(filepath.Dir(destination))
		}
		if err != nil {
			os.Remove(s.Data)
			return ioFault("export", "cannot prepare durable export data", err)
		}
		if err := saveExport(path, &s); err != nil {
			return err
		}
	}
	receipt, err := c.export(ctx, path, &s, d)
	if err != nil {
		return err
	}
	if d.pretty {
		return writePrettyExport(out, receipt)
	}
	return json.NewEncoder(out).Encode(receipt)
}

func openExportLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if errors.Is(err, os.ErrExist) {
		return openExportFile(path, os.O_RDWR)
	}
	return f, err
}

type exportReader struct {
	ctx context.Context
	io.Reader
}

func (r exportReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}
