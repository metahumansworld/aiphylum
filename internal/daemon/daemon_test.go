package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/metahumansworld/soscitea/internal/bounty"
	"github.com/metahumansworld/soscitea/internal/ledger"
	"github.com/metahumansworld/soscitea/internal/orchestrator"
	"github.com/metahumansworld/soscitea/internal/proxy"
	"github.com/metahumansworld/soscitea/internal/rating"
	"github.com/metahumansworld/soscitea/internal/trace"
)

// mathGen is the test supply: keyed, solvable from the prompt.
type mathGen struct{}

func (mathGen) Name() string { return "math" }
func (mathGen) Generate(seed int64, tier int) (bounty.Task, error) {
	word := fmt.Sprintf("w%d-%d", seed, tier)
	return bounty.Task{Prompt: "say exactly: " + word, Answer: word, ReferenceTokens: 20}, nil
}

// briefGen is judged supply — the daemon's derived plans must skip it.
type briefGen struct{}

func (briefGen) Name() string { return "brief" }
func (briefGen) Generate(seed int64, tier int) (bounty.Task, error) {
	return bounty.Task{Prompt: "write a brief", Rubric: "is it brief", ReferenceTokens: 20}, nil
}

// fickleGen generates at tier 1 (so intake's probe accepts it) and errors at
// tier 2 — a posting-time failure that aborts an episode mid-run.
type fickleGen struct{}

func (fickleGen) Name() string { return "fickle" }
func (fickleGen) Generate(seed int64, tier int) (bounty.Task, error) {
	if tier == 2 {
		return bounty.Task{}, fmt.Errorf("fickle: tier 2 is broken")
	}
	return bounty.Task{Prompt: "easy", Answer: "yes", ReferenceTokens: 20}, nil
}

// scriptSteps is the in-process stand-in for the Docker runner: every agent
// bids the reserve on every open bounty and submits the answer read from the
// prompt — or a wrong one, while wrong is set. A non-nil gate makes steps
// block until it closes, so tests can hold an episode mid-run.
type scriptSteps struct {
	mu    sync.Mutex
	wrong bool
	gate  chan struct{}
	bound map[string]orchestrator.ContainerAgent
}

func newScriptSteps() *scriptSteps {
	return &scriptSteps{bound: map[string]orchestrator.ContainerAgent{}}
}

func (f *scriptSteps) setWrong(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wrong = v
}

func (f *scriptSteps) setGate(ch chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gate = ch
}

func (f *scriptSteps) bind(id string, spec orchestrator.ContainerAgent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bound[id] = spec
}

func (f *scriptSteps) boundImage(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bound[id].Image
}

func (f *scriptSteps) RunStep(ctx context.Context, req orchestrator.StepRequest) (orchestrator.StepResult, error) {
	f.mu.Lock()
	gate, wrong := f.gate, f.wrong
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return orchestrator.StepResult{}, ctx.Err()
		}
	}

	var in orchestrator.StepInput
	if err := json.Unmarshal(req.Input, &in); err != nil {
		return orchestrator.StepResult{}, err
	}
	var acts []orchestrator.Action
	switch in.Observation.Phase {
	case orchestrator.PhaseBid:
		for _, b := range in.Observation.Bounties {
			acts = append(acts, orchestrator.Action{Type: orchestrator.ActionBid, Bounty: b.ID, Price: b.Reserve})
		}
	case orchestrator.PhaseAttempt:
		ans := strings.TrimPrefix(in.Observation.Task.Prompt, "say exactly: ")
		if wrong {
			ans = "not-it"
		}
		acts = append(acts, orchestrator.Action{Type: orchestrator.ActionSubmit, Bounty: in.Observation.Task.BountyID, Answer: ans})
	}
	env, err := json.Marshal(map[string][]orchestrator.Action{"actions": acts})
	if err != nil {
		return orchestrator.StepResult{}, err
	}
	return orchestrator.StepResult{Stdout: orchestrator.ActionsSentinel + string(env) + "\n"}, nil
}

// newTestServer wires a full daemon world: real ledger, board, trace, and
// orchestrator; scripted steps instead of Docker; an image checker that
// rejects "missing:latest".
func newTestServer(t *testing.T, extra ...bounty.Generator) (*Server, *scriptSteps) {
	t.Helper()
	dir := t.TempDir()
	return newTestServerAt(t, filepath.Join(dir, "ledger.db"), filepath.Join(dir, "trace.jsonl"), extra...)
}

