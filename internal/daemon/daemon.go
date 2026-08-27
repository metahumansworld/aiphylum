// Package daemon is dungeond's control plane: the HTTP API through which
// agents enter the arena and episodes are started, watched, and read back.
// dungeonctl is its client.
//
// The API is deliberately small and local — it binds to loopback and trusts
// its caller the way any local daemon socket does. Multi-machine operation,
// authentication, and user-funded intake are later phases.
//
// Two rules the handlers enforce everywhere:
//
//   - An agent must have a runnable image before it has a wallet. The step
//     runner treats a missing image as a platform fault and voids the attempt,
//     so a walleted agent with no image would livelock every bounty it wins —
//     win, void, re-open, forever. Intake therefore binds and checks the image
//     first, and only then lets the orchestrator mint a wallet.
//
//   - One episode at a time, and nothing else mutates the world while it runs.
//     The orchestrator's roster is owned by the episode goroutine for the
//     duration, so registration during a run is refused rather than raced.
//
// The world is persistent across episodes: an unsolved bounty stays on the
// board and re-enters auction in the next episode, and bankrupt agents stay
// dead. That is the intended shape of a standing arena, not leakage.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/metahunmei/dungeon/internal/ledger"
	"github.com/metahunmei/dungeon/internal/orchestrator"
	"github.com/metahunmei/dungeon/internal/runner"
)

// Config wires a Server. Orch, Bind, and TracePath are required.
type Config struct {
	Orch *orchestrator.Orchestrator
	// Bind attaches an agent's container spec to the step runner. Called
	// before the orchestrator mints the wallet, so a failed registration
	// leaves an inert binding rather than a walleted agent with no image.
	Bind func(id string, spec orchestrator.ContainerAgent)
	// CheckImage vets an image at intake (nil accepts anything — tests).
	// The live daemon passes runner.ImageExists.
	CheckImage func(ctx context.Context, image string) error
	// TracePath is the trace file /v1/trace serves and follows.
	TracePath string
	// RunCtx bounds episode runs. An episode is started by a request but must
	// outlive it, so it runs under this context — the daemon's own lifetime —
	// not the request's. Nil means context.Background().
	RunCtx context.Context
	Log    *slog.Logger
}

// Server is the control plane. It is an http.Handler; serve it wherever the
// daemon listens.
type Server struct {
	cfg Config
	mux *http.ServeMux

	mu       sync.Mutex
	agents   []*agentRecord
	episodes []*episodeRecord
	running  bool
	// broken latches the error of a failed episode. The orchestrator halts an
	// episode only for platform-serious reasons — a conservation failure above
	// all — and a world whose money may be wrong must stop accepting work, not
	// shrug and take the next request. Cleared only by restarting the daemon.
	broken error
}

type agentRecord struct {
	ID    string
	Image string
	Grant ledger.Credits
}

type episodeRecord struct {
	ID       string
	Seed     int64
	Rounds   int
	Postings int
	State    string // "running", "done", "failed"
	Err      string
	Started  time.Time
	Ended    time.Time
}

