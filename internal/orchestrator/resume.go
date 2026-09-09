// The fair written down and picked up again.
//
// A fair runs on the town's tick, and between two ticks nothing is in
// flight: every attempt is synchronous inside Visit, every window closes at
// a tick number, every purchase has been paid for by the time Visit returns.
// So the fair at a tick boundary is a finite list of plain facts, and this
// file is that list — Save writes it, ResumeFair reads it back onto a fresh
// orchestrator over the same books — and nothing about the fair's own loop
// had to change to make it so.
//
// What a state carries and what it does not is the same rule the trace
// keeps: the recipe, never the answer. Open bounties are stored as the
// generator, seed and tier that made them and regenerated on the way back;
// sealed bids are stored sealed, in the file the world keeps for itself.
package orchestrator

import (
	"context"
	"fmt"

	"github.com/metahumansworld/soscitea/internal/auction"
	"github.com/metahumansworld/soscitea/internal/ledger"
	"github.com/metahumansworld/soscitea/internal/town"
	"github.com/metahumansworld/soscitea/internal/trace"
)

// FairState is the fair at a tick boundary, with the orchestrator's share
// of it — roster, memos, undelivered results, epoch, the open board — since
// a fair resumed onto an orchestrator that remembered none of that would be
// a different fair. The maps are the fair's own, handed over rather than
// copied: a caller writes the state down before the next tick moves them.
type FairState struct {
	Tick   int
	Posted int
	Next   int

	Windows  []WindowState
	Reopens  map[string]int
	Shelved  map[string]bool
	Draws    map[string]int
	Standing []SimStanding // roster order, only the agents with a line
	Held     map[string]int
	Owned    map[string][]string
	Stock    map[string]Offer
	StockRev int
	Shopped  map[string]int
	Pitches  []town.Cell // the ground still unsold

	Agents  []Agent // roster order, retirement included
	Memos   map[string]string
	Limits  map[string]int
	Pending map[string][]AuctionResult
	Epoch   int
	Board   []BountyState // every open bounty, in posting order — shelved ones too
	LastID  int
}

// WindowState is one open bid window: which bounty, when it closes, who has
// been shown it, and the sealed book so far.
type WindowState struct {
	Bounty    string
	CloseTick int
	Salt      uint64
	Shown     map[string]bool
	Bids      []auction.Bid
}

// BountyState is the recipe for one open bounty — enough for Board.Restore
// to make it again under the same ID, and nothing the key could be read
// from.
type BountyState struct {
	ID           string
	Generator    string
	Seed         int64
	Tier         int
	TokenCeiling ledger.Credits
	WallClockSec int
	Failures     int
}

// Save is the fair as it stands, for a checkpoint. Call it between ticks —
// from town.Config.Checkpoint, which fires after Visit — and nowhere else.
func (f *Fair) Save() FairState {
	st := FairState{
		Tick: f.tick, Posted: f.posted, Next: f.next,
		Reopens: f.reopens, Shelved: f.shelved, Draws: f.draws,
		Held: f.held, Owned: f.owned, Stock: f.stock, StockRev: f.stockRev, Shopped: f.shopped,
		Pitches: f.cfg.Pitches,
		Epoch:   f.o.epoch, LastID: f.o.Board.LastID(),
	}
	for _, w := range f.windows {
		st.Windows = append(st.Windows, WindowState{
			Bounty: w.b.ID, CloseTick: w.closeTick, Salt: w.auc.Salt, Shown: w.shown, Bids: w.auc.Book(),
		})
	}
	for _, ag := range f.o.agents {
		st.Agents = append(st.Agents, *ag)
		if s, ok := f.standing[ag.ID]; ok {
			st.Standing = append(st.Standing, *s)
		}
	}
	for _, b := range f.o.Board.Open() {
		st.Board = append(st.Board, BountyState{
			ID: b.ID, Generator: b.Generator, Seed: b.Seed, Tier: b.Tier,
			TokenCeiling: b.TokenCeiling, WallClockSec: b.WallClockSec, Failures: b.Failures,
		})
	}
	st.Memos, st.Limits = f.o.memos.save()
	st.Pending = f.o.crier.save()
	return st
}

