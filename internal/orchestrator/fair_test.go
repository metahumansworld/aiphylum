package orchestrator

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/metahumansworld/soscitea/internal/auction"
	"github.com/metahumansworld/soscitea/internal/ledger"
	"github.com/metahumansworld/soscitea/internal/proxy"
	"github.com/metahumansworld/soscitea/internal/town"
	"github.com/metahumansworld/soscitea/internal/trace"
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
//
// A tweak is handed both configs because the fair's two policies do not live
// together: the tie-break is the auction's and sits on FairConfig, while the
// book is the announcer's and sits on the orchestrator's Config. Both are
// built in here, so a test that wants either has to be given both.
func fairDay(t *testing.T, path string, steps map[string]stepFunc, holding bool, tweak ...func(*Config, *FairConfig)) []trace.Line {
	t.Helper()
	return fairDays(t, path, 1, steps, holding, tweak...)
}

// fairDays is fairDay at any horizon: the same cast, the same money, the
// same posting hours, run for days consecutive days. The default deck grows
// with the horizon — one card per posting slot per day, which is exactly how
// cmd/phylumd sizes a -days run — so a longer fair is a longer supply, not
// the same eight cards posted again.
func fairDays(t *testing.T, path string, days int, steps map[string]stepFunc, holding bool, tweak ...func(*Config, *FairConfig)) []trace.Line {
	t.Helper()
	return fairWorldDays(t, path, days, func(*world) map[string]stepFunc { return steps }, holding, tweak...)
}

// fairWorldDays is fairDays for a cast that cannot be written until the world
// exists: an agent that spends through the proxy has to be told where the
// proxy is, and there is no proxy before there is a world. The cast is built
// once, after the world and both configs, and installed before the first tick.
func fairWorldDays(t *testing.T, path string, days int, cast func(*world) map[string]stepFunc, holding bool, tweak ...func(*Config, *FairConfig)) []trace.Line {
	t.Helper()
	return fairSeatedDays(t, path, days, nil, cast, holding, tweak...)
}

