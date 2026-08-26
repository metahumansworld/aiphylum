package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"

	"github.com/metahunmei/dungeon/internal/ledger"
	"github.com/metahunmei/dungeon/internal/trace"
)

//go:embed templates static
var content embed.FS

// Server serves one episode. It is an http.Handler; everything it renders
// comes from the trace it was built with.
type Server struct {
	mux    *http.ServeMux
	view   *View
	tmpl   *template.Template
	replay template.JS // the replay viewer's event stream, marshalled once
}

// NewServer builds the view from a trace and wires the routes.
func NewServer(path string, lines []trace.Line) (*Server, error) {
	view, err := BuildView(path, lines)
	if err != nil {
		return nil, err
	}

	tmpl, err := template.New("").Funcs(template.FuncMap{
		"eff":  func(f float64) string { return fmt.Sprintf("%.2f", f) },
		"add1": func(i int) int { return i + 1 },
		// cr: a bare credit amount for dense table cells. Cards and badges use
		// the full Credits.String form with the USD conversion; repeating it in
		// every cell drowns the numbers.
		"cr": func(c ledger.Credits) int64 { return int64(c) },
		// laddered: whether an agent has a ladder row at all. Ranked rows sort
		// first, so an unranked or missing agent gets called out under the table.
		"laddered": func(v *View, id string) bool {
			for _, r := range v.Ladder {
				if r.Agent == id {
					return true
				}
			}
			return false
		},
		"statusClass": func(status string) string {
			switch status {
			case "solved", "awarded":
				return "ok"
			case "failed", "voided":
				return "warn"
			default:
				return "open"
			}
		},
	}).ParseFS(content, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}

	replay, err := replayEvents(lines)
	if err != nil {
		return nil, err
	}

	s := &Server{mux: http.NewServeMux(), view: view, tmpl: tmpl, replay: replay}
	s.mux.HandleFunc("GET /{$}", s.overview)
	s.mux.HandleFunc("GET /agent/{id}", s.agent)
	s.mux.HandleFunc("GET /bounty/{id}", s.bounty)
	s.mux.HandleFunc("GET /replay", s.replayPage)
	s.mux.Handle("GET /static/", http.FileServer(http.FS(content)))
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// render executes a page template into a buffer first, so a template error
// becomes a 500 rather than a half-written page.
func (s *Server) render(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, fmt.Sprintf("render %s: %v", name, err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	s.render(w, "overview.html", s.view)
}

func (s *Server) agent(w http.ResponseWriter, r *http.Request) {
	a := s.view.Agent(r.PathValue("id"))
	if a == nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, "agent.html", struct {
		View  *View
		Agent *AgentView
		Spark template.HTML
	}{s.view, a, sparkline(a.Timeline)})
}

func (s *Server) bounty(w http.ResponseWriter, r *http.Request) {
	b := s.view.Bounty(r.PathValue("id"))
	if b == nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, "bounty.html", struct {
		View   *View
		Bounty *BountyView
	}{s.view, b})
}

func (s *Server) replayPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "replay.html", struct {
		View   *View
		Events template.JS
	}{s.view, s.replay})
}

// replayEvents flattens the trace for the viewer's reducer: each event is its
// payload plus seq, type and a pre-worded label. Model-call bodies are
// dropped — the viewer shows the money moving, and the full bytes stay in the
// trace file where replay needs them.
func replayEvents(lines []trace.Line) (template.JS, error) {
	events := make([]map[string]any, 0, len(lines))
	for _, l := range lines {
		var p map[string]any
		if err := json.Unmarshal(l.Payload, &p); err != nil {
			return "", fmt.Errorf("trace seq %d: %w", l.Seq, err)
		}
		delete(p, "request")
		delete(p, "response")
		delete(p, "time") // the stream is ordered by seq; per-payload clocks are noise here
		cleaned, err := json.Marshal(p)
		if err != nil {
			return "", fmt.Errorf("trace seq %d: %w", l.Seq, err)
		}
		p["seq"] = l.Seq
		p["type"] = string(l.Type)
		// Word the label from the cleaned payload, not the original line, so
		// call bodies don't bleed into the log.
		p["label"] = trace.Summary(trace.Line{Type: l.Type, Payload: cleaned})
		events = append(events, p)
	}
	enc, err := json.Marshal(events)
	if err != nil {
		return "", err
	}
	// The stream is embedded in a <script> block; make "</script>" inside
	// string values inert. json.Marshal already escapes < to \u003c by
	// default, which is exactly this protection — asserting it documents why.
	if bytes.Contains(enc, []byte("</")) {
		return "", fmt.Errorf("replay stream contains unescaped markup")
	}
	return template.JS(enc), nil
}

// sparkline renders a balance timeline as a small inline SVG. Server-side so
// the page needs no charting script; the x axis is event order, not time —
// what matters is the sequence of decisions, not the seconds between them.
func sparkline(points []BalancePoint) template.HTML {
	if len(points) < 2 {
		return ""
	}
	const w, h, pad = 640, 120, 6
	maxY := points[0].Balance
	for _, p := range points {
		if p.Balance > maxY {
			maxY = p.Balance
		}
	}
	if maxY == 0 {
		maxY = 1
	}
	x := func(i int) float64 {
		return pad + float64(i)*(w-2*pad)/float64(len(points)-1)
	}
	y := func(b int64) float64 {
		return h - pad - float64(b)*(h-2*pad)/float64(maxY)
	}

	var path bytes.Buffer
	for i, p := range points {
		cmd := 'L'
		if i == 0 {
			cmd = 'M'
		}
		fmt.Fprintf(&path, "%c%.1f %.1f ", cmd, x(i), y(int64(p.Balance)))
	}

	var svg bytes.Buffer
	fmt.Fprintf(&svg, `<svg class="spark" viewBox="0 0 %d %d" role="img" aria-label="balance over the episode">`, w, h)
	fmt.Fprintf(&svg, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" class="spark-zero"/>`, pad, y(0), w-pad, y(0))
	fmt.Fprintf(&svg, `<path d="%s" class="spark-line"/>`, bytes.TrimSpace(path.Bytes()))
	last := points[len(points)-1]
	fmt.Fprintf(&svg, `<circle cx="%.1f" cy="%.1f" r="3" class="spark-dot"/>`, x(len(points)-1), y(int64(last.Balance)))
	svg.WriteString(`</svg>`)
	return template.HTML(svg.String())
}
