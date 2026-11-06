package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// A download's limits: the headers within responseHeaderTimeout, then a byte at least every
// downloadIdleTimeout (a variable so tests can shorten it). No whole-download deadline: a large
// snapshot on a slow line is fine as long as it moves.
const responseHeaderTimeout = 30 * time.Second

var downloadIdleTimeout = 60 * time.Second

var downloadClient = &http.Client{Transport: &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	ResponseHeaderTimeout: responseHeaderTimeout,
}}

var errDownloadStalled = errors.New("the download stalled")

// idleReader restarts the idle timer on every read.
type idleReader struct {
	r     io.Reader
	timer *time.Timer
	idle  time.Duration
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.timer.Reset(r.idle)
	return n, err
}

// download writes url's body to output, or to stdout when output is "" or "-". A file is written
// beside its target and renamed over it only when complete, so a failed download leaves an
// existing file as it was and no partial one behind.
func download(ctx context.Context, url, output string, stdout io.Writer) (int64, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	res, err := downloadClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("downloading: %s", res.Status)
	}
	idle := downloadIdleTimeout
	timer := time.AfterFunc(idle, func() { cancel(errDownloadStalled) })
	defer timer.Stop()
	body := &idleReader{r: res.Body, timer: timer, idle: idle}
	stalled := func(err error) error {
		if errors.Is(context.Cause(ctx), errDownloadStalled) {
			return fmt.Errorf("%w: no data for %s", errDownloadStalled, idle)
		}
		return err
	}

	if output == "" || output == "-" {
		n, err := io.Copy(stdout, body)
		if err != nil {
			return n, stalled(err)
		}
		return n, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(output), ".kaab-snapshot-*")
	if err != nil {
		return 0, err
	}
	kept := false
	defer func() {
		if !kept {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	n, err := io.Copy(tmp, body)
	if err != nil {
		return n, stalled(err)
	}
	if err := tmp.Close(); err != nil {
		return n, err
	}
	if err := os.Rename(tmp.Name(), output); err != nil {
		return n, err
	}
	kept = true
	return n, nil
}
