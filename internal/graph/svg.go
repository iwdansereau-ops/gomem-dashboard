// Package graph renders a differential call graph to SVG. Nodes and edges
// are shaded with a red gradient proportional to their share of the total
// leaked bytes so the eye is drawn to the largest regressions.
package graph

import (
	"fmt"
	"html"
	"io"
	"math"
	"sort"
	"strings"

	"github.com/perplexity/gomem-dashboard/internal/diff"
)

// Options controls graph rendering.
type Options struct {
	// MaxNodes caps the number of functions drawn (largest FlatDelta first).
	MaxNodes int
	// MinEdgeBytes filters out tiny edges to keep the graph readable.
	MinEdgeBytes int64
	// Title rendered at the top of the SVG.
	Title string
}

// DefaultOptions returns opinionated defaults suitable for staging analysis.
func DefaultOptions() Options {
	return Options{MaxNodes: 25, MinEdgeBytes: 4096, Title: "Heap Growth (inuse_space delta)"}
}

// edge is a rendered edge between two drawn nodes.
type edge struct {
	from, to *node
	weight   int64
}

// node is an internal layout record.
type node struct {
	id       int
	name     string
	short    string
	flat     int64
	cum      int64
	file     string
	line     int64
	layer    int
	x, y     float64
	w, h     float64
}

// Render writes an SVG call graph to w based on the diff report.
func Render(w io.Writer, rep *diff.Report, opts Options) error {
	if opts.MaxNodes <= 0 {
		opts.MaxNodes = 25
	}

	// Keep only positive-delta functions and top MaxNodes.
	top := make([]diff.FunctionDelta, 0, opts.MaxNodes)
	for _, f := range rep.Functions {
		if f.FlatDelta <= 0 && f.CumDelta <= 0 {
			continue
		}
		top = append(top, f)
		if len(top) == opts.MaxNodes {
			break
		}
	}
	if len(top) == 0 {
		return renderEmpty(w, opts.Title)
	}

	// Build node table.
	nodes := make(map[string]*node, len(top))
	list := make([]*node, 0, len(top))
	for i, f := range top {
		n := &node{
			id:    i,
			name:  f.Function,
			short: shortName(f.Function),
			flat:  f.FlatDelta,
			cum:   f.CumDelta,
			file:  f.File,
			line:  f.Line,
		}
		nodes[f.Function] = n
		list = append(list, n)
	}

	// Filter edges to those between drawn nodes, above threshold.
	edges := make([]edge, 0)
	for _, e := range rep.Edges {
		if e.Weight < opts.MinEdgeBytes {
			continue
		}
		a, okA := nodes[e.Caller]
		b, okB := nodes[e.Callee]
		if !okA || !okB {
			continue
		}
		edges = append(edges, edge{a, b, e.Weight})
	}

	// Layered layout via longest-path from each root (nodes with no incoming edge in our subset).
	assignLayers(list, edges)

	// Position nodes: group by layer, evenly spaced.
	byLayer := map[int][]*node{}
	maxLayer := 0
	for _, n := range list {
		byLayer[n.layer] = append(byLayer[n.layer], n)
		if n.layer > maxLayer {
			maxLayer = n.layer
		}
	}
	// Sort each layer by decreasing flat so worst offenders are centered/top.
	for _, ns := range byLayer {
		sort.Slice(ns, func(i, j int) bool { return ns[i].flat > ns[j].flat })
	}

	const (
		nodeW       = 260.0
		nodeH       = 62.0
		hGap        = 90.0
		vGap        = 40.0
		marginX     = 40.0
		marginY     = 90.0
	)

	// Compute canvas size.
	widestLayer := 0
	for _, ns := range byLayer {
		if len(ns) > widestLayer {
			widestLayer = len(ns)
		}
	}
	canvasW := marginX*2 + float64(widestLayer)*(nodeW+hGap) - hGap
	canvasH := marginY + float64(maxLayer+1)*(nodeH+vGap) + 60

	for layer := 0; layer <= maxLayer; layer++ {
		ns := byLayer[layer]
		rowW := float64(len(ns))*(nodeW+hGap) - hGap
		startX := (canvasW - rowW) / 2
		y := marginY + float64(layer)*(nodeH+vGap)
		for i, n := range ns {
			n.x = startX + float64(i)*(nodeW+hGap)
			n.y = y
			n.w = nodeW
			n.h = nodeH
		}
	}

	// Determine max delta for color scaling.
	var maxFlat int64
	for _, n := range list {
		if n.flat > maxFlat {
			maxFlat = n.flat
		}
	}
	var maxEdge int64
	for _, e := range edges {
		if e.weight > maxEdge {
			maxEdge = e.weight
		}
	}

	// Begin SVG.
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %.0f %.0f" width="%.0f" height="%.0f" font-family="ui-sans-serif,system-ui,-apple-system,Segoe UI,Roboto,sans-serif">
`, canvasW, canvasH, canvasW, canvasH)

	// Defs: arrow marker.
	fmt.Fprint(w, `<defs>
  <marker id="arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
    <path d="M0,0 L10,5 L0,10 z" fill="#7a1f1f"/>
  </marker>
</defs>
`)

	// Background + header.
	fmt.Fprintf(w, `<rect width="100%%" height="100%%" fill="#fafafa"/>
<text x="%.0f" y="42" font-size="22" font-weight="700" fill="#111">%s</text>
<text x="%.0f" y="66" font-size="13" fill="#555">Total delta: %s   ·   Nodes: %d   ·   Edges: %d</text>
`, marginX, html.EscapeString(opts.Title), marginX, humanBytes(rep.TotalDelta), len(list), len(edges))

	// Edges.
	for _, e := range edges {
		// Draw a smooth cubic Bezier from caller bottom center to callee top center.
		x1 := e.from.x + e.from.w/2
		y1 := e.from.y + e.from.h
		x2 := e.to.x + e.to.w/2
		y2 := e.to.y
		cy := (y1 + y2) / 2
		width := 1.0 + 4.0*float64(e.weight)/float64(maxEdge+1)
		opacity := 0.35 + 0.55*float64(e.weight)/float64(maxEdge+1)
		fmt.Fprintf(w, `<path d="M%.1f,%.1f C%.1f,%.1f %.1f,%.1f %.1f,%.1f" fill="none" stroke="#7a1f1f" stroke-width="%.2f" stroke-opacity="%.2f" marker-end="url(#arrow)"/>`,
			x1, y1, x1, cy, x2, cy, x2, y2, width, opacity)
		// Weight label.
		midX := (x1 + x2) / 2
		midY := cy
		fmt.Fprintf(w, `<text x="%.1f" y="%.1f" font-size="10" fill="#7a1f1f" text-anchor="middle">%s</text>`,
			midX, midY, humanBytes(e.weight))
	}

	// Nodes.
	for _, n := range list {
		ratio := 0.0
		if maxFlat > 0 {
			ratio = float64(n.flat) / float64(maxFlat)
		}
		fill := heatColor(ratio)
		stroke := "#7a1f1f"
		if ratio < 0.15 {
			stroke = "#888"
		}
		// Flip text color to white when the background is dark.
		titleColor, bodyColor, subColor := "#111", "#222", "#444"
		if ratio > 0.55 {
			titleColor, bodyColor, subColor = "#fff", "#f5f5f5", "#eaeaea"
		}
		fmt.Fprintf(w, `<g>
  <rect x="%.1f" y="%.1f" rx="8" ry="8" width="%.1f" height="%.1f" fill="%s" stroke="%s" stroke-width="1.4"/>
  <text x="%.1f" y="%.1f" font-size="12" font-weight="700" fill="%s">%s</text>
  <text x="%.1f" y="%.1f" font-size="11" fill="%s">flat: %s   cum: %s</text>
  <text x="%.1f" y="%.1f" font-size="10" fill="%s">%s</text>
</g>`,
			n.x, n.y, n.w, n.h, fill, stroke,
			n.x+10, n.y+20, titleColor, html.EscapeString(truncate(n.short, 40)),
			n.x+10, n.y+38, bodyColor, humanBytes(n.flat), humanBytes(n.cum),
			n.x+10, n.y+54, subColor, html.EscapeString(truncate(sourceRef(n.file, n.line), 44)),
		)
	}

	fmt.Fprint(w, "\n</svg>\n")
	return nil
}

func renderEmpty(w io.Writer, title string) error {
	_, err := fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 640 200" width="640" height="200" font-family="system-ui">
  <rect width="100%%" height="100%%" fill="#fafafa"/>
  <text x="24" y="48" font-size="20" font-weight="700" fill="#111">%s</text>
  <text x="24" y="90" font-size="14" fill="#555">No positive inuse_space delta detected between snapshots.</text>
</svg>
`, html.EscapeString(title))
	return err
}

