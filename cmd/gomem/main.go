// Command gomem is the operator CLI for the memory analysis dashboard.
//
// Sub-commands:
//
//	gomem capture --url http://staging:6060 --dir ./profiles --interval 30s --count 12
//	gomem diff    --base ./profiles/a.pb.gz --current ./profiles/b.pb.gz --out ./reports/diff
//	gomem report  --dir ./profiles --out ./reports         # diff all consecutive pairs
//	gomem serve   --dir ./profiles --reports ./reports --addr :8080
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/perplexity/gomem-dashboard/internal/capture"
	"github.com/perplexity/gomem-dashboard/internal/diff"
	"github.com/perplexity/gomem-dashboard/internal/gcstats"
	"github.com/perplexity/gomem-dashboard/internal/graph"
	"github.com/perplexity/gomem-dashboard/internal/report"
	"github.com/perplexity/gomem-dashboard/internal/server"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "capture":
		err = runCapture(args)
	case "gcstats":
		err = runGCStats(args)
	case "gcdiff":
		err = runGCDiff(args)
	case "diff":
		err = runDiff(args)
	case "report":
		err = runReport(args)
	case "serve":
		err = runServe(args)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `gomem — Go memory analysis dashboard

Usage:
  gomem capture --url URL --dir DIR [--interval 30s] [--count 12] [--memstats-path /debug/memstats]
  gomem gcstats --base URL [--out FILE]         # fetch one runtime.MemStats snapshot
  gomem gcdiff  --dir DIR   [--out FILE]        # diff first & last gcstats_*.json in DIR
  gomem diff    --base FILE --current FILE --out BASENAME [--top 5]
  gomem report  --dir DIR --out DIR [--top 5]
  gomem serve   --dir DIR --reports DIR [--addr :8080]

`)
}

func runCapture(args []string) error {
	fs := flag.NewFlagSet("capture", flag.ExitOnError)
	url := fs.String("url", "", "pprof root URL, e.g. http://staging:6060")
	dir := fs.String("dir", "./profiles", "output directory")
	interval := fs.Duration("interval", 30*time.Second, "interval between snapshots")
	count := fs.Int("count", 12, "number of snapshots to capture")
	noGC := fs.Bool("no-gc", false, "do not force GC before capture")
	memstatsPath := fs.String("memstats-path", "/debug/memstats",
		"path (appended to --url) exposing JSON-encoded runtime.MemStats; empty disables GC stats capture")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *url == "" {
		return errors.New("--url is required")
	}
	c := capture.NewClient(*url, *dir)
	c.ForceGC = !*noGC
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	fmt.Printf("capturing %d snapshots from %s every %s → %s\n", *count, *url, *interval, *dir)

	// Interleave: each tick fetches a heap profile and (best-effort) a
	// gcstats snapshot. runtime.MemStats is NOT stored inside the pprof
	// heap profile, so we must hit /debug/memstats separately — the two
	// requests land on the same process within ~milliseconds, close enough
	// that their deltas line up.
	var heapErr error
	var anyGCFail bool
	for i := 0; i < *count; i++ {
		snap, err := c.Fetch(ctx)
		if err != nil {
			heapErr = fmt.Errorf("snapshot %d: %w", i, err)
			break
		}
		fmt.Printf("  ✓ %s (%d bytes) @ %s\n",
			filepath.Base(snap.Path), snap.Bytes, snap.Timestamp.Format(time.RFC3339))
		if *memstatsPath != "" {
			gs, err := gcstats.FetchURL(ctx, *url+*memstatsPath, nil)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  ! gcstats fetch failed: %v\n", err)
				anyGCFail = true
			} else if p, err := gs.Save(*dir); err != nil {
				fmt.Fprintf(os.Stderr, "  ! gcstats save failed: %v\n", err)
				anyGCFail = true
			} else {
				fmt.Printf("  ✓ %s (TotalAlloc=%d NumGC=%d)\n",
					filepath.Base(p), gs.TotalAlloc, gs.NumGC)
			}
		}
		if i == *count-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(*interval):
		}
	}
	if heapErr != nil {
		return heapErr
	}
	if anyGCFail {
		fmt.Fprintln(os.Stderr, "note: some gcstats fetches failed; leak-vs-churn classification may be unavailable")
	}
	return nil
}

