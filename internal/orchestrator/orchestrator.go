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

	"github.com/metahumansworld/soscitea/internal/auction"
	"github.com/metahumansworld/soscitea/internal/bounty"
	"github.com/metahumansworld/soscitea/internal/ledger"
	"github.com/metahumansworld/soscitea/internal/proxy"
	"github.com/metahumansworld/soscitea/internal/rating"
	"github.com/metahumansworld/soscitea/internal/trace"
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

// BookPolicy is what the platform tells an auction's bidders about the rest
// of the book when it announces the outcome. Sealed is the zero value and the
// default on every track: each bidder learns its own ask, the clearing price,
// the winner and the head-count, never a rival's number. OpenBook hands every
// bidder the whole book — every name and every ask — and exists so the
// protocol's oldest prediction could be run instead of only argued: that
// agents reading each other's exact asks stop pricing the work and walk the
// price to the reserve. The README keeps what happened.
//
// The policy is the announcer's, not the auction's — the book is public in
// the trace either way, and what varies is what the crier repeats to the
// people it was sealed against — which is why it lives here on Config rather
// than beside TieBreak: Award never reads it. Only the fair's command line
// ever sets it, and only the fair's start line declares it — announce honors
// the field wherever Config carries it, so setting it anywhere else would
// run the experiment undeclared. The arena and the sim leave the zero value
// untouched and announce exactly as they always have.
type BookPolicy int

const (
	SealedBook BookPolicy = iota
	OpenBook
)

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
	// Book is what announce repeats of the auction book. The zero value is
	// SealedBook, which is every track's behaviour before the policy had a
	// name; NewFair refuses a value it does not know rather than falling
	// back, for the tie-break's reason.
	Book BookPolicy
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
	// Judge grades judged bounties. Nil in a ranked world by construction —
	// postBounty refuses judged supply where a ladder is attached. Install it
	// with EnableJudging, which endows the wallet the grading is paid from.
	Judge Judge

	// Suites is the provenance of every imported benchmark suite registered
	// with the board. Set by whoever registered them; the orchestrator only
	// publishes it, once per episode, so that the asterisk an imported bounty
	// wears is explained inside the same trace rather than in documentation
	// the reader may never see.
	Suites []SuiteNote

	tw     *trace.Writer
	faults *faultTracker
	memos  *notebook
	crier  *crier
	// epoch namespaces attempt wallets across episodes. A bounty that failed
	// in one episode re-enters auction in the next with its ID intact; if it
	// is re-awarded at the same round number, the attempt wallet name repeats
	// — and the ledger refuses to recreate a retired account, ever. Zero (the
	// single-episode demo and sim, and every pinned trace) keeps the original
	// un-namespaced names; NextEpoch moves a multi-episode world onto fresh
	// ones.
	epoch  int
	agents []*Agent // registration order: the deterministic iteration order
	// watch, if set, sees every settled attempt alongside the ladder. The sim
	// track uses it to keep its own tallies without becoming a ranking.
	watch func(rating.Attempt)
}

// SuiteNote is one imported suite's provenance, as it appears in the trace.
// It mirrors suites.Manifest without importing it: the orchestrator has no
// business knowing how a suite file is parsed, only what must be published.
type SuiteNote struct {
	Name          string
	Source        string
	Licence       string
	Contamination string
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
		memos:  &notebook{memos: map[string]string{}},
		crier:  &crier{pending: map[string][]AuctionResult{}},
	}
}

// notebook holds what each agent last wrote to itself. It is the platform's
// half of the only promise the step protocol makes about continuity: a step is
// a fresh process with no memory of the last one, so anything an agent wants to
// carry forward has to be carried by something that outlives the container.
// This map is that something.
//
// It is deliberately the same shape as faultTracker — a small mutex-guarded map
// hanging off the orchestrator — but with the opposite lifetime. A fault is
// scoped to one step and cleared on the way in and the way out; a memo is
// scoped to the agent, and nothing clears it. Not the next step, not the next
// attempt, not the next day. Persistence across days is the entire point: an
// agent that could only remember within a day could not learn that a habit is
// costing it money, because the bill arrives tomorrow.
//
// Keyed by agent ID, never by wallet. faultTracker keys the other way for a
// good reason — a fault is a fact about the wallet the call was billed to, and
// attempt wallets are named per bounty per round — but a memo is a fact about
// the agent. Keying it by wallet would silently forget everything each time an
// attempt wallet was minted and retired, which is once per awarded bounty.
//
// The platform never reads what it stores here. It counts the bytes, and that
// is the whole of its interest in the contents.
type notebook struct {
	mu    sync.Mutex
	memos map[string]string
}

