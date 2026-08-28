// The fair track: the arena's economy standing in the town's square.
//
// The sim proved the money could run on a clock; the fair proves it can run
// on a *place*. The town owns the bodies — schedules, walls, walking — and
// hands this file one fact per tick through town.Config.Visit: who is
// standing where. The fair owns everything with money in it: posting,
// auctions, attempts, settlement, all through the same unexported money-path
// the ranked loop and the sim use, because a currency that means two things
// in two venues means nothing.
//
// The one coupling rule is distance: you must be standing at the Bounty
// Office to see the board. A bounty posted while an agent is at lunch is a
// bounty that agent never hears about, and a window that opens and closes to
// an empty room reopens later or is shelved. Nothing enforces drama here
// beyond the schedules.
//
// Unlike the sim, the fair is deterministic. Its clock is the town's tick
// counter, not the wall — windows close at tick numbers, postings land at
// minutes of the simulated day — so the same seed and the same schedules
// write the same trace, byte for byte modulo timestamps. It is unranked for
// the sim's reason: who was standing at the board when a bounty appeared is
// a schedule, not a skill.
package orchestrator

import (
	"context"
	"errors"
	"fmt"

	"github.com/singhtushant3-hub/aiphylum/internal/auction"
	"github.com/singhtushant3-hub/aiphylum/internal/bounty"
	"github.com/singhtushant3-hub/aiphylum/internal/ledger"
	"github.com/singhtushant3-hub/aiphylum/internal/rating"
	"github.com/singhtushant3-hub/aiphylum/internal/town"
	"github.com/singhtushant3-hub/aiphylum/internal/trace"
)

// FairConfig sets the fair's rhythm.
type FairConfig struct {
	// Deck is the supply, posted one card per posting minute in order.
	Deck []Posting
	// PostMinutes are the minutes of the simulated day a deck card is posted
	// at — the office keeps posting hours the way the bakery keeps baking
	// hours. A minute that ticks past with the deck exhausted posts nothing.
	PostMinutes []int
	// WindowTicks is how many town ticks an auction accepts bids before it
	// awards to the lowest ask it has. Zero means the default 3.
	WindowTicks int
	// MaxReopens is how many empty windows a bounty gets before it is shelved,
	// exactly as in the sim. Zero means the default 3.
	MaxReopens int
	// Office is the place ID an agent must be standing in to see the board.
	Office string
}

// fairWindow is one open bid window, closing at a tick number rather than a
// wall-clock deadline — that difference is the whole of why the fair replays
// and the sim does not.
type fairWindow struct {
	auc       *auction.Auction
	b         *bounty.Bounty
	closeTick int
	shown     map[string]bool // agents already shown this window at the board
}

// FairReport is the chronicle a fair day leaves behind. Standings reuse the
// sim's shape and the sim's refusal: attempts and credits in roster order,
// nothing ranked.
type FairReport struct {
	Ticks        int
	Posted       int
	Shelved      int
	Standings    []SimStanding
	Conservation ledger.Conservation
}

// Fair runs the bounty economy against the town's clock. Construct with
// NewFair, hand Visit to town.Config, and call Close once the town's run
// returns.
type Fair struct {
	o   *Orchestrator
	cfg FairConfig

	// Money survives the interrupt. A cancelled context must still be able to
	// refund a fault and close an attempt wallet, so settlement runs on a
	// context that outlives the cancel while the agents' code does not —
	// the same split the sim makes, for the same reason.
	runCtx    context.Context
	settleCtx context.Context

	tick     int
	posted   int
	next     int // next deck card
	windows  []*fairWindow
	reopens  map[string]int
	shelved  map[string]bool
	standing map[string]*SimStanding
}