// assignLayers does a simple longest-path layering. Edges point caller→callee
// (i.e. deeper in the call stack). Callers get a lower layer than callees.
func assignLayers(nodes []*node, edges []edge) {
	incoming := make(map[*node]int)
	adj := make(map[*node][]*node)
	for _, n := range nodes {
		incoming[n] = 0
	}
	for _, e := range edges {
		incoming[e.to]++
		adj[e.from] = append(adj[e.from], e.to)
	}
	// Kahn-style with layer assignment.
	queue := []*node{}
	for _, n := range nodes {
		if incoming[n] == 0 {
			n.layer = 0
			queue = append(queue, n)
		}
	}
	// Process nodes in the queue; assign to max(pred.layer)+1.
	inQueue := make(map[*node]bool)
	for _, n := range queue {
		inQueue[n] = true
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, nxt := range adj[cur] {
			if nxt.layer < cur.layer+1 {
				nxt.layer = cur.layer + 1
			}
			incoming[nxt]--
			if incoming[nxt] == 0 && !inQueue[nxt] {
				queue = append(queue, nxt)
				inQueue[nxt] = true
			}
		}
	}
	// Any nodes left with unresolved layers (cycles or disconnected): put on layer 0.
	for _, n := range nodes {
		if n.layer < 0 {
			n.layer = 0
		}
	}
}

// heatColor returns a hex fill from pale yellow (ratio=0) to deep red (ratio=1).
func heatColor(ratio float64) string {
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	// Interp: (255,247,236) → (127,0,0)
	r := int(math.Round(255 + (127-255)*ratio))
	g := int(math.Round(247 + (0-247)*ratio))
	b := int(math.Round(236 + (0-236)*ratio))
	return fmt.Sprintf("#%02x%02x%02x", r, g, b)
}

// shortName strips leading package paths so nodes render legibly.
// "github.com/foo/bar/pkg.(*Type).Method" → "pkg.(*Type).Method"
func shortName(full string) string {
	// Find the last '/'
	idx := strings.LastIndex(full, "/")
	if idx == -1 {
		return full
	}
	return full[idx+1:]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func sourceRef(file string, line int64) string {
	if file == "" {
		return ""
	}
	// Only keep the last two path components for readability.
	parts := strings.Split(file, "/")
	if len(parts) > 2 {
		file = strings.Join(parts[len(parts)-2:], "/")
	}
	if line > 0 {
		return fmt.Sprintf("%s:%d", file, line)
	}
	return file
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