func runGCStats(args []string) error {
	fs := flag.NewFlagSet("gcstats", flag.ExitOnError)
	url := fs.String("url", "", "full URL to fetch JSON-encoded runtime.MemStats from")
	base := fs.String("base", "", "pprof base URL; --url becomes <base>/debug/memstats")
	out := fs.String("out", "", "output JSON file (defaults to stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	target := *url
	if target == "" {
		if *base == "" {
			return errors.New("--url or --base is required")
		}
		target = *base + "/debug/memstats"
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	s, err := gcstats.FetchURL(ctx, target, nil)
	if err != nil {
		return err
	}
	var w io.Writer = os.Stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

func runGCDiff(args []string) error {
	fs := flag.NewFlagSet("gcdiff", flag.ExitOnError)
	dir := fs.String("dir", "./profiles", "directory containing gcstats_*.json snapshots")
	out := fs.String("out", "", "output JSON file (defaults to stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	entries, err := os.ReadDir(*dir)
	if err != nil {
		return err
	}
	var files []string
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() && strings.HasPrefix(n, "gcstats_") && strings.HasSuffix(n, ".json") {
			files = append(files, filepath.Join(*dir, n))
		}
	}
	sort.Strings(files)
	if len(files) < 2 {
		return fmt.Errorf("need at least 2 gcstats snapshots in %s, found %d", *dir, len(files))
	}
	baseSnap, err := gcstats.Load(files[0])
	if err != nil {
		return fmt.Errorf("load base: %w", err)
	}
	curSnap, err := gcstats.Load(files[len(files)-1])
	if err != nil {
		return fmt.Errorf("load current: %w", err)
	}
	delta := gcstats.Compute(baseSnap, curSnap)
	payload := struct {
		BaseFile    string            `json:"base_file"`
		CurrentFile string            `json:"current_file"`
		Base        *gcstats.Snapshot `json:"base"`
		Current     *gcstats.Snapshot `json:"current"`
		Delta       *gcstats.Delta    `json:"delta"`
	}{
		BaseFile:    filepath.Base(files[0]),
		CurrentFile: filepath.Base(files[len(files)-1]),
		Base:        baseSnap,
		Current:     curSnap,
		Delta:       delta,
	}
	var w io.Writer = os.Stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}

func runDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	base := fs.String("base", "", "base pprof file")
	cur := fs.String("current", "", "current pprof file")
	out := fs.String("out", "", "output basename (e.g. reports/diff_1_2)")
	topN := fs.Int("top", 5, "top N functions to highlight")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *base == "" || *cur == "" || *out == "" {
		return errors.New("--base, --current, and --out are required")
	}
	return produceReport(*base, *cur, *out, *topN)
}

func produceReport(baseFile, curFile, outPath string, topN int) error {
	rep, err := diff.Compute(baseFile, curFile)
	if err != nil {
		return err
	}
	outDir := filepath.Dir(outPath)
	baseName := filepath.Base(outPath)
	if outDir == "" {
		outDir = "."
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	// SVG
	svgPath := filepath.Join(outDir, baseName+".svg")
	f, err := os.Create(svgPath)
	if err != nil {
		return err
	}
	opts := graph.DefaultOptions()
	opts.Title = fmt.Sprintf("Heap Growth: %s → %s",
		filepath.Base(baseFile), filepath.Base(curFile))
	if err := graph.Render(f, rep, opts); err != nil {
		f.Close()
		return err
	}
	f.Close()
	// JSON + MD
	b, err := report.WriteAll(outDir, baseName, rep, topN)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %s (Δ=%s)\n", outPath, humanBytes(b.TotalDelta))
	fmt.Printf("  svg: %s\n  md:  %s\n  json:%s\n",
		filepath.Join(outDir, baseName+".svg"),
		filepath.Join(outDir, baseName+".md"),
		filepath.Join(outDir, baseName+".json"),
	)
	return nil
}

func runReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	dir := fs.String("dir", "./profiles", "directory containing heap_*.pb.gz")
	outDir := fs.String("out", "./reports", "output directory for reports")
	topN := fs.Int("top", 5, "top N functions to highlight")
	if err := fs.Parse(args); err != nil {
		return err
	}
	entries, err := os.ReadDir(*dir)
	if err != nil {
		return err
	}
	var snaps []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".pb.gz") {
			snaps = append(snaps, filepath.Join(*dir, e.Name()))
		}
	}
	sort.Strings(snaps)
	if len(snaps) < 2 {
		return fmt.Errorf("need at least 2 snapshots in %s, found %d", *dir, len(snaps))
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	for i := 0; i < len(snaps)-1; i++ {
		name := fmt.Sprintf("diff_%02d_%02d", i+1, i+2)
		if err := produceReport(snaps[i], snaps[i+1], filepath.Join(*outDir, name), *topN); err != nil {
			return fmt.Errorf("diff %s: %w", name, err)
		}
	}
	return nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dir := fs.String("dir", "./profiles", "profiles directory")
	reports := fs.String("reports", "./reports", "reports directory")
	addr := fs.String("addr", ":8080", "listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s := server.New(server.Config{ProfileDir: *dir, ReportDir: *reports, Addr: *addr})
	return s.ListenAndServe()
}

func humanBytes(b int64) string {
	neg := ""
	v := b
	if v < 0 {
		neg = "-"
		v = -v
	}
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)
	switch {
	case v >= GB:
		return fmt.Sprintf("%s%.2f GB", neg, float64(v)/float64(GB))
	case v >= MB:
		return fmt.Sprintf("%s%.2f MB", neg, float64(v)/float64(MB))
	case v >= KB:
		return fmt.Sprintf("%s%.1f KB", neg, float64(v)/float64(KB))
	default:
		return fmt.Sprintf("%s%d B", neg, v)
	}
}
