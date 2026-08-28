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
	ActionStay   = "stay"
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
	// Place is where the agent is standing and StayPrice is what one more
	// tick of standing there costs it. Both belong to the fair, where being
	// somewhere is the whole coupling; both are omitted everywhere else, so
	// the ranked loop's and the sim's observations are byte-for-byte what
	// they have always been. An agent that is shown no price cannot buy.
	Place     string         `json:"place,omitempty"`
	StayPrice ledger.Credits `json:"stay_price,omitempty"`
	// StayTicksLeft is how much standing the agent has already paid for and
	// not yet used. It is here because a step is a fresh process with no
	// memory of the last one: without being told, an agent cannot know it is
	// already held, and would buy the same ticks over and over at a cost it
	// had no way to see. A price you cannot avoid paying twice is not a
	// price, so the platform says what you already own — the same reason it
	// tells you your balance rather than making you remember it.
	StayTicksLeft int `json:"stay_ticks_left,omitempty"`
}

// StayOffer is the fair's addition to a bid step: where the agent is standing
// and what a tick of standing there longer costs. Nil on every other track —
// the ranked loop and the sim have no geography to linger in — and nil is
// what keeps their observations unchanged.
type StayOffer struct {
	Place     string
	Price     ledger.Credits
	TicksLeft int
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
	Type   string         `json:"type"` // "bid", "submit" or "stay"
	Bounty string         `json:"bounty,omitempty"`
	Price  ledger.Credits `json:"price,omitempty"`
	Answer string         `json:"answer,omitempty"`
	// Ticks is how many more ticks a "stay" buys. The platform charges for
	// all of them up front and holds the agent where it stands; it is the
	// only action here that costs money to ask for rather than to win.
	Ticks int `json:"ticks,omitempty"`
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
