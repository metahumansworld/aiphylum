package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/metahumansworld/soscitea/internal/town"
	"github.com/metahumansworld/soscitea/internal/trace"
)

// The door out. A guest who bids into an open window, buys a stall and
// stocks it, then walks: the balance it left with is the balance at the
// close, the bid it left in the book voids on award with its own reason,
// its shelf is cleared so the square steps nobody for it, and the sweep
// never retires a wallet that is not here to be swept — the guest leaves
// with exactly the dust threshold, the balance the sweep would take from
// anyone still here. The state taken at
// the door carries the gap, so a fair resumed from it closes on the same
// table as the one that never stopped.
func TestFairDeparterKeepsItsBalance(t *testing.T) {
	dir := t.TempDir()
	ocfg := Config{StepTimeout: 5 * time.Second, Dust: 10}
	// stayFair's deck and hours, spelled out: the resumed half needs the
	// same config, not a helper that only knows how to found.
	fcfg := FairConfig{
		Deck: []Posting{post(1, 1), post(2, 1), post(3, 1)}, PostMinutes: []int{540, 550, 560},
		WindowTicks: 3, Office: "office",
		Stall: 400, StallPlace: "square", Pitches: []town.Cell{{X: 7, Y: 7}},
	}
	cast := func(w *world) *buyer {
		pat := &buyer{}
		w.steps.fns["pat"] = pat.step
		w.steps.fns["quinn"] = (&buyer{}).step // never stepped again once gone
		return pat
	}
	// pat is at the office for the three windows, then on the square where
	// a stocked stall would step it.
	at := func(i int) []town.Standing {
		if i < 3 {
			return []town.Standing{{ID: "pat", Place: "office"}, {ID: "quinn", Place: "office"}}
		}
		return []town.Standing{{ID: "pat", Place: "square"}}
	}
	mods := []int{540, 550, 560, 570, 580, 590}
	const gone = 3 // ticks played before quinn leaves

	whole, err := trace.NewWriter(filepath.Join(dir, "whole.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	w := newWorldAt(t, filepath.Join(dir, "whole.db"), whole, ocfg)
	w.orch.Ladder = nil
	w.add(t, "pat", 3000)
	w.add(t, "quinn", fcfg.Stall+ocfg.Dust) // the stall, and then dust to leave with
	pat := cast(w)
	// The stall is bought and stocked in one step, the last before the
	// door: a wallet at dust survives no sweep, and the sweep opens every
	// tick, so quinn reaches the threshold only on its way out.
	w.steps.fns["quinn"] = (&buyer{script: [][]Action{
		{{Type: ActionBid, Bounty: "b0001", Price: 50}},
		{},
		{{Type: ActionBuy, Item: "stall"}, {Type: ActionStock, Item: "candle", Price: 100}},
	}}).step
	f, err := NewFair(w.ctx, w.orch, fcfg)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < gone; i++ {
		if err := f.Visit(1, mods[i], town.HHMM(mods[i]), at(i)); err != nil {
			t.Fatalf("tick %d: %v", i+1, err)
		}
	}

	// The door refuses what it should, then lets quinn out.
	if _, err := f.Leave(w.ctx, "nobody"); err == nil {
		t.Error("left a name nobody holds")
	}
	bal, err := f.Leave(w.ctx, "quinn")
	if err != nil {
		t.Fatal(err)
	}
	if bal != ocfg.Dust {
		t.Errorf("left with %d credits, want %d (the grant less the stall)", bal, ocfg.Dust)
	}
	if _, err := f.Leave(w.ctx, "quinn"); err == nil {
		t.Error("left twice")
	}

	// The record at the door: books copied, state through JSON.
	copyDB := filepath.Join(dir, "door.db")
	if err := w.orch.Ledger.Snapshot(context.Background(), copyDB); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(f.Save())
	if err != nil {
		t.Fatal(err)
	}
	seq := whole.Seq()
	var st FairState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range st.Agents {
		if a.ID == "quinn" {
			found = a.Left && !a.Retired
		}
	}
	if !found {
		t.Fatalf("the state does not carry the gap: %+v", st.Agents)
	}

	for i := gone; i < len(mods); i++ {
		if err := f.Visit(1, mods[i], town.HHMM(mods[i]), at(i)); err != nil {
			t.Fatalf("tick %d: %v", i+1, err)
		}
	}
	w.orch.agent("pat").Retired = true // for one question only
	if _, err := f.Leave(w.ctx, "pat"); err == nil {
		t.Error("a bankrupt left; there is nothing to leave with")
	}
	w.orch.agent("pat").Retired = false
	rep1, err := f.Close()
	if err != nil {
		t.Fatal(err)
	}
	whole.Close()
	all, err := trace.Read(whole.Path())
	if err != nil {
		t.Fatal(err)
	}

	var quinn *SimStanding
	for i := range rep1.Standings {
		if rep1.Standings[i].Agent == "quinn" {
			quinn = &rep1.Standings[i]
		}
	}
	if quinn == nil || !quinn.Left || quinn.Retired {
		t.Fatalf("the departed is not marked left in the closing table: %+v", rep1.Standings)
	}
	if quinn.Balance != bal {
		t.Errorf("closed on %d credits, left with %d; absence should freeze the wallet", quinn.Balance, bal)
	}
	if quinn.Attempts != 0 {
		t.Errorf("the departed attempted %d bounties after leaving", quinn.Attempts)
	}
	if !rep1.Conservation.Holds() {
		t.Errorf("conservation broken: %s", rep1.Conservation)
	}
	voided := 0
	for _, l := range all {
		if l.Type == trace.EventBounty && strings.Contains(string(l.Payload), `"reason":"winner left before attempt"`) {
			voided++
		}
	}
	if voided != 1 {
		t.Errorf("%d cards voided for a winner who left, want 1", voided)
	}
	if n := count(all, trace.EventAgent, "left"); n != 1 {
		t.Errorf("%d left events, want 1", n)
	}
	if n := len(pat.shown()); n != 3 {
		t.Errorf("pat stepped %d times, want 3 — a shelf with no one behind it steps nobody on the square", n)
	}
	if wares := f.wares("pat"); wares != nil {
		t.Errorf("wares = %v, want none from a seller who left", wares)
	}

	// Picked up from the door: the same ticks again, the same table.
	again, err := trace.NewWriter(filepath.Join(dir, "again.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	w2 := newWorldAt(t, copyDB, again, ocfg)
	w2.orch.Ladder = nil
	cast(w2)
	f2, err := ResumeFair(w2.ctx, w2.orch, fcfg, st)
	if err != nil {
		t.Fatal(err)
	}
	for i := gone; i < len(mods); i++ {
		if err := f2.Visit(1, mods[i], town.HHMM(mods[i]), at(i)); err != nil {
			t.Fatalf("resumed tick %d: %v", i+1, err)
		}
	}
	rep2, err := f2.Close()
	if err != nil {
		t.Fatal(err)
	}
	again.Close()
	got, err := trace.Read(again.Path())
	if err != nil {
		t.Fatal(err)
	}
	key := func(l trace.Line) string { return string(l.Type) + " " + timeless(t, l.Payload) }
	tail := all[seq:]
	if len(got) != len(tail)+1 {
		t.Fatalf("resumed fair wrote %d lines, the unbroken tail is %d (+1 for the resumed line)", len(got), len(tail))
	}
	if key(got[0]) != fmt.Sprintf(`episode {"action":"resumed","tick":%d,"track":"fair"}`, gone) {
		t.Fatalf("first resumed line = %s", key(got[0]))
	}
	for i, l := range got[1:] {
		if key(l) != key(tail[i]) {
			t.Fatalf("line %d after the door differs\n  resumed:  %s\n  unbroken: %s", i+1, key(l), key(tail[i]))
		}
	}
	for i, s := range rep2.Standings {
		if fmt.Sprint(s) != fmt.Sprint(rep1.Standings[i]) {
			t.Fatalf("standing %d: resumed %+v, unbroken %+v", i, s, rep1.Standings[i])
		}
	}
}
