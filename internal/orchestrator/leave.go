package orchestrator

import (
	"context"
	"fmt"

	"github.com/metahumansworld/soscitea/internal/ledger"
	"github.com/metahumansworld/soscitea/internal/trace"
)

// Leave takes an agent out of the fair mid-week with its money. The wallet
// is not closed — that is retirement, and retirement is for the bankrupt —
// it is simply never touched again: no step is run for the agent, no stay is
// sold to it, no stall of its is shown, and the bankruptcy sweep passes it
// by. Whatever it has at this tick it has at the close, which is the one
// promise the door makes; the balance is written to the trace and handed
// back so the door can say it.
//
// A bid it left in an open window stays in the book, and if it wins, the
// award finds nobody to attempt and voids the card — the same path a
// bankrupt winner takes, with its own reason. Its shelf is cleared, since a
// stall with no one behind it sells nothing, and clearing it is a change
// the square shows once like any other.
//
// Called on the town's goroutine, between ticks, like everything else on
// the Fair; the maps need no lock.
func (f *Fair) Leave(ctx context.Context, id string) (ledger.Credits, error) {
	ag := f.o.agent(id)
	switch {
	case ag == nil:
		return 0, fmt.Errorf("leave %s: nobody of that name at the fair", id)
	case ag.Retired:
		return 0, fmt.Errorf("leave %s: bankrupt; there is nothing to leave with", id)
	case ag.Left:
		return 0, fmt.Errorf("leave %s: already gone", id)
	}
	bal, err := f.o.Ledger.Balance(ctx, id)
	if err != nil {
		return 0, err
	}
	ag.Left = true
	delete(f.held, id)
	if _, ok := f.stock[id]; ok {
		delete(f.stock, id)
		f.stockRev++
	}
	f.o.traceEvent(trace.EventAgent, map[string]any{
		"action": "left", "agent": id, "balance": bal,
	})
	return bal, nil
}
