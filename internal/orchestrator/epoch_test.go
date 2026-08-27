package orchestrator

import (
	"errors"
	"testing"

	"github.com/metahunmei/dungeon/internal/bounty"
	"github.com/metahunmei/dungeon/internal/ledger"
	"github.com/metahunmei/dungeon/internal/proxy"
)

// A bounty that fails in one episode stays on the board and re-enters auction
// in the next. If it is re-awarded at the same round number, the attempt
// wallet name repeats — and retired accounts are never recreated. NextEpoch
// exists to namespace attempt wallets past that; these tests pin both halves:
// without it the second episode aborts on the collision, with it the carried
// bounty is attempted and solved.

// failThenSolve scripts one agent that bids on everything and answers wrongly
// until *wrong is cleared.
func failThenSolve(wrong *bool) stepFunc {
	return script(bidAll(0.4), func(_ StepRequest, in StepInput) StepResult {
		ans := answerFrom(in.Observation.Task.Prompt)
		if *wrong {
			ans = "not-it"
		}
		return out(Action{Type: ActionSubmit, Bounty: in.Observation.Task.BountyID, Answer: ans})
	})
}

func TestAttemptWalletCollisionWithoutNextEpoch(t *testing.T) {
	w := newWorld(t, &proxy.StubProvider{}, nil, Config{})
	w.add(t, "solo", 1000)
	wrong := true
	w.steps.fns["solo"] = failThenSolve(&wrong)

	// Episode 1: b0001 is posted, awarded, failed — it re-opens on the board.
	if err := w.orch.RunEpisode(w.ctx, Episode{Rounds: [][]Posting{{post(1, 1)}}}); err != nil {
		t.Fatalf("episode 1: %v", err)
	}

	// Episode 2 without NextEpoch: b0001 re-enters auction, is re-awarded at
	// round 1, and its attempt wallet name collides with the retired one.
	wrong = false
	err := w.orch.RunEpisode(w.ctx, Episode{Rounds: [][]Posting{{post(2, 1)}}})
	if !errors.Is(err, ledger.ErrAccountExists) {
		t.Fatalf("expected the attempt-wallet collision, got: %v", err)
	}
}

func TestNextEpochCarriesFailedBountiesForward(t *testing.T) {
	w := newWorld(t, &proxy.StubProvider{}, nil, Config{})
	w.add(t, "solo", 1000)
	wrong := true
	w.steps.fns["solo"] = failThenSolve(&wrong)

	if err := w.orch.RunEpisode(w.ctx, Episode{Rounds: [][]Posting{{post(1, 1)}}}); err != nil {
		t.Fatalf("episode 1: %v", err)
	}
	if got := w.bounty(t, "b0001").State; got != bounty.StateOpen {
		t.Fatalf("b0001 after failed episode 1: %s, want open", got)
	}

	// Episode 2 with NextEpoch: the carried bounty is re-attempted under a
	// namespaced wallet and solved alongside the fresh posting.
	w.orch.NextEpoch()
	wrong = false
	if err := w.orch.RunEpisode(w.ctx, Episode{Rounds: [][]Posting{{post(2, 1)}}}); err != nil {
		t.Fatalf("episode 2: %v", err)
	}
	for _, id := range []string{"b0001", "b0002"} {
		if got := w.bounty(t, id).State; got != bounty.StateSolved {
			t.Fatalf("%s after episode 2: %s, want solved", id, got)
		}
	}
}
