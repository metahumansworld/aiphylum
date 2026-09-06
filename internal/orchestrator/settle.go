// The attempt money-path: fund, perform, settle. Split out of the round loop
// so the sim track's real-time loop ends every attempt in the very same code —
// two tracks that could diverge on what an attempt costs or pays would make
// the shared currency a fiction.
//
// The split is also the concurrency contract. performAttempt touches only
// mutex-guarded pieces (proxy, step runner, fault tracker) and may run in a
// worker goroutine; fundAttempt and settleAttempt mutate the ledger, the
// board and the roster, and belong to whichever single goroutine owns those —
// the round loop itself, or the sim's actor loop.
package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/metahumansworld/aiphylum/internal/bounty"
	"github.com/metahumansworld/aiphylum/internal/judge"
	"github.com/metahumansworld/aiphylum/internal/ledger"
	"github.com/metahumansworld/aiphylum/internal/rating"
	"github.com/metahumansworld/aiphylum/internal/trace"
)

// attemptOutcome is what one attempt step left behind: a submission, or the
// name of whoever broke.
type attemptOutcome struct {
	submitted  string
	attempted  bool
	agentFault string // the agent broke: timeout, crash, bad output, no submission
	runErr     error  // the step runner itself broke — a platform fault

	// Judged bounties only: what the grader said, or why it could not say.
	// judgeErr is a platform fault for the same reason runErr is — the grader
	// is the platform's instrument, and an agent must never be charged for the
	// platform's inability to grade the work it asked for.
	verdict  judge.Verdict
	judged   bool
	judgeErr error
}

// fundAttempt opens the attempt wallet and funds it with min(bankroll, token
// ceiling). The proxy's balance check then IS the ceiling — the same mechanism
// that bounds a delegated subagent bounds the whole attempt, with no extra
// bookkeeping.
func (o *Orchestrator) fundAttempt(ctx context.Context, agentID, attWallet string, ceiling ledger.Credits) (ledger.Credits, error) {
	bal, err := o.Ledger.Balance(ctx, agentID)
	if err != nil {
		return 0, err
	}
	budget := ceiling
	if budget <= 0 || budget > bal {
		budget = bal
	}
	if err := o.Ledger.CreateAccount(ctx, attWallet, ledger.KindAgent, agentID); err != nil {
		return 0, err
	}
	if budget > 0 {
		if _, err := o.Ledger.Transfer(ctx, agentID, attWallet, budget, "attempt budget", attWallet); err != nil {
			return 0, err
		}
	}
	return budget, nil
}

// performAttempt runs the winner's one step and extracts a submission if there
// is one. A returned error means the attempt could not even be described
// (marshalling failed) — the episode is broken, not the agent.
func (o *Orchestrator) performAttempt(ctx context.Context, ag *Agent, b *bounty.Bounty, attWallet string, round int, budget ledger.Credits) (attemptOutcome, error) {
	token := "tok-" + attWallet
	o.Proxy.Authorize(token, attWallet)
	defer o.Proxy.Revoke(token)
	o.faults.reset(attWallet)

	// The memo is keyed by the agent, not by attWallet — unlike the faults
	// just above it, which belong to the wallet the calls are billed to. An
	// attempt wallet is minted for this bounty and retired at the end of it;
	// hanging the agent's memory off it would erase that memory once per win.
	input, err := json.Marshal(StepInput{
		Observation: Observation{Phase: PhaseAttempt, Round: round, Memo: o.memos.get(ag.ID), Task: &TaskView{
			BountyID: b.ID, Tier: b.Tier, Prompt: b.Prompt,
			AskedPrice: b.AskedPrice, Budget: budget, WallClockSec: b.WallClockSec,
			Judged: b.Judged,
		}},
		Wallet: WalletView{ID: attWallet, Balance: budget},
	})
	if err != nil {
		return attemptOutcome{}, err
	}
	timeout := o.Cfg.StepTimeout
	if b.WallClockSec > 0 {
		timeout = time.Duration(b.WallClockSec) * time.Second
	}

	res, runErr := o.Steps.RunStep(ctx, StepRequest{
		AgentID: ag.ID,
		Name:    fmt.Sprintf("%s-%s-r%d", ag.ID, b.ID, round),
		Token:   token,
		Input:   input,
		Timeout: timeout,
	})

	var out attemptOutcome
	switch {
	case runErr != nil:
		out.runErr = runErr
	case res.TimedOut:
		out.agentFault = "wall clock exceeded"
	case res.ExitCode != 0:
		out.agentFault = fmt.Sprintf("container exited %d", res.ExitCode)
	default:
		if actions, perr := ParseActions(res.Stdout); perr != nil {
			out.agentFault = perr.Error()
		} else {
			// Filed before the submission is looked at, and kept regardless of
			// how the attempt goes. What an agent learns from losing is worth
			// at least as much as what it learns from winning, and charging it
			// for the failure while also erasing the note about it would be
			// two punishments for one mistake.
			o.takeMemo(ag.ID, actions)
			for _, a := range actions {
				if a.Type == ActionSubmit && (a.Bounty == "" || a.Bounty == b.ID) {
					out.submitted, out.attempted = a.Answer, true
				}
			}
			if !out.attempted {
				out.agentFault = "no submit action in step output"
			}
		}
	}

	// Grading happens here, on the worker, and only for a submission that
	// exists: an agent that crashed or never submitted has already failed on
	// its own terms, and asking a model about it would spend the platform's
	// tokens to confirm what the step already showed.
	if b.Judged && out.attempted {
		v, err := o.judgeSubmission(ctx, b, round, out.submitted)
		out.verdict, out.judged, out.judgeErr = v, err == nil, err
	}
	return out, nil
}

