package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/metahumansworld/aiphylum/internal/bounty"
	"github.com/metahumansworld/aiphylum/internal/judge"
	"github.com/metahumansworld/aiphylum/internal/ledger"
	"github.com/metahumansworld/aiphylum/internal/proxy"
)

// briefGen is the judged test task: "write: <word>", with the word as the
// rubric. The stub grader passes a submission that contains the rubric, so a
// fake agent decides its own verdict by what it writes — the same way a real
// agent does against a real grader.
type briefGen struct{}

func (briefGen) Name() string { return "brief" }
func (briefGen) Generate(seed int64, tier int) (bounty.Task, error) {
	word := fmt.Sprintf("w%d-%d", seed, tier)
	return bounty.Task{Prompt: "write: " + word, Rubric: word, ReferenceTokens: 20}, nil
}

func briefPost(seed int64, tier int) Posting {
	return Posting{Generator: "brief", Seed: seed, Tier: tier, WallClockSec: 30}
}

// judgedWorld is a world that can post judged bounties: the ladder is removed
// (a ranked world refuses them) and a real judge is wired to the real proxy,
// so grading is a metered call like any other.
func judgedWorld(t *testing.T, endowment ledger.Credits) *world {
	t.Helper()
	w := newWorld(t, &proxy.StubProvider{}, nil, Config{})
	w.orch.Board.RegisterGenerator(briefGen{})
	w.orch.Ladder = nil
	if err := w.orch.EnableJudging(w.ctx, &judge.HTTP{
		Base: w.base, Model: "stub-1", MaxTokens: 64,
	}, endowment); err != nil {
		t.Fatal(err)
	}
	return w
}

// answers submits whatever the given function makes of the task prompt.
func answers(fn func(prompt string) string) stepFunc {
	return script(bidAll(0.5), func(_ StepRequest, in StepInput) StepResult {
		return out(Action{Type: ActionSubmit, Answer: fn(in.Observation.Task.Prompt)})
	})
}

func briefWord(prompt string) string { return strings.TrimPrefix(prompt, "write: ") }

// The structural half of "judged work is never ranked": a world holding a
// ladder refuses the posting outright, rather than posting it and hoping
// every downstream consumer remembers to exclude it.
func TestRankedWorldRefusesJudgedBounty(t *testing.T) {
	w := newWorld(t, &proxy.StubProvider{}, nil, Config{})
	w.orch.Board.RegisterGenerator(briefGen{})
	w.add(t, "solo", 1000)
	w.steps.fns["solo"] = answers(briefWord)

	err := w.orch.RunEpisode(w.ctx, oneRound(briefPost(1, 1)))
	if err == nil {
		t.Fatal("a ranked world posted a judged bounty")
	}
	if !strings.Contains(err.Error(), "judged") {
		t.Errorf("err = %v, want it to name the judged bounty as the reason", err)
	}
	if w.orch.Ladder == nil {
		t.Fatal("the test world lost its ladder; the refusal proves nothing")
	}
}

// A judged bounty the grader passes pays exactly like a verified one: the
// asked price, minted to the winner, with the bounty closed solved.
func TestJudgedBountyPaidOnPassingVerdict(t *testing.T) {
	w := judgedWorld(t, 100_000)
	w.add(t, "careful", 1000)
	w.steps.fns["careful"] = answers(func(p string) string {
		return "summary — " + briefWord(p) + ", as required"
	})

	if err := w.orch.RunEpisode(w.ctx, oneRound(briefPost(1, 1))); err != nil {
		t.Fatal(err)
	}
	b := w.bounty(t, "b0001")
	if b.State != bounty.StateSolved {
		t.Fatalf("bounty state = %s, want solved", b.State)
	}
	// Tier 1 under briefGen: refCost 100, MaxPayout 300, bid at 50% = 150.
	if got := w.balance(t, "careful"); got != 1150 {
		t.Errorf("balance = %d, want 1150 (grant + the asked price, nothing burned)", got)
	}
}

// A failing verdict is an ordinary failure: the agent ate its costs, the
// bounty counts the failure and returns to auction. Nothing about the grader
// being a model changes the money.
func TestJudgedBountyFailsOnFailingVerdict(t *testing.T) {
	w := judgedWorld(t, 100_000)
	w.add(t, "sloppy", 1000)
	w.steps.fns["sloppy"] = answers(func(string) string { return "something else entirely" })

	if err := w.orch.RunEpisode(w.ctx, oneRound(briefPost(1, 1))); err != nil {
		t.Fatal(err)
	}
	b := w.bounty(t, "b0001")
	if b.State != bounty.StateOpen || b.Failures != 1 {
		t.Fatalf("bounty = %s with %d failures, want open with 1", b.State, b.Failures)
	}
	if got := w.balance(t, "sloppy"); got != 1000 {
		t.Errorf("balance = %d, want 1000 (no payout, and this agent spent nothing)", got)
	}
}

