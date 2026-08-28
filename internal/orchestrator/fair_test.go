package orchestrator

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/singhtushant3-hub/aiphylum/internal/proxy"
	"github.com/singhtushant3-hub/aiphylum/internal/town"
	"github.com/singhtushant3-hub/aiphylum/internal/trace"
)

// total counts every event of a type, for events like bids that carry no
// action key at all.
func total(lines []trace.Line, typ trace.EventType) int {
	n := 0
	for _, l := range lines {
		if l.Type == typ {
			n++
		}
	}
	return n
}

// timeless renders a payload with its wall-clock stamp removed: model calls
// carry a time inside the payload as well as on the line. Both runs pass
// through the same map round-trip, so key order cannot differ between them.
func timeless(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "time")
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// The fair carries the sim's structural guarantee: a world with a ladder
// attached cannot hold one at all.
func TestFairRefusesARankedWorld(t *testing.T) {
	w := newWorld(t, &proxy.StubProvider{}, nil, Config{})
	_, err := NewFair(context.Background(), w.orch, FairConfig{
		Deck: []Posting{post(1, 1)}, PostMinutes: []int{540}, Office: "office",
	})
	if err == nil || !strings.Contains(err.Error(), "unranked") {
		t.Fatalf("NewFair against a ladder = %v, want an unranked-by-design refusal", err)
	}
}

// No office, no fair: presence at it is the whole coupling, so a config
// without one is a fair with nothing to be at.
func TestFairNeedsAnOffice(t *testing.T) {
	w := newWorld(t, &proxy.StubProvider{}, nil, Config{})
	w.orch.Ladder = nil
	_, err := NewFair(context.Background(), w.orch, FairConfig{
		Deck: []Posting{post(1, 1)}, PostMinutes: []int{540},
	})
	if err == nil || !strings.Contains(err.Error(), "office") {
		t.Fatalf("NewFair without an office = %v, want a refusal naming it", err)
	}
}

