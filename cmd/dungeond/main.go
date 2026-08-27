// dungeond runs episodes.
//
// Demo mode (-demo, the default; what `make demo` invokes) is the plan's
// verification #10: a seeded multi-round episode on the deterministic stub
// model, reference agents running as host subprocesses through the real
// metering proxy and the real SDK, ending in the printed efficiency ladder.
// No Docker, no key, no network.
//
// Sim mode (-sim) runs the same cast and the same money on a clock instead of
// in rounds: bounties appear on a timer, auctions close on a deadline, and an
// agent busy on an attempt misses the windows that open while it works. It is
// unranked by design — there is no ladder to attach — so it prints a chronicle
// rather than a scoreboard.
//
// Live mode (-demo=false) is the real thing: Docker containers behind the
// zero-egress network, the relay bridging them to this process's proxy, and
// a real provider billed at real prices. It needs docker and ANTHROPIC_API_KEY
// and refuses to start without them.
//
// Live mode is also the only one that does not run an episode of its own. It
// serves a control plane on -listen and waits: dungeonctl submits agents and
// asks for episodes, and the world persists between them in a ledger on disk.
// One episode runs at a time, and a failed one stops the daemon taking work —
// when the money may be wrong, the answer is a human, not another round.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/metahunmei/dungeon/internal/bounty"
	"github.com/metahunmei/dungeon/internal/daemon"
	"github.com/metahunmei/dungeon/internal/generators"
	"github.com/metahunmei/dungeon/internal/judge"
	"github.com/metahunmei/dungeon/internal/ledger"
	"github.com/metahunmei/dungeon/internal/orchestrator"
	"github.com/metahunmei/dungeon/internal/proxy"
	"github.com/metahunmei/dungeon/internal/rating"
	"github.com/metahunmei/dungeon/internal/runner"
	"github.com/metahunmei/dungeon/internal/suites"
	"github.com/metahunmei/dungeon/internal/trace"
)

func main() {
	demo := flag.Bool("demo", true, "run the offline demo episode (stub model, subprocess agents)")
	sim := flag.Bool("sim", false, "run the real-time sim track instead of the ranked round loop")
	rounds := flag.Int("rounds", 8, "rounds in the episode")
	seed := flag.Int64("seed", 1, "episode seed; same seed, same episode")
	tracePath := flag.String("trace", "demo-trace.jsonl", "trace output path")
	dbPath := flag.String("db", "", "ledger database path (default: temp file offline, "+liveDB+" live)")
	listen := flag.String("listen", defaultListen, "live: control-plane address for dungeonctl")
	genDir := flag.String("generators", "generators", "path to the generators directory")
	imported := flag.Bool("imported", false, "also draw bounties from the imported suites in <generators>/suites — ranked, with an asterisk")
	latency := flag.Duration("latency", 0, "per-call stub latency, for believable pacing")
	post := flag.Duration("post", 900*time.Millisecond, "sim: how often the clock ticks and a bounty appears")
	window := flag.Duration("window", 1500*time.Millisecond, "sim: how long an auction takes bids")
	runFor := flag.Duration("for", 0, "sim: stop after this long (0 = until the deck is spent)")
	deck := flag.Int("deck", 16, "sim: how many bounties the world has to give")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	opts := options{
		demo: *demo, sim: *sim, rounds: *rounds, seed: *seed,
		tracePath: *tracePath, dbPath: *dbPath, genDir: *genDir, latency: *latency,
		imported: *imported, listen: *listen,
		post: *post, window: *window, runFor: *runFor, deck: *deck,
	}
	if err := run(ctx, log, opts); err != nil {
		log.Error("dungeond failed", "err", err)
		os.Exit(1)
	}
}

// options is the parsed command line, kept in one place so the three modes do
// not each grow their own argument list.
type options struct {
	demo, sim bool
	rounds    int
	seed      int64
	tracePath string
	dbPath    string
	genDir    string
	latency   time.Duration
	// imported gates the whole imported path at once — load, register,
	// declare, and deal. One switch, because a world that declares a suite it
	// never posts from would print a provenance note explaining nothing, and a
	// world that posts from one it never declared would be the asterisk
	// missing from the board.
	imported bool
	listen   string

	post, window, runFor time.Duration
	deck                 int
}

const (
	// The control plane binds to loopback and carries no authentication: it is
	// an operator's console on the machine running the arena, not a public API.
	defaultListen = "127.0.0.1:8141"
	liveDB        = "dungeon-live.db"
)

