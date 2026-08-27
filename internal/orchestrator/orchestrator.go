// Package orchestrator composes the spine and the track into the game loop.
//
// One round: post bounties, collect sealed bids, award each auction to the
// lowest ask, run each winner's single attempt in its own step, verify, pay
// or charge, and audit the ledger before the next round begins. Everything
// here is deliberately sequential and iterated in registration/posting order —
// determinism is what makes recorded replay exact.
//
// Failure attribution lives here, in one table (see attempt): agent faults
// are charged and counted, platform faults are refunded and voided. The
// distinction is recorded at the proxy and in the trace, so it is auditable
// rather than discretionary.
package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/metahunmei/dungeon/internal/auction"
	"github.com/metahunmei/dungeon/internal/bounty"
	"github.com/metahunmei/dungeon/internal/ledger"
	"github.com/metahunmei/dungeon/internal/proxy"
	"github.com/metahunmei/dungeon/internal/rating"
	"github.com/metahunmei/dungeon/internal/trace"
)

// StepRequest asks a StepRunner to run one agent step.
type StepRequest struct {
	AgentID string
	Name    string        // unique per step; the docker adapter uses it as the container name
	Token   string        // proxy bearer token, authorized for this step's wallet
	Input   []byte        // StepInput JSON, fed on stdin
	Timeout time.Duration // wall-clock ceiling
}

// StepResult is how one step ended. Everything the agent did wrong travels
// here; an error from RunStep itself means the platform broke, not the agent.
type StepResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	TimedOut bool
}

// StepRunner runs one agent step to completion. The Docker adapter runs a
// container behind the zero-egress network; tests run in-process fakes.
type StepRunner interface {
	RunStep(ctx context.Context, req StepRequest) (StepResult, error)
}

// Config sets the orchestrator's policies.
type Config struct {
	// ReserveFraction sets each auction's reserve price as this fraction of
	// the bounty's maximum payout — the anti-collusion floor. Zero means the
	// default 5%.
	ReserveFraction float64
	// StepTimeout bounds the bid step (attempts use the bounty's own wall
	// clock). Zero means 60s.
	StepTimeout time.Duration
	// Dust is the bankruptcy threshold: an agent whose bankroll at attempt
	// settlement is at or below it is burned out and retired. Zero means only
	// an exactly empty wallet bankrupts; operators set it to the cost of the
	// cheapest possible call so broke agents die instead of haunting the board.
	Dust ledger.Credits
}

// Agent is one competitor's lifecycle record. Its wallet shares its ID.
type Agent struct {
	ID      string
	Retired bool
}

// Posting names one bounty to put on the board.
type Posting struct {
	Generator    string
	Seed         int64
	Tier         int
	TokenCeiling ledger.Credits
	WallClockSec int
}

// Episode is a seeded plan: which bounties appear in which round.
type Episode struct {
	Rounds [][]Posting
}

// Orchestrator wires ledger, proxy, board, auctions, steps, trace, and ladder
// into the loop. Construct with New; the embedded Proxy is the http.Handler
// the daemon serves to the relay.
type Orchestrator struct {
	Ledger *ledger.Ledger
	Board  *bounty.Board
	Proxy  *proxy.Proxy
	Steps  StepRunner
	Ladder *rating.Ladder
	Log    *slog.Logger
	Cfg    Config

	tw     *trace.Writer
	faults *faultTracker
	agents []*Agent // registration order: the deterministic iteration order
}

// New assembles the world. The proxy is built here so its recorder can be the
// orchestrator's fault tracker (teeing into the trace) — that is how platform
// faults observed at the proxy reach the attribution decision.
func New(l *ledger.Ledger, board *bounty.Board, table *proxy.PriceTable, provider proxy.Provider,
	tw *trace.Writer, steps StepRunner, ladder *rating.Ladder, cfg Config, log *slog.Logger) *Orchestrator {
	if log == nil {
		log = slog.Default()
	}
	if cfg.ReserveFraction <= 0 {
		cfg.ReserveFraction = 0.05
	}
	if cfg.StepTimeout <= 0 {
		cfg.StepTimeout = 60 * time.Second
	}
	var sink proxy.Recorder
	if tw != nil { // avoid a typed-nil Recorder
		sink = tw
	}
	ft := &faultTracker{next: sink, faulted: map[string]bool{}}
	return &Orchestrator{
		Ledger: l,
		Board:  board,
		Proxy:  proxy.New(l, table, provider, ft, log),
		Steps:  steps,
		Ladder: ladder,
		Log:    log,
		Cfg:    cfg,
		tw:     tw,
		faults: ft,
	}
}