// NewFair wires a fair onto an orchestrator and announces the episode. Like
// RunSim it refuses a ladder: presence is a schedule, not a skill, so a fair
// is watchable, never scored.
func NewFair(ctx context.Context, o *Orchestrator, cfg FairConfig) (*Fair, error) {
	if o.Ladder != nil {
		return nil, errors.New("orchestrator: the fair track is unranked by design — run it with a nil ladder")
	}
	if cfg.WindowTicks <= 0 {
		cfg.WindowTicks = 3
	}
	if cfg.MaxReopens <= 0 {
		cfg.MaxReopens = 3
	}
	if cfg.Office == "" {
		return nil, errors.New("orchestrator: the fair needs an office — presence at it is the whole coupling")
	}
	f := &Fair{
		o:         o,
		cfg:       cfg,
		runCtx:    ctx,
		settleCtx: context.WithoutCancel(ctx),
		reopens:   map[string]int{},
		shelved:   map[string]bool{},
		standing:  map[string]*SimStanding{},
	}
	for _, ag := range o.live() {
		f.standing[ag.ID] = &SimStanding{Agent: ag.ID}
	}
	// The tally rides the same record() call the ladder would have taken —
	// the fair counts exactly what the benchmark counts and refuses to
	// divide the two numbers, same as the sim.
	o.watch = f.tally
	o.traceEvent(trace.EventEpisode, map[string]any{
		"action": "start", "track": "fair",
		"deck": len(cfg.Deck), "post_minutes": cfg.PostMinutes,
		"window_ticks": cfg.WindowTicks, "office": cfg.Office,
	})
	return f, nil
}

// tally folds one settled attempt into its agent's account. Settlement runs
// on the town's goroutine, inside Visit, so the map needs no lock.
func (f *Fair) tally(a rating.Attempt) {
	st, ok := f.standing[a.Agent]
	if !ok {
		return
	}
	st.Attempts++
	st.Earned += a.Earned
	st.Burned += a.Burned
	if a.Success {
		st.Solved++
	}
}

// Visit is the town's seam, and the fair's entire main loop: one call per
// town tick, on the town's goroutine. Everything below is keyed to the tick
// counter and iterated in posting or roster order, so the fair inherits the
// town's determinism instead of trading it away.
func (f *Fair) Visit(day, mod int, clock string, standings []town.Standing) error {
	f.tick++

	// Retire anyone who bid themselves broke since their last settlement.
	// Everything here is synchronous, so nobody has money out in a wallet
	// that still has to come home — the sweep is always safe.
	for _, ag := range f.o.live() {
		if err := f.o.settleAgent(f.settleCtx, ag); err != nil {
			return err
		}
	}

	// Posting hours. The office posts whether or not anyone is standing in
	// it — a bounty nobody was there to see is the point, not a bug.
	for _, pm := range f.cfg.PostMinutes {
		if pm != mod || f.next >= len(f.cfg.Deck) {
			continue
		}
		p := f.cfg.Deck[f.next]
		f.next++
		if _, err := f.o.postBounty(p); err != nil {
			return err
		}
		f.posted++
	}

	// A window for every open bounty that has none: fresh postings, failed
	// attempts back on the board, and no_bids reopens all come through here,
	// in the board's posting order.
	for _, b := range f.o.Board.Open() {
		if f.shelved[b.ID] || f.windowFor(b.ID) != nil {
			continue
		}
		f.windows = append(f.windows, &fairWindow{
			auc:       auction.New(b.ID, b.MaxPayout, f.o.reserveFor(b.MaxPayout)),
			b:         b,
			closeTick: f.tick + f.cfg.WindowTicks,
			shown:     map[string]bool{},
		})
	}

	// Presence gates information: only agents standing at the office are
	// shown the board, and each is shown each window once. Standings arrive
	// in roster order, which is also registration order for the cast, so the
	// bid steps run in the same order every day.
	for _, st := range standings {
		if st.Place != f.cfg.Office {
			continue
		}
		ag := f.o.agent(st.ID)
		if ag == nil || ag.Retired {
			continue // a resident, or a ghost: the seam names everyone in town
		}
		var views []BountyView
		aucs := make(map[string]*auction.Auction, len(f.windows))
		for _, w := range f.windows {
			aucs[w.b.ID] = w.auc // every open window takes bids from the room
			if w.shown[st.ID] {
				continue
			}
			w.shown[st.ID] = true
			views = append(views, f.o.bountyView(w.b))
		}
		if len(views) == 0 {
			continue
		}
		f.o.placeBids(ag, f.o.performBidStep(f.runCtx, ag, f.tick, views), aucs)
	}

	// Close what is due. The award happens at the board; the attempt happens
	// wherever the winner walks next — the bid was made in person, the work
	// is delivered by post — so nothing below asks where anyone stands.
	kept := f.windows[:0]
	for _, w := range f.windows {
		if w.closeTick > f.tick {
			kept = append(kept, w)
			continue
		}
		winner, book, err := w.auc.Award()
		if errors.Is(err, auction.ErrNoBids) {
			f.o.traceEvent(trace.EventBounty, map[string]any{"action": "no_bids", "id": w.b.ID})
			f.reopens[w.b.ID]++
			if f.reopens[w.b.ID] >= f.cfg.MaxReopens {
				f.shelved[w.b.ID] = true
				f.o.traceEvent(trace.EventNote, map[string]any{
					"note": "bounty shelved", "bounty": w.b.ID, "windows": f.reopens[w.b.ID],
				})
			}
			continue
		}
		if err != nil {
			return err
		}
		if err := f.o.Board.Award(w.b.ID, winner.Agent, winner.Price); err != nil {
			return err
		}
		f.o.traceEvent(trace.EventBounty, map[string]any{
			"action": "awarded", "id": w.b.ID, "winner": winner.Agent, "price": winner.Price, "book": book,
		})
		delete(f.reopens, w.b.ID)
		if err := f.attempt(w.b, winner.Agent); err != nil {
			return err
		}
	}
	f.windows = kept
	return nil
}