func run(ctx context.Context, log *slog.Logger, opt options) error {
	dbPath := opt.dbPath
	if dbPath == "" {
		// The offline modes run one episode and print it; their ledger is
		// scaffolding and goes away with them. A live world is the opposite —
		// its balances and permadeaths are the thing being accumulated — so it
		// defaults to a file that survives the process.
		if !opt.demo && !opt.sim {
			dbPath = liveDB
		} else {
			dir, err := os.MkdirTemp("", "dungeon-*")
			if err != nil {
				return err
			}
			defer os.RemoveAll(dir)
			dbPath = filepath.Join(dir, "ledger.db")
		}
	}
	l, err := ledger.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open ledger: %w", err)
	}
	defer l.Close()

	tw, err := trace.NewWriter(opt.tracePath)
	if err != nil {
		return fmt.Errorf("open trace: %w", err)
	}
	defer tw.Close()

	board := bounty.NewBoard()
	for _, g := range generators.Dir(opt.genDir) {
		board.RegisterGenerator(g)
	}
	notes, err := loadSuites(board, opt)
	if err != nil {
		return err
	}

	switch {
	case opt.sim:
		// No ladder, and not as an omission: RunSim refuses a world that has
		// one. Real-time results are not comparable, so they are never scored.
		return runSim(ctx, log, l, board, tw, notes, opt)
	case !opt.demo:
		return runLive(ctx, log, l, board, tw, rating.New(rating.DefaultConfig()), notes, opt.listen, dbPath)
	default:
		return runDemo(ctx, log, l, board, tw, rating.New(rating.DefaultConfig()), notes, opt)
	}
}

// loadSuites registers the imported suites as ordinary generators and returns
// the provenance the orchestrator publishes for them. Without -imported it
// returns nothing at all, which is what keeps the default demo — and every
// number pinned against it — exactly as it was before suites existed.
func loadSuites(board *bounty.Board, opt options) ([]orchestrator.SuiteNote, error) {
	if !opt.imported {
		return nil, nil
	}
	dir := filepath.Join(opt.genDir, "suites")
	loaded, err := suites.LoadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("imported suites: %w", err)
	}
	// LoadDir treats a missing directory as an empty world, which is right for
	// a caller who did not ask. This caller asked, so silence would be a lie.
	if len(loaded) == 0 {
		return nil, fmt.Errorf("-imported was given but %s holds no .jsonl suites", dir)
	}
	notes := make([]orchestrator.SuiteNote, 0, len(loaded))
	for _, s := range loaded {
		board.RegisterGenerator(s)
		notes = append(notes, orchestrator.SuiteNote{
			Name:          s.Manifest.Name,
			Source:        s.Manifest.Source,
			Licence:       s.Manifest.Licence,
			Contamination: s.Manifest.Contamination,
		})
		fmt.Printf("  * %-8s %d instances across tiers %v — imported, ranked with an asterisk\n",
			s.Name(), s.Len(), s.Tiers())
	}
	return notes, nil
}

// offline is one fully wired offline world: stub provider, subprocess agents
// behind the real metering proxy, and an ephemeral HTTP server carrying the
// two apart. Both offline modes build the same one — the only difference
// between the demo and the sim is what drives the clock.
type offline struct {
	orch     *orchestrator.Orchestrator
	steps    *orchestrator.ProcessSteps
	proxyURL string // the judge dials this too: it is metered like anyone else
	stop     func()
}

// roster is one seat in the offline cast.
type roster struct {
	id    string
	grant ledger.Credits
	bio   string
}

// cast is the demo population, shared by both offline modes so the same three
// temperaments meet the same economy under both clocks.
var cast = []roster{
	{"frugal", 2500, "computes, barely spends, bids only what it can solve"},
	{"scholar", 3000, "pays the oracle at spec, double-checks its arithmetic"},
	{"gambler", 1600, "underbids everyone, burns like it means it"},
}

