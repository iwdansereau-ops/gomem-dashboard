// Package gcstats captures runtime.MemStats snapshots from a running Go
// process and computes GC/allocation deltas between two points in time.
//
// runtime.MemStats is NOT part of a pprof heap profile. Heap profiles carry
// per-callstack allocation samples and inuse_space bytes, but the
// process-wide counters TotalAlloc, NumGC, PauseTotalNs, HeapAlloc, etc.
// live only in runtime.MemStats. The convention is to expose them via a
// small HTTP endpoint that JSON-encodes runtime.MemStats — most production
// Go services already do this via expvar or a dedicated /debug/memstats
// handler. See cmd/sample-processor for the two-line implementation.
package gcstats

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Snapshot is a JSON-serialisable subset of runtime.MemStats plus the wall
// clock at which it was captured. We keep the field names identical to the
// Go standard library so downstream tooling can also consume raw
// runtime.MemStats dumps.
type Snapshot struct {
	CapturedAt time.Time `json:"captured_at"`

	// Cumulative counters
	TotalAlloc   uint64 `json:"TotalAlloc"`
	Mallocs      uint64 `json:"Mallocs"`
	Frees        uint64 `json:"Frees"`
	NumGC        uint32 `json:"NumGC"`
	PauseTotalNs uint64 `json:"PauseTotalNs"`

	// Live-heap gauges
	HeapAlloc    uint64 `json:"HeapAlloc"`
	HeapInuse    uint64 `json:"HeapInuse"`
	HeapObjects  uint64 `json:"HeapObjects"`
	HeapSys      uint64 `json:"HeapSys"`
	HeapIdle     uint64 `json:"HeapIdle"`
	HeapReleased uint64 `json:"HeapReleased"`

	// Convenience
	NextGC       uint64  `json:"NextGC"`
	GCCPUFraction float64 `json:"GCCPUFraction"`
	Sys          uint64  `json:"Sys"`
}

// FromMemStats projects a runtime.MemStats into a Snapshot at time t.
func FromMemStats(m *runtime.MemStats, t time.Time) *Snapshot {
	return &Snapshot{
		CapturedAt:    t,
		TotalAlloc:    m.TotalAlloc,
		Mallocs:       m.Mallocs,
		Frees:         m.Frees,
		NumGC:         m.NumGC,
		PauseTotalNs:  m.PauseTotalNs,
		HeapAlloc:     m.HeapAlloc,
		HeapInuse:     m.HeapInuse,
		HeapObjects:   m.HeapObjects,
		HeapSys:       m.HeapSys,
		HeapIdle:      m.HeapIdle,
		HeapReleased:  m.HeapReleased,
		NextGC:        m.NextGC,
		GCCPUFraction: m.GCCPUFraction,
		Sys:           m.Sys,
	}
}

// Fetch downloads a JSON runtime.MemStats from `<baseURL>/debug/memstats`.
// Callers that expose their memstats at a different path should pass the
// full URL directly to FetchURL.
func Fetch(ctx context.Context, baseURL string, hc *http.Client) (*Snapshot, error) {
	return FetchURL(ctx, baseURL+"/debug/memstats", hc)
}

// FetchURL downloads a JSON runtime.MemStats from the given URL.
func FetchURL(ctx context.Context, url string, hc *http.Client) (*Snapshot, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("unexpected status %d from %s: %s", resp.StatusCode, url, string(body))
	}
	var ms runtime.MemStats
	if err := json.NewDecoder(resp.Body).Decode(&ms); err != nil {
		return nil, fmt.Errorf("decode memstats: %w", err)
	}
	return FromMemStats(&ms, time.Now().UTC()), nil
}

// Save writes the snapshot to a timestamped JSON file next to a heap
// profile with the same stem. Returns the path written.
func (s *Snapshot) Save(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("gcstats_%s.json", s.CapturedAt.Format("20060102T150405Z"))
	p := filepath.Join(dir, name)
	f, err := os.Create(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		return "", err
	}
	return p, nil
}

