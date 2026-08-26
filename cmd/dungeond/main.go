// dungeond runs episodes.
//
// Demo mode (-demo, the default; what `make demo` invokes) is the plan's
// verification #10: a seeded multi-round episode on the deterministic stub
// model, reference agents running as host subprocesses through the real
// metering proxy and the real SDK, ending in the printed efficiency ladder.
// No Docker, no key, no network.
//
// Live mode (-demo=false) is the real thing: Docker containers behind the
// zero-egress network, the relay bridging them to this process's proxy, and
// a real provider billed at real prices. It needs docker and ANTHROPIC_API_KEY
// and refuses to start without them.
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
	"github.com/metahunmei/dungeon/internal/generators"
	"github.com/metahunmei/dungeon/internal/ledger"
	"github.com/metahunmei/dungeon/internal/orchestrator"
	"github.com/metahunmei/dungeon/internal/proxy"
	"github.com/metahunmei/dungeon/internal/rating"
	"github.com/metahunmei/dungeon/internal/runner"
	"github.com/metahunmei/dungeon/internal/trace"
)

func main() {
	demo := flag.Bool("demo", true, "run the offline demo episode (stub model, subprocess agents)")
	rounds := flag.Int("rounds", 8, "rounds in the episode")
	seed := flag.Int64("seed", 1, "episode seed; same seed, same episode")
	tracePath := flag.String("trace", "demo-trace.jsonl", "trace output path")
	dbPath := flag.String("db", "", "ledger database path (default: temp file)")
	genDir := flag.String("generators", "generators", "path to the generators directory")
	latency := flag.Duration("latency", 0, "per-call stub latency, for believable pacing")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := run(ctx, log, *demo, *rounds, *seed, *tracePath, *dbPath, *genDir, *latency); err != nil {
		log.Error("dungeond failed", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger, demo bool, rounds int, seed int64,
	tracePath, dbPath, genDir string, latency time.Duration) error {

	if dbPath == "" {
		dir, err := os.MkdirTemp("", "dungeon-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		dbPath = filepath.Join(dir, "ledger.db")
	}
	l, err := ledger.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open ledger: %w", err)
	}
	defer l.Close()

	tw, err := trace.NewWriter(tracePath)
	if err != nil {
		return fmt.Errorf("open trace: %w", err)
	}
	defer tw.Close()

	board := bounty.NewBoard()
	for _, g := range generators.Dir(genDir) {
		board.RegisterGenerator(g)
	}
	ladder := rating.New(rating.DefaultConfig())

	if !demo {
		return runLive(ctx, log, l, board, tw, ladder)
	}
	return runDemo(ctx, log, l, board, tw, ladder, rounds, seed, genDir, latency)
}

// runDemo wires the offline world: stub provider, subprocess agents, an
// ephemeral HTTP server for the proxy.
func runDemo(ctx context.Context, log *slog.Logger, l *ledger.Ledger, board *bounty.Board,
	tw *trace.Writer, ladder *rating.Ladder, rounds int, seed int64, genDir string,
	latency time.Duration) error {

	table := proxy.NewPriceTable()
	table.Set("stub-1", proxy.Price{InputPerTok: 1000, OutputPerTok: 1000})

	// The proxy needs a URL before the orchestrator exists; listen first,
	// serve once the orchestrator has built the handler.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	proxyURL := "http://" + ln.Addr().String()

	steps := orchestrator.NewProcessSteps(proxyURL)
	orch := orchestrator.New(l, board, table, &proxy.StubProvider{Latency: latency},
		tw, steps, ladder, orchestrator.Config{
			StepTimeout: 30 * time.Second,
			// Dust: roughly the cost of one real model call. An agent below it
			// can no longer play — its holds get refused, its attempts burn
			// nothing, and it would haunt the board forever. It dies instead.
			Dust: 200,
		}, log)

	srv := &http.Server{Handler: orch.Proxy}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	agentsDir, err := filepath.Abs(filepath.Join(genDir, "agents"))
	if err != nil {
		return err
	}
	sdkDir, err := filepath.Abs(filepath.Join("sdk", "python"))
	if err != nil {
		return err
	}
	env := map[string]string{"PYTHONPATH": sdkDir + ":" + agentsDir}

	type roster struct {
		id    string
		grant ledger.Credits
		bio   string
	}
	cast := []roster{
		{"frugal", 2500, "computes, barely spends, bids only what it can solve"},
		{"scholar", 3000, "pays the oracle at spec, double-checks its arithmetic"},
		{"gambler", 1600, "underbids everyone, burns like it means it"},
	}
	for _, a := range cast {
		steps.Register(a.id, orchestrator.ProcessAgent{
			Cmd: []string{"python3", filepath.Join(agentsDir, a.id+".py")},
			Env: env,
		})
		if err := orch.AddAgent(ctx, a.id, a.grant); err != nil {
			return err
		}
		fmt.Printf("  + %-8s %5d credits — %s\n", a.id, a.grant, a.bio)
	}

	ep := demoEpisode(seed, rounds)
	fmt.Printf("\nepisode: seed %d, %d rounds, %d bounties — the board opens\n\n",
		seed, rounds, countPostings(ep))

	if err := orch.RunEpisode(ctx, ep); err != nil {
		return fmt.Errorf("episode: %w", err)
	}

	printLadder(orch, ladder, l)
	fmt.Printf("\ntrace: %s (replayable; inspect with dungeonctl)\n", tw.Path())
	return nil
}

// demoEpisode derives the round plan from the seed: every round posts two
// arith and two oracle bounties with tiers cycling 1..3, seeds unique per
// posting. Same seed, same plan — the demo is replayable end to end.
func demoEpisode(seed int64, rounds int) orchestrator.Episode {
	ep := orchestrator.Episode{}
	for r := 0; r < rounds; r++ {
		tierA := r%3 + 1
		tierB := (r+1)%3 + 1
		base := seed*10_000 + int64(r)*10
		ep.Rounds = append(ep.Rounds, []orchestrator.Posting{
			{Generator: "arith", Seed: base + 1, Tier: tierA, WallClockSec: 20},
			{Generator: "arith", Seed: base + 2, Tier: tierB, WallClockSec: 20},
			{Generator: "oracle", Seed: base + 3, Tier: tierA, WallClockSec: 20},
			{Generator: "oracle", Seed: base + 4, Tier: tierB, WallClockSec: 20},
		})
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
// against the real runner and a real provider; it refuses to start unless
// its dependencies actually exist.
func runLive(ctx context.Context, log *slog.Logger, l *ledger.Ledger, board *bounty.Board,
	tw *trace.Writer, ladder *rating.Ladder) error {

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

	// TODO(daemon): episode intake — agent image registration (refusing
	// agents with no image; see the livelock note on DockerSteps.Register),
	// per-episode faultTracker reset, and a submit/run API for dungeonctl.
	// Until then live mode proves the wiring and stops.
	log.Info("live mode wired", "proxy", rn.ProxyURL(), "host_port", hostPort)
	return fmt.Errorf("live mode has no episode intake yet — run -demo")
}
