package orchestrator

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/metahunmei/dungeon/internal/proxy"
	"github.com/metahunmei/dungeon/internal/trace"
)

// newSim is a world with no ladder — the only kind the sim will run in.
func newSim(t *testing.T, tw *trace.Writer, cfg Config) *world {
	t.Helper()
	w := newWorld(t, &proxy.StubProvider{}, tw, cfg)
	w.orch.Ladder = nil
	return w
}

// solve answers the task from its own prompt: an agent that is always right,
// so a sim test measures the clock rather than the agent.
func solve(_ StepRequest, in StepInput) StepResult {
	return out(Action{Type: ActionSubmit, Bounty: in.Observation.Task.BountyID,
		Answer: answerFrom(in.Observation.Task.Prompt)})
}

func simTrace(t *testing.T) (*trace.Writer, func() []trace.Line) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sim.jsonl")
	tw, err := trace.NewWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	return tw, func() []trace.Line {
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		lines, err := trace.Read(path)
		if err != nil {
			t.Fatal(err)
		}
		return lines
	}
}

// count returns how many events of this type carry action == want.
func count(lines []trace.Line, typ trace.EventType, want string) int {
	n := 0
	for _, l := range lines {
		if l.Type != typ {
			continue
		}
		var p struct {
			Action string `json:"action"`
			Note   string `json:"note"`
		}
		if err := json.Unmarshal(l.Payload, &p); err != nil {
			continue
		}
		if p.Action == want || p.Note == want {
			n++
		}
	}
	return n
}

// The unranked guarantee is structural, not a promise in the docs: a world
// with a ladder attached cannot start a sim at all.
func TestSimRefusesARankedWorld(t *testing.T) {
	w := newWorld(t, &proxy.StubProvider{}, nil, Config{})
	_, err := w.orch.RunSim(context.Background(), SimConfig{Deck: []Posting{post(1, 1)}})
	if err == nil || !strings.Contains(err.Error(), "unranked") {
		t.Fatalf("RunSim against a ladder = %v, want an unranked-by-design refusal", err)
	}
}

