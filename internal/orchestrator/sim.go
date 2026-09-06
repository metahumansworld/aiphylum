// The sim track: the same economy, running in real time.
//
// The benchmark track's round loop is a turn-taker — everyone bids, everyone
// attempts, nobody waits. The sim replaces the turn with a clock. Bounties
// appear on a timer, auctions close on a deadline whether or not everyone
// answered, and an agent already busy on an attempt simply misses the window.
// That last rule is the whole point: an agent is one mind, so the cost of a
// long attempt is the auctions it slept through. Nothing enforces drama here
// beyond the clock.
//
// Structure is a single actor. One goroutine owns the ledger, the board, the
// auctions and the roster; agents' code runs in workers and reports back over
// a channel. The money-path is settle.go — the same fund, perform, settle the
// ranked loop uses, because a currency that means two things in two venues
// means nothing.
//
// The sim is unranked by construction: RunSim refuses to start against a
// ladder. Real-time results are not comparable — who was busy when a bounty
// appeared is luck — so they are watchable, never scored.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/metahumansworld/aiphylum/internal/auction"
	"github.com/metahumansworld/aiphylum/internal/bounty"
	"github.com/metahumansworld/aiphylum/internal/ledger"
	"github.com/metahumansworld/aiphylum/internal/rating"
	"github.com/metahumansworld/aiphylum/internal/trace"
)

// SimConfig sets the pace of the world.
type SimConfig struct {
	// PostInterval is how often the clock ticks: one deck bounty appears and
	// any bounty without a live auction goes back up for bids.
	PostInterval time.Duration
	// BidWindow is how long an auction accepts bids before it awards to the
	// lowest ask it has. Agents busy past the deadline are simply not in it.
	BidWindow time.Duration
	// Duration bounds the run. Zero means until the deck is exhausted and the
	// board is quiet, or until the context is cancelled.
	Duration time.Duration
	// MaxReopens is how many windows a bounty nobody bids on gets before it is
	// shelved. Without it a bounty no agent wants costs every live agent a bid
	// step, forever. Zero means the default 3.
	MaxReopens int
	// MaxFailures is how many failed attempts a bounty gets before it is
	// shelved. MaxReopens ends "nobody wants this"; this ends the other way a
	// window repeats forever — somebody wants it every time and can never do
	// it. An agent whose attempts cost it nothing never goes broke, so no rule
	// about money can end that run. The count is the board's own Failures,
	// which excludes voided platform faults but pools every agent's, so a
	// bounty that has beaten the field this often is shelved even if a quieter
	// agent might still have solved it. Zero means the default 6.
	MaxFailures int
	// Deck is the supply, posted one per tick in order.
	Deck []Posting
}

// simAgent is one competitor's real-time state. An agent runs one job at a
// time — that is what makes a long attempt expensive in opportunities as well
// as credits. Only the actor goroutine touches these fields.
type simAgent struct {
	ag    *Agent
	busy  bool
	queue []*bounty.Bounty // auctions won, waiting their turn to be attempted
}

// simAuction is one open bid window.
type simAuction struct {
	auc      *auction.Auction
	b        *bounty.Bounty
	deadline time.Time
}

// simResult is a worker reporting back. Exactly one of bids/attempt is set.
type simResult struct {
	sa     *simAgent
	isBid  bool
	bids   []Action
	b      *bounty.Bounty
	wallet string
	budget ledger.Credits
	out    attemptOutcome
	err    error
}

// SimStanding is one agent's account of its run. It is deliberately not a
// score: attempts and credits, in roster order, with no ordering implied and
// no efficiency computed. Who was idle when a bounty appeared is luck, and the
// sim publishes the luck rather than ranking it.
type SimStanding struct {
	Agent    string
	Attempts int
	Solved   int
	Earned   ledger.Credits
	Burned   ledger.Credits
	Balance  ledger.Credits
	Retired  bool
}

// SimReport is the chronicle a finished world leaves behind.
type SimReport struct {
	Ticks        int
	Posted       int
	Reason       string
	Standings    []SimStanding
	Conservation ledger.Conservation
}

