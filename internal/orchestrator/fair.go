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
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"

	"github.com/metahumansworld/soscitea/internal/auction"
	"github.com/metahumansworld/soscitea/internal/bounty"
	"github.com/metahumansworld/soscitea/internal/ledger"
	"github.com/metahumansworld/soscitea/internal/rating"
	"github.com/metahumansworld/soscitea/internal/town"
	"github.com/metahumansworld/soscitea/internal/trace"
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
	// StayPrice is what one tick of standing at the office longer than the
	// schedule allows costs the agent who asks for it. Zero means the
	// default 8. The fee is burned rather than paid to anyone: nobody is on
	// the other side of the transaction, because what is being bought is not
	// a thing but an absence of walking.
	StayPrice ledger.Credits
	// MaxStayTicks caps a single purchase. The office keeps hours and does
	// not let anyone sleep in the lobby; it also means one malformed action
	// cannot burn a wallet dry in a single tick. Zero means the default 6.
	MaxStayTicks int
	// Notebook is the price of a notebook at the office, or zero to sell
	// none — the default, and what every fair before this one ran with. A
	// notebook raises its owner's memo cap from MaxMemoBytes to
	// NotebookBytes for the rest of the run. Burned like a stay: the agent
	// is buying room in the platform's own book, and nobody is on the
	// other side of that either.
	Notebook ledger.Credits
	// Stall is the price of a stall at the office, or zero to sell none.
	// A stall is the first thing bought that the town can see: the office
	// assigns its buyer the next free cell of Pitches, the purchase carries
	// the cell, and the spectator's page draws it there for the rest of the
	// record. Burned like a notebook — the ground was nobody's to sell.
	Stall ledger.Credits
	// StallPlace names the place the pitches lie in, for the offer: an
	// agent is told where a stall stands before it pays, not which cell,
	// because the cell is the office's to assign when the money moves.
	StallPlace string
	// Pitches is the ground, in the order the office lets it. The fair has
	// no map of its own, so whoever wires it hands over the cells; a stall
	// for sale with nowhere to stand is refused at construction, not at
	// the counter.
	Pitches []town.Cell
	// Tie is the policy an auction at this fair uses when two bids arrive
	// at the same lowest price. The zero value is auction.ByArrival — the
	// earliest bid wins, which is what every fair before this one did
	// without anyone having decided it: arrival at the board is roster
	// order, and roster order is the order the -guest flags were typed.
	// auction.ByLot replaces that queue with a seeded draw among the tied
	// names.
	Tie auction.TieBreak
	// LotSalt seeds the draws when Tie is auction.ByLot; ignored otherwise.
	// Each window mixes it with the bounty's ID and that bounty's window
	// count, so one bounty's draw teaches nothing about another's and a
	// reopened board is a fresh draw rather than the same loser again.
	LotSalt uint64
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

	tick    int
	posted  int
	next    int // next deck card
	windows []*fairWindow
	reopens map[string]int
	shelved map[string]bool
	// draws counts windows opened per bounty under ByLot, never reset — the
	// draw index each window's salt mixes in. A trace reader holds the same
	// number without being told it: it is the count of that bounty's earlier
	// awarded and no_bids events, which is what keeps every lot recomputable
	// from the file alone.
	draws    map[string]int
	standing map[string]*SimStanding
	// held maps an agent to the last tick its standing is paid through. Read
	// by Hold, written by chargeStays, both on the town's goroutine.
	held map[string]int
	// owned is what each agent has bought, in purchase order. Shown back to
	// it on every board, reported in its standing, and written to the trace
	// once per purchase — which is the copy of record: a reader holds this
	// map by collecting the bought events.
	owned map[string][]string
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
	if cfg.StayPrice <= 0 {
		cfg.StayPrice = 8
	}
	if cfg.MaxStayTicks <= 0 {
		cfg.MaxStayTicks = 6
	}
	if cfg.Office == "" {
		return nil, errors.New("orchestrator: the fair needs an office — presence at it is the whole coupling")
	}
	if cfg.Stall > 0 && len(cfg.Pitches) == 0 {
		return nil, errors.New("orchestrator: a stall for sale needs pitches to stand on")
	}
	if cfg.Tie != auction.ByArrival && cfg.Tie != auction.ByLot {
		// Refused rather than defaulted: a tie-break that silently fell
		// back to arrival would be a policy nobody chose, which is the
		// accident this field exists to end.
		return nil, fmt.Errorf("orchestrator: unknown tie-break policy %d", cfg.Tie)
	}
	if o.Cfg.Book != SealedBook && o.Cfg.Book != OpenBook {
		// The tie-break's rule again, on the announcer's policy: a book
		// that silently sealed itself would be a default nobody chose.
		return nil, fmt.Errorf("orchestrator: unknown book policy %d", o.Cfg.Book)
	}
	f := &Fair{
		o:         o,
		cfg:       cfg,
		runCtx:    ctx,
		settleCtx: context.WithoutCancel(ctx),
		reopens:   map[string]int{},
		shelved:   map[string]bool{},
		draws:     map[string]int{},
		standing:  map[string]*SimStanding{},
		held:      map[string]int{},
		owned:     map[string][]string{},
	}
	// The tally rides the same record() call the ladder would have taken —
	// the fair counts exactly what the benchmark counts and refuses to
	// divide the two numbers, same as the sim.
	o.watch = f.tally
	// Deliberately not announcing the stay price here: this line is what a
	// fair trace opens with, and every fair trace ever recorded opens with
	// exactly these keys. Nothing is lost — a stay writes its own event
	// carrying both the ticks and the amount, so the price of a day on which
	// anyone actually bought one is recoverable by division, and on a day
	// when nobody did there was no price paid to record.
	//
	// The tie-break is the one deliberate divergence from that rule, and
	// only on the runs that chose it. A lot is the stay price's opposite:
	// no outcome recovers it. The winners alone cannot say whether a queue
	// or a draw picked them, and the salt is an input, not a consequence —
	// so a ByLot run declares both up front, while every arrival-order
	// trace, which is every trace recorded before the policy had a name,
	// opens with exactly the keys it always did. The salt travels as a
	// decimal string rather than a JSON number so a reader whose numbers
	// lose precision past 2^53 can still rerun every draw.
	start := map[string]any{
		"action": "start", "track": "fair",
		"deck": len(cfg.Deck), "post_minutes": cfg.PostMinutes,
		"window_ticks": cfg.WindowTicks, "office": cfg.Office,
	}
	if cfg.Tie == auction.ByLot {
		start["tiebreak"] = "lot"
		start["lot_salt"] = strconv.FormatUint(cfg.LotSalt, 10)
	}
	if o.Cfg.Book == OpenBook {
		// The book policy is an input for the tie-break's reason: no line
		// of the day records what the bidders were told, because results
		// are never traced, so a reader who was not told up front could
		// replay every award and still not know what kind of market this
		// was. A sealed day carries no key at all — a policy that was not
		// in force should not be in the record.
		start["book"] = "open"
	}
	if cfg.Notebook > 0 {
		// The book's rule again: what was for sale is an input. A purchase
		// records its own price, but a day nobody bought on would otherwise
		// not say whether nobody wanted a notebook or nobody was offered
		// one — and a fair that sold nothing keeps its opening line.
		start["notebook"] = cfg.Notebook
	}
	if cfg.Stall > 0 {
		start["stall"] = cfg.Stall
	}
	o.traceEvent(trace.EventEpisode, start)
	return f, nil
}