// errJudge is a grader that cannot answer — a stand-in for every way the
// platform's own instrument can break.
type errJudge struct{ err error }

func (e errJudge) Decide(context.Context, string, judge.Request) (judge.Verdict, error) {
	return judge.Verdict{}, e.err
}

// Plan verification #7, on the judged path: the grader failing is the
// platform failing. The attempt is voided and refunded, the bounty records no
// failure, and the agent's balance is untouched.
func TestJudgeFailureIsAPlatformFault(t *testing.T) {
	w := judgedWorld(t, 100_000)
	w.orch.Judge = errJudge{errors.New("grader gave no verdict line")}
	w.add(t, "unlucky", 1000)

	// This agent burns real credits on the attempt, so the refund has
	// something to undo.
	w.steps.fns["unlucky"] = script(bidAll(0.5), func(req StepRequest, in StepInput) StepResult {
		if code := callModel(t, w.base, req.Token, in.Observation.Task.Prompt, 64); code != http.StatusOK {
			t.Errorf("model call status = %d, want 200", code)
		}
		return out(Action{Type: ActionSubmit, Answer: briefWord(in.Observation.Task.Prompt)})
	})

	if err := w.orch.RunEpisode(w.ctx, oneRound(briefPost(1, 1))); err != nil {
		t.Fatal(err)
	}
	b := w.bounty(t, "b0001")
	if b.State != bounty.StateOpen {
		t.Fatalf("bounty state = %s, want open (voided returns it to auction)", b.State)
	}
	if b.Failures != 0 {
		t.Errorf("failures = %d, want 0: a grader outage is not the agent's mistake", b.Failures)
	}
	if got := w.balance(t, "unlucky"); got != 1000 {
		t.Errorf("balance = %d, want 1000: the burn must be refunded in full", got)
	}
}

// A judged bounty in a world with no judge is refused the same way, rather
// than quietly resolving as a failure. The agent asked for grading it never
// received.
func TestJudgedBountyWithNoJudgeVoids(t *testing.T) {
	w := judgedWorld(t, 100_000)
	w.orch.Judge = nil
	w.add(t, "orphan", 1000)
	w.steps.fns["orphan"] = answers(briefWord)

	if err := w.orch.RunEpisode(w.ctx, oneRound(briefPost(1, 1))); err != nil {
		t.Fatal(err)
	}
	b := w.bounty(t, "b0001")
	if b.State != bounty.StateOpen || b.Failures != 0 {
		t.Fatalf("bounty = %s with %d failures, want open with 0", b.State, b.Failures)
	}
	if got := w.balance(t, "orphan"); got != 1000 {
		t.Errorf("balance = %d, want 1000", got)
	}
}

// The judge spends its own endowment, never the agent's wallet — otherwise an
// agent's efficiency would depend on how verbose its grader felt, and a
// bounty could bankrupt its winner after the work was already done.
func TestJudgeSpendsItsOwnWallet(t *testing.T) {
	w := judgedWorld(t, 100_000)
	w.add(t, "careful", 1000)
	w.steps.fns["careful"] = answers(briefWord)

	if err := w.orch.RunEpisode(w.ctx, oneRound(briefPost(1, 1))); err != nil {
		t.Fatal(err)
	}
	if got := w.balance(t, JudgeWallet); got >= 100_000 {
		t.Errorf("judge wallet = %d, want less than the endowment: grading is metered", got)
	}
	// The winner paid nothing for being graded: grant + asked price exactly.
	if got := w.balance(t, "careful"); got != 1150 {
		t.Errorf("balance = %d, want 1150 — no grading cost reached the agent", got)
	}
	// And the judge's spend is inside the invariant, not outside it.
	if err := w.orch.Ledger.Verify(w.ctx); err != nil {
		t.Errorf("conservation broke with a judge wallet in play: %v", err)
	}
}

// A judge that cannot pay is refused at the proxy exactly as a broke agent is
// — and that refusal is a platform fault, because the platform is the one who
// ran out of money.
func TestDryJudgeWalletVoidsRatherThanFails(t *testing.T) {
	w := judgedWorld(t, 1) // one credit: not enough to hold a single call
	w.add(t, "careful", 1000)
	w.steps.fns["careful"] = answers(briefWord)

	if err := w.orch.RunEpisode(w.ctx, oneRound(briefPost(1, 1))); err != nil {
		t.Fatal(err)
	}
	b := w.bounty(t, "b0001")
	if b.State != bounty.StateOpen || b.Failures != 0 {
		t.Fatalf("bounty = %s with %d failures, want open with 0", b.State, b.Failures)
	}
	if got := w.balance(t, "careful"); got != 1000 {
		t.Errorf("balance = %d, want 1000: an unaffordable grader must not cost the agent", got)
	}
}
