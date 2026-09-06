// The town mode: the living world. Residents with authored personas keep
// daily schedules on a small map, and the trace records who was where and who
// ran into whom. With -mind they also remember it, say something about it when
// they meet, and decide at the end of the day what the day was — on the
// offline stub, so the whole thing still runs without a key, a ledger, or a
// cent of spend. Pointing the town at a real model is a separate decision,
// deliberately not wired here.
package main

import (
	"context"
	"fmt"

	"github.com/metahumansworld/aiphylum/internal/proxy"
	"github.com/metahumansworld/aiphylum/internal/town"
	"github.com/metahumansworld/aiphylum/internal/trace"
)

func runTown(ctx context.Context, tw *trace.Writer, opt options) error {
	m, people := town.Ashmere()

	places := 0
	for _, p := range m.Places {
		if p.Kind != "street" { // streets are scenery, not destinations
			places++
		}
	}
	fmt.Printf("town: %s — %d places, %d residents\n", m.Name, places, len(people))
	for _, p := range people {
		fmt.Printf("  + %-8s %s\n", p.Name, p.Blurb)
	}
	fmt.Printf("\nwatch it live: phylumctl serve -follow %s 127.0.0.1:8142\n", tw.Path())
	fmt.Printf("the day starts at 07:00 — %d day(s), ten minutes per tick, %s of wall clock each\n\n",
		opt.days, opt.tick)

	cfg := town.Config{
		TickMinutes: 10,
		Interval:    opt.tick,
		Days:        opt.days,
		StartMinute: 7 * 60,
	}
	if opt.mind {
		// The stub, and only the stub: a thinking town spends nothing, and
		// there is deliberately no flag that changes that.
		cfg.Mind = &town.Minds{Provider: &proxy.StubProvider{}}
	}
	rep, err := town.Run(ctx, tw, m, people, cfg)
	if err != nil {
		return fmt.Errorf("town: %w", err)
	}

	fmt.Printf("── %s, after %d ticks (%s) ──────────────────────────────────\n",
		m.Name, rep.Ticks, rep.Reason)
	if opt.mind {
		fmt.Printf("%d meetings, %d lines said, %d evening reflections\n",
			rep.Meetings, rep.Utterances, rep.Thoughts)
		fmt.Printf("%d stub calls, %d tokens in, %d out — measured by the provider, charged to nobody\n",
			rep.Calls, rep.InToks, rep.OutToks)
	} else {
		fmt.Printf("%d meetings recorded — the hooks a memory stream will hang on\n", rep.Meetings)
	}
	fmt.Printf("\ntrace: %s (replayable; inspect with phylumctl)\n", tw.Path())
	return nil
}