// newTestServerAt builds a daemon over ledger and trace files the caller names
// and resumes whatever world they hold. Pointing a second server at the first
// one's files is what a daemon restart is; the roster lives beside the ledger.
func newTestServerAt(t *testing.T, dbPath, tracePath string, extra ...bounty.Generator) (*Server, *scriptSteps) {
	t.Helper()
	srv, steps, err := openServerAt(t, dbPath, tracePath, extra...)
	if err != nil {
		t.Fatal(err)
	}
	return srv, steps
}

func openServerAt(t *testing.T, dbPath, tracePath string, extra ...bounty.Generator) (*Server, *scriptSteps, error) {
	t.Helper()
	led, err := ledger.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { led.Close() })

	tw, err := trace.OpenWriter(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tw.Close() })

	board := bounty.NewBoard()
	board.RegisterGenerator(mathGen{})
	board.RegisterGenerator(briefGen{})
	for _, g := range extra {
		board.RegisterGenerator(g)
	}

	steps := newScriptSteps()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	orch := orchestrator.New(led, board, proxy.NewPriceTable(), &proxy.StubProvider{},
		tw, steps, rating.New(rating.Config{MinAttempts: 1, MinTiers: 1}),
		orchestrator.Config{Dust: 30}, log)

	srv := New(Config{
		Orch: orch,
		Bind: steps.bind,
		CheckImage: func(_ context.Context, image string) error {
			if image == "missing:latest" {
				return fmt.Errorf("no such image")
			}
			return nil
		},
		TracePath:  tracePath,
		RosterPath: filepath.Join(filepath.Dir(dbPath), "roster.json"),
		Log:        log,
	})
	return srv, steps, srv.Resume(context.Background())
}

func do(t *testing.T, srv *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(buf)
	}
	req := httptest.NewRequest(method, path, rd)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func submit(t *testing.T, srv *Server, id string, grant int64) {
	t.Helper()
	rec := do(t, srv, "POST", "/v1/agents", SubmitRequest{ID: id, Image: "agent:latest", Grant: grant})
	if rec.Code != http.StatusOK {
		t.Fatalf("submit %s: %d %s", id, rec.Code, rec.Body.String())
	}
}

// waitIdle polls status until no episode is running, returning the final view.
func waitIdle(t *testing.T, srv *Server) StatusView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st := decode[StatusView](t, do(t, srv, "GET", "/v1/status", nil))
		if st.State != "running" {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("episode never finished")
	return StatusView{}
}

func TestSubmitValidation(t *testing.T) {
	srv, steps := newTestServer(t)

	for name, req := range map[string]SubmitRequest{
		"no image":      {ID: "a", Grant: 100},
		"missing image": {ID: "a", Image: "missing:latest", Grant: 100},
		"no grant":      {ID: "a", Image: "agent:latest"},
		"no id":         {Image: "agent:latest", Grant: 100},
	} {
		if rec := do(t, srv, "POST", "/v1/agents", req); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400 (%s)", name, rec.Code, rec.Body.String())
		}
	}

	rec := do(t, srv, "POST", "/v1/agents", SubmitRequest{ID: "alpha", Image: "agent:latest", Grant: 2500})
	if rec.Code != http.StatusOK {
		t.Fatalf("valid submit: %d %s", rec.Code, rec.Body.String())
	}
	if v := decode[AgentView](t, rec); v.Balance != 2500 {
		t.Errorf("balance after submit: %d, want 2500", v.Balance)
	}
	if img := steps.boundImage("alpha"); img != "agent:latest" {
		t.Errorf("bound image: %q, want agent:latest", img)
	}

	if rec := do(t, srv, "POST", "/v1/agents", SubmitRequest{ID: "alpha", Image: "agent:latest", Grant: 100}); rec.Code != http.StatusConflict {
		t.Errorf("duplicate id: got %d, want 409", rec.Code)
	}

	roster := decode[[]AgentView](t, do(t, srv, "GET", "/v1/agents", nil))
	if len(roster) != 1 || roster[0].ID != "alpha" || roster[0].Balance != 2500 || roster[0].Retired {
		t.Errorf("roster: %+v", roster)
	}
}

