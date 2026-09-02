package orchestrator

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/singhtushant3-hub/aiphylum/internal/auction"
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
	for _, id := range []string{"scholar", "frugal", "gambler"} {
		w.add(t, id, map[string]ledger.Credits{"scholar": 3000, "frugal": 2500, "gambler": 1600}[id])
		w.steps.fns[id] = steps[id]
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
