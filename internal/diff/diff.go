// Package diff parses two pprof heap profiles and computes the inuse_space
// delta per function. It is the analytical core of gomem-dashboard.
//
// Concepts
//
//   - A pprof profile has multiple "samples", each being a stack trace plus a
//     value vector. For heap profiles the value vector is
//     [alloc_objects, alloc_space, inuse_objects, inuse_space].
//   - We identify the inuse_space value type dynamically (its unit is "bytes")
//     so this code keeps working if Go's profile schema changes.
//   - The delta is computed at *function* granularity: sum of inuse_space
//     across every sample whose leaf frame is that function. This matches how
//     `go tool pprof -diff_base ... -top` presents the data.
//   - We also compute an edge weight per (caller, callee) pair, which the
//     graph package uses to render the SVG call graph.
package diff

import (
	"fmt"
	"os"
	"sort"

	"github.com/google/pprof/profile"
)

// FunctionDelta represents the memory growth (in bytes) attributed to a
// single Go function between two snapshots.
type FunctionDelta struct {
	Function   string `json:"function"`
	File       string `json:"file"`
	Line       int64  `json:"line"`
	FlatBefore int64  `json:"flat_before"`
	FlatAfter  int64  `json:"flat_after"`
	FlatDelta  int64  `json:"flat_delta"`
	CumBefore  int64  `json:"cum_before"`
	CumAfter   int64  `json:"cum_after"`
	CumDelta   int64  `json:"cum_delta"`
}

// Edge is a directed caller→callee relationship weighted by the delta bytes
// that flow through it.
type Edge struct {
	Caller string `json:"caller"`
	Callee string `json:"callee"`
	Weight int64  `json:"weight"`
}

// Report is the full diff result.
type Report struct {
	BaseFile     string          `json:"base_file"`
	CurrentFile  string          `json:"current_file"`
	TotalBefore  int64           `json:"total_inuse_before"`
	TotalAfter   int64           `json:"total_inuse_after"`
	TotalDelta   int64           `json:"total_inuse_delta"`
	Functions    []FunctionDelta `json:"functions"`
	Edges        []Edge          `json:"edges"`
	SampleUnit   string          `json:"sample_unit"`
	SampleType   string          `json:"sample_type"`
}

// LoadProfile reads a .pb.gz pprof profile from disk.
func LoadProfile(path string) (*profile.Profile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	p, err := profile.Parse(f)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := p.CheckValid(); err != nil {
		return nil, fmt.Errorf("invalid profile %s: %w", path, err)
	}
	return p, nil
}

// inuseSpaceIndex returns the index of the "inuse_space" value in a heap
// profile's Sample value vector. It falls back to the last value type
// (Go's convention for heap profiles) if a named match isn't found.
func inuseSpaceIndex(p *profile.Profile) (int, *profile.ValueType) {
	for i, vt := range p.SampleType {
		if vt.Type == "inuse_space" {
			return i, vt
		}
	}
	// Fallback: default sample type or last entry.
	if p.DefaultSampleType != "" {
		for i, vt := range p.SampleType {
			if vt.Type == p.DefaultSampleType {
				return i, vt
			}
		}
	}
	idx := len(p.SampleType) - 1
	return idx, p.SampleType[idx]
}

// flatCum aggregates flat (leaf-attributed) and cumulative (any-frame) values
// per function for a single profile at the requested value index.
type flatCum struct {
	flat map[string]int64
	cum  map[string]int64
	loc  map[string]locInfo // function → best-known source location
	edges map[string]int64  // "caller|callee" → weight
}

type locInfo struct {
	File string
	Line int64
}

func aggregate(p *profile.Profile, valueIdx int) *flatCum {
	fc := &flatCum{
		flat:  make(map[string]int64),
		cum:   make(map[string]int64),
		loc:   make(map[string]locInfo),
		edges: make(map[string]int64),
	}
	for _, s := range p.Sample {
		if len(s.Value) <= valueIdx {
			continue
		}
		v := s.Value[valueIdx]
		if v == 0 {
			continue
		}
		// pprof samples list locations leaf-first.
		// Attribute flat to the leaf function, cum to every function on the
		// stack, and edges from callee→caller as we walk up.
		seen := make(map[string]bool)
		var prev string
		for locIdx, loc := range s.Location {
			for _, line := range loc.Line {
				if line.Function == nil {
					continue
				}
				name := line.Function.Name
				if name == "" {
					continue
				}
				if _, ok := fc.loc[name]; !ok {
					fc.loc[name] = locInfo{
						File: line.Function.Filename,
						Line: line.Line,
					}
				}
				if locIdx == 0 && prev == "" {
					fc.flat[name] += v
				}
				if !seen[name] {
					fc.cum[name] += v
					seen[name] = true
				}
				if prev != "" && prev != name {
					// prev is closer to leaf (callee); name is closer to root (caller).
					fc.edges[name+"|"+prev] += v
				}
				prev = name
			}
		}
	}
	return fc
}

