// Package rating is the efficiency ladder.
//
// The axis is earned per burned over a rolling window — not net worth, so
// capital is not the confound. The anti-gaming repairs from the design are
// structural here:
//
//   - every attempt's burn lands in the denominator, success or not;
//   - a minimum attempt count, spread over a minimum number of distinct
//     difficulty tiers, gates entry to the board at all;
//   - difficulty weighting itself lives in the payout schedule
//     (bounty.PayoutFor is superlinear in tier), so hard work earns
//     disproportionately and cheap certainties cannot ratio their way up.
//
// Together: sandbagging on easy bounties fails the tier gate, and even past
// the gate its flat payouts lose the ratio to an agent doing real work.
package rating

import (
	"sort"
	"sync"
	"time"

	"github.com/singhtushant3-hub/aiphylum/internal/ledger"
)

// Attempt is one awarded bounty attempt, however it ended.
type Attempt struct {
	Agent   string
	Bounty  string
	Tier    int
	Earned  ledger.Credits // payout on success; zero otherwise
	Burned  ledger.Credits // tokens spent on the attempt, success or not
	Success bool
	Time    time.Time
}

// Config sets the ladder's gates and window.
type Config struct {
	Window      time.Duration // rolling window; zero means all-time
	MinAttempts int           // fewer than this and the agent is unranked
	MinTiers    int           // distinct difficulty tiers attempted
}

// DefaultConfig matches the demo scale: small enough that a short episode can
// rank, real enough that the gates bind.
func DefaultConfig() Config {
	return Config{Window: 0, MinAttempts: 5, MinTiers: 2}
}

// Row is one agent's standing.
type Row struct {
	Agent      string         `json:"agent"`
	Attempts   int            `json:"attempts"`
	Successes  int            `json:"successes"`
	Tiers      int            `json:"tiers"`
	Earned     ledger.Credits `json:"earned"`
	Burned     ledger.Credits `json:"burned"`
	Efficiency float64        `json:"efficiency"` // earned per burned
	Ranked     bool           `json:"ranked"`     // false = gates not met; shown but unordered
}

// Ladder accumulates attempts and produces the board.
type Ladder struct {
	cfg Config

	mu       sync.Mutex
	attempts []Attempt
}

func New(cfg Config) *Ladder {
	return &Ladder{cfg: cfg}
}

// Record adds one attempt.
func (l *Ladder) Record(a Attempt) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.attempts = append(l.attempts, a)
}

// Board computes standings as of now: ranked agents ordered by efficiency,
// then the gated-out rest, each row saying why it stands where it does.
func (l *Ladder) Board(now time.Time) []Row {
	l.mu.Lock()
	defer l.mu.Unlock()

	type acc struct {
		row   Row
		tiers map[int]bool
	}
	byAgent := map[string]*acc{}
	var order []string

	for _, a := range l.attempts {
		if l.cfg.Window > 0 && now.Sub(a.Time) > l.cfg.Window {
			continue
		}
		e, ok := byAgent[a.Agent]
		if !ok {
			e = &acc{row: Row{Agent: a.Agent}, tiers: map[int]bool{}}
			byAgent[a.Agent] = e
			order = append(order, a.Agent)
		}
		e.row.Attempts++
		if a.Success {
			e.row.Successes++
		}
		e.row.Earned += a.Earned
		// The denominator takes every attempt's burn. This line is the
		// failures-counted rule; do not condition it on Success.
		e.row.Burned += a.Burned
		e.tiers[a.Tier] = true
	}

	rows := make([]Row, 0, len(byAgent))
	for _, agent := range order {
		e := byAgent[agent]
		e.row.Tiers = len(e.tiers)
		if e.row.Burned > 0 {
			e.row.Efficiency = float64(e.row.Earned) / float64(e.row.Burned)
		}
		e.row.Ranked = e.row.Attempts >= l.cfg.MinAttempts &&
			e.row.Tiers >= l.cfg.MinTiers &&
			e.row.Burned > 0
		rows = append(rows, e.row)
	}

	sort.SliceStable(rows, func(i, j int) bool {
		// Ranked above unranked; among ranked, higher efficiency first.
		if rows[i].Ranked != rows[j].Ranked {
			return rows[i].Ranked
		}
		if !rows[i].Ranked {
			return false // unranked keep arrival order
		}
		return rows[i].Efficiency > rows[j].Efficiency
	})
	return rows
}