func (n *notebook) get(agent string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.memos[agent]
}

// write records a memo, or clears it when text is empty. It reports false when
// the text is over the cap: the write is refused, whatever was there before
// still stands, and the caller traces the refusal. Truncating instead would
// hand the agent back a thought it had no way to know was cut in half.
func (n *notebook) write(agent, text string) bool {
	if len(text) > MaxMemoBytes {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if text == "" {
		delete(n.memos, agent)
		return true
	}
	n.memos[agent] = text
	return true
}

// crier holds auction outcomes between the award that produced them and the
// bid step that hears them. Keyed by agent for the reason the notebook is: an
// outcome is a fact about the bidder, and the attempt wallet a bidder spends
// from is retired long before the next observation is built.
//
// The sim awards on its actor goroutine while bid steps run in workers, so
// this is mutex-guarded like everything else two goroutines touch.
type crier struct {
	mu      sync.Mutex
	pending map[string][]AuctionResult
}

func (c *crier) file(agent string, r AuctionResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending[agent] = append(c.pending[agent], r)
}

// take hands over an agent's undelivered outcomes and forgets them. Announced
// once, by a platform that then stops repeating itself: a market tells you the
// price when it closes, and remembering it is your job — which is precisely
// the job the memo exists to do.
//
// Draining here rather than after the step succeeds is deliberate. An agent
// whose process dies before it reads its own stdin has missed the
// announcement, the way it also misses the round's bidding; the platform is
// not obliged to keep shouting at a container that is not listening.
func (c *crier) take(agent string) []AuctionResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	rs := c.pending[agent]
	delete(c.pending, agent)
	return rs
}

// announce files the outcome of one closed auction with every agent that bid
// in it. All three tracks award through here, so a result means the same thing
// whether it came from a round, a sim window, or a bid window at the fair.
//
// Nothing is traced: every field of every result is derivable from the
// "awarded" event the caller has just written, so a line per bidder would say,
// fifteen times over, what one line already said. That stays true under an
// open book — the whole book is on that same awarded line, so opening it
// widens what each bidder is told without widening the record by a byte.
func (o *Orchestrator) announce(bountyID string, round int, winner auction.Bid, book []auction.Bid) {
	var open []BookEntry
	if o.Cfg.Book == OpenBook {
		open = make([]BookEntry, len(book))
		for i, b := range book {
			open[i] = BookEntry{Agent: b.Agent, Asked: b.Price}
		}
	}
	for _, b := range book {
		o.crier.file(b.Agent, AuctionResult{
			Bounty: bountyID, Round: round, Asked: b.Price,
			Won:      b.Agent == winner.Agent,
			Clearing: winner.Price, Winner: winner.Agent, Bidders: len(book),
			Book: open,
		})
	}
}

// faultTracker tees proxy events into the trace and remembers, per wallet,
// whether a platform fault occurred — the proxy's half of failure attribution,
// surfaced to the orchestrator's half.
//
// A remembered fault is scoped to one step: every step clears its wallet on the
// way in and on the way out, so the map holds only steps currently running. It
// has to be, as much as for the memory — attempt wallets are named per bounty
// per round, so a fault kept past its step would be a fact about a wallet that
// no longer exists, in a daemon that never restarts.
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

// resetAll drops every remembered fault. Between episodes nothing is running,
// so anything still in the map is a leftover from a step that never settled —
// it must not colour the first attempt of the next episode.
func (f *faultTracker) resetAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	clear(f.faulted)
}