// Compute produces a Report describing the inuse_space delta from base→current.
func Compute(baseFile, currentFile string) (*Report, error) {
	base, err := LoadProfile(baseFile)
	if err != nil {
		return nil, err
	}
	cur, err := LoadProfile(currentFile)
	if err != nil {
		return nil, err
	}
	idxA, vtA := inuseSpaceIndex(base)
	idxB, vtB := inuseSpaceIndex(cur)
	if vtA.Type != vtB.Type || vtA.Unit != vtB.Unit {
		return nil, fmt.Errorf("incompatible sample types: %s/%s vs %s/%s",
			vtA.Type, vtA.Unit, vtB.Type, vtB.Unit)
	}
	fcA := aggregate(base, idxA)
	fcB := aggregate(cur, idxB)

	// Union of function names.
	names := make(map[string]struct{}, len(fcA.flat)+len(fcB.flat))
	for n := range fcA.cum {
		names[n] = struct{}{}
	}
	for n := range fcB.cum {
		names[n] = struct{}{}
	}

	rep := &Report{
		BaseFile:    baseFile,
		CurrentFile: currentFile,
		SampleType:  vtA.Type,
		SampleUnit:  vtA.Unit,
	}

	for name := range names {
		fd := FunctionDelta{
			Function:   name,
			FlatBefore: fcA.flat[name],
			FlatAfter:  fcB.flat[name],
			CumBefore:  fcA.cum[name],
			CumAfter:   fcB.cum[name],
		}
		fd.FlatDelta = fd.FlatAfter - fd.FlatBefore
		fd.CumDelta = fd.CumAfter - fd.CumBefore
		if li, ok := fcB.loc[name]; ok {
			fd.File, fd.Line = li.File, li.Line
		} else if li, ok := fcA.loc[name]; ok {
			fd.File, fd.Line = li.File, li.Line
		}
		rep.Functions = append(rep.Functions, fd)
	}

	// Totals
	for _, s := range base.Sample {
		if len(s.Value) > idxA {
			rep.TotalBefore += s.Value[idxA]
		}
	}
	for _, s := range cur.Sample {
		if len(s.Value) > idxB {
			rep.TotalAfter += s.Value[idxB]
		}
	}
	rep.TotalDelta = rep.TotalAfter - rep.TotalBefore

	// Edges: union weighted by delta of cur - base for each edge.
	edgeNames := make(map[string]struct{})
	for k := range fcA.edges {
		edgeNames[k] = struct{}{}
	}
	for k := range fcB.edges {
		edgeNames[k] = struct{}{}
	}
	for k := range edgeNames {
		delta := fcB.edges[k] - fcA.edges[k]
		if delta <= 0 {
			continue
		}
		// split "caller|callee"
		var caller, callee string
		for i := 0; i < len(k); i++ {
			if k[i] == '|' {
				caller = k[:i]
				callee = k[i+1:]
				break
			}
		}
		rep.Edges = append(rep.Edges, Edge{Caller: caller, Callee: callee, Weight: delta})
	}

	// Sort functions by FlatDelta desc (primary), CumDelta desc (secondary).
	sort.Slice(rep.Functions, func(i, j int) bool {
		if rep.Functions[i].FlatDelta != rep.Functions[j].FlatDelta {
			return rep.Functions[i].FlatDelta > rep.Functions[j].FlatDelta
		}
		return rep.Functions[i].CumDelta > rep.Functions[j].CumDelta
	})
	sort.Slice(rep.Edges, func(i, j int) bool {
		return rep.Edges[i].Weight > rep.Edges[j].Weight
	})
	return rep, nil
}

// TopN returns the first n growing functions (FlatDelta > 0).
func (r *Report) TopN(n int) []FunctionDelta {
	out := make([]FunctionDelta, 0, n)
	for _, f := range r.Functions {
		if f.FlatDelta <= 0 {
			continue
		}
		out = append(out, f)
		if len(out) == n {
			break
		}
	}
	return out
}