// faultTracker tees proxy events into the trace and remembers, per wallet,
// whether a platform fault occurred — the proxy's half of failure attribution,
// surfaced to the orchestrator's half.
//
// TODO(daemon): only attempt wallets are ever reset; fault entries for bid
// wallets accumulate for the life of the process. Harmless per episode, a slow
// leak in a long-running daemon — clear the map (or reset bid wallets too)
// between episodes.
type faultTracker struct {
	next proxy.Recorder

	mu      sync.Mutex
	faulted map[string]bool
}

func (f *faultTracker) Record(ev proxy.Event) {
	if ev.Outcome == proxy.OutcomePlatformFault {
		f.mu.Lock()
		f.faulted[ev.Wallet] = true
		f.mu.Unlock()
	}
	if f.next != nil {
		f.next.Record(ev)
	}
}

func (f *faultTracker) reset(wallet string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.faulted, wallet)
}

func (f *faultTracker) sawFault(wallet string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.faulted[wallet]
}

// AddAgent registers a competitor: a wallet, a grant, a trace line. The ID
// must be fresh — a retired agent's ID cannot be reused, which is half of what
// makes bankruptcy permanent.
func (o *Orchestrator) AddAgent(ctx context.Context, id string, grant ledger.Credits) error {
	if err := o.Ledger.CreateAccount(ctx, id, ledger.KindAgent, id); err != nil {
		return err
	}
	if grant > 0 {
		if _, err := o.Ledger.Mint(ctx, id, grant, "signing grant", "grant:"+id); err != nil {
			return err
		}
	}
	o.agents = append(o.agents, &Agent{ID: id})
	o.traceEvent(trace.EventAgent, map[string]any{"action": "spawned", "agent": id, "grant": grant})
	return nil
}

// Agents returns the roster in registration order.
func (o *Orchestrator) Agents() []*Agent { return o.agents }

func (o *Orchestrator) agent(id string) *Agent {
	for _, a := range o.agents {
		if a.ID == id {
			return a
		}
	}
	return nil
}

func (o *Orchestrator) live() []*Agent {
	var out []*Agent
	for _, a := range o.agents {
		if !a.Retired {
			out = append(out, a)
		}
	}
	return out
}

// RunEpisode runs the plan round by round, auditing the ledger after each. A
// conservation failure aborts the episode loudly: a world whose money is
// wrong must halt, not continue.
func (o *Orchestrator) RunEpisode(ctx context.Context, ep Episode) error {
	o.traceEvent(trace.EventEpisode, map[string]any{"action": "start", "rounds": len(ep.Rounds)})
	for i, postings := range ep.Rounds {
		round := i + 1
		if err := o.runRound(ctx, round, postings); err != nil {
			return fmt.Errorf("round %d: %w", round, err)
		}
		if err := o.Ledger.Verify(ctx); err != nil {
			return fmt.Errorf("round %d: conservation audit: %w", round, err)
		}
	}
	con, err := o.Ledger.Conservation(ctx)
	if err != nil {
		return err
	}
	o.traceEvent(trace.EventEpisode, map[string]any{"action": "end", "conservation": con.String()})
	return nil
}

