package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/metahumansworld/aiphylum/internal/bounty"
	"github.com/metahumansworld/aiphylum/internal/ledger"
	"github.com/metahumansworld/aiphylum/internal/proxy"
	"github.com/metahumansworld/aiphylum/internal/rating"
	"github.com/metahumansworld/aiphylum/internal/trace"
)

// sayGen is the test task: "say exactly: <word>", where the word is a pure
// function of seed and tier. The fake agents can solve it from the prompt
// alone, so tests control exactly when an agent is right or wrong.
type sayGen struct{}

func (sayGen) Name() string { return "say" }
func (sayGen) Generate(seed int64, tier int) (bounty.Task, error) {
	word := fmt.Sprintf("w%d-%d", seed, tier)
	return bounty.Task{Prompt: "say exactly: " + word, Answer: word, ReferenceTokens: 20}, nil
}

// Tier 1 economics under sayGen: refCost = 20*5 = 100, MaxPayout = 300.

func answerFrom(prompt string) string {
	return strings.TrimPrefix(prompt, "say exactly: ")
}

// stepFunc is an in-process agent: it gets the parsed step input and returns
// the container's result.
type stepFunc func(req StepRequest, in StepInput) StepResult

// fakeSteps satisfies StepRunner without Docker. Steps named in errOn fail as
// the runner itself — the platform-fault path.
//
// The sim track runs several agents' steps at once, so the maps are guarded.
// The lock covers the lookups only: the step function itself runs unlocked, or
// one slow agent would block every other agent's turn — the exact thing the sim
// exists to let happen naturally.
type fakeSteps struct {
	mu       sync.Mutex
	fns      map[string]stepFunc
	errOn    map[string]error
	bidSteps map[string]int // agent → bid-phase invocations
}

func newFakeSteps() *fakeSteps {
	return &fakeSteps{fns: map[string]stepFunc{}, errOn: map[string]error{}, bidSteps: map[string]int{}}
}

func (f *fakeSteps) RunStep(_ context.Context, req StepRequest) (StepResult, error) {
	f.mu.Lock()
	err, failing := f.errOn[req.Name]
	f.mu.Unlock()
	if failing {
		return StepResult{}, err
	}
	var in StepInput
	if err := json.Unmarshal(req.Input, &in); err != nil {
		return StepResult{}, fmt.Errorf("fake step: bad input: %w", err)
	}

	f.mu.Lock()
	if in.Observation.Phase == PhaseBid {
		f.bidSteps[req.AgentID]++
	}
	fn, ok := f.fns[req.AgentID]
	f.mu.Unlock()
	if !ok {
		return StepResult{}, fmt.Errorf("fake step: no script for %s", req.AgentID)
	}
	return fn(req, in), nil
}

// bidStepsFor reads the bid-phase counter under the lock, for tests that assert
// on it while the sim may still have a worker in flight.
func (f *fakeSteps) bidStepsFor(agentID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bidSteps[agentID]
}

// out builds a well-formed step stdout: some agent logging, then the sentinel.
func out(actions ...Action) StepResult {
	env, err := json.Marshal(map[string][]Action{"actions": actions})
	if err != nil {
		panic(err)
	}
	return StepResult{Stdout: "agent chatter\n" + ActionsSentinel + string(env) + "\n"}
}

// script routes bid-phase and attempt-phase steps to separate behaviours.
func script(bid, attempt stepFunc) stepFunc {
	return func(req StepRequest, in StepInput) StepResult {
		if in.Observation.Phase == PhaseBid {
			return bid(req, in)
		}
		return attempt(req, in)
	}
}

// bidsFor is frac × MaxPayout on every open bounty, clamped to the reserve.
func bidsFor(in StepInput, frac float64) []Action {
	var acts []Action
	for _, b := range in.Observation.Bounties {
		price := ledger.Credits(float64(b.MaxPayout) * frac)
		if price < b.Reserve {
			price = b.Reserve
		}
		acts = append(acts, Action{Type: ActionBid, Bounty: b.ID, Price: price})
	}
	return acts
}

// bidAll bids that and nothing else.
func bidAll(frac float64) stepFunc {
	return func(_ StepRequest, in StepInput) StepResult {
		return out(bidsFor(in, frac)...)
	}
}