// RunSim runs the world in real time until its duration elapses, its deck runs
// dry with nothing left in flight, or the context is cancelled.
func (o *Orchestrator) RunSim(ctx context.Context, cfg SimConfig) (SimReport, error) {
	var rep SimReport
	if o.Ladder != nil {
		return rep, errors.New("orchestrator: the sim track is unranked by design — run it with a nil ladder")
	}
	if cfg.PostInterval <= 0 {
		cfg.PostInterval = time.Second
	}
	if cfg.BidWindow <= 0 {
		cfg.BidWindow = 2 * cfg.PostInterval
	}
	if cfg.MaxReopens <= 0 {
		cfg.MaxReopens = 3
	}
	if cfg.MaxFailures <= 0 {
		cfg.MaxFailures = 6
	}

	r := &simRun{
		o:        o,
		cfg:      cfg,
		reopens:  map[string]int{},
		shelved:  map[string]bool{},
		standing: map[string]*SimStanding{},
	}
	// The tally rides on the same record() call the ladder would have taken,
	// so the sim counts exactly what the benchmark counts — it just refuses to
	// divide the two numbers.
	o.watch = r.tally
	defer func() { o.watch = nil }()
	for _, ag := range o.live() {
		r.agents = append(r.agents, &simAgent{ag: ag})
		r.standing[ag.ID] = &SimStanding{Agent: ag.ID}
	}
	// One slot per agent is enough that a worker never blocks reporting home:
	// an agent has at most one job in flight.
	r.results = make(chan simResult, len(r.agents)+1)

	// Money survives the shutdown signal. A cancelled context must still be
	// able to refund a fault and close an attempt wallet, so settlement runs
	// on a context that outlives the cancel while the agents' code does not.
	settleCtx := context.WithoutCancel(ctx)
	r.ctx = settleCtx

	o.traceEvent(trace.EventEpisode, map[string]any{
		"action": "start", "track": "sim",
		"post_interval_ms": cfg.PostInterval.Milliseconds(),
		"bid_window_ms":    cfg.BidWindow.Milliseconds(),
		"duration_ms":      cfg.Duration.Milliseconds(),
		"deck":             len(cfg.Deck),
	})

	post := time.NewTicker(cfg.PostInterval)
	defer post.Stop()
	award := time.NewTimer(time.Hour)
	defer award.Stop()
	var deadline <-chan time.Time
	if cfg.Duration > 0 {
		end := time.NewTimer(cfg.Duration)
		defer end.Stop()
		deadline = end.C
	}

	reason := "deck exhausted"
	for !r.quiet() {
		select {
		case <-ctx.Done():
			reason = "cancelled"
		case <-deadline:
			reason = "time"
		case <-post.C:
			if err := r.tick(ctx); err != nil {
				return rep, err
			}
			r.arm(award)
			continue
		case <-award.C:
			if err := r.closeDue(ctx); err != nil {
				return rep, err
			}
			r.arm(award)
			continue
		case res := <-r.results:
			if err := r.handle(ctx, res); err != nil {
				return rep, err
			}
			r.arm(award)
			continue
		}
		break
	}

	// Closing: no new work is dispatched, but everything already running must
	// still come home and settle. A cancelled attempt lands as a platform
	// fault — refunded and voided — through the same table as any other.
	r.closing = true
	for r.inflight > 0 {
		if err := r.handle(ctx, <-r.results); err != nil {
			return rep, err
		}
	}
	for _, sa := range r.agents {
		for _, b := range sa.queue {
			if err := o.Board.Void(b.ID); err != nil {
				return rep, err
			}
			o.traceEvent(trace.EventBounty, map[string]any{
				"action": "voided", "id": b.ID, "agent": sa.ag.ID, "reason": "world ended before the attempt started",
			})
		}
		sa.queue = nil
	}

	if err := o.Ledger.Verify(settleCtx); err != nil {
		return rep, fmt.Errorf("sim: conservation audit: %w", err)
	}
	con, err := o.Ledger.Conservation(settleCtx)
	if err != nil {
		return rep, err
	}
	o.traceEvent(trace.EventEpisode, map[string]any{
		"action": "end", "conservation": con.String(), "ticks": r.tickNo, "reason": reason,
	})

	rep = SimReport{Ticks: r.tickNo, Posted: r.deck, Reason: reason, Conservation: con}
	for _, sa := range r.agents {
		st := *r.standing[sa.ag.ID]
		st.Retired = sa.ag.Retired
		if !st.Retired {
			if st.Balance, err = o.Ledger.Balance(settleCtx, sa.ag.ID); err != nil {
				return rep, err
			}
		}
		rep.Standings = append(rep.Standings, st)
	}
	return rep, nil
}

// simRun is the actor's state. Every field here belongs to the one goroutine
// running the loop; workers see only their arguments and the results channel.
type simRun struct {
	o   *Orchestrator
	cfg SimConfig
	ctx context.Context // settlement context: outlives cancellation

	agents   []*simAgent
	auctions []*simAuction // open windows, in the order they opened
	reopens  map[string]int
	shelved  map[string]bool
	standing map[string]*SimStanding

	deck     int
	tickNo   int
	inflight int
	closing  bool

	results chan simResult
}

