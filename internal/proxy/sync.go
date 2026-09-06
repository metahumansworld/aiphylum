package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
)

// SyncPrices reads OpenRouter's public catalogue and sets each named model's
// price on the table. It is one GET of a keyless listing: what a model costs
// is public, only calling it is not. Models the listing does not price are
// returned as missing and left as they were, so a hand-set price survives a
// model that has gone away, and the caller decides what a missing one means.
//
// The listing prices in USD per token as decimal strings; the table keeps
// nano-USD per token. A negative price is OpenRouter's "varies" and counts
// as unpriced.
func SyncPrices(ctx context.Context, client *http.Client, base string, table *PriceTable, models []string) (missing []string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openrouter catalogue: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openrouter catalogue: %s", res.Status)
	}
	var listing struct {
		Data []struct {
			ID      string `json:"id"`
			Pricing struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(&listing); err != nil {
		return nil, fmt.Errorf("openrouter catalogue: %w", err)
	}
	priced := map[string]Price{}
	for _, m := range listing.Data {
		in, err1 := nanoPerToken(m.Pricing.Prompt)
		out, err2 := nanoPerToken(m.Pricing.Completion)
		if err1 == nil && err2 == nil {
			priced[m.ID] = Price{InputPerTok: in, OutputPerTok: out}
		}
	}
	for _, id := range models {
		p, ok := priced[id]
		if !ok {
			missing = append(missing, id)
			continue
		}
		table.Set(id, p)
	}
	return missing, nil
}

// nanoPerToken turns a USD-per-token decimal string into nano-USD. The
// rounding is exact at catalogue magnitudes: prices are quoted to at most
// nine decimal places of a dollar, which is one nano-USD.
func nanoPerToken(s string) (int64, error) {
	usd, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	if usd < 0 || math.IsNaN(usd) || math.IsInf(usd, 0) {
		return 0, fmt.Errorf("unpriced: %s", s)
	}
	return int64(math.Round(usd * 1e9)), nil
}