// world is one fully wired in-process world: real ledger, real proxy over
// HTTP, real board and ladder — only the container runner and the model are
// fakes.
type world struct {
	orch  *Orchestrator
	steps *fakeSteps
	base  string // proxy URL for fake agents' model calls
	ctx   context.Context
}

func newWorld(t *testing.T, provider proxy.Provider, tw *trace.Writer, cfg Config) *world {
	t.Helper()
	// Not ":memory:": its shared cache would make two worlds in one test (the
	// replay pair) collide in a single database.
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { led.Close() })

	board := bounty.NewBoard()
	board.RegisterGenerator(sayGen{})

	// 1000 nano-USD per token each way: a call costs exactly
	// inputToks + outputToks credits, which keeps arithmetic legible.
	table := proxy.NewPriceTable()
	table.Set("stub-1", proxy.Price{InputPerTok: 1000, OutputPerTok: 1000})

	steps := newFakeSteps()
	ladder := rating.New(rating.Config{MinAttempts: 1, MinTiers: 1})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	orch := New(led, board, table, provider, tw, steps, ladder, cfg, log)

	srv := httptest.NewServer(orch.Proxy)
	t.Cleanup(srv.Close)

	return &world{orch: orch, steps: steps, base: srv.URL, ctx: context.Background()}
}

func (w *world) add(t *testing.T, id string, grant ledger.Credits) {
	t.Helper()
	if err := w.orch.AddAgent(w.ctx, id, grant); err != nil {
		t.Fatal(err)
	}
}