// The coupling rule itself: only an agent standing at the office is shown the
// board. sam never goes; sam's step function must never run. mira stands at
// the office all day but is a resident, not an agent — the seam names
// everyone in town and the fair must skip her. And pat, who bid in person on
// the first tick, wins on the second from the street: the bid was made at
// the board, the work is delivered by post.
func TestFairPresenceGatesTheBoard(t *testing.T) {
	tw, read := simTrace(t)
	w := newSim(t, tw, Config{StepTimeout: 5 * time.Second, Dust: 10})
	w.add(t, "pat", 3000)
	w.add(t, "sam", 3000)
	w.steps.fns["pat"] = script(bidAll(0.5), solve)
	w.steps.fns["sam"] = func(_ StepRequest, in StepInput) StepResult {
		t.Errorf("sam's step ran in phase %q while sam was at the tavern", in.Observation.Phase)
		return out()
	}

	f, err := NewFair(w.ctx, w.orch, FairConfig{
		Deck: []Posting{post(1, 1)}, PostMinutes: []int{540},
		WindowTicks: 1, Office: "office",
	})
	if err != nil {
		t.Fatal(err)
	}
	at := func(pat string) []town.Standing {
		return []town.Standing{
			{ID: "mira", Place: "office"},
			{ID: "pat", Place: pat},
			{ID: "sam", Place: "tavern"},
		}
	}
	if err := f.Visit(1, 540, town.HHMM(540), at("office")); err != nil {
		t.Fatal(err)
	}
	if err := f.Visit(1, 550, town.HHMM(550), at("")); err != nil {
		t.Fatal(err)
	}
	rep, err := f.Close()
	if err != nil {
		t.Fatal(err)
	}
	lines := read()

	if n := total(lines, trace.EventBid); n != 1 {
		t.Errorf("%d bids, want pat's one", n)
	}
	if n := count(lines, trace.EventBounty, "awarded"); n != 1 {
		t.Errorf("%d awards, want 1", n)
	}
	if n := count(lines, trace.EventBounty, "solved"); n != 1 {
		t.Errorf("%d solves, want 1", n)
	}
	for _, l := range lines {
		if l.Type != trace.EventBid {
			continue
		}
		var p struct{ Agent string }
		if err := json.Unmarshal(l.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Agent != "pat" {
			t.Errorf("bid from %q, want pat", p.Agent)
		}
	}
	if rep.Posted != 1 || rep.Shelved != 0 {
		t.Errorf("posted %d shelved %d, want 1 and 0", rep.Posted, rep.Shelved)
	}
	if !rep.Conservation.Holds() {
		t.Errorf("conservation broken: %s", rep.Conservation)
	}
}

// A bounty posted to an empty room reopens, and after MaxReopens empty
// windows it is shelved — permanently: pat arriving at the office afterwards
// is shown nothing, so the whole day ends without a single bid.
func TestFairEmptyOfficeReopensThenShelves(t *testing.T) {
	tw, read := simTrace(t)
	w := newSim(t, tw, Config{StepTimeout: 5 * time.Second, Dust: 10})
	w.add(t, "pat", 3000)
	w.steps.fns["pat"] = script(bidAll(0.5), solve)

	f, err := NewFair(w.ctx, w.orch, FairConfig{
		Deck: []Posting{post(1, 1)}, PostMinutes: []int{540},
		WindowTicks: 1, MaxReopens: 2, Office: "office",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Ticks 1-4: pat at the tavern while the window opens, closes empty,
	// reopens, and closes empty again — the second miss shelves it.
	// Ticks 5-7: pat at the office, too late.
	for i := 0; i < 7; i++ {
		place := "tavern"
		if i >= 4 {
			place = "office"
		}
		mod := 540 + 10*i
		st := []town.Standing{{ID: "pat", Place: place}}
		if err := f.Visit(1, mod, town.HHMM(mod), st); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := f.Close()
	if err != nil {
		t.Fatal(err)
	}
	lines := read()

	if n := count(lines, trace.EventBounty, "no_bids"); n != 2 {
		t.Errorf("%d empty windows, want the configured 2", n)
	}
	if n := count(lines, trace.EventNote, "bounty shelved"); n != 1 {
		t.Errorf("%d shelving notes, want 1", n)
	}
	if n := total(lines, trace.EventBid); n != 0 {
		t.Errorf("%d bids on a shelved bounty, want none", n)
	}
	if rep.Shelved != 1 {
		t.Errorf("report says %d shelved, want 1", rep.Shelved)
	}
}

// The composition, twice: a full AshmereFair day — bodies on schedules, minds
// on the stub, bounties on the hour — writes the same stream both times,
// modulo wall-clock stamps. This is the fair's own version of the town's
// TestRunDeterministic, and the count checks keep it from passing vacuously:
// a day where nobody bid would also diff clean.
func TestFairDeterministic(t *testing.T) {
	run := func(path string) []trace.Line {
		tw, err := trace.NewWriter(path)
		if err != nil {
			t.Fatal(err)
		}
		w := newSim(t, tw, Config{StepTimeout: 10 * time.Second, Dust: 10})
		w.add(t, "scholar", 3000)
		w.add(t, "frugal", 2500)
		w.add(t, "gambler", 1600)
		w.steps.fns["scholar"] = script(bidAll(0.8), solve)
		w.steps.fns["frugal"] = script(bidAll(0.5), solve)
		w.steps.fns["gambler"] = script(bidAll(0.3), solve)

		deck := make([]Posting, 8)
		for i := range deck {
			deck[i] = post(int64(i+1), 1)
		}
		f, err := NewFair(w.ctx, w.orch, FairConfig{
			Deck:        deck,
			PostMinutes: []int{540, 600, 660, 720, 780, 840, 900, 960},
			WindowTicks: 3, MaxReopens: 3, Office: "office",
		})
		if err != nil {
			t.Fatal(err)
		}
		m, people := town.AshmereFair()
		if _, err := town.Run(context.Background(), tw, m, people, town.Config{
			TickMinutes: 10, Interval: time.Microsecond, Days: 1, StartMinute: 7 * 60,
			Mind:  &town.Minds{Provider: &proxy.StubProvider{}},
			Visit: f.Visit,
		}); err != nil {
			t.Fatal(err)
		}
		rep, err := f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !rep.Conservation.Holds() {
			t.Fatalf("conservation broken: %s", rep.Conservation)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		lines, err := trace.Read(path)
		if err != nil {
			t.Fatal(err)
		}
		return lines
	}
	dir := t.TempDir()
	a := run(filepath.Join(dir, "a.jsonl"))
	b := run(filepath.Join(dir, "b.jsonl"))

	if len(a) != len(b) {
		t.Fatalf("run lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Type != b[i].Type || timeless(t, a[i].Payload) != timeless(t, b[i].Payload) {
			t.Fatalf("line %d differs:\n  %s %s\n  %s %s",
				i, a[i].Type, a[i].Payload, b[i].Type, b[i].Payload)
		}
	}

	// Both halves must actually have happened for the diff to mean anything.
	if n := total(a, trace.EventTown); n == 0 {
		t.Error("no town events: the diff proved nothing about the composition")
	}
	if n := total(a, trace.EventBid); n == 0 {
		t.Error("nobody bid all day")
	}
	if n := count(a, trace.EventBounty, "awarded"); n == 0 {
		t.Error("nothing awarded all day")
	}
	if n := count(a, trace.EventBounty, "solved"); n == 0 {
		t.Error("nothing solved all day")
	}
	if n := count(a, trace.EventBounty, "no_bids"); n == 0 {
		t.Error("no window ever closed empty — the schedule never gated anyone")
	}
}