func (o *Orchestrator) runRound(ctx context.Context, round int, postings []Posting) error {
	// Mark the round in the trace so readers (the replay viewer above all)
	// get boundaries without inferring them from attempt wallet names.
	o.traceEvent(trace.EventEpisode, map[string]any{"action": "round", "round": round, "postings": len(postings)})

	// Retire anyone who limped out of the previous round below the threshold
	// (e.g. burned their bankroll bidding and never won an attempt).
	for _, ag := range o.live() {
		if err := o.settleAgent(ctx, ag); err != nil {
			return err
		}
	}

	// 1. Post this round's bounties. Failed bounties from earlier rounds are
	// already back on the board; they re-enter the auction alongside these.
	for _, p := range postings {
		if _, err := o.postBounty(p); err != nil {
			return err
		}
	}

	// 2. Sealed bids. One auction per open bounty; every live agent gets one
	// bid step showing the whole board.
	open := o.Board.Open()
	if len(open) == 0 {
		return nil
	}
	aucs := make(map[string]*auction.Auction, len(open))
	byID := make(map[string]*bounty.Bounty, len(open))
	views := make([]BountyView, 0, len(open))
	for _, b := range open {
		aucs[b.ID] = auction.New(b.ID, b.MaxPayout, o.reserveFor(b.MaxPayout))
		byID[b.ID] = b
		views = append(views, o.bountyView(b))
	}
	for _, ag := range o.live() {
		o.bidStep(ctx, round, ag, views, aucs)
	}

	// 3. Award, in posting order. The book becomes public at award.
	var awarded []*bounty.Bounty
	for _, b := range open {
		winner, book, err := aucs[b.ID].Award()
		if errors.Is(err, auction.ErrNoBids) {
			o.traceEvent(trace.EventBounty, map[string]any{"action": "no_bids", "id": b.ID})
			continue
		}
		if err != nil {
			return err
		}
		if err := o.Board.Award(b.ID, winner.Agent, winner.Price); err != nil {
			return err
		}
		o.traceEvent(trace.EventBounty, map[string]any{
			"action": "awarded", "id": b.ID, "winner": winner.Agent, "price": winner.Price, "book": book,
		})
		awarded = append(awarded, byID[b.ID])
	}

	// 4. Attempts, one per award, in the same order.
	for _, b := range awarded {
		if err := o.attempt(ctx, round, b); err != nil {
			return err
		}
	}
	return nil
}

// postBounty puts one posting on the board and announces it. Both tracks post
// through here so a bounty looks the same in either trace.
func (o *Orchestrator) postBounty(p Posting) (*bounty.Bounty, error) {
	b, err := o.Board.Post(p.Generator, p.Seed, p.Tier, p.TokenCeiling, p.WallClockSec)
	if err != nil {
		return nil, err
	}
	o.traceEvent(trace.EventBounty, map[string]any{
		"action": "posted", "id": b.ID, "generator": b.Generator, "seed": b.Seed,
		"tier": b.Tier, "max_payout": b.MaxPayout, "reserve": o.reserveFor(b.MaxPayout),
		"answer_digest": b.AnswerDigest(), "token_ceiling": b.TokenCeiling,
		"wall_clock_sec": b.WallClockSec, "failures": b.Failures,
	})
	return b, nil
}

// bountyView is what an agent is shown of a bounty it may bid on.
func (o *Orchestrator) bountyView(b *bounty.Bounty) BountyView {
	return BountyView{
		ID: b.ID, Tier: b.Tier, Prompt: b.Prompt,
		MaxPayout: b.MaxPayout, Reserve: o.reserveFor(b.MaxPayout),
		Failures: b.Failures, AnswerDigest: b.AnswerDigest(),
	}
}

func (o *Orchestrator) reserveFor(maxPayout ledger.Credits) ledger.Credits {
	r := ledger.Credits(float64(maxPayout) * o.Cfg.ReserveFraction)
	if r < 1 {
		r = 1
	}
	return r
}

// bidStep runs one agent's sealed-bid step and places whatever it asked for.
// Nothing in here can fail the round: a broken bid step just means this agent
// places no bids. Model calls made while deciding bids are metered against the
// agent's bankroll like any other spend.
func (o *Orchestrator) bidStep(ctx context.Context, round int, ag *Agent, views []BountyView, aucs map[string]*auction.Auction) {
	o.placeBids(ag, o.performBidStep(ctx, ag, round, views), aucs)
}