// attempt runs one won bounty end to end, synchronously: the fair's clock is
// the town's tick, and a town tick has room in it for a whole attempt. The
// three legs are settle.go's — the same fund, perform, settle as both other
// tracks.
func (f *Fair) attempt(b *bounty.Bounty, agentID string) error {
	ag := f.o.agent(agentID)
	if ag == nil || ag.Retired {
		if err := f.o.Board.Void(b.ID); err != nil {
			return err
		}
		f.o.traceEvent(trace.EventBounty, map[string]any{
			"action": "voided", "id": b.ID, "reason": "winner retired before attempt",
		})
		return nil
	}
	wallet := f.o.attemptWallet(b.ID, f.tick)
	budget, err := f.o.fundAttempt(f.settleCtx, ag.ID, wallet, b.TokenCeiling)
	if err != nil {
		return err
	}
	out, err := f.o.performAttempt(f.runCtx, ag, b, wallet, f.tick, budget)
	if err != nil {
		return err // the attempt could not even be described: the world is broken
	}
	if err := f.o.settleAttempt(f.settleCtx, ag, b, wallet, budget, out); err != nil {
		return err
	}
	if err := f.o.Ledger.Verify(f.settleCtx); err != nil {
		return fmt.Errorf("fair: conservation audit: %w", err)
	}
	return nil
}

// Close ends the episode and writes the chronicle. Windows still open simply
// never award — a board with unclaimed bounties on it at closing time is an
// honest fact about the day, not a leak — and the money is audited whole.
func (f *Fair) Close() (FairReport, error) {
	f.o.watch = nil
	var rep FairReport
	if err := f.o.Ledger.Verify(f.settleCtx); err != nil {
		return rep, fmt.Errorf("fair: conservation audit: %w", err)
	}
	con, err := f.o.Ledger.Conservation(f.settleCtx)
	if err != nil {
		return rep, err
	}
	f.o.traceEvent(trace.EventEpisode, map[string]any{
		"action": "end", "conservation": con.String(), "ticks": f.tick, "track": "fair",
	})
	rep = FairReport{Ticks: f.tick, Posted: f.posted, Shelved: len(f.shelved), Conservation: con}
	for _, ag := range f.o.agents {
		st, ok := f.standing[ag.ID]
		if !ok {
			continue
		}
		out := *st
		out.Retired = ag.Retired
		if !out.Retired {
			if out.Balance, err = f.o.Ledger.Balance(f.settleCtx, ag.ID); err != nil {
				return rep, err
			}
		}
		rep.Standings = append(rep.Standings, out)
	}
	return rep, nil
}

func (f *Fair) windowFor(id string) *fairWindow {
	for _, w := range f.windows {
		if w.b.ID == id {
			return w
		}
	}
	return nil
}