// settleAttempt settles every consequence of a finished attempt: the spend,
// the board, the money, the ladder, bankruptcy.
func (o *Orchestrator) settleAttempt(ctx context.Context, ag *Agent, b *bounty.Bounty, attWallet string, budget ledger.Credits, out attemptOutcome) error {
	askedPrice := b.AskedPrice

	// The attempt's spend is already settled: the proxy resolves every hold
	// inline, before the step returns.
	after, err := o.Ledger.Balance(ctx, attWallet)
	if err != nil {
		return err
	}
	burned := budget - after
	// Two notions of correct, kept apart at the source: a key comparison the
	// board performs itself, or a verdict it is handed. Verify always answers
	// false for a judged bounty, so this is a choice, not a fallback.
	solved := out.attempted && b.Verify(out.submitted)
	if b.Judged {
		solved = out.attempted && out.judged && out.verdict.Pass
	}

	// Merge the attempt wallet back before any payout: the remainder returns
	// to the head, and earnings land in the bankroll, not the attempt purse.
	if after > 0 {
		if _, err := o.Ledger.Transfer(ctx, attWallet, ag.ID, after, "attempt settlement", attWallet); err != nil {
			return err
		}
	}
	if err := o.Ledger.Retire(ctx, attWallet); err != nil {
		// A stranded open hold (proxy died mid-settle) blocks retirement; the
		// credits in it are out of play but conserved. Log, don't halt.
		o.Log.Warn("attempt wallet not retired", "wallet", attWallet, "err", err)
	}

	// The attribution table — plan verification #7. Success always stands: a
	// platform fault excuses a failure, it never negates a solve.
	//
	//   correct answer                       → solved; paid the asked price
	//   runner error, or a proxy-recorded
	//   platform fault, on a failed attempt  → refunded and voided; no failure
	//   timeout, crash, bad output, no
	//   submission, wrong answer             → agent fault; charged and failed
	platformFault := out.runErr != nil || out.judgeErr != nil || o.faults.sawFault(attWallet)
	// The attempt wallet is retired; its fault flag goes with it. Attempt
	// wallets are unique per bounty per round, so keeping them would grow the
	// map for the life of the process.
	o.faults.reset(attWallet)
	// The verdict goes in the trace whether it passed or failed, and before the
	// outcome that followed from it: a judged bounty's whole audit trail is the
	// grader's own words, so a spectator who disagrees can see exactly what was
	// said and by which model.
	if out.judged {
		o.traceEvent(trace.EventBounty, map[string]any{
			"action": "judged", "id": b.ID, "agent": ag.ID,
			"pass": out.verdict.Pass, "reason": out.verdict.Reason, "grader": out.verdict.Grader,
		})
	}
	switch {
	case solved:
		if err := o.resolveOutcome(b, out, true); err != nil {
			return err
		}
		// The payout is the asked price — what the agent bid, not the posted
		// maximum. Underbidding the field is how you win; it is also how you
		// earn less than the bounty could have paid.
		if _, err := o.Ledger.Mint(ctx, ag.ID, askedPrice, "bounty payout", "payout:"+b.ID); err != nil {
			return err
		}
		o.traceEvent(trace.EventCredit, map[string]any{"action": "payout", "agent": ag.ID, "bounty": b.ID, "amount": askedPrice})
		o.traceEvent(trace.EventBounty, map[string]any{"action": "solved", "id": b.ID, "agent": ag.ID, "payout": askedPrice, "burned": burned})
		o.record(rating.Attempt{Agent: ag.ID, Bounty: b.ID, Tier: b.Tier, Earned: askedPrice, Burned: burned, Success: true, Time: time.Now()})

	case platformFault:
		if burned > 0 {
			// Refund what the fault-stricken attempt had already settled:
			// out of the provider pot, back to the bankroll.
			if _, err := o.Ledger.Transfer(ctx, ledger.AcctProvider, ag.ID, burned, "refund: platform fault", "refund:"+b.ID); err != nil {
				return err
			}
			o.traceEvent(trace.EventCredit, map[string]any{"action": "refund", "agent": ag.ID, "bounty": b.ID, "amount": burned})
		}
		if err := o.Board.Void(b.ID); err != nil {
			return err
		}
		reason := "platform fault at the proxy"
		switch {
		case out.runErr != nil:
			reason = "step runner error: " + out.runErr.Error()
		case out.judgeErr != nil:
			reason = "judge error: " + out.judgeErr.Error()
		}
		o.traceEvent(trace.EventBounty, map[string]any{"action": "voided", "id": b.ID, "agent": ag.ID, "reason": reason})
		// No ladder record: a voided attempt never happened for the ranking.
		return nil

	default:
		if err := o.resolveOutcome(b, out, false); err != nil {
			return err
		}
		reason := out.agentFault
		if reason == "" {
			reason = "wrong answer"
			if b.Judged {
				reason = "judged: " + out.verdict.Reason
			}
		}
		o.traceEvent(trace.EventBounty, map[string]any{"action": "failed", "id": b.ID, "agent": ag.ID, "reason": reason, "burned": burned})
		o.record(rating.Attempt{Agent: ag.ID, Bounty: b.ID, Tier: b.Tier, Burned: burned, Success: false, Time: time.Now()})
	}

	return o.settleAgent(ctx, ag)
}