// performBidStep is the half of a bid that runs the agent's code: it asks what
// the agent wants to bid and returns those actions. Like performAttempt it
// touches only mutex-guarded pieces, so the sim can run it in a worker while
// its actor loop keeps the auctions moving.
func (o *Orchestrator) performBidStep(ctx context.Context, ag *Agent, round int, views []BountyView) []Action {
	bal, err := o.Ledger.Balance(ctx, ag.ID)
	if err != nil {
		o.Log.Warn("bid step: balance lookup failed", "agent", ag.ID, "err", err)
		return nil
	}
	input, err := json.Marshal(StepInput{
		Observation: Observation{Phase: PhaseBid, Round: round, Bounties: views},
		Wallet:      WalletView{ID: ag.ID, Balance: bal},
	})
	if err != nil {
		o.Log.Error("bid step: marshal", "agent", ag.ID, "err", err)
		return nil
	}

	token := fmt.Sprintf("tok-%s-r%d-bid", ag.ID, round)
	o.Proxy.Authorize(token, ag.ID)
	defer o.Proxy.Revoke(token)

	res, err := o.Steps.RunStep(ctx, StepRequest{
		AgentID: ag.ID,
		Name:    fmt.Sprintf("%s-r%d-bid", ag.ID, round),
		Token:   token,
		Input:   input,
		Timeout: o.Cfg.StepTimeout,
	})
	if err != nil {
		o.traceEvent(trace.EventNote, map[string]any{"note": "bid step platform error", "agent": ag.ID, "err": err.Error()})
		return nil
	}
	if res.TimedOut || res.ExitCode != 0 {
		o.traceEvent(trace.EventNote, map[string]any{"note": "bid step failed", "agent": ag.ID, "exit": res.ExitCode, "timed_out": res.TimedOut})
		return nil
	}
	actions, err := ParseActions(res.Stdout)
	if err != nil {
		o.traceEvent(trace.EventNote, map[string]any{"note": "bid step output unparseable", "agent": ag.ID, "err": err.Error()})
		return nil
	}
	return actions
}

// placeBids enters an agent's asks into the auctions that are still taking
// them. It mutates the auction book, so it belongs to the goroutine that owns
// the auctions: the round loop, or the sim's actor.
func (o *Orchestrator) placeBids(ag *Agent, actions []Action, aucs map[string]*auction.Auction) {
	for _, a := range actions {
		if a.Type != ActionBid {
			continue
		}
		auc, ok := aucs[a.Bounty]
		if !ok {
			continue
		}
		if err := auc.Place(ag.ID, a.Price); err != nil {
			// Refused bids never enter the book, but the refusal is on record.
			o.traceEvent(trace.EventNote, map[string]any{"note": "bid refused", "agent": ag.ID, "bounty": a.Bounty, "price": a.Price, "err": err.Error()})
			continue
		}
		o.traceEvent(trace.EventBid, map[string]any{"bounty": a.Bounty, "agent": ag.ID, "price": a.Price})
	}
}

// attempt runs the winner's one attempt at a bounty and settles every
// consequence: board state, money, ladder, bankruptcy. The three legs live in
// settle.go, shared with the sim loop — the money-path is the same code in
// both tracks.
func (o *Orchestrator) attempt(ctx context.Context, round int, b *bounty.Bounty) error {
	ag := o.agent(b.Winner)
	if ag == nil || ag.Retired {
		// The winner died earlier this round (an agent can win several
		// bounties and bankrupt on the first attempt). No attempt happened,
		// so no failure is counted: void, back to auction.
		if err := o.Board.Void(b.ID); err != nil {
			return err
		}
		o.traceEvent(trace.EventBounty, map[string]any{"action": "voided", "id": b.ID, "reason": "winner retired before attempt"})
		return nil
	}
	attWallet := fmt.Sprintf("att:%s:r%d", b.ID, round)
	budget, err := o.fundAttempt(ctx, ag.ID, attWallet, b.TokenCeiling)
	if err != nil {
		return err
	}
	out, err := o.performAttempt(ctx, ag, b, attWallet, round, budget)
	if err != nil {
		return err
	}
	return o.settleAttempt(ctx, ag, b, attWallet, budget, out)
}

func (o *Orchestrator) record(a rating.Attempt) {
	if o.Ladder != nil {
		o.Ladder.Record(a)
	}
}

func (o *Orchestrator) traceEvent(typ trace.EventType, payload any) {
	if o.tw == nil {
		return
	}
	if err := o.tw.Append(typ, payload); err != nil {
		o.Log.Warn("trace append failed", "type", typ, "err", err)
	}
}