// stand is an agent's account, opened the first time anyone asks for it.
// On demand rather than seeded at construction because the roster is not
// fixed at construction: a guest can come through the door mid-week, and a
// week that dropped its attempts on the floor and left it off the closing
// table would be a week that half-counted it.
func (f *Fair) stand(id string) *SimStanding {
	st, ok := f.standing[id]
	if !ok {
		st = &SimStanding{Agent: id}
		f.standing[id] = st
	}
	return st
}

// tally folds one settled attempt into its agent's account. Settlement runs
// on the town's goroutine, inside Visit, so the map needs no lock.
func (f *Fair) tally(a rating.Attempt) {
	st := f.stand(a.Agent)
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
		auc := auction.New(b.ID, b.MaxPayout, f.o.reserveFor(b.MaxPayout))
		if f.cfg.Tie == auction.ByLot {
			auc.Tie = auction.ByLot
			auc.Salt = lotSalt(f.cfg.LotSalt, b.ID, f.draws[b.ID])
			f.draws[b.ID]++
		}
		f.windows = append(f.windows, &fairWindow{
			auc:       auc,
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
		// The agent is told where it is standing and what standing there
		// longer costs, and answers with one action list holding both what it
		// wants to bid on and whether it wants to still be here.
		acts := f.o.performBidStep(f.runCtx, ag, f.tick, views, &FairOffer{
			Place: st.Place, Price: f.cfg.StayPrice, TicksLeft: f.staying(ag.ID),
			ForSale: f.catalogue(), Owned: f.owned[ag.ID],
		})
		f.o.placeBids(ag, acts, aucs)
		if err := f.chargeStays(ag, st.Place, acts); err != nil {
			return err
		}
		if err := f.chargeBuys(ag, st.Place, acts); err != nil {
			return err
		}
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
		f.o.announce(w.b.ID, f.tick, winner, book)
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
		out := *f.stand(ag.ID)
		out.Retired = ag.Retired
		out.Owned = f.owned[ag.ID]
		if !out.Retired {
			if out.Balance, err = f.o.Ledger.Balance(f.settleCtx, ag.ID); err != nil {
				return rep, err
			}
		}
		rep.Standings = append(rep.Standings, out)
	}
	return rep, nil
}

// Hold is the town's inward seam and the exact twin of Visit: the town asks,
// once per resident per tick, whether this body has been paid to stay put, and
// the fair answers without telling it anything about money. Called on the
// town's goroutine like Visit, so the map needs no lock.
//
// The arithmetic, because an off-by-one here would silently sell a tick more
// or less than was paid for and nothing else would notice: a stay bought
// during tick T's Visit for N ticks holds the agent through the movements of
// ticks T+1 … T+N. The town moves everyone before it calls Visit, so when it
// asks during tick T+1's movement f.tick is still T — which is where the +1
// below comes from, and why TestFairStayExpiry checks both ends.
func (f *Fair) Hold(id string) bool {
	return f.held[id] >= f.tick+1
}

// staying is how many ticks of standing an agent has already paid for and not
// yet spent, counted from this tick's Visit. It is the same number Hold is
// about to answer with, said as a quantity instead of a yes: staying() >= 1
// exactly when the next movement is held.
func (f *Fair) staying(id string) int {
	if n := f.held[id] - f.tick; n > 0 {
		return n
	}
	return 0
}

// chargeStays buys whatever standing an agent asked for and can afford. It
// runs after the bids from the same step are placed, because the bids are what
// the agent came for and the stay is only about what it will still be here to
// see.
//
// Nothing here can fail the tick. An agent that cannot pay is simply not held,
// and told so in the trace rather than in its next observation: it asked to
// buy something at a price it had been shown, and the answer it gets is the
// ordinary one — it is not there any more.
func (f *Fair) chargeStays(ag *Agent, place string, actions []Action) error {
	for _, a := range actions {
		if a.Type != ActionStay {
			continue
		}
		// The first stay in a step wins and the rest are ignored. An agent
		// that asks twice in one breath is asking for one thing, and a loop
		// in somebody's code should not be able to empty their wallet a
		// hundred times over inside a single minute.
		n := a.Ticks
		if n <= 0 {
			n = 1 // asking to stay, without saying how long, is asking for a tick
		}
		if n > f.cfg.MaxStayTicks {
			n = f.cfg.MaxStayTicks
		}
		cost := f.cfg.StayPrice * ledger.Credits(n)
		bal, err := f.o.Ledger.Balance(f.settleCtx, ag.ID)
		if err != nil {
			return err
		}
		if bal < cost {
			f.o.traceEvent(trace.EventNote, map[string]any{
				"note": "stay refused", "agent": ag.ID, "ticks": n,
				"cost": cost, "balance": bal,
			})
			return nil
		}
		// Burned, not transferred. There is nobody on the other side of this
		// trade — the agent is not buying a thing from anyone, it is buying
		// its own absence of walking — so the credits leave the economy
		// through the sink that exists for exactly that, the same way a
		// failed attempt's spend does.
		if _, err := f.o.Ledger.Burn(f.settleCtx, ag.ID, cost, "stay", place); err != nil {
			return err
		}
		// Renewal extends and never shortens: an agent that pays again while
		// still held has bought the later of the two departures. It pays in
		// full for the ticks it named either way, so overlapping a stay it
		// already had is its own mistake to make.
		if until := f.tick + n; until > f.held[ag.ID] {
			f.held[ag.ID] = until
		}
		f.o.traceEvent(trace.EventCredit, map[string]any{
			"action": "stayed", "agent": ag.ID, "place": place,
			"ticks": n, "amount": cost, "until": f.held[ag.ID],
		})
		return nil
	}
	return nil
}

// catalogue is what the office has for sale: nil when nothing is, which is
// what keeps the observation of every fair that sells nothing unchanged.
func (f *Fair) catalogue() []Offer {
	var out []Offer
	if f.cfg.Notebook > 0 {
		out = append(out, Offer{Item: "notebook", Price: f.cfg.Notebook, MemoBytes: NotebookBytes})
	}
	if f.cfg.Stall > 0 && len(f.cfg.Pitches) > 0 {
		// Off the list once the ground is gone: a line for a stall nobody
		// can be given is a price for nothing, and the refusal it would
		// earn is the plain "not for sale".
		out = append(out, Offer{Item: "stall", Price: f.cfg.Stall, Place: f.cfg.StallPlace})
	}
	return out
}

// chargeBuys is chargeStays for the catalogue: the first buy in a step wins,
// a refusal is a note and not a debt, and nothing here can fail the tick.
// It runs after the stays because a stay is about this tick and a notebook
// is about the rest of the run.
func (f *Fair) chargeBuys(ag *Agent, place string, actions []Action) error {
	for _, a := range actions {
		if a.Type != ActionBuy {
			continue
		}
		var offer *Offer
		for _, o := range f.catalogue() {
			if o.Item == a.Item {
				offer = &o // a copy: catalogue builds a fresh slice each call
				break
			}
		}
		if offer == nil {
			f.o.traceEvent(trace.EventNote, map[string]any{
				"note": "buy refused: not for sale", "agent": ag.ID, "item": a.Item,
			})
			return nil
		}
		for _, have := range f.owned[ag.ID] {
			if have == a.Item {
				// One each. A second notebook would be the first one again,
				// and a loop in somebody's code should not be able to pay
				// for the same page twice.
				f.o.traceEvent(trace.EventNote, map[string]any{
					"note": "buy refused: already owned", "agent": ag.ID, "item": a.Item,
				})
				return nil
			}
		}
		bal, err := f.o.Ledger.Balance(f.settleCtx, ag.ID)
		if err != nil {
			return err
		}
		if bal < offer.Price {
			f.o.traceEvent(trace.EventNote, map[string]any{
				"note": "buy refused", "agent": ag.ID, "item": a.Item,
				"cost": offer.Price, "balance": bal,
			})
			return nil
		}
		// Burned, for the stay's reason: there is no seller. The agent is
		// buying room in the platform's own book, and the platform is not
		// a party that keeps the money. When another agent is on the other
		// side of a trade, that trade will transfer; this one does not.
		if _, err := f.o.Ledger.Burn(f.settleCtx, ag.ID, offer.Price, "buy", a.Item); err != nil {
			return err
		}
		f.owned[ag.ID] = append(f.owned[ag.ID], a.Item)
		ev := map[string]any{
			"action": "bought", "agent": ag.ID, "place": place, "item": a.Item,
			"amount": offer.Price,
		}
		if offer.MemoBytes > 0 {
			// Only a page grants a page: a grant of zero would be the
			// notebook taken back by the next thing bought.
			f.o.memos.grant(ag.ID, offer.MemoBytes)
			ev["memo_bytes"] = offer.MemoBytes
		}
		if a.Item == "stall" {
			// The office assigns the ground: the next pitch in the order
			// it was handed, so two buyers in one tick get two cells and
			// the roster's order says whose is whose. The cell rides the
			// same event as the money because it is the whole of what was
			// bought — the town is not told, and the page that draws the
			// map reads it from here.
			c := f.cfg.Pitches[0]
			f.cfg.Pitches = f.cfg.Pitches[1:]
			ev["x"], ev["y"] = c.X, c.Y
		}
		f.o.traceEvent(trace.EventCredit, ev)
		return nil
	}
	return nil
}

func (f *Fair) windowFor(id string) *fairWindow {
	for _, w := range f.windows {
		if w.b.ID == id {
			return w
		}
	}
	return nil
}

// lotSalt is a ByLot window's seed: the run's salt, the bounty's ID and the
// window's draw index, folded through FNV-1a. Every input is in the trace —
// the salt on the episode-start line, the ID on the award, the index by
// counting the bounty's earlier closed windows — so an auditor can rerun any
// draw from the file alone, which is the property that lets a lot into a
// venue whose whole defence is replayability.
func lotSalt(run uint64, bountyID string, draw int) uint64 {
	h := fnv.New64a()
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], run)
	h.Write(b[:])
	h.Write([]byte(bountyID))
	binary.BigEndian.PutUint64(b[:], uint64(draw))
	h.Write(b[:])
	return h.Sum64()
}
