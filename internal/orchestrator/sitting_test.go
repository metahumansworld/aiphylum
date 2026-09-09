package orchestrator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/metahumansworld/soscitea/internal/bounty"
	"github.com/metahumansworld/soscitea/internal/rating"
	"github.com/metahumansworld/soscitea/internal/town"
	"github.com/metahumansworld/soscitea/internal/trace"
)

// sitter keeps every board it was shown and answers each: bids frac of
// the payout on everything (or only the first card, with first set), then
// solves — or, with wrong set, submits nonsense on the sitting's cards so
// those attempts fail and the cards come back open. It knows a sitting card
// by having been shown it at a sitting.
type sitter struct {
	mu    sync.Mutex
	seen  []Observation
	sat   map[string]bool
	frac  float64
	wrong bool
	first bool
}

func (s *sitter) step(req StepRequest, in StepInput) StepResult {
	s.mu.Lock()
	s.seen = append(s.seen, in.Observation)
	if s.sat == nil {
		s.sat = map[string]bool{}
	}
	if in.Observation.Sitting {
		for _, b := range in.Observation.Bounties {
			s.sat[b.ID] = true
		}
	}
	wrong := s.wrong && in.Observation.Task != nil && s.sat[in.Observation.Task.BountyID]
	s.mu.Unlock()
	if in.Observation.Phase != PhaseBid {
		if wrong {
			return out(Action{Type: ActionSubmit, Bounty: in.Observation.Task.BountyID, Answer: "not-it"})
		}
		return solve(req, in)
	}
	frac := s.frac
	if frac == 0 {
		frac = 0.5
	}
	acts := bidsFor(in, frac)
	if s.first && len(acts) > 1 {
		acts = acts[:1]
	}
	return out(acts...)
}

// boards is every bid-phase observation, split by whether it was the sitting.
func (s *sitter) boards() (office, sitting []Observation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.seen {
		if o.Phase != PhaseBid {
			continue
		}
		if o.Sitting {
			sitting = append(sitting, o)
		} else {
			office = append(office, o)
		}
	}
	return office, sitting
}

