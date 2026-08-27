package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sync"

	"github.com/metahunmei/dungeon/internal/ledger"
	"github.com/metahunmei/dungeon/internal/trace"
)

//go:embed templates static
var content embed.FS

// Server serves one episode. It is an http.Handler; everything it renders
// comes from the trace it was built with. A static server (NewServer) holds a
// finished trace and never re-reads it; a live server (NewLiveServer) follows
// the file as an episode writes it.
type Server struct {
	mux  *http.ServeMux
	tmpl *template.Template
	path string
	live bool

	mu     sync.Mutex
	lines  []trace.Line // every complete line consumed so far
	offset int64        // byte offset just past the last complete line
	view   *View
	replay template.JS // the replay viewer's event stream, marshalled per build
}

// NewServer builds the view from a finished trace and wires the routes.
func NewServer(path string, lines []trace.Line) (*Server, error) {
	s, err := newServer(path, false)
	if err != nil {
		return nil, err
	}
	s.lines = lines
	if err := s.rebuildLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

func newServer(path string, live bool) (*Server, error) {
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

	s := &Server{mux: http.NewServeMux(), tmpl: tmpl, path: path, live: live}
	s.mux.HandleFunc("GET /{$}", s.overview)
	s.mux.HandleFunc("GET /agent/{id}", s.agent)
	s.mux.HandleFunc("GET /bounty/{id}", s.bounty)
	s.mux.HandleFunc("GET /replay", s.replayPage)
	s.mux.Handle("GET /static/", http.FileServer(http.FS(content)))
	if live {
		s.mux.HandleFunc("GET /events", s.events)
	}
	return s, nil
}

// rebuildLocked derives view and replay stream from s.lines. Callers hold
// s.mu (construction, before the handler is shared, counts).
func (s *Server) rebuildLocked() error {
	view, err := BuildView(s.path, s.lines)
	if err != nil {
		return err
	}
	view.Live = s.live
	replay, err := replayEvents(s.lines)
	if err != nil {
		return err
	}
	s.view, s.replay = view, replay
	return nil
}

// current returns the view and replay stream, refreshed from the file first
// when the server is live.
func (s *Server) current() (*View, template.JS, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live {
		if err := s.refreshLocked(); err != nil {
			return nil, "", err
		}
	}
	return s.view, s.replay, nil
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
	v, replay, err := s.current()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// A town trace has no board, no ladder and no money: the map is its whole
	// surface, so it is what every route shows.
	if v.Town != nil {
		s.renderTown(w, v, replay)
		return
	}
	s.render(w, "overview.html", v)
}

func (s *Server) renderTown(w http.ResponseWriter, v *View, replay template.JS) {
	s.render(w, "town.html", struct {
		View   *View
		Events template.JS
		Live   bool
	}{v, replay, s.live})
}

func (s *Server) agent(w http.ResponseWriter, r *http.Request) {
	v, _, err := s.current()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a := v.Agent(r.PathValue("id"))
	if a == nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, "agent.html", struct {
		View  *View
		Agent *AgentView
		Spark template.HTML
	}{v, a, sparkline(a.Timeline)})
}

func (s *Server) bounty(w http.ResponseWriter, r *http.Request) {
	v, _, err := s.current()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	b := v.Bounty(r.PathValue("id"))
	if b == nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, "bounty.html", struct {
		View   *View
		Bounty *BountyView
	}{v, b})
}

func (s *Server) replayPage(w http.ResponseWriter, r *http.Request) {
	v, replay, err := s.current()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if v.Town != nil {
		s.renderTown(w, v, replay)
		return
	}
	s.render(w, "replay.html", struct {
		View   *View
		Events template.JS
		Live   bool
	}{v, replay, s.live})
}

// cleanEvent flattens one trace line for the viewer: its payload plus seq,
// type and a pre-worded label, with model-call bodies and per-payload clocks
// stripped. Both the embedded replay stream and the live event feed go
// through here, so the two surfaces can never diverge on what leaks.
func cleanEvent(l trace.Line) (map[string]any, error) {
	var p map[string]any
	if err := json.Unmarshal(l.Payload, &p); err != nil {
		return nil, fmt.Errorf("trace seq %d: %w", l.Seq, err)
	}
	delete(p, "request")
	delete(p, "response")
	delete(p, "time") // the stream is ordered by seq; per-payload clocks are noise here
	cleaned, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("trace seq %d: %w", l.Seq, err)
	}
	p["seq"] = l.Seq
	p["type"] = string(l.Type)
	// Word the label from the cleaned payload, not the original line, so
	// call bodies don't bleed into the log.
	p["label"] = trace.Summary(trace.Line{Type: l.Type, Payload: cleaned})
	return p, nil
}

// replayEvents marshals the viewer's whole event stream for embedding.
func replayEvents(lines []trace.Line) (template.JS, error) {
	events := make([]map[string]any, 0, len(lines))
	for _, l := range lines {
		p, err := cleanEvent(l)
		if err != nil {
			return "", err
		}
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