func TestEpisodeRefusals(t *testing.T) {
	srv, _ := newTestServer(t)

	if rec := do(t, srv, "POST", "/v1/episodes", EpisodeRequest{Seed: 1, Rounds: 1}); rec.Code != http.StatusBadRequest {
		t.Errorf("no agents: got %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	submit(t, srv, "alpha", 2500)
	if rec := do(t, srv, "POST", "/v1/episodes", EpisodeRequest{Seed: 1, Rounds: 0}); rec.Code != http.StatusBadRequest {
		t.Errorf("zero rounds: got %d, want 400", rec.Code)
	}
}

func TestEpisodeLifecycle(t *testing.T) {
	srv, _ := newTestServer(t)
	submit(t, srv, "alpha", 2500)

	rec := do(t, srv, "POST", "/v1/episodes", EpisodeRequest{Seed: 1, Rounds: 2})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("run: %d %s", rec.Code, rec.Body.String())
	}
	ev := decode[EpisodeView](t, rec)
	// The plan uses math only: brief is judged and must be skipped.
	if ev.ID != "ep1" || ev.Postings != 2 {
		t.Fatalf("episode view: %+v (want ep1 with 2 postings)", ev)
	}

	st := waitIdle(t, srv)
	if st.State != "idle" {
		t.Fatalf("state after run: %s (%s)", st.State, st.Error)
	}
	if st.Conservation == "" {
		t.Error("status has no conservation line")
	}

	got := decode[EpisodeView](t, do(t, srv, "GET", "/v1/episodes/ep1", nil))
	if got.State != "done" || got.Ended == "" {
		t.Fatalf("episode record: %+v", got)
	}

	// Every bounty solved at the asked price: the bankroll grew.
	roster := decode[[]AgentView](t, do(t, srv, "GET", "/v1/agents", nil))
	if roster[0].Balance <= 2500 {
		t.Errorf("balance after a clean sweep: %d, want > 2500", roster[0].Balance)
	}

	trace := do(t, srv, "GET", "/v1/trace", nil)
	if !strings.Contains(trace.Body.String(), `"action":"solved"`) {
		t.Error("trace has no solved bounty")
	}
}

func TestBusyRefusals(t *testing.T) {
	srv, steps := newTestServer(t)
	submit(t, srv, "alpha", 2500)

	gate := make(chan struct{})
	steps.setGate(gate)
	if rec := do(t, srv, "POST", "/v1/episodes", EpisodeRequest{Seed: 1, Rounds: 1}); rec.Code != http.StatusAccepted {
		t.Fatalf("run: %d %s", rec.Code, rec.Body.String())
	}

	if rec := do(t, srv, "POST", "/v1/agents", SubmitRequest{ID: "beta", Image: "agent:latest", Grant: 100}); rec.Code != http.StatusConflict {
		t.Errorf("submit while running: got %d, want 409", rec.Code)
	}
	if rec := do(t, srv, "POST", "/v1/episodes", EpisodeRequest{Seed: 2, Rounds: 1}); rec.Code != http.StatusConflict {
		t.Errorf("run while running: got %d, want 409", rec.Code)
	}

	close(gate)
	if st := waitIdle(t, srv); st.State != "idle" {
		t.Fatalf("state after release: %s (%s)", st.State, st.Error)
	}
}

func TestBrokenWorldRefusesWork(t *testing.T) {
	// fickle probes clean at tier 1 but errors at tier 2, which the derived
	// plan reaches in round 2 — a mid-episode posting failure.
	srv, _ := newTestServer(t, fickleGen{})
	submit(t, srv, "alpha", 2500)

	if rec := do(t, srv, "POST", "/v1/episodes", EpisodeRequest{Seed: 1, Rounds: 3}); rec.Code != http.StatusAccepted {
		t.Fatalf("run: %d %s", rec.Code, rec.Body.String())
	}
	st := waitIdle(t, srv)
	if st.State != "broken" || !strings.Contains(st.Error, "fickle") {
		t.Fatalf("state after mid-episode failure: %s (%s), want broken", st.State, st.Error)
	}

	if rec := do(t, srv, "POST", "/v1/episodes", EpisodeRequest{Seed: 2, Rounds: 1}); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("run on a broken world: got %d, want 503", rec.Code)
	}
	if rec := do(t, srv, "POST", "/v1/agents", SubmitRequest{ID: "beta", Image: "agent:latest", Grant: 100}); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("submit on a broken world: got %d, want 503", rec.Code)
	}
}

