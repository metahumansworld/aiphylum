package orchestrator

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/singhtushant3-hub/aiphylum/internal/ledger"
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
// fairDay runs one whole AshmereFair day into path and returns its stream:
// bodies on schedules, minds on the stub, bounties on the hour, three agents
// on the same money. steps is the cast's behaviour, and holding wires the
// inward seam so an agent that buys standing actually gets it.
func fairDay(t *testing.T, path string, steps map[string]stepFunc, holding bool) []trace.Line {
	t.Helper()
	tw, err := trace.NewWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	w := newSim(t, tw, Config{StepTimeout: 10 * time.Second, Dust: 10})
	for _, id := range []string{"scholar", "frugal", "gambler"} {
		w.add(t, id, map[string]ledger.Credits{"scholar": 3000, "frugal": 2500, "gambler": 1600}[id])
		w.steps.fns[id] = steps[id]
	}

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
	cfg := town.Config{
		TickMinutes: 10, Interval: time.Microsecond, Days: 1, StartMinute: 7 * 60,
		Mind:  &town.Minds{Provider: &proxy.StubProvider{}},
		Visit: f.Visit,
	}
	if holding {
		cfg.Hold = f.Hold
	}
	m, people := town.AshmereFair()
	if _, err := town.Run(context.Background(), tw, m, people, cfg); err != nil {
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

func TestFairDeterministic(t *testing.T) {
	steps := map[string]stepFunc{
		"scholar": script(bidAll(0.8), solve),
		"frugal":  script(bidAll(0.5), solve),
		"gambler": script(bidAll(0.3), solve),
	}
	run := func(path string) []trace.Line { return fairDay(t, path, steps, false) }
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

// stayer buys standing once and bids on nothing, so the balance after such a
// day is the grant minus the standing exactly, with no award or attempt in
// the way. It keeps every offer it was shown, which is how the tests read the
// countdown the platform sent it.
type stayer struct {
	mu     sync.Mutex
	ask    int
	bought bool
	seen   []Observation
}

func (s *stayer) step(_ StepRequest, in StepInput) StepResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, in.Observation)
	if s.bought {
		return out()
	}
	s.bought = true
	return out(Action{Type: ActionStay, Ticks: s.ask})
}

func (s *stayer) offers() []Observation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Observation(nil), s.seen...)
}

