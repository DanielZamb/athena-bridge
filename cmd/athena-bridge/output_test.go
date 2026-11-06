package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCSVPreservesNullEmptyBackslashAndLargeNumbers(t *testing.T) {
	var out bytes.Buffer
	w := resultWriter{out: &out, format: "csv"}
	err := w.write(page{Columns: []column{{"value", "varchar"}}, Rows: [][]*string{{nil}, {str("")}, {str(`\N`)}, {str(`a\b`)}, {str("9007199254740993")}, {str("a,b\nc")}}})
	if err != nil {
		t.Fatal(err)
	}
	expected := "value\n\\N\n\"\"\n\\\\N\na\\\\b\n9007199254740993\n\"a,b\nc\"\n"
	if out.String() != expected {
		t.Fatalf("CSV = %q; want %q", out.String(), expected)
	}
	rows, err := csv.NewReader(strings.NewReader(out.String())).ReadAll()
	if err != nil || len(rows) != 7 || rows[2][0] != "" {
		t.Fatalf("CSV lost the empty row: %v, %v", rows, err)
	}
}

func TestDownloadWritesCompleteDatasetAndRejectsRedirect(t *testing.T) {
	dataset := "id,value\n1,alpha\n2,beta\n"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Amz-Security-Token") != "" {
			t.Error("AWS credentials forwarded to download host")
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/file", 302)
			return
		}
		io.WriteString(w, dataset)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "dataset.csv")
	size := int64(len(dataset))
	if err := atomicOutput(path, func(out io.Writer) error {
		_, err := downloadFile(context.Background(), download{URL: server.URL + "/file?token=fake-bearer", Format: "csv", SizeBytes: &size}, out, 1024, server.Client())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != dataset {
		t.Fatalf("downloaded %q", data)
	}
	var out bytes.Buffer
	_, err := downloadFile(context.Background(), download{URL: server.URL + "/redirect?token=fake-bearer", Format: "csv"}, &out, 1024, server.Client())
	if err == nil || !strings.Contains(err.Error(), "302") || strings.Contains(err.Error(), "fake-bearer") || out.Len() != 0 {
		t.Fatalf("unsafe redirect handling: %v, data=%q", err, out.String())
	}
}

func TestDownloadFailuresNeverPublishPartialFilesOrURLs(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/denied" {
			w.WriteHeader(403)
			io.WriteString(w, "do not expose response body")
			return
		}
		io.WriteString(w, "1234567890")
	}))
	defer server.Close()
	size := int64(100)
	for _, d := range []download{
		{URL: server.URL + "/denied?token=fake-bearer", Format: "csv"},
		{URL: server.URL + "/file?token=fake-bearer", Format: "csv"},
		{URL: server.URL + "/file?token=fake-bearer", Format: "csv", SizeBytes: &size},
		{URL: "http://example.invalid/?token=fake-bearer", Format: "csv"},
		{URL: "https://%invalid/?token=fake-bearer", Format: "csv"},
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "output.csv")
		err := atomicOutput(path, func(out io.Writer) error {
			_, err := downloadFile(context.Background(), d, out, 5, server.Client())
			return err
		})
		if err == nil || strings.Contains(err.Error(), "fake-bearer") || strings.Contains(err.Error(), "do not expose") {
			t.Fatalf("unexpected download error: %v", err)
		}
		files, _ := os.ReadDir(dir)
		if len(files) != 0 {
			t.Error("failed download left a file behind")
		}
	}
}

func TestDownloadStreamingInterruptionPreservesContext(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		name := "canceled"
		if deadline {
			name = "deadline"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			want := context.Canceled
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
				want = context.DeadlineExceeded
			}
			defer cancel()
			server := streamingDownload(t)
			interrupt := cancel
			if deadline {
				interrupt = func() { <-ctx.Done() }
			}
			var out bytes.Buffer
			dir := t.TempDir()
			var n int64
			err := atomicOutput(filepath.Join(dir, "output.csv"), func(w io.Writer) error {
				var err error
				n, err = downloadFile(ctx, download{URL: server.URL + "/file?token=fake-bearer", Format: "csv"}, io.MultiWriter(w, &out), 1024, interruptDownloadAfterRead(server.Client(), interrupt))
				return err
			})
			if n != int64(len("partial data\n")) || out.String() != "partial data\n" {
				t.Errorf("interrupted before body transfer: n=%d body=%q", n, out.String())
			}
			if !errors.Is(err, want) {
				t.Errorf("download error = %v; want errors.Is(_, %v)", err, want)
			}
			if err != nil && (strings.Contains(err.Error(), "fake-bearer") || strings.Contains(err.Error(), server.URL)) {
				t.Errorf("download error exposed signed URL: %v", err)
			}
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 0 {
				t.Errorf("interrupted download left files: %v, %v", files, err)
			}
		})
	}
}

func streamingDownload(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "partial data\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	return server
}

func interruptDownloadAfterRead(hc *http.Client, interrupt func()) *http.Client {
	c := *hc
	c.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := hc.Transport.RoundTrip(req)
		if err == nil {
			resp.Body = &interruptingBody{ReadCloser: resp.Body, interrupt: interrupt}
		}
		return resp, err
	})
	return &c
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type interruptingBody struct {
	io.ReadCloser
	interrupt func()
}

func (b *interruptingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.interrupt()
	}
	return n, err
}

func TestAtomicOutputDoesNotOverwriteRacingDestination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output.csv")
	err := atomicOutput(path, func(out io.Writer) error {
		if err := os.WriteFile(path, []byte("other writer"), 0600); err != nil {
			return err
		}
		_, err := io.WriteString(out, "new contents")
		return err
	})
	if err == nil {
		t.Fatal("overwrote racing destination")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "other writer" {
		t.Fatalf("existing contents changed: %q", data)
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("broken output") }

func TestFormatsPropagateOutputFailures(t *testing.T) {
	for _, format := range []string{"table", "json", "csv"} {
		w := resultWriter{out: brokenWriter{}, format: format}
		if err := w.write(page{Columns: []column{{"id", "bigint"}}, Rows: [][]*string{{str("1")}}}); err == nil {
			t.Errorf("%s hid output failure", format)
		}
	}
}