// ResumeFair is NewFair for a fair that already started: the same checks
// and wiring onto an orchestrator with no roster yet, then the state put
// back — the roster seated without a grant or a spawned line, because the
// wallets are in the books the orchestrator was opened over, and every
// window, memo and standing where Save found it. The trace gets one line,
// "resumed", and no opening line: the episode was opened by the process
// this one is picking up after, and its start is already in the file.
//
// The books are checked against the state at the one point they overlap:
// an agent the state calls retired must be closed in the ledger and the
// other way round, or the checkpoint and the copy of the books it was
// taken beside are not from the same moment.
func ResumeFair(ctx context.Context, o *Orchestrator, cfg FairConfig, st FairState) (*Fair, error) {
	if len(o.agents) > 0 {
		return nil, fmt.Errorf("orchestrator: resume onto a world that already seated %d agents", len(o.agents))
	}
	f, err := newFair(ctx, o, cfg)
	if err != nil {
		return nil, err
	}
	for _, a := range st.Agents {
		acct, err := o.Ledger.Get(ctx, a.ID)
		if err != nil {
			return nil, fmt.Errorf("resume %s: %w", a.ID, err)
		}
		if acct.Kind != ledger.KindAgent {
			return nil, fmt.Errorf("resume %s: %s is not an agent wallet", a.ID, acct.Kind)
		}
		if acct.Closed != a.Retired {
			return nil, fmt.Errorf("resume %s: the books say closed=%v, the checkpoint says retired=%v", a.ID, acct.Closed, a.Retired)
		}
		a := a
		o.agents = append(o.agents, &a)
	}
	o.memos.load(st.Memos, st.Limits)
	o.crier.load(st.Pending)
	o.epoch = st.Epoch
	for _, b := range st.Board {
		if _, err := o.Board.Restore(b.ID, b.Generator, b.Seed, b.Tier, b.TokenCeiling, b.WallClockSec, b.Failures); err != nil {
			return nil, fmt.Errorf("resume board: %w", err)
		}
	}
	o.Board.Resume(st.LastID)

	f.tick, f.posted, f.next = st.Tick, st.Posted, st.Next
	f.stockRev = st.StockRev
	f.cfg.Pitches = st.Pitches
	// A map JSON never saw becomes nil on the way back; the loop indexes
	// and assigns into every one of these, so each is at least empty.
	if f.reopens = st.Reopens; f.reopens == nil {
		f.reopens = map[string]int{}
	}
	if f.shelved = st.Shelved; f.shelved == nil {
		f.shelved = map[string]bool{}
	}
	if f.draws = st.Draws; f.draws == nil {
		f.draws = map[string]int{}
	}
	if f.held = st.Held; f.held == nil {
		f.held = map[string]int{}
	}
	if f.owned = st.Owned; f.owned == nil {
		f.owned = map[string][]string{}
	}
	if f.stock = st.Stock; f.stock == nil {
		f.stock = map[string]Offer{}
	}
	if f.shopped = st.Shopped; f.shopped == nil {
		f.shopped = map[string]int{}
	}
	for _, s := range st.Standing {
		s := s
		f.standing[s.Agent] = &s
	}
	for _, w := range st.Windows {
		b, err := o.Board.Get(w.Bounty)
		if err != nil {
			return nil, fmt.Errorf("resume window: %w", err)
		}
		auc := auction.New(b.ID, b.MaxPayout, o.reserveFor(b.MaxPayout))
		if cfg.Tie == auction.ByLot {
			auc.Tie, auc.Salt = auction.ByLot, w.Salt
		}
		auc.Load(w.Bids)
		shown := w.Shown
		if shown == nil {
			shown = map[string]bool{}
		}
		f.windows = append(f.windows, &fairWindow{auc: auc, b: b, closeTick: w.CloseTick, shown: shown})
	}
	o.traceEvent(trace.EventEpisode, map[string]any{"action": "resumed", "track": "fair", "tick": st.Tick})
	return f, nil
}