// stayFair is a fair that posts one bounty a tick, so an agent standing at
// the office is handed a step every tick and can be asked what it owns.
func stayFair(t *testing.T, w *world, cfg FairConfig) *Fair {
	t.Helper()
	cfg.Deck = []Posting{post(1, 1), post(2, 1), post(3, 1)}
	cfg.PostMinutes = []int{540, 550, 560}
	cfg.WindowTicks, cfg.Office = 3, "office"
	f, err := NewFair(w.ctx, w.orch, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// Standing still is a purchase like any other: paid up front, worth exactly
// the ticks it bought, and over. The two Hold checks either side of the last
// paid tick are the point — a hold that never lifts is a body the town has
// lost, not an agent that bought a minute.
func TestFairStayHoldsTheBodyForWhatItPaid(t *testing.T) {
	tw, read := simTrace(t)
	w := newSim(t, tw, Config{StepTimeout: 5 * time.Second, Dust: 10})
	w.add(t, "pat", 3000)
	s := &stayer{ask: 2}
	w.steps.fns["pat"] = s.step

	f := stayFair(t, w, FairConfig{StayPrice: 20, MaxStayTicks: 6})
	at := []town.Standing{{ID: "pat", Place: "office"}}
	hold := []bool{}
	for i, mod := range []int{540, 550, 560} {
		if err := f.Visit(1, mod, town.HHMM(mod), at); err != nil {
			t.Fatalf("tick %d: %v", i+1, err)
		}
		hold = append(hold, f.Hold("pat"))
	}
	rep, err := f.Close()
	if err != nil {
		t.Fatal(err)
	}
	lines := read()

	// Two ticks bought, two movements held, and then the third is free.
	if want := []bool{true, true, false}; !reflect.DeepEqual(hold, want) {
		t.Errorf("holds %v, want %v — two paid ticks, then release", hold, want)
	}
	if got, want := w.balance(t, "pat"), ledger.Credits(3000-40); got != want {
		t.Errorf("balance %d, want %d — 2 ticks at the list price of 20", got, want)
	}
	if n := count(lines, trace.EventCredit, "stayed"); n != 1 {
		t.Errorf("%d stay events, want the one purchase", n)
	}

	// What the platform told it, tick by tick. An agent is a fresh process
	// with no memory, so a countdown it is not shown is a countdown it cannot
	// act on — and it would buy the same minute twice, every time.
	var left []int
	for _, obs := range s.offers() {
		if obs.Place != "office" || obs.StayPrice != 20 {
			t.Errorf("offer says %q at %d, want the office at 20", obs.Place, obs.StayPrice)
		}
		left = append(left, obs.StayTicksLeft)
	}
	if want := []int{0, 1, 0}; !reflect.DeepEqual(left, want) {
		t.Errorf("countdown %v, want %v — nothing owned, one left, spent", left, want)
	}
	if !rep.Conservation.Holds() {
		t.Errorf("conservation broken: %s", rep.Conservation)
	}
}

// A price you cannot pay is a refusal, not a debt: the day goes on, the agent
// keeps its money, and it is not held.
func TestFairStayRefusedWhenBroke(t *testing.T) {
	tw, read := simTrace(t)
	w := newSim(t, tw, Config{StepTimeout: 5 * time.Second, Dust: 10})
	w.add(t, "pat", 30)
	s := &stayer{ask: 2}
	w.steps.fns["pat"] = s.step

	f := stayFair(t, w, FairConfig{StayPrice: 20, MaxStayTicks: 6})
	if err := f.Visit(1, 540, town.HHMM(540), []town.Standing{{ID: "pat", Place: "office"}}); err != nil {
		t.Fatal(err)
	}
	if f.Hold("pat") {
		t.Error("pat is held on 40 credits' worth of standing it could not pay for")
	}
	if _, err := f.Close(); err != nil {
		t.Fatal(err)
	}
	lines := read()

	if got, want := w.balance(t, "pat"), ledger.Credits(30); got != want {
		t.Errorf("balance %d, want the untouched %d", got, want)
	}
	if n := count(lines, trace.EventCredit, "stayed"); n != 0 {
		t.Errorf("%d stay events on a refused purchase, want none", n)
	}
	if n := count(lines, trace.EventNote, "stay refused"); n != 1 {
		t.Errorf("%d refusal notes, want 1 — a refusal nobody records is a silent one", n)
	}
}

// The office keeps hours. An agent that asks to stand there all week is
// charged for the cap and held for the cap, not for what it asked.
func TestFairStayClampedToTheCap(t *testing.T) {
	tw, read := simTrace(t)
	w := newSim(t, tw, Config{StepTimeout: 5 * time.Second, Dust: 10})
	w.add(t, "pat", 3000)
	s := &stayer{ask: 99}
	w.steps.fns["pat"] = s.step

	f := stayFair(t, w, FairConfig{StayPrice: 20, MaxStayTicks: 2})
	at := []town.Standing{{ID: "pat", Place: "office"}}
	for _, mod := range []int{540, 550, 560} {
		if err := f.Visit(1, mod, town.HHMM(mod), at); err != nil {
			t.Fatal(err)
		}
	}
	if f.Hold("pat") {
		t.Error("still held after the cap: the clamp bought more than it charged for")
	}
	if _, err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := w.balance(t, "pat"), ledger.Credits(3000-40); got != want {
		t.Errorf("balance %d, want %d — charged for the cap of 2, not the 99 asked", got, want)
	}
	if n := count(read(), trace.EventCredit, "stayed"); n != 1 {
		t.Errorf("%d stay events, want 1", n)
	}
}

// lingers bids like bidAll and, whenever it is shown a price for standing it
// does not already own, buys two more ticks at the board — vigil's rule in
// Go, and the whole of it: buy only what you have not already got.
func lingers(frac float64) stepFunc {
	return func(req StepRequest, in StepInput) StepResult {
		if in.Observation.Phase != PhaseBid {
			return solve(req, in)
		}
		acts := bidsFor(in, frac)
		if in.Observation.StayPrice > 0 && in.Observation.StayTicksLeft == 0 {
			acts = append(acts, Action{Type: ActionStay, Ticks: 2})
		}
		return out(acts...)
	}
}

// The whole day again, twice, with money now able to move a body. Holding is
// the one thing in the fair that changes what the town does next, so it is
// the one thing that could make two identical days diverge — and the counts
// below keep the diff from passing on a day where nobody bought anything.
func TestFairStayDeterministic(t *testing.T) {
	steps := map[string]stepFunc{
		"scholar": lingers(0.8),
		"frugal":  script(bidAll(0.5), solve),
		"gambler": script(bidAll(0.3), solve),
	}
	dir := t.TempDir()
	a := fairDay(t, filepath.Join(dir, "a.jsonl"), steps, true)
	b := fairDay(t, filepath.Join(dir, "b.jsonl"), steps, true)

	if len(a) != len(b) {
		t.Fatalf("run lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Type != b[i].Type || timeless(t, a[i].Payload) != timeless(t, b[i].Payload) {
			t.Fatalf("line %d differs:\n  %s %s\n  %s %s",
				i, a[i].Type, a[i].Payload, b[i].Type, b[i].Payload)
		}
	}
	if n := count(a, trace.EventCredit, "stayed"); n == 0 {
		t.Error("nobody paid to stand anywhere: the diff proved nothing about lingering")
	}

	// And the town obeyed the purchase. A resident who is waiting is a
	// resident the fair's money moved — or rather, kept from moving.
	waited := false
	for _, l := range a {
		if l.Type == trace.EventTown && strings.Contains(string(l.Payload), `"activity":"waiting"`) {
			waited = true
			break
		}
	}
	if !waited {
		t.Error("credits left the ledger but no body ever stood still")
	}
}