// The sim's acceptance check: the world runs on a clock, every bounty in the
// deck gets solved and paid, the book still balances, and the trace says which
// track a spectator is watching.
func TestSimRunsOnTheClockAndConserves(t *testing.T) {
	tw, read := simTrace(t)
	w := newSim(t, tw, Config{StepTimeout: 2 * time.Second, Dust: 10})
	w.add(t, "cheap", 5000)
	w.add(t, "dear", 5000)
	w.steps.fns["cheap"] = script(bidAll(0.4), solve)
	w.steps.fns["dear"] = script(bidAll(0.9), solve)

	deck := []Posting{post(1, 1), post(2, 1), post(3, 2), post(4, 1)}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rep, err := w.orch.RunSim(ctx, SimConfig{
		PostInterval: 10 * time.Millisecond,
		BidWindow:    15 * time.Millisecond,
		Duration:     20 * time.Second,
		MaxReopens:   50,
		Deck:         deck,
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := read()

	// The report is the chronicle, and it adds up to the deck: no ranking, but
	// every solve accounted to somebody.
	if len(rep.Standings) != 2 {
		t.Fatalf("standings for %d agents, want 2", len(rep.Standings))
	}
	solved := 0
	for _, st := range rep.Standings {
		solved += st.Solved
		if st.Solved > 0 && st.Earned == 0 {
			t.Errorf("%s solved %d bounties and earned nothing", st.Agent, st.Solved)
		}
	}
	if solved != len(deck) {
		t.Errorf("report counts %d solves, deck had %d", solved, len(deck))
	}
	if !rep.Conservation.Holds() {
		t.Errorf("report conservation broken: %s", rep.Conservation)
	}

	if n := count(lines, trace.EventBounty, "solved"); n != len(deck) {
		t.Errorf("solved %d of %d deck bounties", n, len(deck))
	}
	if n := count(lines, trace.EventCredit, "payout"); n != len(deck) {
		t.Errorf("%d payouts for %d solves", n, len(deck))
	}
	con, err := w.orch.Ledger.Conservation(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !con.Holds() {
		t.Fatalf("conservation broken after the sim: %s", con)
	}

	// The first event names the track, and carries no round count: a real-time
	// world does not know in advance how long it runs.
	var start struct {
		Action string `json:"action"`
		Track  string `json:"track"`
	}
	if err := json.Unmarshal(lines[len(lines)-1].Payload, &start); err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		if l.Type != trace.EventEpisode {
			continue
		}
		if err := json.Unmarshal(l.Payload, &start); err != nil {
			t.Fatal(err)
		}
		if start.Action != "start" {
			continue
		}
		if start.Track != "sim" {
			t.Errorf("episode start says track %q, want sim", start.Track)
		}
		if strings.Contains(string(l.Payload), `"rounds"`) {
			t.Error("a sim episode should not claim a round count")
		}
		break
	}
}

// The rule that makes the sim a different game: an agent is one mind, so an
// agent deep in an attempt is not bidding, and auctions close without it.
func TestSimBusyAgentMissesTheWindow(t *testing.T) {
	tw, read := simTrace(t)
	w := newSim(t, tw, Config{StepTimeout: 5 * time.Second, Dust: 10})
	w.add(t, "slow", 5000)
	w.steps.fns["slow"] = script(bidAll(0.5), func(req StepRequest, in StepInput) StepResult {
		time.Sleep(80 * time.Millisecond) // several bid windows long
		return solve(req, in)
	})

	deck := []Posting{post(1, 1), post(2, 1), post(3, 1)}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rep, err := w.orch.RunSim(ctx, SimConfig{
		PostInterval: 10 * time.Millisecond,
		BidWindow:    15 * time.Millisecond,
		Duration:     20 * time.Second,
		MaxReopens:   50,
		Deck:         deck,
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := read()

	// One mind, three bounties: the report credits every solve to it, however
	// many windows it slept through getting there.
	if len(rep.Standings) != 1 || rep.Standings[0].Solved != len(deck) {
		t.Errorf("report standings = %+v, want one agent with %d solves", rep.Standings, len(deck))
	}

	if n := count(lines, trace.EventBounty, "no_bids"); n == 0 {
		t.Error("the only agent was busy for whole windows, yet no auction closed unbid")
	}
	// Missing a window costs the opportunity, not the bounty: it comes back.
	if n := count(lines, trace.EventBounty, "solved"); n != len(deck) {
		t.Errorf("solved %d of %d after the reopens", n, len(deck))
	}
}

// A bounty nobody wants must stop costing everyone a bid step. Without the
// shelf, an unbid bounty burns real credits every tick, forever.
func TestSimShelvesABountyNobodyBidsOn(t *testing.T) {
	tw, read := simTrace(t)
	w := newSim(t, tw, Config{StepTimeout: 2 * time.Second, Dust: 10})
	w.add(t, "aloof", 5000)
	w.steps.fns["aloof"] = script(
		func(StepRequest, StepInput) StepResult { return out() }, // bids on nothing
		solve,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := w.orch.RunSim(ctx, SimConfig{
		PostInterval: 5 * time.Millisecond,
		BidWindow:    5 * time.Millisecond,
		Duration:     20 * time.Second,
		MaxReopens:   2,
		Deck:         []Posting{post(1, 1)},
	}); err != nil {
		t.Fatal(err)
	}
	lines := read()

	if n := count(lines, trace.EventNote, "bounty shelved"); n != 1 {
		t.Errorf("%d shelving notes, want 1", n)
	}
	if n := count(lines, trace.EventBounty, "no_bids"); n != 2 {
		t.Errorf("%d unbid windows, want the configured 2", n)
	}
}

// Cancellation is a shutdown, not a leak: in-flight attempts come home, their
// money settles, and the audit at the end still balances.
func TestSimCancellationSettlesInFlightWork(t *testing.T) {
	tw, read := simTrace(t)
	w := newSim(t, tw, Config{StepTimeout: 5 * time.Second, Dust: 10})
	w.add(t, "slow", 5000)
	w.steps.fns["slow"] = script(bidAll(0.5), func(req StepRequest, in StepInput) StepResult {
		time.Sleep(60 * time.Millisecond)
		return solve(req, in)
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(40 * time.Millisecond)
		cancel()
	}()
	if _, err := w.orch.RunSim(ctx, SimConfig{
		PostInterval: 5 * time.Millisecond,
		BidWindow:    5 * time.Millisecond,
		Duration:     20 * time.Second,
		Deck:         []Posting{post(1, 1), post(2, 1), post(3, 1)},
	}); err != nil {
		t.Fatal(err)
	}
	lines := read()

	con, err := w.orch.Ledger.Conservation(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !con.Holds() {
		t.Fatalf("conservation broken after cancellation: %s", con)
	}
	if err := w.orch.Ledger.Verify(w.ctx); err != nil {
		t.Fatal(err)
	}
	// No attempt wallet may still be holding money after the world stops.
	for _, l := range lines {
		if l.Type != trace.EventEpisode {
			continue
		}
		var p struct{ Action, Conservation string }
		if err := json.Unmarshal(l.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Action == "end" && !strings.Contains(p.Conservation, "drift=0") {
			t.Errorf("episode end reports %s", p.Conservation)
		}
	}
}