// tally folds one settled attempt into its agent's account. It runs on the
// actor goroutine, inside settleAttempt, so the map needs no lock.
func (r *simRun) tally(a rating.Attempt) {
	st, ok := r.standing[a.Agent]
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

// tick is one turn of the clock: retire the broke, post the next bounty, and
// put everything unsold back up for bids.
func (r *simRun) tick(ctx context.Context) error {
	r.tickNo++
	posted := 0
	if r.deck < len(r.cfg.Deck) {
		posted = 1
	}
	// The tick is the sim's round: it names attempt wallets and step
	// containers exactly as the ranked loop does, so one replay viewer reads
	// both tracks.
	r.o.traceEvent(trace.EventEpisode, map[string]any{
		"action": "round", "round": r.tickNo, "postings": posted,
	})

	// Retire anyone who bid themselves broke. Only idle agents: an agent with
	// an attempt in flight has money in a wallet that still has to merge home,
	// and settling it retires it a moment later anyway.
	for _, sa := range r.agents {
		if sa.ag.Retired || sa.busy {
			continue
		}
		if err := r.o.settleAgent(r.ctx, sa.ag); err != nil {
			return err
		}
	}

	if posted == 1 {
		p := r.cfg.Deck[r.deck]
		r.deck++
		if _, err := r.o.postBounty(p); err != nil {
			return err
		}
	}
	return r.openWindows(ctx)
}

// openWindows starts an auction for every open bounty that has none, and asks
// every idle agent what it would bid for the ones that just opened.
func (r *simRun) openWindows(ctx context.Context) error {
	var (
		fresh []BountyView
		ids   []string
	)
	for _, b := range r.o.Board.Open() {
		if r.shelved[b.ID] || r.auctionFor(b.ID) != nil {
			continue
		}
		// The second shelf, and the one that makes quiet total: a bounty that
		// keeps being won and keeps being failed would otherwise reopen here
		// every tick until somebody stopped the world from outside.
		if b.Failures >= r.cfg.MaxFailures {
			r.shelved[b.ID] = true
			r.o.traceEvent(trace.EventNote, map[string]any{
				"note": "bounty shelved", "bounty": b.ID, "failures": b.Failures,
			})
			continue
		}
		r.auctions = append(r.auctions, &simAuction{
			auc:      auction.New(b.ID, b.MaxPayout, r.o.reserveFor(b.MaxPayout)),
			b:        b,
			deadline: time.Now().Add(r.cfg.BidWindow),
		})
		fresh = append(fresh, r.o.bountyView(b))
		ids = append(ids, b.ID)
	}
	if len(fresh) == 0 {
		return nil
	}
	for _, sa := range r.agents {
		if sa.ag.Retired || sa.busy {
			continue // busy agents miss the window; that is the cost of a long attempt
		}
		r.dispatchBid(ctx, sa, fresh)
	}
	return nil
}

// dispatchBid runs one agent's bid step in a worker.
func (r *simRun) dispatchBid(ctx context.Context, sa *simAgent, views []BountyView) {
	sa.busy = true
	r.inflight++
	tick := r.tickNo
	go func() {
		// Nil stay offer: the sim runs on a clock, not a map — its agents are
		// always at the board, so there is no absence to buy your way out of.
		bids := r.o.performBidStep(ctx, sa.ag, tick, views, nil)
		r.results <- simResult{sa: sa, isBid: true, bids: bids}
	}()
}

// closeDue awards every window whose deadline has passed.
func (r *simRun) closeDue(ctx context.Context) error {
	now := time.Now()
	kept := r.auctions[:0]
	for _, sa := range r.auctions {
		if sa.deadline.After(now) {
			kept = append(kept, sa)
			continue
		}
		winner, book, err := sa.auc.Award()
		if errors.Is(err, auction.ErrNoBids) {
			r.o.traceEvent(trace.EventBounty, map[string]any{"action": "no_bids", "id": sa.b.ID})
			r.reopens[sa.b.ID]++
			if r.reopens[sa.b.ID] >= r.cfg.MaxReopens {
				r.shelved[sa.b.ID] = true
				r.o.traceEvent(trace.EventNote, map[string]any{
					"note": "bounty shelved", "bounty": sa.b.ID, "windows": r.reopens[sa.b.ID],
				})
			}
			continue
		}
		if err != nil {
			return err
		}
		if err := r.o.Board.Award(sa.b.ID, winner.Agent, winner.Price); err != nil {
			return err
		}
		r.o.traceEvent(trace.EventBounty, map[string]any{
			"action": "awarded", "id": sa.b.ID, "winner": winner.Agent, "price": winner.Price, "book": book,
		})
		r.o.announce(sa.b.ID, r.tickNo, winner, book)
		delete(r.reopens, sa.b.ID)

		wa := r.simAgent(winner.Agent)
		if wa == nil {
			if err := r.o.Board.Void(sa.b.ID); err != nil {
				return err
			}
			continue
		}
		wa.queue = append(wa.queue, sa.b)
		if err := r.dispatchNext(ctx, wa); err != nil {
			return err
		}
	}
	r.auctions = kept
	return nil
}

// dispatchNext starts the agent's next won attempt, if it is free to take one.
//
// The attempt is funded here rather than at award: an agent can bankrupt on
// one attempt while another waits in its queue, and money moved into a wallet
// belonging to an agent that dies before using it has nowhere to go home to.
func (r *simRun) dispatchNext(ctx context.Context, sa *simAgent) error {
	for !r.closing && !sa.busy && len(sa.queue) > 0 {
		b := sa.queue[0]
		sa.queue = sa.queue[1:]

		if sa.ag.Retired {
			// Won it, died before attempting it. No attempt happened, so no
			// failure is counted: void, back to the board.
			if err := r.o.Board.Void(b.ID); err != nil {
				return err
			}
			r.o.traceEvent(trace.EventBounty, map[string]any{
				"action": "voided", "id": b.ID, "reason": "winner retired before attempt",
			})
			continue
		}

		wallet := fmt.Sprintf("att:%s:r%d", b.ID, r.tickNo)
		budget, err := r.o.fundAttempt(r.ctx, sa.ag.ID, wallet, b.TokenCeiling)
		if err != nil {
			return err
		}
		sa.busy = true
		r.inflight++
		tick := r.tickNo
		go func() {
			out, err := r.o.performAttempt(ctx, sa.ag, b, wallet, tick, budget)
			r.results <- simResult{sa: sa, b: b, wallet: wallet, budget: budget, out: out, err: err}
		}()
		return nil
	}
	return nil
}

// handle takes one worker's report: place its bids, or settle its attempt.
func (r *simRun) handle(ctx context.Context, res simResult) error {
	res.sa.busy = false
	r.inflight--

	if res.isBid {
		r.placeLate(res.sa.ag, res.bids)
		return r.dispatchNext(ctx, res.sa)
	}
	if res.err != nil {
		return res.err // the attempt could not even be described: the world is broken
	}
	if err := r.o.settleAttempt(r.ctx, res.sa.ag, res.b, res.wallet, res.budget, res.out); err != nil {
		return err
	}
	if err := r.o.Ledger.Verify(r.ctx); err != nil {
		return fmt.Errorf("sim: conservation audit: %w", err)
	}
	return r.dispatchNext(ctx, res.sa)
}

// placeLate enters an agent's asks into the windows still open. An agent that
// thought too long finds the auction gone — recorded, so a spectator can see
// why an agent that wanted a bounty never got one.
func (r *simRun) placeLate(ag *Agent, bids []Action) {
	for _, a := range bids {
		if a.Type != ActionBid {
			continue
		}
		sa := r.auctionFor(a.Bounty)
		if sa == nil {
			r.o.traceEvent(trace.EventNote, map[string]any{
				"note": "bid arrived after the window closed", "agent": ag.ID, "bounty": a.Bounty, "price": a.Price,
			})
			continue
		}
		if err := sa.auc.Place(ag.ID, a.Price); err != nil {
			r.o.traceEvent(trace.EventNote, map[string]any{
				"note": "bid refused", "agent": ag.ID, "bounty": a.Bounty, "price": a.Price, "err": err.Error(),
			})
			continue
		}
		r.o.traceEvent(trace.EventBid, map[string]any{"bounty": a.Bounty, "agent": ag.ID, "price": a.Price})
	}
}

// arm points the award timer at the next deadline.
func (r *simRun) arm(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	d := time.Hour
	for _, sa := range r.auctions {
		if until := time.Until(sa.deadline); until < d {
			d = until
		}
	}
	if d < 0 {
		d = 0
	}
	t.Reset(d)
}

// quiet reports a world with nothing left to do: the deck is spent, no window
// is open, no worker is running, and whatever is still on the board is shelved.
func (r *simRun) quiet() bool {
	if r.deck < len(r.cfg.Deck) || len(r.auctions) > 0 || r.inflight > 0 {
		return false
	}
	for _, sa := range r.agents {
		if len(sa.queue) > 0 {
			return false
		}
	}
	for _, b := range r.o.Board.Open() {
		if !r.shelved[b.ID] {
			return false
		}
	}
	return true
}

func (r *simRun) auctionFor(id string) *simAuction {
	for _, sa := range r.auctions {
		if sa.b.ID == id {
			return sa
		}
	}
	return nil
}

func (r *simRun) simAgent(id string) *simAgent {
	for _, sa := range r.agents {
		if sa.ag.ID == id {
			return sa
		}
	}
	return nil
}
