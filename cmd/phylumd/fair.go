// The fair mode: the composition. The sim proved the economy runs on a clock;
// the town proved bodies keep schedules on a map; the fair runs both at once
// and couples them through exactly one seam, town.Config.Visit. The cast are
// lodgers at the tavern with daily rounds of their own, the office posts
// bounties on the hour whether anyone is there or not, and only an agent
// standing at the office is shown the board. Everything money stays in the
// orchestrator; everything bodily stays in the town; this file is the plug.
//
// Still all on the stub, still zero spend: same rule as the town, stated once
// more because this mode is the first with both a ledger and a map, and it
// would be the tempting place to blur that line.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/metahumansworld/soscitea/internal/auction"
	"github.com/metahumansworld/soscitea/internal/bounty"
	"github.com/metahumansworld/soscitea/internal/judge"
	"github.com/metahumansworld/soscitea/internal/ledger"
	"github.com/metahumansworld/soscitea/internal/orchestrator"
	"github.com/metahumansworld/soscitea/internal/proxy"
	"github.com/metahumansworld/soscitea/internal/service"
	"github.com/metahumansworld/soscitea/internal/town"
	"github.com/metahumansworld/soscitea/internal/trace"
)

// fairPostMinutes are the office's posting hours: on the hour, nine to four.
// Deliberately human hours — the gambler sleeps through two of them, and the
// one o'clock posting opens to a room everyone has left for lunch.
var fairPostMinutes = []int{540, 600, 660, 720, 780, 840, 900, 960}