// fairSeatedDays is fairWorldDays with guests. The cast sits where it always
// has; seated is the order the guests join after it, each on the guest
// lodging's schedule with the newcomer's 2,000, which is what the daemon does
// with the -guest flags in the order they are written. Two guests keep the
// same hours, so the order they are seated in is the order their asks reach
// the board — the seat the queue reads when they tie. Every caller above
// seats nobody; the seating test seats the same two guests twice, the other
// way round the second time.
func fairSeatedDays(t *testing.T, path string, days int, seated []string, cast func(*world) map[string]stepFunc, holding bool, tweak ...func(*Config, *FairConfig)) []trace.Line {
	t.Helper()
	tw, err := trace.NewWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	ocfg := Config{StepTimeout: 10 * time.Second, Dust: 10}
	fcfg := FairConfig{
		Deck:        nil, // filled below, once the deck exists
		PostMinutes: []int{540, 600, 660, 720, 780, 840, 900, 960},
		WindowTicks: 3, MaxReopens: 3, Office: "office",
	}
	for _, tk := range tweak {
		tk(&ocfg, &fcfg)
	}
	w := newSim(t, tw, ocfg)
	steps := cast(w)
	for _, id := range []string{"scholar", "frugal", "gambler"} {
		w.add(t, id, map[string]ledger.Credits{"scholar": 3000, "frugal": 2500, "gambler": 1600}[id])
		w.steps.fns[id] = steps[id]
	}
	m, people := town.AshmereFair()
	for _, id := range seated {
		w.add(t, id, 2000)
		w.steps.fns[id] = steps[id]
		people = append(people, town.Guest(id, id, "a guest, seated by the test"))
	}

	if fcfg.Deck == nil {
		deck := make([]Posting, len(fcfg.PostMinutes)*days)
		for i := range deck {
			deck[i] = post(int64(i+1), 1)
		}
		fcfg.Deck = deck
	}
	f, err := NewFair(w.ctx, w.orch, fcfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg := town.Config{
		TickMinutes: 10, Interval: time.Microsecond, Days: days, StartMinute: 7 * 60,
		Mind:  &town.Minds{Provider: &proxy.StubProvider{}},
		Visit: f.Visit,
	}
	if holding {
		cfg.Hold = f.Hold
	}
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

// The lot keeps the fair's first property: same seed, same trace. A draw
// that broke determinism would trade the replay away for the fairness, and
// the design refuses the trade. Everyone bids the same fraction here, so
// every award all day is a three-way tie and the draw is what this diff is
// actually diffing.
func TestFairLotDeterministic(t *testing.T) {
	steps := map[string]stepFunc{
		"scholar": script(bidAll(0.4), solve),
		"frugal":  script(bidAll(0.4), solve),
		"gambler": script(bidAll(0.4), solve),
	}
	lot := func(_ *Config, cfg *FairConfig) { cfg.Tie = auction.ByLot; cfg.LotSalt = 1 }
	dir := t.TempDir()
	a := fairDay(t, filepath.Join(dir, "a.jsonl"), steps, false, lot)
	b := fairDay(t, filepath.Join(dir, "b.jsonl"), steps, false, lot)

	if len(a) != len(b) {
		t.Fatalf("run lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Type != b[i].Type || timeless(t, a[i].Payload) != timeless(t, b[i].Payload) {
			t.Fatalf("line %d differs:\n  %s %s\n  %s %s",
				i, a[i].Type, a[i].Payload, b[i].Type, b[i].Payload)
		}
	}
	if n := count(a, trace.EventBounty, "awarded"); n == 0 {
		t.Error("nothing awarded all day: the diff proved nothing about the draw")
	}
}

// Every lot is recomputable from the trace alone, the same right the auction
// results bought in their milestone: the salt is on the episode-start line
// (as a decimal string), the bounty's ID is on the award, the draw index is
// the count of that bounty's earlier closed windows, and the tied names are
// in the book. This is the property that lets a draw into a venue whose
// whole defence is replayability — and it is the off-by-one guard on the
// draw counter, which is exactly the bug a per-window index invites.
func TestFairLotDrawsAreDerivableFromTheTraceAlone(t *testing.T) {
	// Every agent flubs its first attempt of the day, so whoever wins the
	// first award fails it, the bounty is re-auctioned, and the re-auction is
	// the same three-way tie again — this time at a draw index above zero.
	// That is the only place a miscounted window could be caught: at index
	// zero the orchestrator's counter and the trace-side reconstruction are
	// both trivially zero and agree by accident.
	var mu sync.Mutex
	flubbed := map[string]bool{}
	flubOnce := func(req StepRequest, in StepInput) StepResult {
		mu.Lock()
		first := !flubbed[req.AgentID]
		flubbed[req.AgentID] = true
		mu.Unlock()
		if first {
			return out(Action{Type: ActionSubmit,
				Bounty: in.Observation.Task.BountyID, Answer: "not it"})
		}
		return solve(req, in)
	}
	steps := map[string]stepFunc{
		"scholar": script(bidAll(0.4), flubOnce),
		"frugal":  script(bidAll(0.4), flubOnce),
		"gambler": script(bidAll(0.4), flubOnce),
	}
	lines := fairDay(t, filepath.Join(t.TempDir(), "lot.jsonl"), steps, false,
		func(_ *Config, cfg *FairConfig) { cfg.Tie = auction.ByLot; cfg.LotSalt = 42 })

	var salt uint64
	declared := false
	closed := map[string]int{} // windows each bounty has already had
	ties, reTies := 0, 0
	for _, l := range lines {
		var p struct {
			Action string `json:"action"`
			Track  string `json:"track"`
			Tie    string `json:"tiebreak"`
			Salt   string `json:"lot_salt"`
			ID     string `json:"id"`
			Winner string `json:"winner"`
			Book   []struct {
				Agent string
				Price ledger.Credits
			} `json:"book"`
		}
		if err := json.Unmarshal(l.Payload, &p); err != nil {
			continue
		}
		switch {
		case l.Type == trace.EventEpisode && p.Action == "start" && p.Track == "fair":
			if p.Tie != "lot" {
				t.Fatalf("episode start declares tiebreak %q, want lot", p.Tie)
			}
			s, err := strconv.ParseUint(p.Salt, 10, 64)
			if err != nil {
				t.Fatalf("lot_salt %q does not parse: %v", p.Salt, err)
			}
			salt, declared = s, true
		case l.Type == trace.EventBounty && p.Action == "no_bids":
			closed[p.ID]++
		case l.Type == trace.EventBounty && p.Action == "awarded":
			if !declared {
				t.Fatal("an award arrived before the episode start declared the salt")
			}
			draw := closed[p.ID]
			closed[p.ID]++
			low := p.Book[0].Price
			for _, b := range p.Book {
				if b.Price < low {
					low = b.Price
				}
			}
			want, wantKey, tied := "", uint64(0), 0
			for _, b := range p.Book { // book order is arrival order: the collision fallback for free
				if b.Price != low {
					continue
				}
				tied++
				if k := auction.Draw(lotSalt(salt, p.ID, draw), b.Agent); want == "" || k < wantKey {
					want, wantKey = b.Agent, k
				}
			}
			if tied > 1 {
				ties++
				if draw > 0 {
					reTies++
				}
			}
			if p.Winner != want {
				t.Errorf("%s window %d: trace says %s won, the recomputed draw says %s", p.ID, draw, p.Winner, want)
			}
		}
	}
	if ties == 0 {
		t.Fatal("no award ever carried a tie: nothing here exercised the lot")
	}
	if reTies == 0 {
		t.Fatal("no re-auctioned window ever carried a tie: the draw index in the salt was only checked at zero")
	}
}

// The whole cost of the open book, in the trace: one key on one line.
//
// The results an agent reads are not traced — they never were, because every
// one of them is a function of the awarded event plus the reader's identity,
// and that stayed true when the book was opened. So a day run -book open must
// write the same stream as the sealed day it is otherwise identical to,
// except for the episode-start line, which gains "book": "open" so a reader
// of the record knows what kind of market this was. The sealed day carries no
// such key: a policy that was not in force should not be in the record.
//
// If this test ever fails by finding a second differing line, the milestone's
// central claim — that opening the book widens what bidders are told without
// widening the record by a byte — has stopped being true.
func TestFairOpenBookChangesOnlyTheStartLine(t *testing.T) {
	steps := map[string]stepFunc{
		"scholar": script(bidAll(0.8), solve),
		"frugal":  script(bidAll(0.5), solve),
		"gambler": script(bidAll(0.3), solve),
	}
	dir := t.TempDir()
	sealed := fairDay(t, filepath.Join(dir, "sealed.jsonl"), steps, false)
	open := fairDay(t, filepath.Join(dir, "open.jsonl"), steps, false,
		func(cfg *Config, _ *FairConfig) { cfg.Book = OpenBook })

	if len(sealed) != len(open) {
		t.Fatalf("run lengths differ: sealed %d, open %d", len(sealed), len(open))
	}
	var differing []int
	for i := range sealed {
		if sealed[i].Type != open[i].Type || timeless(t, sealed[i].Payload) != timeless(t, open[i].Payload) {
			differing = append(differing, i)
		}
	}
	if len(differing) != 1 {
		t.Fatalf("%d lines differ, want exactly the start line: %v", len(differing), differing)
	}

	// And the one line differs by exactly the one key.
	i := differing[0]
	var was, now map[string]any
	if err := json.Unmarshal(sealed[i].Payload, &was); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(open[i].Payload, &now); err != nil {
		t.Fatal(err)
	}
	if was["action"] != "start" {
		t.Fatalf("the differing line is a %q, want the episode start: %s", was["action"], sealed[i].Payload)
	}
	if _, ok := was["book"]; ok {
		t.Errorf("the sealed day declared a book policy it was not asked for: %s", sealed[i].Payload)
	}
	if now["book"] != "open" {
		t.Errorf("the open day's start line says book=%v, want \"open\": %s", now["book"], open[i].Payload)
	}
	delete(now, "book")
	if !reflect.DeepEqual(was, now) {
		t.Errorf("the start line changed by more than the book key:\n  %s\n  %s", sealed[i].Payload, open[i].Payload)
	}

	// Vacuity guard: a day where nothing was awarded would diff clean too, and
	// would say nothing at all about what bidders were told.
	if n := count(sealed, trace.EventBounty, "awarded"); n == 0 {
		t.Error("no awards all day; the diff proves nothing about the book")
	}
}

// The shelf's arithmetic is derivable from the trace alone, the same right
// the auction results and the lot draws bought in their milestones: the
// counter a shelving note reports is per bounty, grows only on that bounty's
// own empty windows, and is deleted — not decremented — the moment that
// bounty is awarded. A reader replaying no_bids and awarded events therefore
// predicts every shelving note in the stream, with its windows count, to the
// line. Failure is deliberately absent from the replay: a won-and-flubbed
// delivery goes back on the board with its counter gone, which is why the
// zombie card below can be failed all day and never shelved.
func TestFairShelvingIsDerivableFromTheTraceAlone(t *testing.T) {
	tw, read := simTrace(t)
	w := newSim(t, tw, Config{StepTimeout: 5 * time.Second, Dust: 10})
	w.add(t, "pat", 3000)

	// One agent, four cards, four fates. b0001 is worked and solved. b0002 is
	// shunned outright. b0003 is shunned twice, won on its third window,
	// flubbed, then shunned for good — the card that catches a counter that
	// resets globally, or never resets at all. b0004 is won and flubbed every
	// time it comes up.
	sight := map[string]int{}
	bid := func(_ StepRequest, in StepInput) StepResult {
		var acts []Action
		for _, b := range in.Observation.Bounties {
			sight[b.ID]++
			switch b.ID {
			case "b0002":
				continue
			case "b0003":
				if sight[b.ID] != 3 {
					continue
				}
			}
			acts = append(acts, Action{Type: ActionBid, Bounty: b.ID, Price: b.MaxPayout / 2})
		}
		return out(acts...)
	}
	attempt := func(req StepRequest, in StepInput) StepResult {
		if id := in.Observation.Task.BountyID; id == "b0003" || id == "b0004" {
			return out(Action{Type: ActionSubmit, Bounty: id, Answer: "not it"})
		}
		return solve(req, in)
	}
	w.steps.fns["pat"] = script(bid, attempt)

	const maxReopens = 3
	f, err := NewFair(w.ctx, w.orch, FairConfig{
		Deck:        []Posting{post(1, 1), post(2, 1), post(3, 1), post(4, 1)},
		PostMinutes: []int{540, 550, 560, 570},
		WindowTicks: 1, MaxReopens: maxReopens, Office: "office",
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		mod := 540 + 10*i
		st := []town.Standing{{ID: "pat", Place: "office"}}
		if err := f.Visit(1, mod, town.HHMM(mod), st); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Conservation.Holds() {
		t.Fatalf("conservation broken: %s", rep.Conservation)
	}
	lines := read()

	// The replay: per-bounty counter up on no_bids, gone on awarded, a note
	// predicted the moment a counter reaches MaxReopens — and alongside it,
	// the counts the prediction is only meaningful against.
	type shelf struct {
		Bounty  string
		Windows int
	}
	var want, got []shelf
	reopens := map[string]int{}
	shelved := map[string]bool{}
	empties := map[string]int{}  // lifetime empty windows per bounty
	preAward := map[string]int{} // empty windows before the first award
	awards := map[string]int{}
	failed := map[string]int{}
	beforeFirstNote := map[string]bool{}
	for _, l := range lines {
		var p struct {
			Action  string `json:"action"`
			ID      string `json:"id"`
			Note    string `json:"note"`
			Bounty  string `json:"bounty"`
			Windows int    `json:"windows"`
		}
		if err := json.Unmarshal(l.Payload, &p); err != nil {
			continue
		}
		switch {
		case l.Type == trace.EventBounty && p.Action == "no_bids":
			if shelved[p.ID] {
				t.Errorf("%s got a window after its shelving note", p.ID)
			}
			if len(got) == 0 {
				beforeFirstNote[p.ID] = true
			}
			empties[p.ID]++
			if awards[p.ID] == 0 {
				preAward[p.ID]++
			}
			reopens[p.ID]++
			if reopens[p.ID] >= maxReopens {
				want = append(want, shelf{p.ID, reopens[p.ID]})
			}
		case l.Type == trace.EventBounty && p.Action == "awarded":
			if shelved[p.ID] {
				t.Errorf("%s was awarded after its shelving note", p.ID)
			}
			awards[p.ID]++
			delete(reopens, p.ID)
		case l.Type == trace.EventBounty && p.Action == "failed":
			failed[p.ID]++
		case l.Type == trace.EventNote && p.Note == "bounty shelved":
			got = append(got, shelf{p.Bounty, p.Windows})
			shelved[p.Bounty] = true
		}
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("replayed shelvings %v, trace says %v", want, got)
	}
	if rep.Shelved != len(got) {
		t.Errorf("report says %d shelved, the stream holds %d notes", rep.Shelved, len(got))
	}

	// Vacuity guards, each one a wrong counter this day would otherwise let
	// agree by accident.
	if len(got) == 0 {
		t.Fatal("nothing was shelved: the replay predicted nothing")
	}
	if len(beforeFirstNote) < 2 {
		t.Fatal("no two bounties' empty windows interleaved before the first note: a counter shared across bounties would have agreed anyway")
	}
	if awards["b0003"] == 0 || preAward["b0003"] < 2 || !shelved["b0003"] {
		t.Fatalf("b0003 (awards %d, empty windows before %d, shelved %v) never exercised the reset",
			awards["b0003"], preAward["b0003"], shelved["b0003"])
	}
	if empties["b0003"] <= maxReopens {
		t.Fatalf("b0003 shelved on %d lifetime empty windows: a counter that never resets would have agreed", empties["b0003"])
	}
	if failed["b0004"] < 3 {
		t.Fatalf("b0004 failed %d deliveries, want at least 3 to say failure never shelves", failed["b0004"])
	}
	if shelved["b0004"] {
		t.Error("b0004 was shelved: failure is not supposed to count against the shelf")
	}
}

// Three days, twenty-four cards, and the rule the fair inherited from the
// sim: every bounty the office posts ends exactly one way. Solved, shelved,
// and voided are the ends the stream can name; a card still open when the
// run stops is the fourth, named by silence. The same pass audits the money
// — every payout credit belongs to a solved bounty and matches its recorded
// payout, the identity the README's three-day ledgers rest on. Nothing here
// pins a credit total: this cast is Go, not the fair's python guests, and
// the property is the partition, not the price.
func TestFairEveryCardEndsExactlyOneWay(t *testing.T) {
	// Everyone bids the same fraction on everything — except two cards the
	// whole room refuses, which the shelf must catch, and one the winner can
	// only flub, which failure must put back on the board rather than end.
	shun := map[string]bool{"b0010": true, "b0021": true}
	const zombie = "b0017"
	bid := func(_ StepRequest, in StepInput) StepResult {
		var acts []Action
		for _, b := range in.Observation.Bounties {
			if shun[b.ID] {
				continue
			}
			price := ledger.Credits(float64(b.MaxPayout) * 0.4)
			if price < b.Reserve {
				price = b.Reserve
			}
			acts = append(acts, Action{Type: ActionBid, Bounty: b.ID, Price: price})
		}
		return out(acts...)
	}
	attempt := func(req StepRequest, in StepInput) StepResult {
		if in.Observation.Task.BountyID == zombie {
			return out(Action{Type: ActionSubmit, Bounty: zombie, Answer: "not it"})
		}
		return solve(req, in)
	}
	steps := map[string]stepFunc{
		"scholar": script(bid, attempt),
		"frugal":  script(bid, attempt),
		"gambler": script(bid, attempt),
	}
	lines := fairDays(t, filepath.Join(t.TempDir(), "three.jsonl"), 3, steps, false)

	posted := map[string]bool{}
	ends := map[string][]string{}
	paid := map[string]ledger.Credits{}
	solvedPay := map[string]ledger.Credits{}
	zombieFails := 0
	for _, l := range lines {
		var p struct {
			Action string         `json:"action"`
			ID     string         `json:"id"`
			Note   string         `json:"note"`
			Bounty string         `json:"bounty"`
			Payout ledger.Credits `json:"payout"`
			Amount ledger.Credits `json:"amount"`
		}
		if err := json.Unmarshal(l.Payload, &p); err != nil {
			continue
		}
		switch {
		case l.Type == trace.EventBounty && p.Action == "posted":
			posted[p.ID] = true
		case l.Type == trace.EventBounty && p.Action == "solved":
			ends[p.ID] = append(ends[p.ID], "solved")
			solvedPay[p.ID] = p.Payout
		case l.Type == trace.EventBounty && p.Action == "voided":
			ends[p.ID] = append(ends[p.ID], "voided")
		case l.Type == trace.EventBounty && p.Action == "failed" && p.ID == zombie:
			zombieFails++
		case l.Type == trace.EventNote && p.Note == "bounty shelved":
			ends[p.Bounty] = append(ends[p.Bounty], "shelved")
		case l.Type == trace.EventCredit && p.Action == "payout":
			paid[p.Bounty] += p.Amount
		}
	}

	// The partition. Exactly the full deck was posted; nothing ended twice;
	// nothing ended without having been posted.
	if len(posted) != 24 {
		t.Fatalf("%d cards posted, want the full 3-day deck of 24", len(posted))
	}
	for id := range posted {
		if n := len(ends[id]); n > 1 {
			t.Errorf("%s ended %d ways: %v", id, n, ends[id])
		}
	}
	for id := range ends {
		if !posted[id] {
			t.Errorf("%s ended without ever being posted", id)
		}
	}

	// The money. One payout credit per solved bounty, at its recorded price,
	// and not a credit anywhere else.
	if !reflect.DeepEqual(paid, solvedPay) {
		t.Errorf("payout credits %v, solved payouts %v", paid, solvedPay)
	}

	// Vacuity guards: each engineered fate actually happened.
	if len(solvedPay) == 0 {
		t.Fatal("nothing was solved: the payout identity was checked against silence")
	}
	for id := range shun {
		if len(ends[id]) != 1 || ends[id][0] != "shelved" {
			t.Errorf("%s, which the whole room shuns, ended %v, want exactly one shelving", id, ends[id])
		}
	}
	if zombieFails == 0 {
		t.Fatal("the zombie never failed a delivery: nothing walked the road back to the board")
	}
	for _, end := range ends[zombie] {
		if end == "solved" {
			t.Errorf("%s was solved, but its every delivery was a flub", zombie)
		}
	}
}

// The floor is the platform's, and the trace says so without help.
//
// Every open-book entry on the README walks toward the same number — the
// card's reserve, the place the walk is said to be walking to — and every
// fair bidder that exists, the python guests and this file's Go casts alike,
// clamps its own ask to b.Reserve before bidding. So no fair test has ever
// asked the question the walk turns on: is the floor the guests' manners or
// the platform's rule? This one removes the clamp. The sinker bids a sliding
// percentage of the maximum with no floor of its own — the huckster's gait
// minus its one act of politeness — and crosses under the reserve within the
// run. The floorer bids exactly the reserve on every card: the destination
// as a standing policy. The scholar is the room. The trace is then read back
// cold, using nothing but each card's posted reserve:
//
//  1. every bid on record is at or above its card's reserve, and every ask
//     under it appears only as a refusal note carrying the auction's own
//     error — the platform said no; the guest never said anything;
//  2. no award clears below the reserve, and a refused ask leaves no trace
//     in any book;
//  3. a reserve-priced ask is a legal winning ask, paid at exactly the
//     reserve — the destination is a place a bidder can stand;
//  4. the posted reserve is the documented fraction of the posted maximum on
//     every card — the number the guests clamp to is derivable, not
//     decorative;
//  5. and none of it was checked against silence: the sinker did sink, and
//     it sank on a card the floorer then won at the floor.
//
// Nothing here pins a price or a total: the property is who holds the floor,
// not where it is.
func TestFairReserveIsTheFloorAndIsDerivableFromTheTraceAlone(t *testing.T) {
	var mu sync.Mutex
	pct := 6 // one point above the reserve's five: two lessons to the floor, a third to cross it
	sinker := func(_ StepRequest, in StepInput) StepResult {
		mu.Lock()
		defer mu.Unlock()
		var acts []Action
		for _, b := range in.Observation.Bounties {
			price := ledger.Credits(float64(b.MaxPayout) * float64(pct) / 100)
			if price < 1 {
				price = 1 // the only floor it has: a bid of nothing is not a bid
			}
			acts = append(acts, Action{Type: ActionBid, Bounty: b.ID, Price: price})
		}
		if len(acts) > 0 && pct > 1 {
			pct-- // a lesson per auction bid in, and no reserve in the rule
		}
		return out(acts...)
	}
	floorer := func(_ StepRequest, in StepInput) StepResult {
		var acts []Action
		for _, b := range in.Observation.Bounties {
			acts = append(acts, Action{Type: ActionBid, Bounty: b.ID, Price: b.Reserve})
		}
		return out(acts...)
	}
	steps := map[string]stepFunc{
		"scholar": script(bidAll(0.4), solve),
		"frugal":  script(floorer, solve),
		"gambler": script(sinker, solve),
	}
	lines := fairDays(t, filepath.Join(t.TempDir(), "floor.jsonl"), 2, steps, false)

	const sinkerID, floorerID = "gambler", "frugal"
	reserve := map[string]ledger.Credits{}
	sunk := map[string]int{}                 // sub-reserve refusals per card
	floorWins := map[string]int{}            // awards to the floorer at exactly the reserve, per card
	floorPaid := map[string]ledger.Credits{} // payout credits to the floorer, per card
	for _, l := range lines {
		var p struct {
			Action string         `json:"action"`
			Note   string         `json:"note"`
			ID     string         `json:"id"`
			Bounty string         `json:"bounty"`
			Agent  string         `json:"agent"`
			Price  ledger.Credits `json:"price"`
			Max    ledger.Credits `json:"max_payout"`
			Res    ledger.Credits `json:"reserve"`
			Err    string         `json:"err"`
			Winner string         `json:"winner"`
			Amount ledger.Credits `json:"amount"`
			Book   []struct {
				Agent string
				Price ledger.Credits
			} `json:"book"`
		}
		if err := json.Unmarshal(l.Payload, &p); err != nil {
			continue
		}
		switch {
		case l.Type == trace.EventBounty && p.Action == "posted":
			reserve[p.ID] = p.Res
			// 4. Derivable: the same arithmetic reserveFor does, from the
			// posted maximum and the documented default fraction alone.
			want := ledger.Credits(float64(p.Max) * 0.05)
			if want < 1 {
				want = 1
			}
			if p.Res != want {
				t.Errorf("%s posted with reserve %d on a maximum of %d, want %d (5%%, floor 1)", p.ID, p.Res, p.Max, want)
			}
		case l.Type == trace.EventBid:
			// 1. Nothing under the reserve ever entered a book.
			r, ok := reserve[p.Bounty]
			if !ok {
				t.Fatalf("%s bid on %s before it was posted", p.Agent, p.Bounty)
			}
			if p.Price < r {
				t.Errorf("%s's ask of %d on %s entered the book under the reserve %d", p.Agent, p.Price, p.Bounty, r)
			}
		case l.Type == trace.EventNote && p.Note == "bid refused" && strings.Contains(p.Err, auction.ErrBelowReserve.Error()):
			// 1. Every ask under the reserve is on record as a refusal, in
			// the auction's own words, and every one of them is the sinker's.
			r := reserve[p.Bounty]
			if p.Price >= r {
				t.Errorf("%s's ask of %d on %s was refused as below the reserve, but the reserve is %d", p.Agent, p.Price, p.Bounty, r)
			}
			if p.Agent != sinkerID {
				t.Errorf("%s was refused under the reserve on %s at %d, and only the sinker bids without a clamp", p.Agent, p.Bounty, p.Price)
			}
			sunk[p.Bounty]++
		case l.Type == trace.EventBounty && p.Action == "awarded":
			// 2. No award below the reserve, and no refused ask in any book.
			r := reserve[p.ID]
			if p.Price < r {
				t.Errorf("%s awarded to %s at %d, under the reserve %d", p.ID, p.Winner, p.Price, r)
			}
			for _, b := range p.Book {
				if b.Price < r {
					t.Errorf("%s's ask of %d sits in %s's book under the reserve %d: a refused ask left a trace", b.Agent, b.Price, p.ID, r)
				}
			}
			if p.Winner == floorerID && p.Price == r {
				floorWins[p.ID]++
			}
		case l.Type == trace.EventCredit && p.Action == "payout" && p.Agent == floorerID:
			floorPaid[p.Bounty] += p.Amount
		}
	}

	// 3. The destination is a legal place to stand, and it pays what it says.
	if len(floorWins) == 0 {
		t.Fatal("the floorer never won at the reserve: a reserve-priced ask was never shown to be a legal winning ask")
	}
	for id := range floorWins {
		if got := floorPaid[id]; got != reserve[id] {
			t.Errorf("%s: the floorer won at the reserve %d and was paid %d", id, reserve[id], got)
		}
	}

	// 5. Vacuity guards: the sinker sank, and on a card the floorer won at
	// the floor — the refusal and the legal floor bid seen on the same card.
	if len(sunk) == 0 {
		t.Fatal("the sinker never sank: no ask went under the reserve, and every guard above was checked against silence")
	}
	both := 0
	for id := range sunk {
		if floorWins[id] > 0 {
			both++
		}
	}
	if both == 0 {
		t.Fatal("no card carried both a sub-reserve refusal and a floor-priced award: the refusal and the floor were never seen together")
	}
}

// The reserve floors the ask, and nothing floors the margin. A guest that
// bids the platform's floor on a card whose work costs more than the floor is
// awarded the card, does the work, is paid the floor and keeps the loss: the
// engine settles a losing delivery exactly as it settles a winning one, and
// every number the loss is made of is on the trace — the ask, the payout that
// equals it, the meter's calls that exceed it, and the absence of any refund
// or void. The fair-peddlers week's tier-one oracles are this at seven days;
// this is the shape of it on a deck that fits in CI, without python.
func TestFairTheReserveFloorsTheAskAndNotTheMargin(t *testing.T) {
	const floorerID = "frugal"
	lines := fairWorldDays(t, filepath.Join(t.TempDir(), "margin.jsonl"), 2, func(w *world) map[string]stepFunc {
		floorer := func(_ StepRequest, in StepInput) StepResult {
			var acts []Action
			for _, b := range in.Observation.Bounties {
				acts = append(acts, Action{Type: ActionBid, Bounty: b.ID, Price: b.Reserve})
			}
			return out(acts...)
		}
		// One consultation before answering, long enough to cost more than
		// any floor on this deck: work priced under what it costs, which is
		// the peddler's tier-one oracle with the numbers made small.
		spender := func(req StepRequest, in StepInput) StepResult {
			if code := callModel(t, w.base, req.Token, in.Observation.Task.Prompt, 64); code != http.StatusOK {
				t.Errorf("the floorer's one model call on %s returned %d", in.Observation.Task.BountyID, code)
			}
			return solve(req, in)
		}
		return map[string]stepFunc{
			"scholar": script(bidAll(0.4), solve),
			"frugal":  script(floorer, spender),
			"gambler": script(bidAll(0.25), solve),
		}
	}, false)

	type ending struct{ payout, burned ledger.Credits }
	reserve := map[string]ledger.Credits{}
	ask := map[string]ledger.Credits{}     // the floorer's winning ask, per card
	metered := map[string]ledger.Credits{} // the meter's cost per card, from the attempt wallet's name
	paid := map[string]ledger.Credits{}    // payout credits to the floorer, per card
	solved := map[string]ending{}
	madeGood := map[string][]string{} // anything that gives a loss back: refunds, voids
	for _, l := range lines {
		var p struct {
			Action string         `json:"action"`
			ID     string         `json:"id"`
			Bounty string         `json:"bounty"`
			Agent  string         `json:"agent"`
			Winner string         `json:"winner"`
			Wallet string         `json:"wallet"`
			Price  ledger.Credits `json:"price"`
			Res    ledger.Credits `json:"reserve"`
			Payout ledger.Credits `json:"payout"`
			Burned ledger.Credits `json:"burned"`
			Amount ledger.Credits `json:"amount"`
			Cost   ledger.Credits `json:"cost"`
		}
		if err := json.Unmarshal(l.Payload, &p); err != nil {
			continue
		}
		switch {
		case l.Type == trace.EventBounty && p.Action == "posted":
			reserve[p.ID] = p.Res
		case l.Type == trace.EventBounty && p.Action == "awarded" && p.Winner == floorerID:
			ask[p.ID] = p.Price
		case l.Type == trace.EventModelCall:
			// A metered call is billed to an attempt wallet, and the wallet
			// is named for its card: "att:<bounty>:r<round>".
			if parts := strings.Split(p.Wallet, ":"); len(parts) == 3 && parts[0] == "att" {
				metered[parts[1]] += p.Cost
			}
		case l.Type == trace.EventBounty && p.Action == "solved" && p.Agent == floorerID:
			solved[p.ID] = ending{p.Payout, p.Burned}
		case l.Type == trace.EventCredit && p.Action == "payout" && p.Agent == floorerID:
			paid[p.Bounty] += p.Amount
		case l.Type == trace.EventCredit && p.Action == "refund" && p.Agent == floorerID:
			madeGood[p.Bounty] = append(madeGood[p.Bounty], "refund")
		case l.Type == trace.EventBounty && p.Action == "voided":
			madeGood[p.ID] = append(madeGood[p.ID], "voided")
		}
	}

	if len(solved) == 0 {
		t.Fatal("the floorer never delivered: nothing below was checked against anything")
	}
	for id, e := range solved {
		// The vacuity guard first: every delivery here has to be a loss, or
		// this is the reserve test above under a longer name.
		if e.burned <= e.payout {
			t.Fatalf("%s: the floorer burned %d to be paid %d; the work cost less than the floor, and the test measured nothing", id, e.burned, e.payout)
		}
		// 1. Paid its ask, and its ask was the floor: not the loss made up,
		// not the maximum.
		if e.payout != ask[id] || ask[id] != reserve[id] {
			t.Errorf("%s: awarded at %d on a reserve of %d and paid %d; a losing delivery is paid its ask, and its ask was the floor", id, ask[id], reserve[id], e.payout)
		}
		// 2. The ledger says what the settlement says.
		if paid[id] != e.payout {
			t.Errorf("%s: solved with payout %d, credited %d", id, e.payout, paid[id])
		}
		// 3. The loss is the meter's sum, call by call, on the trace.
		if metered[id] != e.burned {
			t.Errorf("%s: solved with burned %d, and the metered calls on its attempt wallet sum to %d", id, e.burned, metered[id])
		}
		// 4. Nothing gave it back.
		if len(madeGood[id]) > 0 {
			t.Errorf("%s: a losing delivery was made good by %v; the reserve floors the ask, and nothing floors the margin", id, madeGood[id])
		}
	}
}

// Who the open book is for, the back of the queue: the README's four-corner
// grid rests on one sentence — swap the two guests' seats and the whole
// difference is the opening card crossing the table, because the queue decides
// a tie and nothing else. Two guests who tie on the first card and never again,
// seated both ways round. From the trace alone: every card the pair tie on goes
// to the seat that spawned first, every other card is awarded to the same name
// at the same price in both seatings, the fair pays out the same total either
// way, and each guest's earnings move by exactly the tied cards' payouts.
func TestFairSwappingTheSeatsMovesOnlyTheTies(t *testing.T) {
	const reader, blind = "reader", "blind"
	cast := func(*world) map[string]stepFunc {
		// Both open at 20% of the maximum. From the second card on the
		// reader asks a point under, so the pair tie once and the price,
		// not the queue, decides everything after.
		undercut := func(_ StepRequest, in StepInput) StepResult {
			var acts []Action
			for _, b := range in.Observation.Bounties {
				frac := 0.19
				if b.ID == "b0001" {
					frac = 0.2
				}
				price := ledger.Credits(float64(b.MaxPayout) * frac)
				if price < b.Reserve {
					price = b.Reserve
				}
				acts = append(acts, Action{Type: ActionBid, Bounty: b.ID, Price: price})
			}
			return out(acts...)
		}
		return map[string]stepFunc{
			"scholar": script(bidAll(0.5), solve),
			"frugal":  script(bidAll(0.5), solve),
			"gambler": script(bidAll(0.5), solve),
			reader:    script(undercut, solve),
			blind:     script(bidAll(0.2), solve),
		}
	}

	type award struct {
		price  ledger.Credits
		winner string
		tied   bool // the pair both at the winning price
	}
	type seating struct {
		spawned map[string]int // seq of each agent's spawned line
		awards  map[string]award
		payout  map[string]ledger.Credits // per bounty
		earned  map[string]ledger.Credits // per agent
		total   ledger.Credits
	}
	read := func(lines []trace.Line) seating {
		s := seating{map[string]int{}, map[string]award{}, map[string]ledger.Credits{}, map[string]ledger.Credits{}, 0}
		for _, l := range lines {
			var p struct {
				Action string         `json:"action"`
				ID     string         `json:"id"`
				Bounty string         `json:"bounty"`
				Agent  string         `json:"agent"`
				Winner string         `json:"winner"`
				Price  ledger.Credits `json:"price"`
				Amount ledger.Credits `json:"amount"`
				Book   []struct {
					Agent string
					Price ledger.Credits
				} `json:"book"`
			}
			if err := json.Unmarshal(l.Payload, &p); err != nil {
				continue
			}
			switch {
			case l.Type == trace.EventAgent && p.Action == "spawned":
				s.spawned[p.Agent] = int(l.Seq)
			case l.Type == trace.EventBounty && p.Action == "awarded":
				a := award{price: p.Price, winner: p.Winner}
				at := map[string]bool{}
				for _, b := range p.Book {
					if b.Price == p.Price {
						at[b.Agent] = true
					}
				}
				a.tied = at[reader] && at[blind]
				s.awards[p.ID] = a
			case l.Type == trace.EventCredit && p.Action == "payout":
				s.payout[p.Bounty] += p.Amount
				s.earned[p.Agent] += p.Amount
				s.total += p.Amount
			}
		}
		return s
	}
	dir := t.TempDir()
	front := read(fairSeatedDays(t, filepath.Join(dir, "reader-first.jsonl"), 1, []string{reader, blind}, cast, false))
	back := read(fairSeatedDays(t, filepath.Join(dir, "blind-first.jsonl"), 1, []string{blind, reader}, cast, false))

	// The guards. The pair have to tie somewhere, on the same cards in both
	// seatings, or nothing below is about the queue; and the swap has to
	// move at least one of those cards, or the seat was never read.
	var tied []string
	moved := false
	for id, a := range front.awards {
		b, ok := back.awards[id]
		if !ok {
			t.Fatalf("%s: awarded with the reader in front and never with it behind", id)
		}
		if a.tied != b.tied {
			t.Fatalf("%s: the pair tie at the winning price in one seating and not the other", id)
		}
		if a.tied {
			tied = append(tied, id)
			moved = moved || a.winner != b.winner
		}
	}
	if len(tied) == 0 {
		t.Fatal("the pair never tied at a winning price: the queue decided nothing, and the test measured nothing")
	}
	if !moved {
		t.Fatal("the pair tied and the same name won both ways round: the seat was not read, and the test measured nothing")
	}
	sort.Strings(tied)

	for name, s := range map[string]seating{"reader in front": front, "blind in front": back} {
		first := reader
		if s.spawned[blind] < s.spawned[reader] {
			first = blind
		}
		for id, a := range s.awards {
			if a.tied && a.winner != first {
				t.Errorf("%s: %s tied and went to %s; the seat that spawned first was %s", name, id, a.winner, first)
			}
		}
	}
	var crossed ledger.Credits
	for _, id := range tied {
		crossed += front.payout[id]
		if front.payout[id] != back.payout[id] {
			t.Errorf("%s: a tied card paid %d one way round and %d the other", id, front.payout[id], back.payout[id])
		}
	}
	for id, a := range front.awards {
		if b := back.awards[id]; !a.tied && (a.winner != b.winner || a.price != b.price) {
			t.Errorf("%s: not a tie, and the swap moved it — %s at %d against %s at %d", id, a.winner, a.price, b.winner, b.price)
		}
	}
	if front.total != back.total {
		t.Errorf("the fair paid out %d with the reader in front and %d with it behind; the seat is not a price", front.total, back.total)
	}
	if got := front.earned[reader] - back.earned[reader]; got != crossed {
		t.Errorf("the reader earned %d more in front than behind; the tied cards %v paid %d", got, tied, crossed)
	}
	if got := back.earned[blind] - front.earned[blind]; got != crossed {
		t.Errorf("the blind seat earned %d more in front than behind; the tied cards %v paid %d", got, tied, crossed)
	}
	if crossed == 0 {
		t.Errorf("the tied cards %v paid nothing: nothing crossed the table", tied)
	}
}

// A floor read from the meter is derivable from the trace alone. A guest that
// prices an answer at what its last answer cost has one rule — never ask under
// the dearest chain paid for so far — and every number that rule acts on is on
// the trace before the rule acts: the meter's cost per call on the attempt
// wallet, the delivery's burned that equals their sum, and the ask that
// follows. So every ask the guest ever makes is recomputable from the events
// before it: the card's reserve, or the dearest burned of its own earlier
// deliveries, whichever is higher. The costermonger's week is this rule on a
// board; this is the rule on a deck that fits in CI, without python, with the
// guest's memo made a closure.
func TestFairAFloorReadFromTheMeterIsDerivableFromTheTraceAlone(t *testing.T) {
	const readerID = "frugal"
	lines := fairWorldDays(t, filepath.Join(t.TempDir(), "meter.jsonl"), 2, func(w *world) map[string]stepFunc {
		var dearest ledger.Credits // the memo: the dearest reading so far
		floored := func(_ StepRequest, in StepInput) StepResult {
			var acts []Action
			for _, b := range in.Observation.Bounties {
				price := b.Reserve
				if dearest > price {
					price = dearest
				}
				acts = append(acts, Action{Type: ActionBid, Bounty: b.ID, Price: price})
			}
			return out(acts...)
		}
		// One call, then the meter: the purse opened the step at
		// in.Wallet.Balance, and the call is the only thing that drew on it.
		metered := func(req StepRequest, in StepInput) StepResult {
			before := in.Wallet.Balance
			if code := callModel(t, w.base, req.Token, in.Observation.Task.Prompt, 64); code != http.StatusOK {
				t.Errorf("the reader's one model call on %s returned %d", in.Observation.Task.BountyID, code)
			}
			if spent := before - walletBalance(t, w.base, req.Token); spent > dearest {
				dearest = spent
			}
			return solve(req, in)
		}
		return map[string]stepFunc{
			"scholar": script(bidAll(0.4), solve),
			readerID:  script(floored, metered),
			"gambler": script(bidAll(0.25), solve),
		}
	}, false)

	type ask struct {
		seq    int64
		bounty string
		price  ledger.Credits
	}
	type delivery struct {
		seq    int64
		burned ledger.Credits
		payout ledger.Credits
	}
	reserve := map[string]ledger.Credits{}
	metered := map[string]ledger.Credits{} // the meter's calls summed per card, from the attempt wallet's name
	var asks []ask
	var deliveries []delivery
	for _, l := range lines {
		var p struct {
			Action string         `json:"action"`
			ID     string         `json:"id"`
			Bounty string         `json:"bounty"`
			Agent  string         `json:"agent"`
			Wallet string         `json:"wallet"`
			Price  ledger.Credits `json:"price"`
			Res    ledger.Credits `json:"reserve"`
			Payout ledger.Credits `json:"payout"`
			Burned ledger.Credits `json:"burned"`
			Cost   ledger.Credits `json:"cost"`
		}
		if err := json.Unmarshal(l.Payload, &p); err != nil {
			continue
		}
		switch {
		case l.Type == trace.EventBounty && p.Action == "posted":
			reserve[p.ID] = p.Res
		case l.Type == trace.EventBid && p.Agent == readerID:
			asks = append(asks, ask{l.Seq, p.Bounty, p.Price})
		case l.Type == trace.EventModelCall:
			if parts := strings.Split(p.Wallet, ":"); len(parts) == 3 && parts[0] == "att" {
				metered[parts[1]] += p.Cost
			}
		case l.Type == trace.EventBounty && p.Action == "solved" && p.Agent == readerID:
			if metered[p.ID] != p.Burned {
				t.Errorf("%s: solved with burned %d, and the meter's calls on its attempt wallet sum to %d", p.ID, p.Burned, metered[p.ID])
			}
			deliveries = append(deliveries, delivery{l.Seq, p.Burned, p.Payout})
		}
	}
	if len(asks) == 0 || len(deliveries) == 0 {
		t.Fatalf("the reader asked %d times and delivered %d times: nothing below was checked against anything", len(asks), len(deliveries))
	}

	// Every ask, recomputed from the events before it.
	raised, lagged := 0, 0
	for _, a := range asks {
		var dearest ledger.Credits
		for _, d := range deliveries {
			if d.seq < a.seq && d.burned > dearest {
				dearest = d.burned
			}
		}
		want := reserve[a.bounty]
		if dearest > want {
			want = dearest
			raised++
		}
		if a.price != want {
			t.Errorf("%s at seq %d: asked %d; the reserve is %d and the dearest delivery before it burned %d", a.bounty, a.seq, a.price, reserve[a.bounty], dearest)
		}
	}
	// The floor lags: a chain dearer than any paid for so far is a delivery
	// paid for, once. Counted, because it is the finding and not a fault.
	for i, d := range deliveries {
		var before ledger.Credits
		for _, e := range deliveries[:i] {
			if e.burned > before {
				before = e.burned
			}
		}
		if d.burned > before && d.burned > d.payout {
			lagged++
		}
	}
	// The guard: the floor has to have raised at least one ask above the
	// reserve, or the meter was never read and the test measured nothing.
	if raised == 0 {
		t.Fatal("no ask was ever above the reserve: the meter was never read, and the test measured nothing")
	}
	t.Logf("asks %d, raised above the reserve %d; deliveries %d, paid to win while the floor lagged %d", len(asks), raised, len(deliveries), lagged)
}

// A rise on the book is a floor, and it is derivable from the trace alone. A
// reader that has never been paid has never read its meter; what it has read
// is the book, and the book shows a rival that stood at the reserve on the
// last card and stands above it now. The higgler's rule is that such a rival
// has left the floor and its ask is the floor to match — and every number the
// rule acts on is on the trace before it acts: the posted reserve, and the
// awarded book. So every ask the reader makes is recomputable from the
// events before it: the reserve, or the highest ask a rival at the reserve
// last time has risen to since, whichever is higher. A riser seated first
// stands in for the costermonger whose floor rose; the reader sits behind it.
func TestFairARiseOnTheBookIsAFloorAndIsDerivableFromTheTraceAlone(t *testing.T) {
	const riser, reader = "riser", "reader"
	cast := func(*world) map[string]stepFunc {
		// The riser asks the reserve on the deck's first two cards and
		// three times it from the third: a bidder leaving the floor.
		rise := func(_ StepRequest, in StepInput) StepResult {
			var acts []Action
			for _, b := range in.Observation.Bounties {
				price := b.Reserve
				if b.ID > "b0002" {
					price = 3 * b.Reserve
				}
				acts = append(acts, Action{Type: ActionBid, Bounty: b.ID, Price: price})
			}
			return out(acts...)
		}
		// The reader: the memo made a closure — the reserve of every card
		// asked on, who stood at the reserve on the last book, the floor.
		placed := map[string]ledger.Credits{}
		at := map[string]bool{}
		var floor ledger.Credits
		read := func(_ StepRequest, in StepInput) StepResult {
			for _, r := range in.Observation.Results {
				reserve, ok := placed[r.Bounty]
				if !ok || len(r.Book) == 0 {
					continue
				}
				next := map[string]bool{}
				for _, e := range r.Book {
					if e.Agent == reader {
						continue
					}
					if at[e.Agent] && e.Asked > reserve && e.Asked > floor {
						floor = e.Asked
					}
					if e.Asked == reserve {
						next[e.Agent] = true
					}
				}
				at = next
			}
			var acts []Action
			for _, b := range in.Observation.Bounties {
				price := b.Reserve
				if floor > price {
					price = floor
				}
				placed[b.ID] = b.Reserve
				acts = append(acts, Action{Type: ActionBid, Bounty: b.ID, Price: price})
			}
			return out(acts...)
		}
		return map[string]stepFunc{
			"scholar": script(bidAll(0.5), solve),
			"frugal":  script(bidAll(0.5), solve),
			"gambler": script(bidAll(0.5), solve),
			riser:     script(rise, solve),
			reader:    script(read, solve),
		}
	}
	lines := fairSeatedDays(t, filepath.Join(t.TempDir(), "rise.jsonl"), 1, []string{riser, reader}, cast, false,
		func(cfg *Config, _ *FairConfig) { cfg.Book = OpenBook })

	// Replay the rule from the trace alone, in seq order: every awarded
	// book the reader is in updates who stood at the reserve and the floor;
	// every bid the reader makes is checked against the floor as it stood.
	reserve := map[string]ledger.Credits{}
	at := map[string]bool{}
	var floor ledger.Credits
	asks, raised, rises := 0, 0, 0
	for _, l := range lines {
		var p struct {
			Action string         `json:"action"`
			ID     string         `json:"id"`
			Bounty string         `json:"bounty"`
			Agent  string         `json:"agent"`
			Price  ledger.Credits `json:"price"`
			Res    ledger.Credits `json:"reserve"`
			Book   []struct {
				Agent string
				Price ledger.Credits
			} `json:"book"`
		}
		if err := json.Unmarshal(l.Payload, &p); err != nil {
			continue
		}
		switch {
		case l.Type == trace.EventBounty && p.Action == "posted":
			reserve[p.ID] = p.Res
		case l.Type == trace.EventBounty && p.Action == "awarded":
			in := false
			for _, e := range p.Book {
				in = in || e.Agent == reader
			}
			if !in {
				continue
			}
			next := map[string]bool{}
			for _, e := range p.Book {
				if e.Agent == reader {
					continue
				}
				if at[e.Agent] && e.Price > reserve[p.ID] {
					rises++
					if e.Price > floor {
						floor = e.Price
					}
				}
				if e.Price == reserve[p.ID] {
					next[e.Agent] = true
				}
			}
			at = next
		case l.Type == trace.EventBid && p.Agent == reader:
			asks++
			want := reserve[p.Bounty]
			if floor > want {
				want = floor
				raised++
			}
			if p.Price != want {
				t.Errorf("%s at seq %d: the reader asked %d; the reserve is %d and the highest rise read before it is %d", p.Bounty, l.Seq, p.Price, reserve[p.Bounty], floor)
			}
		}
	}
	// The guards, both fatal: a book has to have shown a rival leaving the
	// floor, or there was nothing to read; and the reader has to have asked
	// above the reserve at least once, or the rise was read and not acted on.
	if rises == 0 {
		t.Fatal("no book showed a rival at the reserve rising above it: nothing rose, and the test measured nothing")
	}
	if raised == 0 {
		t.Fatal("the reader never asked above the reserve: the rise was read and not acted on, and the test measured nothing")
	}
	t.Logf("reader asks %d, above the reserve %d; rises read %d; floor %d", asks, raised, rises, floor)
}

// buyer is a scripted guest for the catalogue tests: one action list per
// step, in order, and nothing after the script runs out. It keeps every
// observation it was handed, the stayer's way.
type buyer struct {
	mu     sync.Mutex
	script [][]Action
	seen   []Observation
}

func (b *buyer) step(_ StepRequest, in StepInput) StepResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seen = append(b.seen, in.Observation)
	if len(b.script) == 0 {
		return out()
	}
	acts := b.script[0]
	b.script = b.script[1:]
	return out(acts...)
}

func (b *buyer) shown() []Observation {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Observation(nil), b.seen...)
}

// A notebook is the first thing on the catalogue: bought once, paid in full
// and burned, and from then on the agent's memo is held to the bigger cap.
// The same oversize memo is refused before the purchase and kept after it,
// which is the whole of what the money bought.
func TestFairNotebookRaisesTheCapForWhoBought(t *testing.T) {
	tw, read := simTrace(t)
	w := newSim(t, tw, Config{StepTimeout: 5 * time.Second, Dust: 10})
	w.add(t, "pat", 3000)
	long := strings.Repeat("d", MaxMemoBytes+100)
	b := &buyer{script: [][]Action{
		{{Type: ActionMemo, Text: long}, {Type: ActionBuy, Item: "notebook"}},
		{{Type: ActionMemo, Text: long}, {Type: ActionBuy, Item: "notebook"}},
	}}
	w.steps.fns["pat"] = b.step

	f := stayFair(t, w, FairConfig{Notebook: 400})
	at := []town.Standing{{ID: "pat", Place: "office"}}
	for i, mod := range []int{540, 550, 560} {
		if err := f.Visit(1, mod, town.HHMM(mod), at); err != nil {
			t.Fatalf("tick %d: %v", i+1, err)
		}
	}
	rep, err := f.Close()
	if err != nil {
		t.Fatal(err)
	}
	lines := read()

	// Paid once. The second buy is refused as already owned and costs
	// nothing, so a loop that asks every step buys one notebook.
	if got, want := w.balance(t, "pat"), ledger.Credits(3000-400); got != want {
		t.Errorf("balance %d, want %d — one notebook at the list price of 400", got, want)
	}
	if n := count(lines, trace.EventCredit, "bought"); n != 1 {
		t.Errorf("%d bought events, want the one purchase", n)
	}
	if n := count(lines, trace.EventNote, "buy refused: already owned"); n != 1 {
		t.Errorf("%d already-owned refusals, want 1", n)
	}
	// The memo written in the same step as the purchase is over the old cap
	// and refused: the buy settles after the step's actions are filed, so
	// the page the agent paid for is there from its next step. The second
	// write is the same text and is kept.
	if n := count(lines, trace.EventNote, "memo refused: over limit"); n != 1 {
		t.Errorf("%d memo refusals, want 1 — before the notebook, not after", n)
	}
	if n := count(lines, trace.EventAgent, "memo"); n != 1 {
		t.Errorf("%d memos accepted, want 1 — the write after the purchase", n)
	}
	if got := w.orch.memos.get("pat"); got != long {
		t.Errorf("memo held is %d bytes, want the %d-byte one the notebook made room for", len(got), len(long))
	}

	// What it was told, step by step: the catalogue on every board, and
	// what it owns from the step after the purchase on.
	var owned [][]string
	for _, obs := range b.shown() {
		want := []Offer{{Item: "notebook", Price: 400, MemoBytes: NotebookBytes}}
		if !reflect.DeepEqual(obs.ForSale, want) {
			t.Errorf("for sale %+v, want %+v", obs.ForSale, want)
		}
		owned = append(owned, obs.Owned)
	}
	if want := [][]string{nil, {"notebook"}, {"notebook"}}; !reflect.DeepEqual(owned, want) {
		t.Errorf("owned %v, want %v — nothing, then the notebook, kept", owned, want)
	}

	// The standing says what the trace says. A reader holding only the
	// file collects the bought events per agent and has the same list.
	if len(rep.Standings) != 1 || !reflect.DeepEqual(rep.Standings[0].Owned, []string{"notebook"}) {
		t.Errorf("standings %+v, want pat owning a notebook", rep.Standings)
	}
	fromTrace := map[string][]string{}
	for _, l := range lines {
		if l.Type != trace.EventCredit {
			continue
		}
		var ev struct {
			Action, Agent, Item string
			Amount              ledger.Credits
			MemoBytes           int `json:"memo_bytes"`
		}
		if err := json.Unmarshal(l.Payload, &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Action != "bought" {
			continue
		}
		if ev.Amount != 400 || ev.MemoBytes != NotebookBytes {
			t.Errorf("bought event %+v, want 400 credits for %d bytes", ev, NotebookBytes)
		}
		fromTrace[ev.Agent] = append(fromTrace[ev.Agent], ev.Item)
	}
	if !reflect.DeepEqual(fromTrace["pat"], rep.Standings[0].Owned) {
		t.Errorf("trace says pat owns %v, the standing says %v", fromTrace["pat"], rep.Standings[0].Owned)
	}
	if !rep.Conservation.Holds() {
		t.Errorf("conservation broken: %s", rep.Conservation)
	}
}

// A notebook you cannot pay for is a refusal, not a debt: the money stays,
// the cap stays, and the refusal is on the record.
func TestFairNotebookRefusedWhenBroke(t *testing.T) {
	tw, read := simTrace(t)
	w := newSim(t, tw, Config{StepTimeout: 5 * time.Second, Dust: 10})
	w.add(t, "pat", 300)
	b := &buyer{script: [][]Action{{{Type: ActionBuy, Item: "notebook"}}}}
	w.steps.fns["pat"] = b.step

	f := stayFair(t, w, FairConfig{Notebook: 400})
	if err := f.Visit(1, 540, town.HHMM(540), []town.Standing{{ID: "pat", Place: "office"}}); err != nil {
		t.Fatal(err)
	}
	rep, err := f.Close()
	if err != nil {
		t.Fatal(err)
	}
	lines := read()

	if got, want := w.balance(t, "pat"), ledger.Credits(300); got != want {
		t.Errorf("balance %d, want the untouched %d", got, want)
	}
	if n := count(lines, trace.EventCredit, "bought"); n != 0 {
		t.Errorf("%d bought events on a refused purchase, want none", n)
	}
	if n := count(lines, trace.EventNote, "buy refused"); n != 1 {
		t.Errorf("%d refusal notes, want 1 — a refusal nobody records is a silent one", n)
	}
	if got := w.orch.memos.limit("pat"); got != MaxMemoBytes {
		t.Errorf("memo cap %d after a refused purchase, want the baseline %d", got, MaxMemoBytes)
	}
	if len(rep.Standings) != 1 || rep.Standings[0].Owned != nil {
		t.Errorf("standings %+v, want pat owning nothing", rep.Standings)
	}
}

// A fair that sells nothing shows nothing: no catalogue on the board, no
// owned list, and a buy is refused as not for sale. The observation is
// checked at the byte, because every trace recorded before the catalogue
// existed was written by a fair like this one.
func TestFairSellingNothingShowsNothing(t *testing.T) {
	tw, read := simTrace(t)
	w := newSim(t, tw, Config{StepTimeout: 5 * time.Second, Dust: 10})
	w.add(t, "pat", 3000)
	var raw []byte
	b := &buyer{script: [][]Action{{{Type: ActionBuy, Item: "notebook"}}}}
	w.steps.fns["pat"] = func(req StepRequest, in StepInput) StepResult {
		raw = append([]byte(nil), req.Input...)
		return b.step(req, in)
	}

	f := stayFair(t, w, FairConfig{})
	if err := f.Visit(1, 540, town.HHMM(540), []town.Standing{{ID: "pat", Place: "office"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Close(); err != nil {
		t.Fatal(err)
	}
	lines := read()

	for _, key := range []string{`"for_sale"`, `"owned"`} {
		if strings.Contains(string(raw), key) {
			t.Errorf("the step input carries %s on a fair that sells nothing:\n%s", key, raw)
		}
	}
	if got, want := w.balance(t, "pat"), ledger.Credits(3000); got != want {
		t.Errorf("balance %d, want the untouched %d", got, want)
	}
	if n := count(lines, trace.EventNote, "buy refused: not for sale"); n != 1 {
		t.Errorf("%d not-for-sale refusals, want 1", n)
	}
	if n := count(lines, trace.EventCredit, "bought"); n != 0 {
		t.Errorf("%d bought events, want none", n)
	}
	// And the opening line is the one every earlier fair wrote: no
	// notebook key on a day none was offered.
	for _, l := range lines {
		if l.Type == trace.EventEpisode && strings.Contains(string(l.Payload), `"notebook"`) {
			t.Errorf("episode line names a notebook on a fair that sold none: %s", l.Payload)
		}
	}
}