// TestFailedBountiesCarryAcrossEpisodes is the control plane's half of the
// attempt-wallet regression: episode 1 fails its bounties, episode 2 re-awards
// them at the same round numbers, and only the per-episode namespace (NextEpoch
// in runEpisode) keeps the second episode from aborting on a wallet collision.
func TestFailedBountiesCarryAcrossEpisodes(t *testing.T) {
	srv, steps := newTestServer(t)
	submit(t, srv, "alpha", 2500)

	steps.setWrong(true)
	if rec := do(t, srv, "POST", "/v1/episodes", EpisodeRequest{Seed: 1, Rounds: 1}); rec.Code != http.StatusAccepted {
		t.Fatalf("run 1: %d %s", rec.Code, rec.Body.String())
	}
	if st := waitIdle(t, srv); st.State != "idle" {
		t.Fatalf("state after episode 1: %s (%s)", st.State, st.Error)
	}

	steps.setWrong(false)
	if rec := do(t, srv, "POST", "/v1/episodes", EpisodeRequest{Seed: 2, Rounds: 1}); rec.Code != http.StatusAccepted {
		t.Fatalf("run 2: %d %s", rec.Code, rec.Body.String())
	}
	if st := waitIdle(t, srv); st.State != "idle" {
		t.Fatalf("state after episode 2: %s (%s) — the carried bounty collided", st.State, st.Error)
	}

	// Both the carried bounty and the fresh one ended solved.
	tr := do(t, srv, "GET", "/v1/trace", nil).Body.String()
	for _, want := range [][2]string{{"failed", "b0001"}, {"solved", "b0001"}, {"solved", "b0002"}} {
		if !traceHas(tr, want[0], want[1]) {
			t.Errorf("trace missing %s of %s", want[0], want[1])
		}
	}
}

// A daemon restart is a second server over the first one's ledger, trace and
// roster. The money was always on disk; these pin that the rest of the world
// comes back with it — the roster, the epoch, the board's numbering — and that
// what comes back is a world that can go on running with drift 0.

func TestRestartSeatsTheSameWorld(t *testing.T) {
	dir := t.TempDir()
	db, tr := filepath.Join(dir, "ledger.db"), filepath.Join(dir, "trace.jsonl")

	srv1, _ := newTestServerAt(t, db, tr)
	submit(t, srv1, "alpha", 2500)
	submit(t, srv1, "beta", 2500)
	if rec := do(t, srv1, "POST", "/v1/episodes", EpisodeRequest{Seed: 1, Rounds: 1}); rec.Code != http.StatusAccepted {
		t.Fatalf("run 1: %d %s", rec.Code, rec.Body.String())
	}
	if st := waitIdle(t, srv1); st.State != "idle" {
		t.Fatalf("state after episode 1: %s (%s)", st.State, st.Error)
	}
	before := decode[[]AgentView](t, do(t, srv1, "GET", "/v1/agents", nil))

	srv2, steps2 := newTestServerAt(t, db, tr)
	after := decode[[]AgentView](t, do(t, srv2, "GET", "/v1/agents", nil))
	if len(after) != 2 || after[0].ID != "alpha" || after[1].ID != "beta" {
		t.Fatalf("roster after restart: %+v", after)
	}
	for i := range before {
		if after[i] != before[i] {
			t.Errorf("agent %s changed across restart: %+v -> %+v", before[i].ID, before[i], after[i])
		}
	}
	if _, ok := steps2.bound["alpha"]; !ok {
		t.Fatal("alpha's image was not re-bound to the step runner")
	}
	// Resubmitting a seated agent is still a taken name.
	if rec := do(t, srv2, "POST", "/v1/agents", SubmitRequest{ID: "alpha", Image: "agent:latest", Grant: 1}); rec.Code != http.StatusConflict {
		t.Fatalf("re-submit after restart: %d %s, want 409", rec.Code, rec.Body.String())
	}

	// The same plan again: every bounty id is new, every attempt wallet is
	// under a fresh epoch, and the episode is the second, not the first.
	rec := decode[EpisodeView](t, do(t, srv2, "POST", "/v1/episodes", EpisodeRequest{Seed: 1, Rounds: 1}))
	if rec.ID != "ep2" {
		t.Fatalf("episode after restart is %s, want ep2", rec.ID)
	}
	st := waitIdle(t, srv2)
	if st.State != "idle" {
		t.Fatalf("first episode after restart: state %q, err %q", st.State, st.Error)
	}
	if !strings.Contains(st.Conservation, "drift=0") {
		t.Fatalf("conservation after restart: %s", st.Conservation)
	}
	lines, err := trace.Read(tr)
	if err != nil {
		t.Fatal(err)
	}
	posted := map[string]int{}
	for _, ln := range lines {
		var p struct{ Action, ID string }
		if ln.Type == trace.EventBounty && json.Unmarshal(ln.Payload, &p) == nil && p.Action == "posted" {
			posted[p.ID]++
		}
	}
	if len(posted) == 0 {
		t.Fatal("no postings in the trace")
	}
	for id, n := range posted {
		if n != 1 {
			t.Errorf("bounty %s posted %d times across the restart", id, n)
		}
	}
	if lines[len(lines)-1].Seq != int64(len(lines)) {
		t.Errorf("trace seq %d on line %d: the record forked", lines[len(lines)-1].Seq, len(lines))
	}
}