// NextEpoch advances the world to a fresh episode: attempt wallets get a new
// namespace and the fault tracker starts clean. A caller running more than one
// episode on the same orchestrator MUST call this before each RunEpisode after
// the first — without it, a bounty failed in one episode and re-awarded at the
// same round number in the next would collide with its own retired attempt
// wallet and abort the episode.
func (o *Orchestrator) NextEpoch() {
	o.epoch++
	o.faults.resetAll()
}

// attemptWallet names the wallet one attempt is funded through. Epoch zero
// keeps the historical two-part name so every trace pinned before epochs
// existed still replays byte for byte.
func (o *Orchestrator) attemptWallet(bountyID string, round int) string {
	if o.epoch == 0 {
		return fmt.Sprintf("att:%s:r%d", bountyID, round)
	}
	return fmt.Sprintf("att:%s:e%d:r%d", bountyID, o.epoch, round)
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

// Readmit seats an agent whose wallet is already in the book: a daemon
// restarting over its own ledger. Nothing is minted — the grant was paid once,
// by AddAgent, and the balance is whatever the agent has made of it since —
// and retirement is read from the ledger, so a bankrupt comes back bankrupt.
func (o *Orchestrator) Readmit(ctx context.Context, id string) error {
	acct, err := o.Ledger.Get(ctx, id)
	if err != nil {
		return err
	}
	if acct.Kind != ledger.KindAgent {
		return fmt.Errorf("readmit %s: %s is not an agent wallet", id, acct.Kind)
	}
	o.agents = append(o.agents, &Agent{ID: id, Retired: acct.Closed})
	o.traceEvent(trace.EventAgent, map[string]any{
		"action": "readmitted", "agent": id, "balance": acct.Balance, "retired": acct.Closed,
	})
	return nil
}

// SetEpoch resumes the attempt-wallet namespace where an earlier process left
// it. A world restarted at a stale epoch reaches for wallet names its
// predecessor retired, and the ledger halts it; the daemon writes the epoch
// down before every episode so that this number is always the true one.
func (o *Orchestrator) SetEpoch(n int) { o.epoch = n }

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
//
// A caller running several episodes on one orchestrator must call NextEpoch
// between them — see its comment for what goes wrong without it.
func (o *Orchestrator) RunEpisode(ctx context.Context, ep Episode) error {
	o.traceEvent(trace.EventEpisode, map[string]any{"action": "start", "rounds": len(ep.Rounds)})
	// Declare imported supply before any of it is posted, so a reader meets
	// the licence and the contamination note before the first asterisk. A
	// world with no imports emits nothing here and its trace is unchanged.
	for _, s := range o.Suites {
		o.traceEvent(trace.EventSuite, map[string]any{
			"action": "registered", "suite": s.Name, "source": s.Source,
			"licence": s.Licence, "contamination": s.Contamination,
		})
	}
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
		o.announce(b.ID, round, winner, book)
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
// through here, so a bounty looks the same in either trace — and so this is
// the one place that has to refuse judged supply in a ranked world.
//
// A model's opinion must never become a rank. The refusal halts the episode
// rather than quietly skipping the posting: a ranked world configured with
// judged generators is a mistake in the wiring, and continuing would mean
// running a different world than the operator asked for.
func (o *Orchestrator) postBounty(p Posting) (*bounty.Bounty, error) {
	b, err := o.Board.Post(p.Generator, p.Seed, p.Tier, p.TokenCeiling, p.WallClockSec)
	if err != nil {
		return nil, err
	}
	if b.Judged && o.Ladder != nil {
		return nil, fmt.Errorf(
			"orchestrator: %s (%s) is judged by a model, and this world is ranked — judged bounties are sim-only by design",
			b.ID, b.Generator)
	}
	payload := map[string]any{
		"action": "posted", "id": b.ID, "generator": b.Generator, "seed": b.Seed,
		"tier": b.Tier, "max_payout": b.MaxPayout, "reserve": o.reserveFor(b.MaxPayout),
		"answer_digest": b.AnswerDigest(), "token_ceiling": b.TokenCeiling,
		"wall_clock_sec": b.WallClockSec, "failures": b.Failures,
	}
	// Only when true: a keyed bounty's posting line stays exactly what it was,
	// so benchmark traces recorded before judging existed still replay byte
	// for byte. For a judged bounty the digest above is of the rubric.
	if b.Judged {
		payload["judged"] = true
	}
	// Same conditional, same reason: generated supply's posting line keeps the
	// exact shape it has always had, so traces recorded before suites existed
	// still replay byte for byte.
	if b.Suite != "" {
		payload["suite"] = b.Suite
	}
	o.traceEvent(trace.EventBounty, payload)
	return b, nil
}

// bountyView is what an agent is shown of a bounty it may bid on.
func (o *Orchestrator) bountyView(b *bounty.Bounty) BountyView {
	return BountyView{
		ID: b.ID, Tier: b.Tier, Prompt: b.Prompt,
		MaxPayout: b.MaxPayout, Reserve: o.reserveFor(b.MaxPayout),
		Failures: b.Failures, AnswerDigest: b.AnswerDigest(), Judged: b.Judged,
		Suite: b.Suite,
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
	// No stay offer: the ranked loop has rounds, not places, so there is
	// nowhere to linger and nothing to charge for lingering there.
	o.placeBids(ag, o.performBidStep(ctx, ag, round, views, nil), aucs)
}

// performBidStep is the half of a bid that runs the agent's code: it asks what
// the agent wants to bid and returns those actions. Like performAttempt it
// touches only mutex-guarded pieces, so the sim can run it in a worker while
// its actor loop keeps the auctions moving.
// A non-nil stay is the fair handing the agent its own whereabouts and the
// price of staying there; nil omits both fields, which is why a track without
// geography writes exactly the observation it always did.
func (o *Orchestrator) performBidStep(ctx context.Context, ag *Agent, round int, views []BountyView, stay *StayOffer) []Action {
	bal, err := o.Ledger.Balance(ctx, ag.ID)
	if err != nil {
		o.Log.Warn("bid step: balance lookup failed", "agent", ag.ID, "err", err)
		return nil
	}
	obs := Observation{
		Phase: PhaseBid, Round: round, Bounties: views,
		Memo: o.memos.get(ag.ID), Results: o.crier.take(ag.ID),
	}
	if stay != nil {
		obs.Place, obs.StayPrice, obs.StayTicksLeft = stay.Place, stay.Price, stay.TicksLeft
	}
	input, err := json.Marshal(StepInput{
		Observation: obs,
		Wallet:      WalletView{ID: ag.ID, Balance: bal},
	})
	if err != nil {
		o.Log.Error("bid step: marshal", "agent", ag.ID, "err", err)
		return nil
	}

	// A bid step spends from the bankroll directly, so its faults are recorded
	// against the agent's own wallet. Clear it both ways: an agent runs one
	// step at a time, and last round's fault is not this round's.
	token := fmt.Sprintf("tok-%s-r%d-bid", ag.ID, round)
	o.Proxy.Authorize(token, ag.ID)
	o.faults.reset(ag.ID)
	defer o.Proxy.Revoke(token)
	defer o.faults.reset(ag.ID)

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
	o.takeMemo(ag.ID, actions)
	return actions
}

// takeMemo files whatever the agent wrote to itself this step. The last memo
// wins, for the reason the last sentinel line does: an agent that says two
// contradictory things has said the second one.
//
// Every step that reaches an agent's code runs through here, both phases, so a
// memo written while attempting is waiting at the next bid — which is the only
// way an agent can carry the outcome of an attempt forward, since the attempt
// wallet it spent from is retired before the next observation is built.
func (o *Orchestrator) takeMemo(agentID string, actions []Action) {
	text, wrote := "", false
	for _, a := range actions {
		if a.Type == ActionMemo {
			text, wrote = a.Text, true
		}
	}
	if !wrote {
		return
	}
	if !o.memos.write(agentID, text) {
		o.traceEvent(trace.EventNote, map[string]any{
			"note": "memo refused: over limit", "agent": agentID,
			"bytes": len(text), "limit": MaxMemoBytes,
		})
		return
	}
	o.traceEvent(trace.EventAgent, map[string]any{"action": "memo", "agent": agentID, "memo": text})
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
	attWallet := o.attemptWallet(b.ID, round)
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
	// The unranked tracks still want the tallies the ladder would have kept.
	if o.watch != nil {
		o.watch(a)
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
