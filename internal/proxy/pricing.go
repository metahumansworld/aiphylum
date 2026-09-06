package proxy

import (
	"fmt"
	"sort"

	"github.com/metahumansworld/aiphylum/internal/ledger"
)

// Price is the cost of one model in nano-USD per token. Nano rather than micro
// so that cheap models keep integer precision: $0.25 per million input tokens
// is 250 nano-USD per token, which would round to zero in micro.
type Price struct {
	InputPerTok  int64 // nano-USD per input token
	OutputPerTok int64 // nano-USD per output token
}

// Cost converts measured usage to credits (micro-USD), rounding up: the
// platform never undercharges by a fraction of a credit.
func (p Price) Cost(inputToks, outputToks int64) ledger.Credits {
	nano := inputToks*p.InputPerTok + outputToks*p.OutputPerTok
	return ledger.Credits((nano + 999) / 1000)
}

// PriceTable is the allowlist: a model absent from the table cannot be called
// at any price, which makes the open-allowlist policy a data change rather
// than a code change.
type PriceTable struct {
	models map[string]Price
}

func NewPriceTable() *PriceTable {
	return &PriceTable{models: map[string]Price{}}
}

// Set adds or updates a model's price.
func (t *PriceTable) Set(model string, p Price) {
	t.models[model] = p
}

// Lookup returns the price for a model, or ok=false if the model is not on
// the allowlist.
func (t *PriceTable) Lookup(model string) (Price, bool) {
	p, ok := t.models[model]
	return p, ok
}

// Models lists the allowlisted model names, sorted, for the agent-facing
// catalogue endpoint.
func (t *PriceTable) Models() []string {
	out := make([]string, 0, len(t.models))
	for m := range t.models {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// MaxCost is the worst case a single call could cost, used to size the hold
// taken before the provider is contacted. maxOutput is the call's output
// ceiling (from the request's max_tokens), which every provider enforces.
func (t *PriceTable) MaxCost(model string, inputToks, maxOutput int64) (ledger.Credits, error) {
	p, ok := t.Lookup(model)
	if !ok {
		return 0, fmt.Errorf("%w: %s", ErrModelNotAllowed, model)
	}
	return p.Cost(inputToks, maxOutput), nil
}