// sittingFair is a fair with one office card at nine and a sitting at five,
// the town's minute for it: after the last window has closed.
func sittingFair(t *testing.T, w *world, cards int) *Fair {
	t.Helper()
	f, err := NewFair(w.ctx, w.orch, FairConfig{
		Deck: []Posting{post(1, 1)}, PostMinutes: []int{540},
		WindowTicks: 1, Office: "office",
		Sitting: &SittingConfig{Minute: 17 * 60, Cards: cards, Deal: func(i int) Posting {
			return post(int64(100+i), 1)
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func postedRanked(t *testing.T, lines []trace.Line) map[string]bool {
	t.Helper()
	got := map[string]bool{}
	for _, l := range lines {
		if l.Type != trace.EventBounty {
			continue
		}
		var p struct {
			Action string `json:"action"`
			ID     string `json:"id"`
			Ranked *bool  `json:"ranked"`
		}
		if err := json.Unmarshal(l.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Action != "posted" {
			continue
		}
		if p.Ranked != nil && !*p.Ranked {
			t.Errorf("%s posted with ranked:false — the key is written only when true", p.ID)
		}
		got[p.ID] = p.Ranked != nil
	}
	return got
}

// The sitting removes the schedule from the question. pat, enrolled and at
// the library, is shown the ranked board and wins on it; sam, not enrolled
// and standing at the office, is shown nothing at five o'clock and solves
// an office card in the morning that moves the ladder by exactly nothing.
func TestFairSittingSeatsTheEnrolledWhereverTheyStand(t *testing.T) {
	tw, read := simTrace(t)
	w := newSim(t, tw, Config{StepTimeout: 5 * time.Second, Dust: 10})
	w.add(t, "pat", 3000)
	w.add(t, "sam", 3000)
	pat, sam := &sitter{}, &sitter{}
	w.steps.fns["pat"], w.steps.fns["sam"] = pat.step, sam.step
	f := sittingFair(t, w, 1)
	if err := f.Enrol("pat"); err != nil {
		t.Fatal(err)
	}
	at := func(pat string) []town.Standing {
		return []town.Standing{{ID: "mira", Place: "office"}, {ID: "pat", Place: pat}, {ID: "sam", Place: "office"}}
	}
	for _, mod := range []int{540, 550, 1020, 1030} {
		if err := f.Visit(1, mod, town.HHMM(mod), at("library")); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := f.Close()
	if err != nil {
		t.Fatal(err)
	}
	lines := read()

	if office, sitting := pat.boards(); len(office) != 0 || len(sitting) != 1 {
		t.Fatalf("pat was shown %d office boards and %d sittings, want 0 and 1", len(office), len(sitting))
	} else if o := sitting[0]; o.Place != "" || o.StayPrice != 0 || len(o.ForSale) != 0 || len(o.Bounties) != 1 {
		t.Errorf("the sitting's board = %+v, want one card and nothing else set", o)
	}
	if office, sitting := sam.boards(); len(office) != 1 || len(sitting) != 0 {
		t.Fatalf("sam was shown %d office boards and %d sittings, want 1 and 0", len(office), len(sitting))
	}
	if n := count(lines, trace.EventBounty, "solved"); n != 2 {
		t.Errorf("%d solves, want sam's office card and pat's sitting card", n)
	}
	if n := count(lines, trace.EventAgent, "enrolled"); n != 1 {
		t.Errorf("%d enrolled lines, want 1", n)
	}
	if n := count(lines, trace.EventEpisode, "sitting"); n != 1 {
		t.Errorf("%d sitting lines, want 1", n)
	}
	if got := postedRanked(t, lines); !got["b0002"] || got["b0001"] {
		t.Errorf("ranked marks by card = %v, want the sitting's b0002 only", got)
	}
	if rep.Sittings != 1 || rep.Dealt != 1 || rep.Withdrawn != 0 {
		t.Errorf("sittings %d dealt %d withdrawn %d, want 1, 1, 0", rep.Sittings, rep.Dealt, rep.Withdrawn)
	}
	if len(rep.Ladder) != 1 || rep.Ladder[0].Agent != "pat" || rep.Ladder[0].Attempts != 1 || rep.Ladder[0].Successes != 1 {
		t.Fatalf("ladder = %+v, want pat's one attempt and nobody else's", rep.Ladder)
	}
	if rep.Ladder[0].Ranked {
		t.Errorf("pat ranked on one attempt; the ladder's gates are the arena's")
	}
	if !rep.Conservation.Holds() {
		t.Errorf("conservation broken: %s", rep.Conservation)
	}
}

// A card the sitting leaves open is withdrawn, never reopened: pat's wrong
// answer fails the first card and nobody bids on the second, and neither is
// on the office's board next morning although pat is standing right there.
func TestFairSittingCardThatFailsIsWithdrawn(t *testing.T) {
	tw, read := simTrace(t)
	w := newSim(t, tw, Config{StepTimeout: 5 * time.Second, Dust: 10})
	w.add(t, "pat", 3000)
	pat := &sitter{wrong: true, first: true}
	w.steps.fns["pat"] = pat.step
	f := sittingFair(t, w, 2)
	if err := f.Enrol("pat"); err != nil {
		t.Fatal(err)
	}
	at := []town.Standing{{ID: "pat", Place: "office"}}
	// Day 1: the office card at nine, the sitting at five. Day 2: pat at
	// the office all morning with two withdrawn cards on the board.
	for _, mod := range []int{540, 550, 1020, 1030} {
		if err := f.Visit(1, mod, town.HHMM(mod), at); err != nil {
			t.Fatal(err)
		}
	}
	for _, mod := range []int{540, 550, 560} {
		if err := f.Visit(2, mod, town.HHMM(mod), at); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := f.Close()
	if err != nil {
		t.Fatal(err)
	}
	lines := read()

	// Day 1's office card, and then the sitting's: nothing of day 2's.
	if n := total(lines, trace.EventBid); n != 2 {
		t.Errorf("%d bids, want pat's office bid and its one sitting bid", n)
	}
	if n := count(lines, trace.EventBounty, "failed"); n != 1 {
		t.Errorf("%d failures, want 1", n)
	}
	if n := count(lines, trace.EventBounty, "no_bids"); n != 1 {
		t.Errorf("%d empty windows, want the sitting's second card only", n)
	}
	if n := count(lines, trace.EventNote, "sitting card withdrawn"); n != 2 {
		t.Errorf("%d withdrawals, want both cards", n)
	}
	if office, _ := pat.boards(); len(office) != 1 {
		t.Errorf("pat was shown %d office boards, want day 1's only — a withdrawn card never opens a window", len(office))
	}
	for _, b := range w.orch.Board.Open() {
		if b.State != bounty.StateOpen {
			t.Errorf("%s is %s on the open board", b.ID, b.State)
		}
	}
	if rep.Withdrawn != 2 || rep.Dealt != 2 || rep.Sittings != 1 {
		t.Errorf("withdrawn %d dealt %d sittings %d, want 2, 2, 1", rep.Withdrawn, rep.Dealt, rep.Sittings)
	}
	if len(rep.Ladder) != 1 || rep.Ladder[0].Successes != 0 || rep.Ladder[0].Attempts != 1 {
		t.Errorf("ladder = %+v, want pat's one failed attempt", rep.Ladder)
	}
}

// The structural half of "judged work is never ranked", at the table: a
// sitting dealt a judged card refuses it outright, rather than dealing it
// and hoping the ladder remembers to look away.
func TestFairSittingRefusesJudgedCards(t *testing.T) {
	w := judgedWorld(t, 100_000)
	w.add(t, "pat", 3000)
	w.steps.fns["pat"] = answers(briefWord)
	f, err := NewFair(w.ctx, w.orch, FairConfig{
		Deck: []Posting{}, PostMinutes: []int{540}, Office: "office",
		Sitting: &SittingConfig{Minute: 1020, Cards: 1, Deal: func(i int) Posting { return briefPost(int64(i+1), 1) }},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Enrol("pat"); err != nil {
		t.Fatal(err)
	}
	err = f.Visit(1, 1020, town.HHMM(1020), []town.Standing{{ID: "pat", Place: "office"}})
	if err == nil || !strings.Contains(err.Error(), "judged") {
		t.Fatalf("err = %v, want the judged card refused", err)
	}
}

// Enrolment is a fact the record carries exactly once, about someone who
// is there.
func TestFairEnrolNeedsAName(t *testing.T) {
	tw, read := simTrace(t)
	w := newSim(t, tw, Config{StepTimeout: 5 * time.Second, Dust: 10})
	w.add(t, "pat", 3000)
	w.add(t, "kim", 3000)
	f := sittingFair(t, w, 1)
	if err := f.Enrol("nobody"); err == nil {
		t.Error("enrolled a name not at the fair")
	}
	if err := f.Enrol("pat"); err != nil {
		t.Fatal(err)
	}
	if err := f.Enrol("pat"); err == nil {
		t.Error("enrolled pat twice")
	}
	if _, err := f.Leave(w.ctx, "kim"); err != nil {
		t.Fatal(err)
	}
	if err := f.Enrol("kim"); err == nil {
		t.Error("enrolled kim after kim left")
	}
	if n := count(read(), trace.EventAgent, "enrolled"); n != 1 {
		t.Errorf("%d enrolled lines, want pat's one", n)
	}
}

// A sitting nobody sits is no sitting: the week with the sitting configured
// and nobody enrolled is, payload for payload, the week without it.
func TestFairSittingWithNoSittersIsNoSitting(t *testing.T) {
	steps := map[string]stepFunc{
		"scholar": script(bidAll(0.8), solve),
		"frugal":  script(bidAll(0.5), solve),
		"gambler": script(bidAll(0.3), solve),
	}
	dir := t.TempDir()
	plain := fairDays(t, filepath.Join(dir, "plain.jsonl"), 2, steps, true)
	held := fairDays(t, filepath.Join(dir, "held.jsonl"), 2, steps, true, func(_ *Config, fc *FairConfig) {
		fc.Sitting = &SittingConfig{Minute: 1020, Deal: func(i int) Posting { return post(int64(100+i), 1) }}
	})
	if !bytes.Equal(payloads(plain), payloads(held)) {
		t.Fatal("a sitting nobody sat changed the week")
	}
	if n := count(plain, trace.EventEpisode, "sitting"); n != 0 {
		t.Fatalf("%d sittings in the plain week", n)
	}
}

// The sitting crosses a checkpoint whole: who is enrolled, which cards were
// withdrawn, and every attempt the ladder is built from. The checkpoint is
// taken the morning after the first sitting, so the resumed fair holds its
// second sitting from the state alone — and the two ladders agree.
func TestFairSittingSurvivesACheckpoint(t *testing.T) {
	// gambler asks least and bids on one card only, wrong at the sitting
	// every time; scholar takes the other card and solves it — so both are
	// on the ladder, and every sitting withdraws a card.
	scholar, gambler := &sitter{frac: 0.8}, &sitter{frac: 0.3, first: true, wrong: true}
	steps := map[string]stepFunc{
		"scholar": scholar.step,
		"frugal":  script(bidAll(0.5), solve),
		"gambler": gambler.step,
	}
	const days, at = 3, 163 // day 2, 10:10 — one sitting behind, two ahead
	fcfg := func() FairConfig {
		deck := make([]Posting, 8*days)
		for i := range deck {
			deck[i] = post(int64(i+1), 1)
		}
		return FairConfig{
			Deck: deck, PostMinutes: []int{540, 600, 660, 720, 780, 840, 900, 960},
			WindowTicks: 3, MaxReopens: 3, Office: "office",
			Sitting: &SittingConfig{Minute: 1020, Cards: 2, Deal: func(i int) Posting {
				return post(int64(100+i), i%3+1)
			}},
		}
	}
	r := resumedWeek(t, fcfg, steps, days, at, []string{"scholar", "gambler"})
	st := r.state
	if len(st.Ranked) != 2 || st.Sittings != 1 || st.SitNext != 2 || len(st.Ladder) == 0 {
		t.Fatalf("a dull checkpoint proves little: ranked %v, sittings %d, dealt %d, ladder %d",
			st.Ranked, st.Sittings, st.SitNext, len(st.Ladder))
	}
	r.sameTail(t)
	r.sameChronicle(t)
	if r.rep2.Sittings != days || r.rep1.Sittings != days {
		t.Fatalf("sittings: unbroken %d, resumed %d, want %d each", r.rep1.Sittings, r.rep2.Sittings, days)
	}
	if got, want := fmtRows(r.rep2.Ladder), fmtRows(r.rep1.Ladder); got != want {
		t.Fatalf("ladders differ\n  resumed:  %s\n  unbroken: %s", got, want)
	}
	if len(r.rep1.Ladder) != 2 {
		t.Fatalf("ladder = %s, want scholar and gambler on it", fmtRows(r.rep1.Ladder))
	}
	if r.rep1.Withdrawn == 0 {
		t.Error("gambler's wrong answers withdrew nothing all week")
	}
}

// fmtRows is a ladder as one line, for comparing two.
func fmtRows(rows []rating.Row) string {
	parts := make([]string, len(rows))
	for i, r := range rows {
		parts[i] = fmt.Sprintf("%+v", r)
	}
	return strings.Join(parts, "; ")
}