// resolveOutcome closes the bounty by whichever kind of truth it has. Keyed
// bounties re-verify at the board — the board owns the key and decides for
// itself, so a caller cannot talk it into a payout — while judged ones hand
// over the verdict the grader already returned.
func (o *Orchestrator) resolveOutcome(b *bounty.Bounty, out attemptOutcome, solved bool) error {
	if b.Judged {
		return o.Board.ResolveJudged(b.ID, solved)
	}
	_, err := o.Board.Resolve(b.ID, out.submitted, out.attempted)
	return err
}

// settleAgent applies the bankruptcy rule: at or below the dust threshold the
// remainder is burned and the wallet retired. Retirement is permanent — the
// closed account refuses all future postings, and AddAgent refuses the ID.
func (o *Orchestrator) settleAgent(ctx context.Context, ag *Agent) error {
	bal, err := o.Ledger.Balance(ctx, ag.ID)
	if err != nil {
		return err
	}
	if bal > o.Cfg.Dust {
		return nil
	}
	if bal > 0 {
		if _, err := o.Ledger.Burn(ctx, ag.ID, bal, "bankruptcy dust", "bankrupt:"+ag.ID); err != nil {
			return err
		}
		o.traceEvent(trace.EventCredit, map[string]any{"action": "dust_burn", "agent": ag.ID, "amount": bal})
	}
	if err := o.Ledger.Retire(ctx, ag.ID); err != nil {
		return err
	}
	ag.Retired = true
	o.traceEvent(trace.EventAgent, map[string]any{"action": "bankrupt", "agent": ag.ID})
	o.Log.Info("agent bankrupt", "agent", ag.ID)
	return nil
}