// Load reads a Snapshot back from disk.
func Load(path string) (*Snapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var s Snapshot
	if err := json.NewDecoder(f).Decode(&s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Delta is the pairwise diff between two Snapshots plus derived rate signals.
type Delta struct {
	DurationSeconds float64 `json:"duration_seconds"`

	// Absolute deltas
	TotalAllocDelta int64 `json:"total_alloc_delta_bytes"`
	MallocsDelta    int64 `json:"mallocs_delta"`
	FreesDelta      int64 `json:"frees_delta"`
	NumGCDelta      int64 `json:"num_gc_delta"`
	PauseNsDelta    int64 `json:"pause_ns_delta"`
	HeapInuseDelta  int64 `json:"heap_inuse_delta_bytes"`
	HeapObjectsDelta int64 `json:"heap_objects_delta"`

	// Rates
	AllocRateBytesPerSec float64 `json:"alloc_rate_bytes_per_sec"`
	GCPerSec             float64 `json:"gc_per_sec"`
	AvgGCPauseMs         float64 `json:"avg_gc_pause_ms"`

	// Churn ratio: how many bytes were allocated for each byte that
	// remained live. High = allocation churn, low = pure retention.
	// Undefined when HeapInuseDelta ≤ 0 (encoded as +Inf → we use -1 sentinel).
	ChurnRatio float64 `json:"churn_ratio"`

	// GCCPU is the ending GCCPUFraction (0–1). Reported for context.
	GCCPUFractionEnd float64 `json:"gc_cpu_fraction_end"`

	// Absolute end values (useful in the PR comment header)
	EndHeapInuse   uint64 `json:"end_heap_inuse_bytes"`
	EndHeapObjects uint64 `json:"end_heap_objects"`
	EndNumGC       uint32 `json:"end_num_gc"`
}

// Compute produces a Delta from base → current.
func Compute(base, current *Snapshot) *Delta {
	dur := current.CapturedAt.Sub(base.CapturedAt).Seconds()
	if dur <= 0 {
		dur = 1 // avoid div-by-zero; caller sees duration_seconds and can judge.
	}
	d := &Delta{
		DurationSeconds:  current.CapturedAt.Sub(base.CapturedAt).Seconds(),
		TotalAllocDelta:  int64(current.TotalAlloc) - int64(base.TotalAlloc),
		MallocsDelta:     int64(current.Mallocs) - int64(base.Mallocs),
		FreesDelta:       int64(current.Frees) - int64(base.Frees),
		NumGCDelta:       int64(current.NumGC) - int64(base.NumGC),
		PauseNsDelta:     int64(current.PauseTotalNs) - int64(base.PauseTotalNs),
		HeapInuseDelta:   int64(current.HeapInuse) - int64(base.HeapInuse),
		HeapObjectsDelta: int64(current.HeapObjects) - int64(base.HeapObjects),
		GCCPUFractionEnd: current.GCCPUFraction,
		EndHeapInuse:     current.HeapInuse,
		EndHeapObjects:   current.HeapObjects,
		EndNumGC:         current.NumGC,
	}
	d.AllocRateBytesPerSec = float64(d.TotalAllocDelta) / dur
	d.GCPerSec = float64(d.NumGCDelta) / dur
	if d.NumGCDelta > 0 {
		d.AvgGCPauseMs = float64(d.PauseNsDelta) / float64(d.NumGCDelta) / 1e6
	}
	switch {
	case d.HeapInuseDelta > 0:
		d.ChurnRatio = float64(d.TotalAllocDelta) / float64(d.HeapInuseDelta)
	case d.TotalAllocDelta > 0:
		d.ChurnRatio = -1 // "infinite" churn: allocated a lot, retained ≤ 0
	default:
		d.ChurnRatio = 0
	}
	return d
}
