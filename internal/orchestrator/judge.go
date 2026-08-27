// The judged-bounty seam: who grades open-ended work, who pays for it, and
// why it can never reach the ladder.
//
// A judged bounty has no answer key, so a model decides. That makes the
// verdict a judgement rather than a fact, and the plan is explicit about what
// follows: judged work lives in the sim track and is never ranked. The
// enforcement is structural, at the one choke point both tracks post through
// (postBounty), not a promise in a doc comment.
//
// The judge's tokens are the platform's, not the agent's. Grading is something
// the platform does to an attempt, so charging the agent for it would make an
// agent's efficiency depend on how verbose the grader felt — and would let a
// bounty bankrupt its winner after the work was already done. The judge spends
// from its own endowed wallet, metered through the same proxy as everyone
// else, and a judge wallet that runs dry refuses the call, which is a platform
// fault: the attempt is voided and refunded, not failed.
package orchestrator

import (
	"context"
	"errors"
	"fmt"

	"github.com/singhtushant3-hub/aiphylum/internal/bounty"
	"github.com/singhtushant3-hub/aiphylum/internal/judge"
	"github.com/singhtushant3-hub/aiphylum/internal/ledger"
	"github.com/singhtushant3-hub/aiphylum/internal/trace"
)

// JudgeWallet is the platform's grading purse. It is an experiment wallet: it
// holds real credits and is counted by the conservation audit like any other,
// but nothing about it is ranked and it cannot go bankrupt — it simply stops
// being able to judge.
const JudgeWallet = "platform:judge"

// Judge grades one submission against a hidden rubric. The orchestrator hands
// it a proxy token rather than a wallet: the judge spends, but never decides
// whose money it is.
type Judge interface {
	Decide(ctx context.Context, token string, r judge.Request) (judge.Verdict, error)
}

// ErrNoJudge is returned when a judged bounty is attempted in a world that
// never enabled judging. It is a wiring mistake, not an agent failure.
var ErrNoJudge = errors.New("orchestrator: judged bounty with no judge enabled")

// EnableJudging endows the judge wallet and installs the grader. Call once,
// before the world runs.
func (o *Orchestrator) EnableJudging(ctx context.Context, j Judge, endowment ledger.Credits) error {
	if j == nil {
		return errors.New("orchestrator: EnableJudging needs a judge")
	}
	if err := o.Ledger.CreateAccount(ctx, JudgeWallet, ledger.KindExperiment, "platform"); err != nil {
		return err
	}
	if endowment > 0 {
		if _, err := o.Ledger.Mint(ctx, JudgeWallet, endowment, "judge endowment", "judge:endowment"); err != nil {
			return err
		}
	}
	o.Judge = j
	o.traceEvent(trace.EventNote, map[string]any{
		"note": "judging enabled", "wallet": JudgeWallet, "endowment": endowment,
	})
	return nil
}

// judgeSubmission grades one attempt. It runs on the worker goroutine that ran
// the attempt, not on whichever goroutine owns the ledger: a grading call is a
// model call, and a model call on the actor loop would stall every auction in
// the sim for as long as the grader takes to think.
//
// The token is unique per attempt because the sim grades several attempts at
// once against the same wallet. Faults are reported by the returned error
// rather than read back out of the fault tracker — the tracker is keyed by
// wallet, and concurrent judge calls sharing one wallet would overwrite each
// other's flag.
func (o *Orchestrator) judgeSubmission(ctx context.Context, b *bounty.Bounty, round int, submitted string) (judge.Verdict, error) {
	if o.Judge == nil {
		return judge.Verdict{}, ErrNoJudge
	}
	token := fmt.Sprintf("tok-judge-%s-r%d", b.ID, round)
	o.Proxy.Authorize(token, JudgeWallet)
	defer o.Proxy.Revoke(token)

	return o.Judge.Decide(ctx, token, judge.Request{
		Bounty:     b.ID,
		Task:       b.Prompt,
		Rubric:     b.Rubric(),
		Submission: submitted,
	})
}