func (w *world) balance(t *testing.T, id string) ledger.Credits {
	t.Helper()
	bal, err := w.orch.Ledger.Balance(w.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return bal
}

func (w *world) bounty(t *testing.T, id string) *bounty.Bounty {
	t.Helper()
	b, err := w.orch.Board.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// callModel makes a deterministic Messages call through the real proxy, as a
// containerised agent would, and returns the HTTP status.
func callModel(t *testing.T, base, token, prompt string, maxTokens int) int {
	t.Helper()
	body := fmt.Sprintf(`{"model":"stub-1","max_tokens":%d,"messages":[{"role":"user","content":%q}]}`,
		maxTokens, prompt)
	req, err := http.NewRequest(http.MethodPost, base+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// walletBalance reads the token's wallet through the real proxy, the way the
// SDK's Model.wallet does: the one number an agent can read about its own
// spend mid-step.
func walletBalance(t *testing.T, base, token string) ledger.Credits {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/v1/wallet", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Balance ledger.Credits `json:"balance"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("wallet lookup returned %d", resp.StatusCode)
	}
	return body.Balance
}

func oneRound(postings ...Posting) Episode {
	return Episode{Rounds: [][]Posting{postings}}
}

func post(seed int64, tier int) Posting {
	return Posting{Generator: "say", Seed: seed, Tier: tier, WallClockSec: 30}
}

// Plan verification #5, end to end: several agents bid; the lowest wins; the
// losers are charged nothing; the winner is paid its asked price — not the
// posted maximum — and its model spend comes out of its own wallet.
func TestEpisodeLowestBidWinsAndPaysAsked(t *testing.T) {
	w := newWorld(t, &proxy.StubProvider{}, nil, Config{})
	w.add(t, "cheap", 1000)
	w.add(t, "pricey", 1000)

	// cheap bids 40% of max (120), consults the model once, answers right.
	w.steps.fns["cheap"] = script(bidAll(0.4), func(req StepRequest, in StepInput) StepResult {
		if code := callModel(t, w.base, req.Token, in.Observation.Task.Prompt, 64); code != http.StatusOK {
			t.Errorf("model call status = %d, want 200", code)
		}
		return out(Action{Type: ActionSubmit, Answer: answerFrom(in.Observation.Task.Prompt)})
	})
	// pricey bids 90% (270) and must never get the award.
	w.steps.fns["pricey"] = script(bidAll(0.9), func(StepRequest, StepInput) StepResult {
		t.Error("pricey attempted a bounty it should have lost")
		return out()
	})

	if err := w.orch.RunEpisode(w.ctx, oneRound(post(7, 1))); err != nil {
		t.Fatal(err)
	}

	b := w.bounty(t, "b0001")
	if b.State != bounty.StateSolved {
		t.Fatalf("bounty state = %s, want solved", b.State)
	}

	rows := w.orch.Ladder.Board(time.Now())
	if len(rows) != 1 || rows[0].Agent != "cheap" || !rows[0].Ranked {
		t.Fatalf("ladder = %+v, want one ranked row for cheap", rows)
	}
	if rows[0].Earned != 120 {
		t.Fatalf("earned = %d, want the asked price 120, not the max payout 300", rows[0].Earned)
	}
	burned := rows[0].Burned
	if burned <= 0 {
		t.Fatal("cheap's model call burned nothing; the meter is not running")
	}
	if got, want := w.balance(t, "cheap"), ledger.Credits(1000)+120-burned; got != want {
		t.Fatalf("cheap balance = %d, want %d (grant + asked - burned)", got, want)
	}
	if got := w.balance(t, "pricey"); got != 1000 {
		t.Fatalf("pricey balance = %d, want untouched 1000 (losers are charged nothing)", got)
	}
}

// The winner eats its costs on failure: wrong answer → charged for the burn,
// nothing earned, bounty back on the board with the failure counted.
func TestWinnerEatsCostsOnFailure(t *testing.T) {
	w := newWorld(t, &proxy.StubProvider{}, nil, Config{})
	w.add(t, "solo", 1000)

	w.steps.fns["solo"] = script(bidAll(0.4), func(req StepRequest, in StepInput) StepResult {
		callModel(t, w.base, req.Token, in.Observation.Task.Prompt, 64)
		return out(Action{Type: ActionSubmit, Answer: "confidently wrong"})
	})

	if err := w.orch.RunEpisode(w.ctx, oneRound(post(7, 1))); err != nil {
		t.Fatal(err)
	}

	b := w.bounty(t, "b0001")
	if b.State != bounty.StateOpen || b.Failures != 1 {
		t.Fatalf("bounty state=%s failures=%d, want open/1", b.State, b.Failures)
	}
	rows := w.orch.Ladder.Board(time.Now())
	if len(rows) != 1 || rows[0].Successes != 0 || rows[0].Burned <= 0 {
		t.Fatalf("ladder = %+v, want one failed attempt with burn counted", rows)
	}
	if got, want := w.balance(t, "solo"), ledger.Credits(1000)-rows[0].Burned; got != want {
		t.Fatalf("balance = %d, want %d (grant - burn, nothing earned)", got, want)
	}
}

// An agent-side timeout is an agent fault: charged and failed, exactly like a
// wrong answer.
func TestAgentTimeoutCharged(t *testing.T) {
	w := newWorld(t, &proxy.StubProvider{}, nil, Config{})
	w.add(t, "slow", 1000)

	w.steps.fns["slow"] = script(bidAll(0.4), func(req StepRequest, in StepInput) StepResult {
		callModel(t, w.base, req.Token, in.Observation.Task.Prompt, 64)
		return StepResult{TimedOut: true, ExitCode: -1} // killed at the wall clock
	})

	if err := w.orch.RunEpisode(w.ctx, oneRound(post(7, 1))); err != nil {
		t.Fatal(err)
	}

	b := w.bounty(t, "b0001")
	if b.State != bounty.StateOpen || b.Failures != 1 {
		t.Fatalf("bounty state=%s failures=%d, want open/1", b.State, b.Failures)
	}
	rows := w.orch.Ladder.Board(time.Now())
	if len(rows) != 1 || rows[0].Burned <= 0 {
		t.Fatalf("ladder = %+v, want the timeout's burn counted", rows)
	}
	if got, want := w.balance(t, "slow"), ledger.Credits(1000)-rows[0].Burned; got != want {
		t.Fatalf("balance = %d, want %d", got, want)
	}
}

// flakyProvider fails on exactly one call (1-indexed), a stand-in for a
// provider outage mid-attempt.
type flakyProvider struct {
	inner  proxy.StubProvider
	calls  int
	failOn int
}

func (f *flakyProvider) Invoke(ctx context.Context, model string, body []byte) ([]byte, proxy.Usage, error) {
	f.calls++
	if f.calls == f.failOn {
		return nil, proxy.Usage{}, errors.New("provider melted")
	}
	return f.inner.Invoke(ctx, model, body)
}

// Plan verification #7, the refund half: a platform fault during a failed
// attempt refunds every credit the attempt burned, voids the award, counts no
// failure, and records nothing on the ladder.
func TestPlatformFaultRefundedAndVoided(t *testing.T) {
	w := newWorld(t, &flakyProvider{failOn: 2}, nil, Config{})
	w.add(t, "victim", 1000)

	w.steps.fns["victim"] = script(bidAll(0.4), func(req StepRequest, in StepInput) StepResult {
		// First call settles real cost; second dies at the provider.
		if code := callModel(t, w.base, req.Token, in.Observation.Task.Prompt, 64); code != http.StatusOK {
			t.Errorf("first call status = %d, want 200", code)
		}
		if code := callModel(t, w.base, req.Token, in.Observation.Task.Prompt+" again", 64); code != http.StatusBadGateway {
			t.Errorf("second call status = %d, want 502", code)
		}
		return out() // gives up: no submission
	})

	if err := w.orch.RunEpisode(w.ctx, oneRound(post(7, 1))); err != nil {
		t.Fatal(err)
	}

	if got := w.balance(t, "victim"); got != 1000 {
		t.Fatalf("balance = %d, want the full 1000 back (settled spend refunded)", got)
	}
	b := w.bounty(t, "b0001")
	if b.State != bounty.StateOpen || b.Failures != 0 || b.Winner != "" {
		t.Fatalf("bounty %+v, want open with zero failures — voided, not failed", b)
	}
	if rows := w.orch.Ladder.Board(time.Now()); len(rows) != 0 {
		t.Fatalf("ladder = %+v, want empty: a voided attempt never happened", rows)
	}
}

// The other side of attribution: success always stands. A platform fault
// during an attempt that still submits the right answer excuses nothing —
// the solve is real and is paid.
func TestSolveStandsDespitePlatformFault(t *testing.T) {
	w := newWorld(t, &flakyProvider{failOn: 1}, nil, Config{})
	w.add(t, "sturdy", 1000)

	w.steps.fns["sturdy"] = script(bidAll(0.4), func(req StepRequest, in StepInput) StepResult {
		if code := callModel(t, w.base, req.Token, in.Observation.Task.Prompt, 64); code != http.StatusBadGateway {
			t.Errorf("call status = %d, want 502", code)
		}
		// The model died, but this task is solvable from the prompt alone.
		return out(Action{Type: ActionSubmit, Answer: answerFrom(in.Observation.Task.Prompt)})
	})

	if err := w.orch.RunEpisode(w.ctx, oneRound(post(7, 1))); err != nil {
		t.Fatal(err)
	}

	if b := w.bounty(t, "b0001"); b.State != bounty.StateSolved {
		t.Fatalf("bounty state = %s, want solved — a fault never negates a solve", b.State)
	}
	// The failed call released its hold, so nothing was burned: grant + asked.
	if got := w.balance(t, "sturdy"); got != 1000+120 {
		t.Fatalf("balance = %d, want 1120", got)
	}
}

// A remembered fault outlives nothing. Attempt wallets are named per bounty
// per round, so a tracker that kept them would grow forever in a daemon that
// never restarts — and would be answering questions about wallets that no
// longer exist.
func TestFaultMemoryIsScopedToTheStep(t *testing.T) {
	w := newWorld(t, &flakyProvider{failOn: 1}, nil, Config{})
	w.add(t, "sturdy", 4000)

	w.steps.fns["sturdy"] = script(bidAll(0.4), func(req StepRequest, in StepInput) StepResult {
		callModel(t, w.base, req.Token, in.Observation.Task.Prompt, 64)
		return out(Action{Type: ActionSubmit, Answer: answerFrom(in.Observation.Task.Prompt)})
	})

	ep := orchestratorEpisode(post(7, 1), post(8, 1), post(9, 2))
	if err := w.orch.RunEpisode(w.ctx, ep); err != nil {
		t.Fatal(err)
	}

	w.orch.faults.mu.Lock()
	left := len(w.orch.faults.faulted)
	w.orch.faults.mu.Unlock()
	if left != 0 {
		t.Errorf("fault tracker still remembers %d wallets after the episode, want 0", left)
	}
}

// orchestratorEpisode puts each posting in its own round.
func orchestratorEpisode(ps ...Posting) Episode {
	ep := Episode{}
	for _, p := range ps {
		ep.Rounds = append(ep.Rounds, []Posting{p})
	}
	return ep
}

// A step-runner failure (docker itself broke) is likewise refunded and voided.
func TestRunnerErrorVoids(t *testing.T) {
	w := newWorld(t, &proxy.StubProvider{}, nil, Config{})
	w.add(t, "unlucky", 1000)

	w.steps.fns["unlucky"] = script(bidAll(0.4), func(StepRequest, StepInput) StepResult {
		t.Error("attempt fn ran despite errOn")
		return out()
	})
	w.steps.errOn["unlucky-b0001-r1"] = errors.New("docker daemon exploded")

	if err := w.orch.RunEpisode(w.ctx, oneRound(post(7, 1))); err != nil {
		t.Fatal(err)
	}

	if got := w.balance(t, "unlucky"); got != 1000 {
		t.Fatalf("balance = %d, want untouched 1000", got)
	}
	b := w.bounty(t, "b0001")
	if b.State != bounty.StateOpen || b.Failures != 0 {
		t.Fatalf("bounty state=%s failures=%d, want open/0", b.State, b.Failures)
	}
	if rows := w.orch.Ladder.Board(time.Now()); len(rows) != 0 {
		t.Fatalf("ladder = %+v, want empty", rows)
	}
}

// Plan verification #4: an agent that spends itself under the dust threshold
// is refused mid-attempt by the proxy, dust-burned to zero, retired, and can
// never come back — not by mint, not by re-registration, not by another round.
func TestBankruptcyIsPermanent(t *testing.T) {
	w := newWorld(t, &proxy.StubProvider{}, nil, Config{Dust: 180})
	w.add(t, "gambler", 200)

	w.steps.fns["gambler"] = script(bidAll(0.4), func(req StepRequest, in StepInput) StepResult {
		// One affordable call drops the balance below dust...
		if code := callModel(t, w.base, req.Token, "hello", 50); code != http.StatusOK {
			t.Errorf("first call status = %d, want 200", code)
		}
		// ...and the next is refused before any provider is contacted.
		if code := callModel(t, w.base, req.Token, "hello", 200); code != http.StatusPaymentRequired {
			t.Errorf("overdraft call status = %d, want 402", code)
		}
		return out(Action{Type: ActionSubmit, Answer: "wrong"})
	})

	ep := Episode{Rounds: [][]Posting{{post(7, 1)}, {post(8, 1)}}}
	if err := w.orch.RunEpisode(w.ctx, ep); err != nil {
		t.Fatal(err)
	}

	ag := w.orch.Agents()[0]
	if !ag.Retired {
		t.Fatal("gambler not retired")
	}
	if got := w.balance(t, "gambler"); got != 0 {
		t.Fatalf("balance = %d, want 0 (dust burned)", got)
	}
	// No resurrection by mint: the account is closed.
	if _, err := w.orch.Ledger.Mint(w.ctx, "gambler", 1000, "bribe", "no"); !errors.Is(err, ledger.ErrClosed) {
		t.Fatalf("mint into retired wallet: err = %v, want ErrClosed", err)
	}
	// No resurrection by re-registration: the ID is spent.
	if err := w.orch.AddAgent(w.ctx, "gambler", 1000); err == nil {
		t.Fatal("re-registering a retired agent succeeded; permadeath is not permanent")
	}
	// And the dead take no further steps: round 2 never invoked it.
	if got := w.steps.bidStepsFor("gambler"); got != 1 {
		t.Fatalf("bid steps = %d, want 1 (round 2 must skip the retired)", got)
	}
}

// Plan verification #9: the attempt wallet is the budget ceiling. An agent
// with a fat bankroll attempts under a small ceiling; the proxy refuses the
// call that would exceed it, with the bankroll untouched behind it. This same
// mechanism is what bounds a delegated subagent's allowance.
func TestAttemptBudgetCeiling(t *testing.T) {
	w := newWorld(t, &proxy.StubProvider{}, nil, Config{})
	w.add(t, "whale", 10000)

	w.steps.fns["whale"] = script(bidAll(0.4), func(req StepRequest, in StepInput) StepResult {
		if in.Wallet.Balance != 200 {
			t.Errorf("attempt wallet = %d, want the 200 ceiling, not the 10000 bankroll", in.Wallet.Balance)
		}
		// A small call fits under the ceiling...
		if code := callModel(t, w.base, req.Token, "cheap thought", 16); code != http.StatusOK {
			t.Errorf("small call status = %d, want 200", code)
		}
		// ...a big one is refused despite 10000 credits sitting in the bankroll.
		if code := callModel(t, w.base, req.Token, "expensive thought", 500); code != http.StatusPaymentRequired {
			t.Errorf("big call status = %d, want 402 at the ceiling", code)
		}
		return out(Action{Type: ActionSubmit, Answer: answerFrom(in.Observation.Task.Prompt)})
	})

	p := post(7, 1)
	p.TokenCeiling = 200
	if err := w.orch.RunEpisode(w.ctx, oneRound(p)); err != nil {
		t.Fatal(err)
	}

	if b := w.bounty(t, "b0001"); b.State != bounty.StateSolved {
		t.Fatalf("bounty state = %s, want solved", b.State)
	}
	rows := w.orch.Ladder.Board(time.Now())
	if len(rows) != 1 || rows[0].Burned <= 0 || rows[0].Burned > 200 {
		t.Fatalf("burned = %v, want within (0, 200]", rows)
	}
	if got, want := w.balance(t, "whale"), ledger.Credits(10000)+120-rows[0].Burned; got != want {
		t.Fatalf("balance = %d, want %d", got, want)
	}
}

// Malformed or empty step output is an agent fault, never a platform fault:
// the burn stands and the failure is counted. These pin the parse-error and
// no-submission rows of the attribution table, which no other test reaches.
func TestBadOutputIsAgentFault(t *testing.T) {
	cases := []struct {
		name    string
		attempt func(t *testing.T, w *world) stepFunc
	}{
		{
			// Stdout with no sentinel line at all: unparseable.
			name: "garbage stdout",
			attempt: func(t *testing.T, w *world) stepFunc {
				return func(req StepRequest, in StepInput) StepResult {
					callModel(t, w.base, req.Token, in.Observation.Task.Prompt, 64)
					return StepResult{Stdout: "spent the whole budget monologuing\n"}
				}
			},
		},
		{
			// Well-formed sentinel, but nothing submitted in it.
			name: "no submission",
			attempt: func(t *testing.T, w *world) stepFunc {
				return func(req StepRequest, in StepInput) StepResult {
					callModel(t, w.base, req.Token, in.Observation.Task.Prompt, 64)
					// A bid is meaningless in the attempt phase; it is ignored,
					// which leaves the attempt without a submission.
					return out(Action{Type: ActionBid, Bounty: "b0001", Price: 1})
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t, &proxy.StubProvider{}, nil, Config{})
			w.add(t, "mumbler", 1000)
			w.steps.fns["mumbler"] = script(bidAll(0.4), tc.attempt(t, w))

			if err := w.orch.RunEpisode(w.ctx, oneRound(post(7, 1))); err != nil {
				t.Fatal(err)
			}

			b := w.bounty(t, "b0001")
			if b.State != bounty.StateOpen || b.Failures != 1 {
				t.Fatalf("bounty state=%s failures=%d, want open/1 — an agent fault, not a void", b.State, b.Failures)
			}
			rows := w.orch.Ladder.Board(time.Now())
			if len(rows) != 1 || rows[0].Successes != 0 || rows[0].Burned <= 0 {
				t.Fatalf("ladder = %+v, want one failed attempt with burn counted", rows)
			}
			if got, want := w.balance(t, "mumbler"), ledger.Credits(1000)-rows[0].Burned; got != want {
				t.Fatalf("balance = %d, want %d (charged, not refunded)", got, want)
			}
		})
	}
}

// The two safety rails interact: an agent that wins several bounties in one
// round and bankrupts on the first attempt is gone before the second. That
// award is voided — no step runs, no failure counted, no ladder entry.
func TestRetiredWinnerSecondAwardVoided(t *testing.T) {
	w := newWorld(t, &proxy.StubProvider{}, nil, Config{Dust: 180})
	w.add(t, "doomed", 200)

	attempts := 0
	w.steps.fns["doomed"] = script(bidAll(0.4), func(req StepRequest, in StepInput) StepResult {
		attempts++
		// One call drops the bankroll under the dust threshold...
		if code := callModel(t, w.base, req.Token, "hello", 50); code != http.StatusOK {
			t.Errorf("call status = %d, want 200", code)
		}
		// ...and a wrong answer makes it a charged agent fault, so the
		// bankruptcy check runs at settlement.
		return out(Action{Type: ActionSubmit, Answer: "wrong"})
	})

	if err := w.orch.RunEpisode(w.ctx, oneRound(post(7, 1), post(8, 1))); err != nil {
		t.Fatal(err)
	}

	if attempts != 1 {
		t.Fatalf("attempt steps = %d, want 1: the dead take no second attempt", attempts)
	}
	if ag := w.orch.Agents()[0]; !ag.Retired {
		t.Fatal("doomed not retired after the first attempt")
	}
	// First bounty: a real failure, charged and counted.
	if b := w.bounty(t, "b0001"); b.State != bounty.StateOpen || b.Failures != 1 {
		t.Fatalf("b0001 state=%s failures=%d, want open/1", b.State, b.Failures)
	}
	// Second bounty: voided — back on the board as if never awarded.
	if b := w.bounty(t, "b0002"); b.State != bounty.StateOpen || b.Failures != 0 || b.Winner != "" {
		t.Fatalf("b0002 %+v, want open, unblamed, winnerless", b)
	}
	rows := w.orch.Ladder.Board(time.Now())
	if len(rows) != 1 || rows[0].Attempts != 1 {
		t.Fatalf("ladder = %+v, want exactly the one real attempt", rows)
	}
	if got := w.balance(t, "doomed"); got != 0 {
		t.Fatalf("balance = %d, want 0 (dust burned)", got)
	}
}

// Plan verification #6: replay a recorded episode against the trace and diff.
// The replayed world consumes every recorded call, diverges on none, and
// produces an identical trace.
func TestReplayExactness(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "original.jsonl")
	pathB := filepath.Join(dir, "replay.jsonl")

	// The scripted agents: alpha bids 50%, solves with one model call per
	// attempt; beta bids 70% and burns a call each bid phase deciding. All
	// request bodies are pure functions of the observation, so the replayed
	// run makes byte-identical calls.
	wire := func(w *world) {
		w.steps.fns["alpha"] = script(bidAll(0.5), func(req StepRequest, in StepInput) StepResult {
			callModel(t, w.base, req.Token, in.Observation.Task.Prompt, 64)
			return out(Action{Type: ActionSubmit, Answer: answerFrom(in.Observation.Task.Prompt)})
		})
		w.steps.fns["beta"] = script(func(req StepRequest, in StepInput) StepResult {
			callModel(t, w.base, req.Token, fmt.Sprintf("ponder round %d", in.Observation.Round), 32)
			return bidAll(0.7)(req, in)
		}, func(StepRequest, StepInput) StepResult {
			t.Error("beta won an award it should have lost")
			return out()
		})
	}
	ep := Episode{Rounds: [][]Posting{{post(7, 1), post(8, 2)}, {post(9, 1)}}}

	// Original run against the stub model.
	twA, err := trace.NewWriter(pathA)
	if err != nil {
		t.Fatal(err)
	}
	wA := newWorld(t, &proxy.StubProvider{}, twA, Config{})
	wA.add(t, "alpha", 5000)
	wA.add(t, "beta", 5000)
	wire(wA)
	if err := wA.orch.RunEpisode(wA.ctx, ep); err != nil {
		t.Fatal(err)
	}
	if err := twA.Close(); err != nil {
		t.Fatal(err)
	}

	// Replay: a fresh world whose "model" is the recorded trace.
	playback, err := trace.NewPlayback(pathA)
	if err != nil {
		t.Fatal(err)
	}
	twB, err := trace.NewWriter(pathB)
	if err != nil {
		t.Fatal(err)
	}
	wB := newWorld(t, playback, twB, Config{})
	wB.add(t, "alpha", 5000)
	wB.add(t, "beta", 5000)
	wire(wB)
	if err := wB.orch.RunEpisode(wB.ctx, ep); err != nil {
		t.Fatal(err)
	}
	if err := twB.Close(); err != nil {
		t.Fatal(err)
	}

	if n := playback.Remaining(); n != 0 {
		t.Fatalf("replay left %d recorded calls unconsumed", n)
	}
	if err := trace.Compare(pathA, pathB); err != nil {
		t.Fatalf("replay diverged from the original: %v", err)
	}
}