func TestRestartRefusesAWorldWhoseImageIsGone(t *testing.T) {
	dir := t.TempDir()
	db, tr := filepath.Join(dir, "ledger.db"), filepath.Join(dir, "trace.jsonl")

	srv1, _ := newTestServerAt(t, db, tr)
	submit(t, srv1, "alpha", 2500)

	// The image is pulled out from under the roster between boots.
	rosterPath := filepath.Join(dir, "roster.json")
	raw, err := os.ReadFile(rosterPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rosterPath, bytes.ReplaceAll(raw, []byte("agent:latest"), []byte("missing:latest")), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err = openServerAt(t, db, tr)
	if err == nil || !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "missing:latest") {
		t.Fatalf("boot over a vanished image: %v; want a refusal naming alpha and the image", err)
	}
}

// A roster entry with no wallet is the crash intake's ordering allows — the
// file written, the process gone before the mint. Boot seats it and pays the
// grant once.
func TestResumeMintsForARosterEntryWithNoWallet(t *testing.T) {
	dir := t.TempDir()
	db, tr := filepath.Join(dir, "ledger.db"), filepath.Join(dir, "trace.jsonl")
	r := roster{Agents: []SubmitRequest{{ID: "alpha", Image: "agent:latest", Grant: 700}}}
	if err := r.save(filepath.Join(dir, "roster.json")); err != nil {
		t.Fatal(err)
	}
	srv, _ := newTestServerAt(t, db, tr)
	agents := decode[[]AgentView](t, do(t, srv, "GET", "/v1/agents", nil))
	if len(agents) != 1 || agents[0].Balance != 700 {
		t.Fatalf("seated from a wallet-less roster: %+v", agents)
	}
	srv2, _ := newTestServerAt(t, db, tr)
	if agents := decode[[]AgentView](t, do(t, srv2, "GET", "/v1/agents", nil)); agents[0].Balance != 700 {
		t.Fatalf("second boot minted again: %+v", agents)
	}
}

// traceHas reports whether any trace line records the given action on the given
// bounty. Payloads are maps, so their keys land in sorted order — matching a
// whole line beats guessing where "id" ends up relative to "action".
func traceHas(traceText, action, bountyID string) bool {
	for _, line := range strings.Split(traceText, "\n") {
		if strings.Contains(line, `"action":"`+action+`"`) && strings.Contains(line, `"id":"`+bountyID+`"`) {
			return true
		}
	}
	return false
}

func TestTraceTail(t *testing.T) {
	srv, _ := newTestServer(t)
	submit(t, srv, "alpha", 2500)

	rec := do(t, srv, "GET", "/v1/trace", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"action":"spawned"`) {
		t.Fatalf("trace read: %d %q", rec.Code, rec.Body.String())
	}

	// Follow over a real connection: the first line arrives from the existing
	// file, the second only once a later registration appends it.
	ts := httptest.NewServer(srv)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", ts.URL+"/v1/trace?follow=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	if !sc.Scan() || !strings.Contains(sc.Text(), "alpha") {
		t.Fatalf("first followed line: %q", sc.Text())
	}
	submit(t, srv, "beta", 2500)
	for sc.Scan() {
		if strings.Contains(sc.Text(), "beta") {
			return // the appended event arrived through the follow stream
		}
	}
	t.Fatal("follow stream never delivered the appended event")
}