func New(cfg Config) *Server {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.RunCtx == nil {
		cfg.RunCtx = context.Background()
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /v1/status", s.handleStatus)
	s.mux.HandleFunc("GET /v1/agents", s.handleAgentsList)
	s.mux.HandleFunc("POST /v1/agents", s.handleAgentSubmit)
	s.mux.HandleFunc("GET /v1/episodes", s.handleEpisodesList)
	s.mux.HandleFunc("POST /v1/episodes", s.handleEpisodeRun)
	s.mux.HandleFunc("GET /v1/episodes/{id}", s.handleEpisodeGet)
	s.mux.HandleFunc("GET /v1/trace", s.handleTrace)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// ── wire shapes ─────────────────────────────────────────────────────────────

// AgentView is one roster row as the API reports it. Balance and Retired come
// from the ledger at read time — the ledger is the authority on both, and
// reading it avoids touching the orchestrator's roster while a run owns it.
type AgentView struct {
	ID      string         `json:"id"`
	Image   string         `json:"image"`
	Grant   ledger.Credits `json:"grant"`
	Balance ledger.Credits `json:"balance"`
	Retired bool           `json:"retired"`
}

// SubmitRequest registers one agent: an image to run and a signing grant.
type SubmitRequest struct {
	ID     string   `json:"id"`
	Image  string   `json:"image"`
	Cmd    []string `json:"cmd,omitempty"`
	Grant  int64    `json:"grant"`
	Memory string   `json:"memory,omitempty"`
	CPUs   string   `json:"cpus,omitempty"`
	// Mounts are host:container pairs, mounted read-only.
	Mounts []MountView `json:"mounts,omitempty"`
}

type MountView struct {
	Host      string `json:"host"`
	Container string `json:"container"`
}

// EpisodeRequest starts one episode. The posting plan is derived from the
// seed and the board's generator registry — same seed, same plan.
type EpisodeRequest struct {
	Seed         int64 `json:"seed"`
	Rounds       int   `json:"rounds"`
	WallClockSec int   `json:"wall_clock_sec,omitempty"`
	TokenCeiling int64 `json:"token_ceiling,omitempty"`
}

// EpisodeView is one episode's record as the API reports it.
type EpisodeView struct {
	ID       string `json:"id"`
	Seed     int64  `json:"seed"`
	Rounds   int    `json:"rounds"`
	Postings int    `json:"postings"`
	State    string `json:"state"`
	Error    string `json:"error,omitempty"`
	Started  string `json:"started"`
	Ended    string `json:"ended,omitempty"`
}

// StatusView is the daemon's one-look summary.
type StatusView struct {
	State        string `json:"state"` // "idle", "running", "broken"
	Agents       int    `json:"agents"`
	Episodes     int    `json:"episodes"`
	Conservation string `json:"conservation,omitempty"`
	Error        string `json:"error,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, format string, args ...any) {
	writeJSON(w, code, map[string]string{"error": fmt.Sprintf(format, args...)})
}

// ── handlers ────────────────────────────────────────────────────────────────

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	st := StatusView{State: "idle", Agents: len(s.agents), Episodes: len(s.episodes)}
	if s.running {
		st.State = "running"
	}
	if s.broken != nil {
		st.State, st.Error = "broken", s.broken.Error()
	}
	s.mu.Unlock()

	if con, err := s.cfg.Orch.Ledger.Conservation(r.Context()); err == nil {
		st.Conservation = con.String()
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleAgentsList(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	recs := make([]*agentRecord, len(s.agents))
	copy(recs, s.agents)
	s.mu.Unlock()

	out := make([]AgentView, 0, len(recs))
	for _, rec := range recs {
		v := AgentView{ID: rec.ID, Image: rec.Image, Grant: rec.Grant}
		if acct, err := s.cfg.Orch.Ledger.Get(r.Context(), rec.ID); err == nil {
			v.Balance, v.Retired = acct.Balance, acct.Closed
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAgentSubmit(w http.ResponseWriter, r *http.Request) {
	var req SubmitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad request body: %v", err)
		return
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, "agent id is required")
		return
	}
	if req.Image == "" {
		// The livelock rule: no image, no wallet.
		writeError(w, http.StatusBadRequest, "agent %s has no image — an agent must arrive with a runnable container", req.ID)
		return
	}
	if req.Grant <= 0 {
		writeError(w, http.StatusBadRequest, "grant must be positive — a broke agent is dead on arrival")
		return
	}
	if s.cfg.CheckImage != nil {
		if err := s.cfg.CheckImage(r.Context(), req.Image); err != nil {
			writeError(w, http.StatusBadRequest, "image check failed: %v", err)
			return
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken != nil {
		writeError(w, http.StatusServiceUnavailable, "world is halted: %v", s.broken)
		return
	}
	if s.running {
		// The orchestrator's roster belongs to the episode goroutine while a
		// run is on; registration would race it.
		writeError(w, http.StatusConflict, "an episode is running — submit between episodes")
		return
	}

	spec := orchestrator.ContainerAgent{
		Image: req.Image, Cmd: req.Cmd, Memory: req.Memory, CPUs: req.CPUs,
	}
	for _, m := range req.Mounts {
		spec.Mounts = append(spec.Mounts, runner.Mount{Host: m.Host, Container: m.Container})
	}
	// Bind before the wallet exists: if AddAgent refuses, the binding is inert.
	// The reverse order would be the livelock — a walleted agent with no image.
	s.cfg.Bind(req.ID, spec)
	if err := s.cfg.Orch.AddAgent(r.Context(), req.ID, ledger.Credits(req.Grant)); err != nil {
		if errors.Is(err, ledger.ErrAccountExists) {
			writeError(w, http.StatusConflict, "agent id %s is taken (bankrupt IDs are never reused)", req.ID)
			return
		}
		writeError(w, http.StatusInternalServerError, "register agent: %v", err)
		return
	}

	rec := &agentRecord{ID: req.ID, Image: req.Image, Grant: ledger.Credits(req.Grant)}
	s.agents = append(s.agents, rec)
	s.cfg.Log.Info("agent registered", "agent", req.ID, "image", req.Image, "grant", req.Grant)
	writeJSON(w, http.StatusOK, AgentView{
		ID: rec.ID, Image: rec.Image, Grant: rec.Grant, Balance: rec.Grant,
	})
}

func (s *Server) handleEpisodesList(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	out := make([]EpisodeView, 0, len(s.episodes))
	for _, rec := range s.episodes {
		out = append(out, rec.view())
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleEpisodeGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.episodes {
		if rec.ID == id {
			writeJSON(w, http.StatusOK, rec.view())
			return
		}
	}
	writeError(w, http.StatusNotFound, "no episode %s", id)
}

func (rec *episodeRecord) view() EpisodeView {
	v := EpisodeView{
		ID: rec.ID, Seed: rec.Seed, Rounds: rec.Rounds, Postings: rec.Postings,
		State: rec.State, Error: rec.Err, Started: rec.Started.Format(time.RFC3339),
	}
	if !rec.Ended.IsZero() {
		v.Ended = rec.Ended.Format(time.RFC3339)
	}
	return v
}

func (s *Server) handleEpisodeRun(w http.ResponseWriter, r *http.Request) {
	var req EpisodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad request body: %v", err)
		return
	}
	if req.Rounds < 1 || req.Rounds > 99 {
		writeError(w, http.StatusBadRequest, "rounds must be in 1..99, got %d", req.Rounds)
		return
	}
	if req.WallClockSec == 0 {
		req.WallClockSec = 60
	}
	if req.WallClockSec < 1 || req.WallClockSec > 600 {
		writeError(w, http.StatusBadRequest, "wall_clock_sec must be in 1..600, got %d", req.WallClockSec)
		return
	}

	// The plan is derived outside the lock — probing generators can do real
	// work — and an empty registry is a refusal, not an empty episode.
	ep, note := derivePlan(s.cfg.Orch.Board, req)
	if len(ep.Rounds) == 0 || len(ep.Rounds[0]) == 0 {
		writeError(w, http.StatusBadRequest, "no usable generators on the board (%s)", note)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken != nil {
		writeError(w, http.StatusServiceUnavailable, "world is halted: %v", s.broken)
		return
	}
	if s.running {
		writeError(w, http.StatusConflict, "an episode is already running")
		return
	}
	live := 0
	for _, rec := range s.agents {
		if acct, err := s.cfg.Orch.Ledger.Get(r.Context(), rec.ID); err == nil && !acct.Closed {
			live++
		}
	}
	if live == 0 {
		writeError(w, http.StatusBadRequest, "no live agents — submit at least one first")
		return
	}

	rec := &episodeRecord{
		ID:   fmt.Sprintf("ep%d", len(s.episodes)+1),
		Seed: req.Seed, Rounds: req.Rounds, Postings: countPostings(ep),
		State: "running", Started: time.Now(),
	}
	s.episodes = append(s.episodes, rec)
	s.running = true
	s.cfg.Log.Info("episode starting", "episode", rec.ID, "seed", rec.Seed, "rounds", rec.Rounds, "postings", rec.Postings, "note", note)

	go s.runEpisode(rec, ep)
	writeJSON(w, http.StatusAccepted, rec.view())
}

// runEpisode owns the world for the duration of one run. It is the only
// goroutine that touches the orchestrator while running is true — the
// handlers' 409s are what make that ownership real.
func (s *Server) runEpisode(rec *episodeRecord, ep orchestrator.Episode) {
	// Fresh attempt-wallet namespace and a clean fault tracker; without this,
	// a bounty failed last episode and re-awarded at the same round number
	// this episode would collide with its own retired attempt wallet.
	s.cfg.Orch.NextEpoch()
	err := s.cfg.Orch.RunEpisode(s.cfg.RunCtx, ep)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = false
	rec.Ended = time.Now()
	if err != nil {
		rec.State, rec.Err = "failed", err.Error()
		s.broken = fmt.Errorf("episode %s: %w", rec.ID, err)
		s.cfg.Log.Error("episode failed — world halted", "episode", rec.ID, "err", err)
		return
	}
	rec.State = "done"
	s.cfg.Log.Info("episode done", "episode", rec.ID)
}

// tailInterval matches web.NewLiveServer's poll cadence: the trace writer
// flushes per line, so a quarter second is fresh enough to watch.
const tailInterval = 250 * time.Millisecond

func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request) {
	f, err := os.Open(s.cfg.TracePath)
	if err != nil {
		writeError(w, http.StatusNotFound, "trace not readable: %v", err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/x-ndjson")

	if r.URL.Query().Get("follow") != "1" {
		io.Copy(w, f)
		return
	}
	fl, _ := w.(http.Flusher)
	for {
		if n, err := io.Copy(w, f); err != nil {
			return
		} else if n > 0 && fl != nil {
			fl.Flush()
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(tailInterval):
		}
	}
}