func runFair(ctx context.Context, log *slog.Logger, l *ledger.Ledger, board *bounty.Board,
	tw *trace.Writer, notes []orchestrator.SuiteNote, opt options) error {

	m, people := town.AshmereFair()

	fmt.Printf("fair: %s — the arena's cast takes lodgings at the tavern\n", m.Name)
	w, err := newOffline(ctx, log, l, board, tw, nil, notes, opt)
	if err != nil {
		return err
	}
	defer w.stop()

	// Judged briefs are legal here for the sim's reason: no ladder, so
	// nothing a grader says can touch a score.
	if err := w.orch.EnableJudging(ctx, &judge.HTTP{
		Base: w.proxyURL, Model: "stub-1", MaxTokens: 64,
	}, judgeEndowment); err != nil {
		return fmt.Errorf("enable judging: %w", err)
	}
	fmt.Printf("  + %-8s %5d credits — grades the open-ended briefs, spends its own money\n",
		"judge", judgeEndowment)

	// Guest intake. The flag brought the trader; town.Guest brings the body.
	// A guest's file runs exactly the way the cast's do — python3, the SDK on
	// the path, every model call through the metering proxy — so the only
	// difference between a guest and the cast is who wrote it.
	taken := map[string]bool{"judge": true}
	for _, a := range cast {
		taken[a.id] = true
	}
	for _, p := range people {
		taken[p.ID] = true
	}
	guests, err := guestRoster(opt.guests, taken)
	if err != nil {
		return err
	}
	sdkDir, err := filepath.Abs(filepath.Join("sdk", "python"))
	if err != nil {
		return err
	}
	for _, g := range guests {
		w.steps.Register(g.id, orchestrator.ProcessAgent{
			Cmd: []string{"python3", g.path},
			// The guest's own directory joins the path so it can split
			// itself into modules; the SDK joins it because the SDK is
			// the platform's half of the bargain.
			Env: map[string]string{"PYTHONPATH": sdkDir + ":" + filepath.Dir(g.path)},
		})
		if err := w.orch.AddAgent(ctx, g.id, guestGrant); err != nil {
			return err
		}
		name := strings.ToUpper(g.id[:1]) + g.id[1:]
		people = append(people, town.Guest(g.id, name,
			"a guest at the Bell & Bushel; not of the cast — brought to the fair by its author"))
		fmt.Printf("  + %-8s %5d credits — a guest, come for the board (yours: %s)\n",
			g.id, guestGrant, filepath.Base(g.path))
	}

	// Lodger intake. The flag brought the spec; the service brings the
	// agent, the same service that would answer for it on the builder page,
	// built here over the fair's own proxy so every call it makes lands in
	// the fair's trace and books. The lodger's owner is the lodger: its
	// wallet is the fair wallet the grant went to, so there is no second
	// purse for the board to miss — and the step token, not the owner's,
	// pays each call, so an attempt's spend stays inside the attempt.
	lodgers, err := lodgerRoster(opt.lodgers, taken)
	if err != nil {
		return err
	}
	if len(lodgers) > 0 {
		svc, err := service.New(service.Config{Proxy: w.orch.Proxy, Ledger: l, Log: log})
		if err != nil {
			return err
		}
		ls := &lodgerSteps{svc: svc, ids: map[string]string{}, other: w.orch.Steps}
		for _, ld := range lodgers {
			// The spec names the model its author chose; offline that
			// model is the stub under the author's name for it, priced as
			// the stub is, the rule serve keeps for a locked catalogue.
			if _, ok := w.orch.Proxy.Table.Lookup(ld.spec.Model); !ok {
				w.orch.Proxy.Table.Set(ld.spec.Model, proxy.Price{InputPerTok: 1000, OutputPerTok: 1000})
			}
			if err := w.orch.AddAgent(ctx, ld.id, guestGrant); err != nil {
				return err
			}
			ag, err := svc.Create(ctx, service.Owner{ID: ld.id, Wallet: ld.id}, ld.spec)
			if err != nil {
				return fmt.Errorf("lodger %s: %w", ld.id, err)
			}
			ls.ids[ld.id] = ag.ID
			name := strings.ToUpper(ld.id[:1]) + ld.id[1:]
			people = append(people, town.Guest(ld.id, name,
				"a lodger at the Bell & Bushel; not of the cast — built on the builder's page and brought to the fair by its owner"))
			fmt.Printf("  + %-8s %5d credits — a lodger, come for the board (yours: %s, on %s)\n",
				ld.id, guestGrant, ld.spec.Name, ld.spec.Model)
		}
		w.orch.Steps = ls
	}

	// One card per posting hour per day: the deck is sized by the calendar,
	// not by a flag, because the office cannot post more often than it opens.
	deck := simDeck(opt.seed, len(fairPostMinutes)*opt.days, notes)
	fcfg := orchestrator.FairConfig{
		Deck:        deck,
		PostMinutes: fairPostMinutes,
		WindowTicks: 3, // 30 simulated minutes to bid
		MaxReopens:  3,
		Office:      "office",
		Notebook:    opt.notebook,
	}
	if opt.tiebreak == "lot" {
		// The lot's salt is the episode seed: one number already governs
		// the deck, so the same -seed replays the same draws, and a
		// different seed is a different day in every sense at once.
		fcfg.Tie = auction.ByLot
		fcfg.LotSalt = uint64(opt.seed)
	}
	fair, err := orchestrator.NewFair(ctx, w.orch, fcfg)
	if err != nil {
		return err
	}

	fmt.Printf("\nwatch it live: phylumctl serve -follow %s 127.0.0.1:8143\n", tw.Path())
	fmt.Printf("seed %d, %d bounties posted on the hour 09:00–16:00, 30-minute bid windows — the office opens\n\n",
		opt.seed, len(deck))

	rep, err := town.Run(ctx, tw, m, people, town.Config{
		TickMinutes: 10,
		Interval:    opt.tick,
		Days:        opt.days,
		StartMinute: 7 * 60,
		// The stub, and only the stub — the town's rule, unchanged by the
		// money next door.
		Mind: &town.Minds{Provider: &proxy.StubProvider{}},
		// The two halves of the same seam: Visit tells the fair where
		// everyone is standing, Hold lets the fair keep one of them there.
		// Neither carries money in either direction.
		Visit: fair.Visit,
		Hold:  fair.Hold,
	})
	if err != nil {
		return fmt.Errorf("fair: %w", err)
	}
	frep, err := fair.Close()
	if err != nil {
		return err
	}

	fmt.Printf("── the fair, after %d ticks (%s) ─────────────────────────────────────\n",
		frep.Ticks, rep.Reason)
	fmt.Printf("%-9s %8s %9s %8s %8s  %s\n",
		"agent", "attempts", "successes", "earned", "burned", "fate")
	for _, st := range frep.Standings {
		fate := fmt.Sprintf("alive, %d credits", st.Balance)
		if st.Retired {
			fate = "☠ bankrupt"
		}
		if len(st.Owned) > 0 {
			fate += ", owns " + strings.Join(st.Owned, ", ")
		}
		fmt.Printf("%-9s %8d %9d %8d %8d  %s\n",
			st.Agent, st.Attempts, st.Solved, st.Earned, st.Burned, fate)
	}
	fmt.Printf("─────────────────────────────────────────────────────────────────────\n")
	fmt.Printf("%d of %d posted, %d shelved for want of anyone at the board · unranked by design\n",
		frep.Posted, len(deck), frep.Shelved)
	fmt.Printf("%d meetings, %d lines said, %d evening reflections — the town went on being a town\n",
		rep.Meetings, rep.Utterances, rep.Thoughts)
	fmt.Printf("conservation: %s\n", frep.Conservation)
	fmt.Printf("\ntrace: %s (replayable; inspect with phylumctl)\n", tw.Path())
	return nil
}