func newOffline(ctx context.Context, log *slog.Logger, l *ledger.Ledger, board *bounty.Board,
	tw *trace.Writer, ladder *rating.Ladder, notes []orchestrator.SuiteNote, opt options) (*offline, error) {

	table := proxy.NewPriceTable()
	table.Set("stub-1", proxy.Price{InputPerTok: 1000, OutputPerTok: 1000})

	// The proxy needs a URL before the orchestrator exists; listen first,
	// serve once the orchestrator has built the handler.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	proxyURL := "http://" + ln.Addr().String()

	steps := orchestrator.NewProcessSteps(proxyURL)
	orch := orchestrator.New(l, board, table, &proxy.StubProvider{Latency: opt.latency},
		tw, steps, ladder, orchestrator.Config{
			StepTimeout: 30 * time.Second,
			// Dust: roughly the cost of one real model call. An agent below it
			// can no longer play — its holds get refused, its attempts burn
			// nothing, and it would haunt the board forever. It dies instead.
			Dust: 200,
		}, log)
	// Published once at the head of every episode, ahead of the supply it
	// explains, so a reader meets the asterisk before the scores it qualifies.
	orch.Suites = notes

	srv := &http.Server{Handler: orch.Proxy}
	go srv.Serve(ln)
	w := &offline{orch: orch, steps: steps, proxyURL: proxyURL, stop: func() {
		srv.Shutdown(context.Background())
		ln.Close()
	}}

	agentsDir, err := filepath.Abs(filepath.Join(opt.genDir, "agents"))
	if err != nil {
		w.stop()
		return nil, err
	}
	sdkDir, err := filepath.Abs(filepath.Join("sdk", "python"))
	if err != nil {
		w.stop()
		return nil, err
	}
	env := map[string]string{"PYTHONPATH": sdkDir + ":" + agentsDir}

	for _, a := range cast {
		steps.Register(a.id, orchestrator.ProcessAgent{
			Cmd: []string{"python3", filepath.Join(agentsDir, a.id+".py")},
			Env: env,
		})
		if err := orch.AddAgent(ctx, a.id, a.grant); err != nil {
			w.stop()
			return nil, err
		}
		fmt.Printf("  + %-8s %5d credits — %s\n", a.id, a.grant, a.bio)
	}
	return w, nil
}

// runDemo is the ranked round loop: a seeded episode, ending in the ladder.
func runDemo(ctx context.Context, log *slog.Logger, l *ledger.Ledger, board *bounty.Board,
	tw *trace.Writer, ladder *rating.Ladder, notes []orchestrator.SuiteNote, opt options) error {

	w, err := newOffline(ctx, log, l, board, tw, ladder, notes, opt)
	if err != nil {
		return err
	}
	defer w.stop()

	ep := demoEpisode(opt.seed, opt.rounds, notes)
	fmt.Printf("\nepisode: seed %d, %d rounds, %d bounties — the board opens\n\n",
		opt.seed, opt.rounds, countPostings(ep))

	if err := w.orch.RunEpisode(ctx, ep); err != nil {
		return fmt.Errorf("episode: %w", err)
	}

	printLadder(w.orch, ladder, l)
	fmt.Printf("\ntrace: %s (replayable; inspect with dungeonctl)\n", tw.Path())
	return nil
}

// runSim is the same world on a clock. It takes a nil ladder because RunSim
// refuses any other kind, and prints a chronicle instead of a ranking.
func runSim(ctx context.Context, log *slog.Logger, l *ledger.Ledger, board *bounty.Board,
	tw *trace.Writer, notes []orchestrator.SuiteNote, opt options) error {

	w, err := newOffline(ctx, log, l, board, tw, nil, notes, opt)
	if err != nil {
		return err
	}
	defer w.stop()

	// Judging is enabled only here, in the sim. The orchestrator refuses to
	// post a judged bounty into a world that has a ladder, so the demo could
	// not grade one even if it were wired to; this is the other half of the
	// same rule, stated where an operator reads it.
	if err := w.orch.EnableJudging(ctx, &judge.HTTP{
		Base: w.proxyURL, Model: "stub-1", MaxTokens: 64,
	}, judgeEndowment); err != nil {
		return fmt.Errorf("enable judging: %w", err)
	}
	fmt.Printf("  + %-8s %5d credits — grades the open-ended briefs, spends its own money\n",
		"judge", judgeEndowment)

	deck := simDeck(opt.seed, opt.deck, notes)
	fmt.Printf("\nsim: seed %d, %d bounties, one every %s, %s bid windows — the clock starts\n\n",
		opt.seed, len(deck), opt.post, opt.window)

	rep, err := w.orch.RunSim(ctx, orchestrator.SimConfig{
		PostInterval: opt.post,
		BidWindow:    opt.window,
		Duration:     opt.runFor,
		Deck:         deck,
	})
	if err != nil {
		return fmt.Errorf("sim: %w", err)
	}

	printChronicle(rep)
	fmt.Printf("\ntrace: %s (replayable; inspect with dungeonctl)\n", tw.Path())
	return nil
}

