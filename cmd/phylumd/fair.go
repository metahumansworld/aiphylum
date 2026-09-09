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
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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
	tw *trace.Writer, notes []orchestrator.SuiteNote, opt options, cp *checkpoint) error {

	m, people := town.AshmereFair()

	if cp == nil {
		fmt.Printf("fair: %s — the arena's cast takes lodgings at the tavern\n", m.Name)
	} else {
		minute := 7*60 + cp.Tick*10
		fmt.Printf("fair: %s — picked up at tick %d (day %d, %s) from %s\n",
			m.Name, cp.Tick, minute/town.DayMinutes+1, town.HHMM(minute%town.DayMinutes), opt.checkpoint)
	}
	w, err := newOffline(ctx, log, l, board, tw, nil, notes, opt)
	if err != nil {
		return err
	}
	defer w.stop()

	// Judged briefs are legal here for the sim's reason: no ladder, so
	// nothing a grader says can touch a score.
	grader := &judge.HTTP{Base: w.proxyURL, Model: "stub-1", MaxTokens: 64}
	if cp != nil {
		if err := w.orch.ResumeJudging(ctx, grader); err != nil {
			return err
		}
	} else {
		if err := w.orch.EnableJudging(ctx, grader, judgeEndowment); err != nil {
			return fmt.Errorf("enable judging: %w", err)
		}
		fmt.Printf("  + %-8s %5d credits — grades the open-ended briefs, spends its own money\n",
			"judge", judgeEndowment)
	}

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
	sdkDir, err := filepath.Abs(filepath.Join("sdk", "python"))
	if err != nil {
		return err
	}
	// The process half of admission, on its own because a resumed guest
	// needs only this: its wallet is in the books and its body in the
	// state, and the checkpoint keeps the roster the door will be checked
	// against.
	var admitted []guestRecord
	register := func(g guest) {
		w.steps.Register(g.id, orchestrator.ProcessAgent{
			Cmd: []string{"python3", g.path},
			// The guest's own directory joins the path so it can split
			// itself into modules; the SDK joins it because the SDK is
			// the platform's half of the bargain.
			Env: map[string]string{"PYTHONPATH": sdkDir + ":" + filepath.Dir(g.path)},
		})
		admitted = append(admitted, guestRecord{ID: g.id, Path: g.path})
	}
	// admit is the one way in, at boot or mid-week: the process, the
	// wallet, the body. The same three whichever door a guest came through.
	admit := func(g guest, when string) (town.Persona, error) {
		register(g)
		if err := w.orch.AddAgent(ctx, g.id, guestGrant); err != nil {
			return town.Persona{}, err
		}
		name := strings.ToUpper(g.id[:1]) + g.id[1:]
		fmt.Printf("  + %-8s %5d credits — a guest, %s for the board (yours: %s)\n",
			g.id, guestGrant, when, filepath.Base(g.path))
		return town.Guest(g.id, name,
			"a guest at the Bell & Bushel; not of the cast — brought to the fair by its author"), nil
	}
	if cp != nil {
		for _, g := range cp.Guests {
			if taken[g.ID] {
				return fmt.Errorf("-resume: the checkpoint's guest %q is a name this world already has", g.ID)
			}
			if _, err := os.Stat(g.Path); err != nil {
				return fmt.Errorf("-resume: guest %s: %w", g.ID, err)
			}
			taken[g.ID] = true
			register(guest{id: g.ID, path: g.Path})
			fmt.Printf("  + %-8s still here — a guest, back at the board (yours: %s)\n", g.ID, filepath.Base(g.Path))
		}
	} else {
		guests, err := guestRoster(opt.guests, taken)
		if err != nil {
			return err
		}
		for _, g := range guests {
			p, err := admit(g, "come")
			if err != nil {
				return err
			}
			people = append(people, p)
		}
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
	// A week with no closing day has no calendar to size it by, so it deals
	// the same cards one at a time, for as long as it runs.
	endless := opt.days == 0
	var deck []orchestrator.Posting
	if !endless {
		deck = simDeck(opt.seed, len(fairPostMinutes)*opt.days, notes)
	}
	fcfg := orchestrator.FairConfig{
		Deck:        deck,
		Deal:        func(i int) orchestrator.Posting { return simCard(opt.seed, i, notes) },
		PostMinutes: fairPostMinutes,
		WindowTicks: 3, // 30 simulated minutes to bid
		MaxReopens:  3,
		Office:      "office",
		Notebook:    opt.notebook,
		Stall:       opt.stall,
		StallPlace:  "square",
		Pitches:     pitchesOn(m, "square"),
	}
	if opt.tiebreak == "lot" {
		// The lot's salt is the episode seed: one number already governs
		// the deck, so the same -seed replays the same draws, and a
		// different seed is a different day in every sense at once.
		fcfg.Tie = auction.ByLot
		fcfg.LotSalt = uint64(opt.seed)
	}
	var fair *orchestrator.Fair
	if cp != nil {
		fair, err = orchestrator.ResumeFair(ctx, w.orch, fcfg, cp.Fair)
	} else {
		fair, err = orchestrator.NewFair(ctx, w.orch, fcfg)
	}
	if err != nil {
		return err
	}

	// The checkpoint, if one was asked for: after every tick, the books
	// copied and then the record written, so the two are always of the
	// same boundary. The cost is one database copy per tick, which is why
	// it is a flag and not the default.
	var save func(town.State) error
	var from *town.State
	if cp != nil {
		from = &cp.Town
	}
	if opt.checkpoint != "" {
		save = func(st town.State) error {
			if err := l.Snapshot(ctx, booksOf(opt.checkpoint)); err != nil {
				return err
			}
			return writeCheckpoint(opt.checkpoint, checkpoint{
				Seed: opt.seed, Days: opt.days, Tiebreak: opt.tiebreak, Book: opt.book,
				Notebook: opt.notebook, Stall: opt.stall,
				Tick: st.Tick, Seq: tw.Seq(), Guests: admitted, Town: st, Fair: fair.Save(),
			})
		}
	}

	// The door, open for the whole week, both ways. Bound before the run so
	// a busy port is an error now and not a join that silently never
	// lands; a second fair on the same machine wants its own -listen. Each
	// knock is answered on the town's goroutine at the next tick: the
	// filename rules and the taken map are the same ones boot used, so
	// nothing about a guest depends on which door they came through.
	// Lodgers do not join this way — their service exists only when
	// -lodger was passed at boot — and only a guest leaves this way: the
	// cast, the residents and the lodgers are the week's, not their
	// author's.
	joins := make(chan joinReq)
	leaves := make(chan leaveReq)
	done := make(chan struct{})
	ln, err := net.Listen("tcp", opt.listen)
	if err != nil {
		return fmt.Errorf("the fair's door (-listen %s): %w", opt.listen, err)
	}
	api := &http.Server{Handler: doorHandler(joins, leaves, done)}
	go api.Serve(ln)
	defer api.Close()
	arrive := func(day, mod int, clock string) []town.Persona {
		var came []town.Persona
		for {
			select {
			case req := <-joins:
				rep := doorReply{Day: day, Clock: clock}
				var p town.Persona
				gs, err := guestRoster([]string{req.path}, taken)
				if err == nil {
					p, err = admit(gs[0], fmt.Sprintf("joined day %d at %s,", day, clock))
				}
				if err != nil {
					rep.Err = err.Error()
				} else {
					rep.ID = p.ID
					came = append(came, p)
				}
				req.reply <- rep
			default:
				return came
			}
		}
	}
	// depart is the way out: a name the door admitted, still at the fair.
	// The fair freezes the wallet and says what is in it; the record stops
	// naming the file, so a later -resume never looks for a body that is
	// gone; the name stays taken, because the wallet under it stays.
	depart := func(day, mod int, clock string) []string {
		var went []string
		for {
			select {
			case req := <-leaves:
				rep := doorReply{ID: req.id, Day: day, Clock: clock}
				i := slices.IndexFunc(admitted, func(g guestRecord) bool { return g.ID == req.id })
				if i < 0 {
					rep.Err = fmt.Sprintf("%s is not a guest; only a guest may leave", req.id)
				} else if bal, err := fair.Leave(ctx, req.id); err != nil {
					rep.Err = err.Error()
				} else {
					rep.Balance = bal
					admitted = slices.Delete(admitted, i, i+1)
					went = append(went, req.id)
					fmt.Printf("left day %d at %s, %s with %d credits\n", day, clock, req.id, bal)
				}
				req.reply <- rep
			default:
				return went
			}
		}
	}

	fmt.Printf("\nwatch it live: phylumctl serve -follow %s 127.0.0.1:8143\n", tw.Path())
	fmt.Printf("join it live:  phylumctl join <guest.py>   (the door is on %s)\n", opt.listen)
	fmt.Printf("leave it live: phylumctl leave <name>       (a guest goes with what it has)\n")
	if endless {
		fmt.Printf("seed %d, a bounty posted on the hour 09:00–16:00, 30-minute bid windows, no closing day — the office opens\n\n",
			opt.seed)
	} else {
		fmt.Printf("seed %d, %d bounties posted on the hour 09:00–16:00, 30-minute bid windows — the office opens\n\n",
			opt.seed, len(deck))
	}

	rep, err := town.Run(ctx, tw, m, people, town.Config{
		TickMinutes: 10,
		Interval:    opt.tick,
		Days:        opt.days,
		Endless:     endless,
		StartMinute: 7 * 60,
		// The stub, and only the stub — the town's rule, unchanged by the
		// money next door.
		Mind: &town.Minds{Provider: &proxy.StubProvider{}},
		// The two halves of the same seam: Visit tells the fair where
		// everyone is standing, Hold lets the fair keep one of them there.
		// Neither carries money in either direction.
		Visit: fair.Visit,
		Hold:  fair.Hold,
		// And the door, both ways: whoever knocked since the last tick.
		Arrive: arrive,
		Leave:  depart,
		// The world written down after each tick, and where to pick it
		// up from, when either was asked for.
		Checkpoint: save,
		From:       from,
	})
	close(done) // the week is over; a knock from here on is refused
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
		} else if st.Left {
			fate = fmt.Sprintf("left, %d credits", st.Balance)
		}
		if len(st.Owned) > 0 {
			fate += ", owns " + strings.Join(st.Owned, ", ")
		}
		fmt.Printf("%-9s %8d %9d %8d %8d  %s\n",
			st.Agent, st.Attempts, st.Solved, st.Earned, st.Burned, fate)
	}
	fmt.Printf("─────────────────────────────────────────────────────────────────────\n")
	if endless {
		fmt.Printf("%d posted, no closing day, %d shelved for want of anyone at the board · unranked by design\n",
			frep.Posted, frep.Shelved)
	} else {
		fmt.Printf("%d of %d posted, %d shelved for want of anyone at the board · unranked by design\n",
			frep.Posted, len(deck), frep.Shelved)
	}
	fmt.Printf("%d meetings, %d lines said, %d evening reflections — the town went on being a town\n",
		rep.Meetings, rep.Utterances, rep.Thoughts)
	fmt.Printf("conservation: %s\n", frep.Conservation)
	fmt.Printf("\ntrace: %s (replayable; inspect with phylumctl)\n", tw.Path())
	return nil
}

// pitchesOn is where bought stalls go: the south row of the place, west to
// east. The square and not the market, because a stall among the market's
// stalls reads as nothing built and a stall on the square reads as the map
// changing; the south row and not the middle, because the fountain is there.
// Never a street: a street is where people walk. The town is not told — the
// square stays open ground to the router, and a stall on it is the spectator's
// to draw.
func pitchesOn(m town.Map, id string) []town.Cell {
	p, ok := m.Place(id)
	if !ok {
		return nil
	}
	var cells []town.Cell
	for x := p.X; x < p.X+p.W; x++ {
		cells = append(cells, town.Cell{X: x, Y: p.Y + p.H - 1})
	}
	return cells
}
