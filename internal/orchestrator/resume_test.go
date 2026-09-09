package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/metahumansworld/soscitea/internal/bounty"
	"github.com/metahumansworld/soscitea/internal/ledger"
	"github.com/metahumansworld/soscitea/internal/proxy"
	"github.com/metahumansworld/soscitea/internal/town"
	"github.com/metahumansworld/soscitea/internal/trace"
)

// A fair picked up from a checkpoint — the state, the books copied at the
// same boundary, the trace cut back to the same line — writes exactly what
// the unbroken fair wrote from that tick on, after one "resumed" line. The
// state goes through JSON, as the daemon carries it, and the checkpoint is
// taken mid-week with windows open, a notebook sold and results undelivered,
// so every piece of FairState is in play.
func TestFairResumesFromACheckpoint(t *testing.T) {
	steps := map[string]stepFunc{
		"scholar": script(bidAll(0.8), solve),
		"frugal":  script(buyAndBid("notebook", 0.5), solve),
		"gambler": script(bidAll(0.3), solve),
	}
	const days, at = 3, 163 // day 2, 10:10: a window open on the ten o'clock card, bids in it
	dir := t.TempDir()
	m, people := town.AshmereFair()
	ocfg := Config{StepTimeout: 10 * time.Second, Dust: 10}
	fcfg := func() FairConfig {
		deck := make([]Posting, 8*days)
		for i := range deck {
			deck[i] = post(int64(i+1), 1)
		}
		return FairConfig{
			Deck: deck, PostMinutes: []int{540, 600, 660, 720, 780, 840, 900, 960},
			WindowTicks: 3, MaxReopens: 3, Office: "office", Notebook: 300,
		}
	}

	// The unbroken week, checkpointed at tick `at`.
	whole, err := trace.NewWriter(filepath.Join(dir, "whole.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	w := newWorldAt(t, filepath.Join(dir, "whole.db"), whole, ocfg)
	for _, id := range []string{"scholar", "frugal", "gambler"} {
		w.add(t, id, map[string]ledger.Credits{"scholar": 3000, "frugal": 2500, "gambler": 1600}[id])
		w.steps.fns[id] = steps[id]
	}
	f, err := NewFair(w.ctx, w.orch, fcfg())
	if err != nil {
		t.Fatal(err)
	}
	var saved []byte
	var savedTown *town.State
	var seq int64
	copyDB := filepath.Join(dir, "boundary.db")
	cfg := town.Config{
		TickMinutes: 10, Interval: time.Microsecond, Days: days, StartMinute: 7 * 60,
		Mind: &town.Minds{Provider: &proxy.StubProvider{}}, Visit: f.Visit, Hold: f.Hold,
		Checkpoint: func(st town.State) error {
			if st.Tick != at {
				return nil
			}
			if err := w.orch.Ledger.Snapshot(context.Background(), copyDB); err != nil {
				return err
			}
			raw, err := json.Marshal(f.Save())
			if err != nil {
				return err
			}
			saved, seq = raw, whole.Seq()
			raw, err = json.Marshal(st)
			if err != nil {
				return err
			}
			savedTown = &town.State{}
			return json.Unmarshal(raw, savedTown)
		},
	}
	if _, err := town.Run(context.Background(), whole, m, people, cfg); err != nil {
		t.Fatal(err)
	}
	rep1, err := f.Close()
	if err != nil {
		t.Fatal(err)
	}
	whole.Close()
	all, err := trace.Read(whole.Path())
	if err != nil {
		t.Fatal(err)
	}
	var st FairState
	if err := json.Unmarshal(saved, &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Windows) == 0 || len(st.Board) == 0 || len(st.Memos)+len(st.Pending) == 0 || len(st.Owned["frugal"]) == 0 {
		t.Fatalf("a dull checkpoint proves little: windows %d, board %d, memos %d, pending %d, owned %v",
			len(st.Windows), len(st.Board), len(st.Memos), len(st.Pending), st.Owned)
	}

	// The same week again from the boundary: the books are the copy, the
	// trace is a fresh file, the roster comes from the state and not from
	// AddAgent.
	again, err := trace.NewWriter(filepath.Join(dir, "again.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	w2 := newWorldAt(t, copyDB, again, ocfg)
	for id, fn := range steps {
		w2.steps.fns[id] = fn
	}
	f2, err := ResumeFair(w2.ctx, w2.orch, fcfg(), st)
	if err != nil {
		t.Fatal(err)
	}
	cfg2 := cfg
	cfg2.Checkpoint, cfg2.From, cfg2.Visit, cfg2.Hold = nil, savedTown, f2.Visit, f2.Hold
	if _, err := town.Run(context.Background(), again, m, nil, cfg2); err != nil {
		t.Fatal(err)
	}
	rep2, err := f2.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !rep2.Conservation.Holds() {
		t.Fatalf("resumed conservation broken: %s", rep2.Conservation)
	}
	again.Close()
	got, err := trace.Read(again.Path())
	if err != nil {
		t.Fatal(err)
	}

	key := func(l trace.Line) string { return string(l.Type) + " " + timeless(t, l.Payload) }
	tail := all[seq:]
	if len(got) != len(tail)+1 {
		t.Fatalf("resumed run wrote %d lines, the unbroken tail is %d (+1 for the resumed line)", len(got), len(tail))
	}
	if key(got[0]) != `episode {"action":"resumed","tick":163,"track":"fair"}` {
		t.Fatalf("first resumed line = %s", key(got[0]))
	}
	for i, l := range got[1:] {
		if key(l) != key(tail[i]) {
			t.Fatalf("line %d after the resume differs\n  resumed:  %s\n  unbroken: %s", i+1, key(l), key(tail[i]))
		}
	}

	// And the chronicle agrees at the end: every standing the same, the
	// tallies carried across the boundary included.
	if len(rep2.Standings) != len(rep1.Standings) {
		t.Fatalf("resumed chronicle has %d standings, unbroken %d", len(rep2.Standings), len(rep1.Standings))
	}
	for i, s := range rep2.Standings {
		if fmt.Sprint(s) != fmt.Sprint(rep1.Standings[i]) {
			t.Fatalf("standing %d: resumed %+v, unbroken %+v", i, s, rep1.Standings[i])
		}
	}
	if rep2.Ticks != rep1.Ticks || rep2.Posted != rep1.Posted || rep2.Shelved != rep1.Shelved {
		t.Fatalf("resumed chronicle %+v, unbroken %+v", rep2, rep1)
	}
}

// buyAndBid is bidAll with the first offer at the office bought first.
func buyAndBid(item string, frac float64) stepFunc {
	return func(_ StepRequest, in StepInput) StepResult {
		acts := bidsFor(in, frac)
		for _, o := range in.Observation.ForSale {
			if o.Item == item {
				acts = append([]Action{{Type: ActionBuy, Item: item}}, acts...)
			}
		}
		return out(acts...)
	}
}

// newWorldAt is newWorld with the ledger at a path of the test's choosing —
// the copy a checkpoint left, for a resumed world.
func newWorldAt(t *testing.T, dbPath string, tw *trace.Writer, cfg Config) *world {
	t.Helper()
	led, err := ledger.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { led.Close() })
	board := bounty.NewBoard()
	board.RegisterGenerator(sayGen{})
	table := proxy.NewPriceTable()
	table.Set("stub-1", proxy.Price{InputPerTok: 1000, OutputPerTok: 1000})
	steps := newFakeSteps()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	orch := New(led, board, table, &proxy.StubProvider{}, tw, steps, nil, cfg, log)
	srv := httptest.NewServer(orch.Proxy)
	t.Cleanup(srv.Close)
	return &world{orch: orch, steps: steps, base: srv.URL, ctx: context.Background()}
}