// judgeEndowment funds the sim's grader. Generous on purpose: a grader that
// runs out mid-sim does not fail attempts, it voids them, and a chronicle full
// of voided bounties would say nothing about the agents.
const judgeEndowment = ledger.Credits(250_000)

// simDeck is the sim's supply: the demo's two keyed generators plus the
// judged one the ranked track is not allowed to post, flattened into one
// ordered deck the clock deals from. Imported suites join the rotation as
// ordinary generators — with none loaded the deck is exactly what it was.
func simDeck(seed int64, n int, notes []orchestrator.SuiteNote) []orchestrator.Posting {
	gens := []string{"arith", "oracle", "brief"}
	for _, s := range notes {
		gens = append(gens, s.Name)
	}
	deck := make([]orchestrator.Posting, 0, n)
	for i := 0; i < n; i++ {
		deck = append(deck, orchestrator.Posting{
			Generator: gens[i%len(gens)],
			Seed:      seed*10_000 + int64(i),
			// Tier advances a rank per full pass of the generators, not per
			// card: cycling both on the same stride would pin each generator
			// to one difficulty forever.
			Tier:         (i/len(gens))%3 + 1,
			WallClockSec: 20,
		})
	}
	return deck
}

// printChronicle reports what happened, in roster order, with no rank column
// and no efficiency: the sim is watched, not scored.
func printChronicle(rep orchestrator.SimReport) {
	fmt.Printf("── the world, after %d ticks (%s) ───────────────────────────────────\n",
		rep.Ticks, rep.Reason)
	fmt.Printf("%-9s %8s %9s %8s %8s  %s\n",
		"agent", "attempts", "successes", "earned", "burned", "fate")
	for _, st := range rep.Standings {
		fate := fmt.Sprintf("alive, %d credits", st.Balance)
		if st.Retired {
			fate = "☠ bankrupt"
		}
		fmt.Printf("%-9s %8d %9d %8d %8d  %s\n",
			st.Agent, st.Attempts, st.Solved, st.Earned, st.Burned, fate)
	}
	fmt.Printf("─────────────────────────────────────────────────────────────────────\n")
	fmt.Printf("%d of the deck dealt · unranked by design\n", rep.Posted)
	fmt.Printf("conservation: %s\n", rep.Conservation)
}

// demoEpisode derives the round plan from the seed: every round posts two
// arith and two oracle bounties with tiers cycling 1..3, seeds unique per
// posting. Same seed, same plan — the demo is replayable end to end.
//
// Each imported suite adds one bounty per round, appended after the generated
// four so their seeds and their order are untouched. With nothing imported the
// plan is byte for byte the plan that produced the pinned ladder.
func demoEpisode(seed int64, rounds int, notes []orchestrator.SuiteNote) orchestrator.Episode {
	ep := orchestrator.Episode{}
	for r := 0; r < rounds; r++ {
		tierA := r%3 + 1
		tierB := (r+1)%3 + 1
		base := seed*10_000 + int64(r)*10
		round := []orchestrator.Posting{
			{Generator: "arith", Seed: base + 1, Tier: tierA, WallClockSec: 20},
			{Generator: "arith", Seed: base + 2, Tier: tierB, WallClockSec: 20},
			{Generator: "oracle", Seed: base + 3, Tier: tierA, WallClockSec: 20},
			{Generator: "oracle", Seed: base + 4, Tier: tierB, WallClockSec: 20},
		}
		for i, s := range notes {
			// Distinct generators, so one seed per round serves them all; the
			// tier is offset per suite so a multi-suite world is not stuck at
			// one difficulty for the round.
			round = append(round, orchestrator.Posting{
				Generator: s.Name, Seed: base + 5, Tier: (r+i)%3 + 1, WallClockSec: 20,
			})
		}
		ep.Rounds = append(ep.Rounds, round)
	}
	return ep
}

func countPostings(ep orchestrator.Episode) int {
	n := 0
	for _, r := range ep.Rounds {
		n += len(r)
	}
	return n
}

