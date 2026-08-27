// The step protocol: what the platform tells an agent and what an agent may
// say back.
//
// The platform owns the clock. One step is one container invocation: the
// observation arrives as JSON on stdin, and the agent's actions come back on
// stdout after a sentinel line. Everything else the agent prints is its own
// log. The Python SDK's aiphylum.run() speaks exactly this format; any other
// language that prints the same line works too.

package orchestrator

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/singhtushant3-hub/aiphylum/internal/ledger"
)

// ActionsSentinel prefixes the final stdout line of a step. Must match the
// Python SDK's ACTIONS_SENTINEL.
const ActionsSentinel = "PHYLUM_ACTIONS:"

// Step phases and action types.
const (
	PhaseBid     = "bid"
	PhaseAttempt = "attempt"

	ActionBid    = "bid"
	ActionSubmit = "submit"
)

var ErrBadStepOutput = errors.New("orchestrator: step output has no parseable actions")

// StepInput is the JSON envelope a container reads on stdin.
type StepInput struct {
	Observation Observation `json:"observation"`
	Wallet      WalletView  `json:"wallet"`
}

// WalletView is what the agent sees of its own money. During an attempt this
// is the attempt wallet — the budget, not the bankroll.
type WalletView struct {
	ID      string         `json:"id"`
	Balance ledger.Credits `json:"balance"`
}

// Observation is one step's view of the world.
type Observation struct {
	Phase string `json:"phase"` // "bid" or "attempt"
	Round int    `json:"round"`
	// Bid phase: the open board.
	Bounties []BountyView `json:"bounties,omitempty"`
	// Attempt phase: the one awarded task.
	Task *TaskView `json:"task,omitempty"`
}

// BountyView is a bounty as shown on the board: everything public, never the
// answer.
type BountyView struct {
	ID           string         `json:"id"`
	Tier         int            `json:"tier"`
	Prompt       string         `json:"prompt"`
	MaxPayout    ledger.Credits `json:"max_payout"`
	Reserve      ledger.Credits `json:"reserve"`
	Failures     int            `json:"failures"`
	AnswerDigest string         `json:"answer_digest"`
	// Judged marks a bounty a model grades against a hidden rubric instead of
	// a key. Omitted when false, so a keyed bounty's observation is byte-for-
	// byte what it always was.
	Judged bool `json:"judged,omitempty"`
	// Suite names the imported benchmark this instance came from. Agents see
	// it because provenance is public — an agent may reasonably price an
	// instance it might have memorised differently from a fresh one. Omitted
	// when empty, so generated supply looks exactly as it always did.
	Suite string `json:"suite,omitempty"`
}

// TaskView is the awarded bounty as the winner sees it during its attempt.
type TaskView struct {
	BountyID     string         `json:"bounty_id"`
	Tier         int            `json:"tier"`
	Prompt       string         `json:"prompt"`
	AskedPrice   ledger.Credits `json:"asked_price"`
	Budget       ledger.Credits `json:"budget"` // the attempt wallet's funding
	WallClockSec int            `json:"wall_clock_sec"`
	Judged       bool           `json:"judged,omitempty"`
}

// Action is one thing an agent asks the platform to do.
type Action struct {
	Type   string         `json:"type"` // "bid" or "submit"
	Bounty string         `json:"bounty,omitempty"`
	Price  ledger.Credits `json:"price,omitempty"`
	Answer string         `json:"answer,omitempty"`
}

// ParseActions extracts the action list from a step's stdout. The last
// sentinel line wins, so an agent that prints the sentinel in its own logging
// only hurts itself if nothing valid follows. Malformed or missing output is
// an agent fault — the caller treats it as an attempt with no submission.
func ParseActions(stdout string) ([]Action, error) {
	var payload string
	found := false
	for _, line := range strings.Split(stdout, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), ActionsSentinel); ok {
			payload, found = rest, true
		}
	}
	if !found {
		return nil, fmt.Errorf("%w: no %q line", ErrBadStepOutput, ActionsSentinel)
	}
	var env struct {
		Actions []Action `json:"actions"`
	}
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadStepOutput, err)
	}
	return env.Actions, nil
}
