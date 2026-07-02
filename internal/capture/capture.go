// Package capture fetches heap profiles from a running Go process that
// exposes net/http/pprof and persists them as timestamped .pb.gz files.
package capture

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Snapshot is a single captured heap profile on disk.
type Snapshot struct {
	Path      string    `json:"path"`
	Timestamp time.Time `json:"timestamp"`
	Endpoint  string    `json:"endpoint"`
	Bytes     int64     `json:"bytes"`
}

// Client fetches heap profiles from a pprof endpoint.
type Client struct {
	// BaseURL is the root of the target service, e.g. "http://staging:6060".
	BaseURL string
	// GC=1 forces a garbage collection before the profile is written, giving
	// a much more accurate inuse_space reading.
	ForceGC bool
	// OutDir is where .pb.gz files are written.
	OutDir string
	// HTTP is overridable for tests.
	HTTP *http.Client
}

// NewClient constructs a capture client with sensible defaults.
func NewClient(baseURL, outDir string) *Client {
	return &Client{
		BaseURL: baseURL,
		OutDir:  outDir,
		ForceGC: true,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

// Fetch downloads a single heap profile and stores it under OutDir.
func (c *Client) Fetch(ctx context.Context) (*Snapshot, error) {
	if err := os.MkdirAll(c.OutDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}
	url := c.BaseURL + "/debug/pprof/heap"
	if c.ForceGC {
		url += "?gc=1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, url)
	}

	ts := time.Now().UTC()
	name := fmt.Sprintf("heap_%s.pb.gz", ts.Format("20060102T150405Z"))
	path := filepath.Join(c.OutDir, name)
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	n, err := io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	return &Snapshot{
		Path:      path,
		Timestamp: ts,
		Endpoint:  c.BaseURL,
		Bytes:     n,
	}, nil
}

// Loop captures `count` snapshots spaced `interval` apart. The context can
// cancel the loop early. Snapshots are returned in capture order.
func (c *Client) Loop(ctx context.Context, interval time.Duration, count int) ([]*Snapshot, error) {
	out := make([]*Snapshot, 0, count)
	for i := 0; i < count; i++ {
		snap, err := c.Fetch(ctx)
		if err != nil {
			return out, fmt.Errorf("snapshot %d: %w", i, err)
		}
		out = append(out, snap)
		if i == count-1 {
			break
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(interval):
		}
	}
	return out, nil
}

// ListSnapshots returns every heap_*.pb.gz file in dir, sorted by name
// (which is chronological because the filename is timestamped).
func ListSnapshots(dir string) ([]*Snapshot, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []*Snapshot
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".gz" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, &Snapshot{
			Path:      filepath.Join(dir, e.Name()),
			Timestamp: info.ModTime().UTC(),
			Bytes:     info.Size(),
		})
	}
	return out, nil
}
