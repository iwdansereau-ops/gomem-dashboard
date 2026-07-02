// Package report renders diff results as JSON + Markdown remediation reports.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/perplexity/gomem-dashboard/internal/diff"
)

// Bundle is what serve/dashboard displays and what CI can machine-read.
type Bundle struct {
	GeneratedAt time.Time         `json:"generated_at"`
	BaseFile    string            `json:"base_file"`
	CurrentFile string            `json:"current_file"`
	TotalDelta  int64             `json:"total_inuse_delta_bytes"`
	TotalBefore int64             `json:"total_inuse_before_bytes"`
	TotalAfter  int64             `json:"total_inuse_after_bytes"`
	Top         []diff.FunctionDelta `json:"top_functions"`
	SVGPath     string            `json:"svg_path,omitempty"`
	MDPath      string            `json:"md_path,omitempty"`
}

// WriteAll writes the JSON bundle, Markdown remediation report, and returns the bundle.
// The caller writes the SVG separately (see internal/graph).
func WriteAll(outDir, baseName string, rep *diff.Report, topN int) (*Bundle, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	top := rep.TopN(topN)
	b := &Bundle{
		GeneratedAt: time.Now().UTC(),
		BaseFile:    rep.BaseFile,
		CurrentFile: rep.CurrentFile,
		TotalDelta:  rep.TotalDelta,
		TotalBefore: rep.TotalBefore,
		TotalAfter:  rep.TotalAfter,
		Top:         top,
	}

	jsonPath := filepath.Join(outDir, baseName+".json")
	mdPath := filepath.Join(outDir, baseName+".md")
	svgPath := filepath.Join(outDir, baseName+".svg")
	b.SVGPath = filepath.Base(svgPath)
	b.MDPath = filepath.Base(mdPath)

	jf, err := os.Create(jsonPath)
	if err != nil {
		return nil, err
	}
	enc := json.NewEncoder(jf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(b); err != nil {
		jf.Close()
		return nil, err
	}
	if err := jf.Close(); err != nil {
		return nil, err
	}

	mf, err := os.Create(mdPath)
	if err != nil {
		return nil, err
	}
	if err := writeMarkdown(mf, b, rep); err != nil {
		mf.Close()
		return nil, err
	}
	if err := mf.Close(); err != nil {
		return nil, err
	}
	return b, nil
}

func writeMarkdown(w io.Writer, b *Bundle, rep *diff.Report) error {
	var sb strings.Builder
	sb.WriteString("# Memory Growth Report\n\n")
	fmt.Fprintf(&sb, "- **Generated:** %s\n", b.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(&sb, "- **Base snapshot:** `%s`\n", filepath.Base(b.BaseFile))
	fmt.Fprintf(&sb, "- **Current snapshot:** `%s`\n", filepath.Base(b.CurrentFile))
	fmt.Fprintf(&sb, "- **Total inuse_space before:** %s\n", human(b.TotalBefore))
	fmt.Fprintf(&sb, "- **Total inuse_space after:** %s\n", human(b.TotalAfter))
	fmt.Fprintf(&sb, "- **Total delta:** **%s**\n\n", human(b.TotalDelta))

	sb.WriteString("## Top 5 leaking functions (flat inuse_space delta)\n\n")
	sb.WriteString("| Rank | Function | Flat Δ | Cum Δ | Source |\n")
	sb.WriteString("|-----:|----------|-------:|------:|--------|\n")
	for i, f := range b.Top {
		src := "—"
		if f.File != "" {
			src = fmt.Sprintf("`%s:%d`", trimPath(f.File), f.Line)
		}
		fmt.Fprintf(&sb, "| %d | `%s` | %s | %s | %s |\n",
			i+1, f.Function, human(f.FlatDelta), human(f.CumDelta), src)
	}
	sb.WriteString("\n")

	sb.WriteString("## Remediation checklist\n\n")
	for i, f := range b.Top {
		fmt.Fprintf(&sb, "### %d. `%s`\n", i+1, f.Function)
		if f.File != "" {
			fmt.Fprintf(&sb, "**Source:** `%s:%d`  \n", trimPath(f.File), f.Line)
		}
		fmt.Fprintf(&sb, "**Retained between snapshots:** %s (flat) / %s (cum)  \n\n",
			human(f.FlatDelta), human(f.CumDelta))
		sb.WriteString("Suggested checks:\n")
		sb.WriteString("- Are any goroutines started here blocked on channels or waiting indefinitely?\n")
		sb.WriteString("- Are slices/maps/buffers appended to but never reset or bounded?\n")
		sb.WriteString("- Is a cache missing an eviction policy (TTL, LRU, size cap)?\n")
		sb.WriteString("- Are `sync.Pool` objects being retained beyond their intended lifetime?\n")
		sb.WriteString("- Are HTTP response bodies / DB rows / files being closed on every path?\n\n")
	}

	sb.WriteString("## Full ranked list\n\n")
	sb.WriteString("| Function | Flat Δ | Cum Δ | Source |\n")
	sb.WriteString("|----------|-------:|------:|--------|\n")
	limit := 50
	for i, f := range rep.Functions {
		if i >= limit {
			break
		}
		if f.FlatDelta <= 0 && f.CumDelta <= 0 {
			continue
		}
		src := "—"
		if f.File != "" {
			src = fmt.Sprintf("`%s:%d`", trimPath(f.File), f.Line)
		}
		fmt.Fprintf(&sb, "| `%s` | %s | %s | %s |\n",
			f.Function, human(f.FlatDelta), human(f.CumDelta), src)
	}

	_, err := io.WriteString(w, sb.String())
	return err
}

func trimPath(p string) string {
	parts := strings.Split(p, "/")
	if len(parts) <= 3 {
		return p
	}
	return ".../" + strings.Join(parts[len(parts)-3:], "/")
}

func human(b int64) string {
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