func printLadder(orch *orchestrator.Orchestrator, ladder *rating.Ladder, l *ledger.Ledger) {
	fmt.Println("── efficiency ladder ────────────────────────────────────────────────")
	fmt.Printf("%-4s %-9s %8s %9s %6s %8s %8s %11s  %s\n",
		"#", "agent", "attempts", "successes", "tiers", "earned", "burned", "efficiency", "fate")

	retired := map[string]bool{}
	for _, a := range orch.Agents() {
		retired[a.ID] = a.Retired
	}
	rank := 0
	for _, row := range ladder.Board(time.Now()) {
		pos := "—"
		if row.Ranked {
			rank++
			pos = fmt.Sprint(rank)
		}
		fate := "alive"
		if retired[row.Agent] {
			fate = "☠ bankrupt"
		} else if bal, err := l.Balance(context.Background(), row.Agent); err == nil {
			fate = fmt.Sprintf("alive, %d credits", bal)
		}
		eff := fmt.Sprintf("%.2f", row.Efficiency)
		if !row.Ranked {
			eff = "unranked"
		}
		fmt.Printf("%-4s %-9s %8d %9d %6d %8d %8d %11s  %s\n",
			pos, row.Agent, row.Attempts, row.Successes, row.Tiers,
			row.Earned, row.Burned, eff, fate)
	}

	if con, err := l.Conservation(context.Background()); err == nil {
		fmt.Printf("─────────────────────────────────────────────────────────────────────\n")
		fmt.Printf("conservation: %s\n", con)
	}
}

// runLive is the Docker path. It compiles the same wiring the demo proves,
// against the real runner and a real provider; it refuses to start unless its
// dependencies actually exist. Unlike the other two modes it does not run an
// episode and exit — it serves the control plane and waits to be told what to
// run, because a live world outlives any one episode: agents keep their
// balances, failed bounties stay on the board, and the ladder accumulates.
func runLive(ctx context.Context, log *slog.Logger, l *ledger.Ledger, board *bounty.Board,
	tw *trace.Writer, ladder *rating.Ladder, notes []orchestrator.SuiteNote,
	listen, dbPath string) error {

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("live mode needs ANTHROPIC_API_KEY (use -demo for the offline episode)")
	}
	rn := runner.New(log)
	if !runner.Available(ctx) {
		return fmt.Errorf("live mode needs docker running (use -demo for the offline episode)")
	}
	if err := rn.EnsureNetwork(ctx); err != nil {
		return err
	}

	table := proxy.NewPriceTable()
	// Real prices in nano-USD per token.
	table.Set("claude-haiku-4-5-20251001", proxy.Price{InputPerTok: 1_000, OutputPerTok: 5_000})
	table.Set("claude-sonnet-5", proxy.Price{InputPerTok: 3_000, OutputPerTok: 15_000})

	steps := orchestrator.NewDockerSteps(rn)
	orch := orchestrator.New(l, board, table, &proxy.AnthropicProvider{APIKey: apiKey},
		tw, steps, ladder, orchestrator.Config{Dust: 30}, log)
	orch.Suites = notes

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	srv := &http.Server{Handler: orch.Proxy}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	hostPort := ln.Addr().(*net.TCPAddr).Port
	if err := rn.StartRelay(ctx, hostPort); err != nil {
		return err
	}
	defer rn.StopRelay(context.Background())

	control := daemon.New(daemon.Config{
		Orch:       orch,
		Bind:       steps.Register,
		CheckImage: rn.ImageExists,
		TracePath:  tw.Path(),
		// Episodes outlive the requests that start them; they end when the
		// daemon does, not when a client hangs up.
		RunCtx: ctx,
		Log:    log,
	})

	api := &http.Server{Addr: listen, Handler: control}
	errs := make(chan error, 1)
	go func() {
		if err := api.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errs <- err
		}
	}()
	log.Info("live mode up", "listen", listen, "proxy", rn.ProxyURL(), "host_port", hostPort,
		"trace", tw.Path(), "ledger", dbPath)
	fmt.Printf("dungeond listening on %s — submit agents with `dungeonctl submit`, run with `dungeonctl run`\n", listen)

	select {
	case err := <-errs:
		return fmt.Errorf("control plane: %w", err)
	case <-ctx.Done():
	}

	// Interrupt: stop taking work, then let anything in flight land. The
	// episode itself is already unwinding — it runs under ctx.
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := api.Shutdown(shutdown); err != nil {
		log.Warn("control plane shutdown", "err", err)
	}
	log.Info("live mode down")
	return nil
}
