// Package server implements the web dashboard.
package server

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Config is the dashboard configuration.
type Config struct {
	ProfileDir string
	ReportDir  string
	Addr       string
}

// Server serves the dashboard.
type Server struct {
	cfg Config
	tpl *template.Template
}

// New constructs a dashboard server.
func New(cfg Config) *Server {
	tpl := template.Must(template.New("dash").Funcs(template.FuncMap{
		"human": humanBytes,
	}).Parse(indexHTML))
	return &Server{cfg: cfg, tpl: tpl}
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/state", s.handleState)
	mux.Handle("/reports/", http.StripPrefix("/reports/", http.FileServer(http.Dir(s.cfg.ReportDir))))
	mux.Handle("/profiles/", http.StripPrefix("/profiles/", http.FileServer(http.Dir(s.cfg.ProfileDir))))
	return mux
}

// ListenAndServe starts the dashboard.
func (s *Server) ListenAndServe() error {
	fmt.Printf("gomem-dashboard listening on http://%s\n", s.cfg.Addr)
	fmt.Printf("  profiles dir: %s\n", s.cfg.ProfileDir)
	fmt.Printf("  reports  dir: %s\n", s.cfg.ReportDir)
	return http.ListenAndServe(s.cfg.Addr, s.Handler())
}

type snapshotView struct {
	Name    string
	Path    string
	Size    int64
	ModTime time.Time
}

type reportView struct {
	Name        string
	SVG         string
	MD          string
	JSON        string
	TotalDelta  int64
	GeneratedAt time.Time
	Top         []topRow
}

type topRow struct {
	Rank     int
	Function string
	Flat     int64
	Cum      int64
	Source   string
}

func (s *Server) collect() ([]snapshotView, []reportView, error) {
	// Snapshots
	var snaps []snapshotView
	entries, err := os.ReadDir(s.cfg.ProfileDir)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if filepath.Ext(name) != ".gz" {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			snaps = append(snaps, snapshotView{
				Name:    name,
				Path:    "/profiles/" + name,
				Size:    info.Size(),
				ModTime: info.ModTime(),
			})
		}
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].Name < snaps[j].Name })

	// Reports: any .json in reports dir with matching svg/md.
	var reps []reportView
	entries, err = os.ReadDir(s.cfg.ReportDir)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
				continue
			}
			b, err := os.ReadFile(filepath.Join(s.cfg.ReportDir, e.Name()))
			if err != nil {
				continue
			}
			var bundle struct {
				GeneratedAt time.Time `json:"generated_at"`
				TotalDelta  int64     `json:"total_inuse_delta_bytes"`
				Top         []struct {
					Function  string `json:"function"`
					File      string `json:"file"`
					Line      int64  `json:"line"`
					FlatDelta int64  `json:"flat_delta"`
					CumDelta  int64  `json:"cum_delta"`
				} `json:"top_functions"`
			}
			if err := json.Unmarshal(b, &bundle); err != nil {
				continue
			}
			base := strings.TrimSuffix(e.Name(), ".json")
			rv := reportView{
				Name:        base,
				JSON:        "/reports/" + e.Name(),
				SVG:         "/reports/" + base + ".svg",
				MD:          "/reports/" + base + ".md",
				TotalDelta:  bundle.TotalDelta,
				GeneratedAt: bundle.GeneratedAt,
			}
			for i, t := range bundle.Top {
				src := "—"
				if t.File != "" {
					src = fmt.Sprintf("%s:%d", filepath.Base(t.File), t.Line)
				}
				rv.Top = append(rv.Top, topRow{
					Rank:     i + 1,
					Function: t.Function,
					Flat:     t.FlatDelta,
					Cum:      t.CumDelta,
					Source:   src,
				})
			}
			reps = append(reps, rv)
		}
	}
	sort.Slice(reps, func(i, j int) bool { return reps[i].Name < reps[j].Name })
	return snaps, reps, nil
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	snaps, reps, err := s.collect()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := struct {
		Snapshots []snapshotView
		Reports   []reportView
		Now       time.Time
	}{snaps, reps, time.Now()}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.Execute(w, data); err != nil {
		fmt.Fprintln(w, err.Error())
	}
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	snaps, reps, err := s.collect()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"snapshots": snaps,
		"reports":   reps,
	})
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

const indexHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>gomem-dashboard</title>
<style>
  :root { color-scheme: light dark; }
  body { font-family: ui-sans-serif, system-ui, -apple-system, Segoe UI, Roboto, sans-serif; margin: 0; background: #0e1116; color: #e6edf3; }
  header { padding: 20px 32px; background: #161b22; border-bottom: 1px solid #30363d; }
  header h1 { margin: 0; font-size: 20px; }
  header p { margin: 4px 0 0; color: #8b949e; font-size: 13px; }
  main { padding: 24px 32px; display: grid; grid-template-columns: 320px 1fr; gap: 24px; }
  section h2 { font-size: 14px; text-transform: uppercase; letter-spacing: 0.06em; color: #8b949e; margin: 0 0 12px; }
  .panel { background: #161b22; border: 1px solid #30363d; border-radius: 8px; padding: 14px 16px; margin-bottom: 16px; }
  .snap { display: flex; justify-content: space-between; font-size: 12px; padding: 4px 0; border-bottom: 1px dashed #30363d; }
  .snap:last-child { border-bottom: none; }
  .snap code { color: #d2a8ff; }
  .report { background: #161b22; border: 1px solid #30363d; border-radius: 8px; padding: 16px 20px; margin-bottom: 20px; }
  .report h3 { margin: 0 0 6px; font-size: 15px; }
  .badge { display: inline-block; padding: 2px 8px; border-radius: 999px; font-size: 12px; font-weight: 600; background: #7a1f1f; color: #fff; margin-left: 8px; }
  .badge.zero { background: #2ea043; }
  .meta { color: #8b949e; font-size: 12px; margin-bottom: 12px; }
  .meta a { color: #79c0ff; margin-right: 12px; }
  table { width: 100%; border-collapse: collapse; font-size: 13px; margin-top: 8px; }
  th, td { text-align: left; padding: 6px 10px; border-bottom: 1px solid #21262d; }
  th { color: #8b949e; font-weight: 600; }
  td code { color: #ffa657; }
  .svg-wrap { margin-top: 14px; overflow-x: auto; background: #fff; border-radius: 8px; padding: 8px; }
  .empty { color: #8b949e; font-style: italic; }
</style>
</head>
<body>
<header>
  <h1>gomem-dashboard</h1>
  <p>Differential heap analysis for Go processors · rendered {{.Now.Format "2006-01-02 15:04:05 MST"}}</p>
</header>
<main>
  <aside>
    <section class="panel">
      <h2>Snapshots ({{len .Snapshots}})</h2>
      {{if .Snapshots}}
        {{range .Snapshots}}
          <div class="snap"><code>{{.Name}}</code><span>{{human .Size}}</span></div>
        {{end}}
      {{else}}
        <p class="empty">No heap profiles captured yet.</p>
      {{end}}
    </section>
  </aside>
  <section>
    <h2>Diff reports</h2>
    {{if .Reports}}
      {{range .Reports}}
        <div class="report">
          <h3>{{.Name}}
            {{if gt .TotalDelta 0}}<span class="badge">+{{human .TotalDelta}}</span>
            {{else if lt .TotalDelta 0}}<span class="badge zero">{{human .TotalDelta}}</span>
            {{else}}<span class="badge zero">no change</span>{{end}}
          </h3>
          <div class="meta">
            Generated {{.GeneratedAt.Format "2006-01-02 15:04:05"}} ·
            <a href="{{.SVG}}" target="_blank">SVG</a>
            <a href="{{.MD}}" target="_blank">Markdown</a>
            <a href="{{.JSON}}" target="_blank">JSON</a>
          </div>
          {{if .Top}}
          <table>
            <thead><tr><th>#</th><th>Function</th><th>Flat Δ</th><th>Cum Δ</th><th>Source</th></tr></thead>
            <tbody>
              {{range .Top}}
                <tr><td>{{.Rank}}</td><td><code>{{.Function}}</code></td><td>{{human .Flat}}</td><td>{{human .Cum}}</td><td><code>{{.Source}}</code></td></tr>
              {{end}}
            </tbody>
          </table>
          {{end}}
          <div class="svg-wrap"><object data="{{.SVG}}" type="image/svg+xml" width="100%"></object></div>
        </div>
      {{end}}
    {{else}}
      <p class="empty">No diff reports yet — capture at least two snapshots and run <code>gomem diff</code>.</p>
    {{end}}
  </section>
</main>
</body>
</html>`
