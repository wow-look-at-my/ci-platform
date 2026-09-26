// Command demofixtures captures the demo site's data by asking the real API
// for it.
//
// The point is that the demo cannot drift from the product. It seeds a real
// store through the real model, serves it with the real internal/api handlers,
// and records the exact JSON those handlers return. A hand-written fixture file
// would be a second, unchecked description of the API -- and a demo showing a
// shape the server does not produce is precisely the kind of quiet lie this
// platform exists to refuse.
//
// see docs/demo.md
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/api"
	"github.com/wow-look-at-my/ci-platform/internal/blob/disk"
	"github.com/wow-look-at-my/ci-platform/internal/demoseed"
	"github.com/wow-look-at-my/ci-platform/internal/logstore"
	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store/sqlite"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "demofixtures: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("demofixtures", flag.ContinueOnError)
	fs.SetOutput(out)
	var (
		outPath = fs.String("out", "web-src/demo/fixtures.json", "where to write the captured responses")
		check   = fs.Bool("check", false, "recapture and fail if the committed file differs")
		seedDir = fs.String("seed", "", "seed DIR/ciplatform.db and DIR/blobs for a live control plane, instead of capturing")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *seedDir != "" {
		return seedOnly(*seedDir, out)
	}

	captured, err := capture()
	if err != nil {
		return err
	}
	body, err := json.MarshalIndent(captured, "", "  ")
	if err != nil {
		return fmt.Errorf("encode fixtures: %w", err)
	}
	body = append(body, '\n')

	if *check {
		have, err := os.ReadFile(*outPath)
		if err != nil {
			return fmt.Errorf("read %s: %w", *outPath, err)
		}
		if string(have) != string(body) {
			return fmt.Errorf("%s is stale: regenerate it with `go run ./cmd/demofixtures`", *outPath)
		}
		fmt.Fprintf(out, "demofixtures -check: %s matches a fresh capture\n", *outPath)
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(*outPath), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(*outPath), err)
	}
	if err := os.WriteFile(*outPath, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", *outPath, err)
	}
	fmt.Fprintf(out, "demofixtures: wrote %d responses to %s\n", len(captured), *outPath)
	return nil
}

// seeded is a demo dataset on disk, laid out the way the control plane reads
// it: CIPLATFORM_DATABASE_URL=DIR/ciplatform.db, CIPLATFORM_BLOB_ROOT=DIR/blobs.
type seeded struct {
	st    *sqlite.Store
	blobs *disk.Store
	logs  *logstore.Log
	data  *demoseed.Seeded
}

// seedInto seeds a fresh dataset under dir. The directory must not already
// hold a database: seeding on top of one would duplicate every run.
func seedInto(ctx context.Context, dir string) (*seeded, error) {
	dbPath := filepath.Join(dir, "ciplatform.db")
	if _, err := os.Stat(dbPath); err == nil {
		return nil, fmt.Errorf("%s already exists; seed into an empty directory", dbPath)
	}
	st, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	if err := st.Migrate(ctx); err != nil {
		_ = st.Close()
		return nil, err
	}

	blobs, err := disk.New(filepath.Join(dir, "blobs"))
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("blob store: %w", err)
	}
	logs, err := logstore.New(logstore.Options{Blob: blobs, KeyPrefix: "logs"})
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("log store: %w", err)
	}

	data, err := demoseed.Seed(ctx, st, logs)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	return &seeded{st: st, blobs: blobs, logs: logs, data: data}, nil
}

// seedOnly leaves the dataset on disk for a live control plane to serve.
func seedOnly(dir string, out io.Writer) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	s, err := seedInto(context.Background(), dir)
	if err != nil {
		return err
	}
	if err := s.st.Close(); err != nil {
		return err
	}
	fmt.Fprintf(out, "demofixtures: seeded %s\n", dir)
	return nil
}

// capture serves the seeded store with the real API and records every response
// the UI asks for.
func capture() (map[string]json.RawMessage, error) {
	dir, err := os.MkdirTemp("", "ciplatform-demo")
	if err != nil {
		return nil, fmt.Errorf("temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	s, err := seedInto(context.Background(), dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.st.Close() }()
	seeded := s.data

	srv := api.New(api.Config{
		Store: s.st, Logs: s.logs, Controller: refusingController{},
		Blobs: blobOpener{s.blobs},
		// A fixed clock keeps "3m ago" style output stable, so recapturing an
		// unchanged seed produces an unchanged file.
		Now: func() time.Time { return demoseed.Now },
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	captured := map[string]json.RawMessage{}
	for _, path := range seeded.Paths() {
		body, err := get(ts.URL + path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		captured[path] = body
	}
	return captured, nil
}

func get(url string) (json.RawMessage, error) {
	resp, err := http.Get(url) //nolint:gosec // the URL is this process's own test server
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	// Re-encode so the file is stable regardless of handler formatting.
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("response is not JSON: %w", err)
	}
	return json.Marshal(v)
}

// refusingController satisfies the API's collaborator check without pretending
// the demo can act. Nothing calls it: the demo's own client answers cancel and
// re-run without a request. It exists so /healthz does not report a missing
// scheduler in a snapshot where that would be misleading noise.
type refusingController struct{}

func (refusingController) Cancel(context.Context, int64, model.CancelReason) error { return errDemo }
func (refusingController) CancelJob(context.Context, int64, model.CancelReason) error {
	return errDemo
}
func (refusingController) Rerun(context.Context, int64, string) error       { return errDemo }
func (refusingController) RerunFailed(context.Context, int64, string) error { return errDemo }
func (refusingController) RerunJob(context.Context, int64, string) error    { return errDemo }

var errDemo = fmt.Errorf("this is a captured demo: there is no control plane to act on")

type blobOpener struct{ blobs *disk.Store }

func (b blobOpener) Open(ctx context.Context, a *model.Artifact) (io.ReadCloser, error) {
	return b.blobs.Get(ctx, a.StorageKey)
}
