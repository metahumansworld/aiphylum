// The town mode: milestone one of the living world. Residents with authored
// personas keep daily schedules on a small map, and the trace records who was
// where, and who ran into whom. Entirely offline — no ledger, no model, no
// spend — because in this pass the world only moves; it does not yet think.
package main

import (
	"context"
	"fmt"

	"github.com/metahunmei/dungeon/internal/town"
	"github.com/metahunmei/dungeon/internal/trace"
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
	fmt.Printf("\nwatch it live: dungeonctl serve -follow %s 127.0.0.1:8142\n", tw.Path())
	fmt.Printf("the day starts at 07:00 — %d day(s), ten minutes per tick, %s of wall clock each\n\n",
		opt.days, opt.tick)

	rep, err := town.Run(ctx, tw, m, people, town.Config{
		TickMinutes: 10,
		Interval:    opt.tick,
		Days:        opt.days,
		StartMinute: 7 * 60,
	})
	if err != nil {
		return fmt.Errorf("town: %w", err)
	}

	fmt.Printf("── %s, after %d ticks (%s) ──────────────────────────────────\n",
		m.Name, rep.Ticks, rep.Reason)
	fmt.Printf("%d meetings recorded — the hooks a memory stream will hang on\n", rep.Meetings)
	fmt.Printf("\ntrace: %s (replayable; inspect with dungeonctl)\n", tw.Path())
	return nil
}
