// Deriving an episode plan from the board's generator registry. The daemon
// does not hard-code supply the way the demo does: whatever generators the
// operator loaded are what the episodes post from, in sorted-name order, with
// tiers cycling so no generator is pinned to one difficulty. Same seed, same
// plan — derived episodes replay like scripted ones.
package daemon

import (
	"fmt"
	"strings"

	"github.com/metahumansworld/soscitea/internal/bounty"
	"github.com/metahumansworld/soscitea/internal/ledger"
	"github.com/metahumansworld/soscitea/internal/orchestrator"
)

// derivePlan builds the posting plan for one episode: one posting per usable
// generator per round. Judged generators are excluded up front — the live
// daemon is a ranked world, and postBounty would halt the episode over one —
// as are generators that cannot produce a task at all. The returned note says
// what was used and what was skipped, for the operator's log.
func derivePlan(board *bounty.Board, req EpisodeRequest) (orchestrator.Episode, string) {
	var used, skipped []string
	for _, name := range board.Generators() {
		task, err := board.Probe(name, req.Seed, 1)
		switch {
		case err != nil:
			skipped = append(skipped, name+" (probe: "+err.Error()+")")
		case task.Rubric != "":
			skipped = append(skipped, name+" (judged; sim-only)")
		default:
			used = append(used, name)
		}
	}

	note := "generators: " + strings.Join(used, ", ")
	if len(skipped) > 0 {
		note += " — skipped " + strings.Join(skipped, ", ")
	}
	if len(used) == 0 {
		return orchestrator.Episode{}, fmt.Sprintf("skipped %s", strings.Join(skipped, ", "))
	}

	ep := orchestrator.Episode{}
	for r := 0; r < req.Rounds; r++ {
		base := req.Seed*100_000 + int64(r)*1_000
		round := make([]orchestrator.Posting, 0, len(used))
		for i, g := range used {
			round = append(round, orchestrator.Posting{
				Generator: g,
				Seed:      base + int64(i),
				// Offset per generator and per round: every generator walks
				// the tiers, and no round is all one difficulty.
				Tier:         (r+i)%3 + 1,
				TokenCeiling: ledger.Credits(req.TokenCeiling),
				WallClockSec: req.WallClockSec,
			})
		}
		ep.Rounds = append(ep.Rounds, round)
	}
	return ep, note
}

func countPostings(ep orchestrator.Episode) int {
	n := 0
	for _, r := range ep.Rounds {
		n += len(r)
	}
	return n
}
