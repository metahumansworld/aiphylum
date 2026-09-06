// phylumd runs episodes.
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
// Fair mode (-fair) is the sim's economy standing in the town's square: the
// same cast, given bodies and daily schedules, and a bounty office that posts
// on the hour. You must be standing at the office to see the board, so who
// hears about a bounty is a matter of where they happen to be — the economy
// and the town compose across one seam (town.Config.Visit) and neither learns
// the other's internals. Stub only, deterministic, unranked, zero spend.
//
// Live mode (-demo=false) is the real thing: Docker containers behind the
// zero-egress network, the relay bridging them to this process's proxy, and
// a real provider billed at real prices. It needs docker and ANTHROPIC_API_KEY
// and refuses to start without them.
//
// Live mode is also the only one that does not run an episode of its own. It
// serves a control plane on -listen and waits: phylumctl submits agents and
// asks for episodes, and the world persists between them in a ledger on disk.
// One episode runs at a time, and a failed one stops the daemon taking work —
// when the money may be wrong, the answer is a human, not another round.
//
// That persistence lasts as long as the process does, and no longer. Only the
// money is on disk; the roster is not, so a used book cannot be resumed and a
// live boot refuses one rather than coming up subtly wrong. See
// refuseUsedLedger.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/metahumansworld/aiphylum/internal/bounty"
	"github.com/metahumansworld/aiphylum/internal/daemon"
	"github.com/metahumansworld/aiphylum/internal/generators"
	"github.com/metahumansworld/aiphylum/internal/judge"
	"github.com/metahumansworld/aiphylum/internal/ledger"
	"github.com/metahumansworld/aiphylum/internal/orchestrator"
	"github.com/metahumansworld/aiphylum/internal/proxy"
	"github.com/metahumansworld/aiphylum/internal/rating"
	"github.com/metahumansworld/aiphylum/internal/runner"
	"github.com/metahumansworld/aiphylum/internal/suites"
	"github.com/metahumansworld/aiphylum/internal/trace"
)

func main() {
	demo := flag.Bool("demo", true, "run the offline demo episode (stub model, subprocess agents)")
	sim := flag.Bool("sim", false, "run the real-time sim track instead of the ranked round loop")
	townMode := flag.Bool("town", false, "run the town: residents on daily schedules, no economy; add -mind for the thinking half")
	mindFlag := flag.Bool("mind", false, "town: residents keep memories, talk when they meet, and reflect at day's end — on the offline stub, zero API calls")
	fairMode := flag.Bool("fair", false, "run the fair: the sim's economy at the town's bounty office — the cast gets bodies, and only who is standing at the board can bid; stub only, zero spend")
	var guests []string
	flag.Func("guest", "fair: `path` to a user-authored agent (a Python file on the SDK); it takes lodgings at the tavern and bids against the cast — repeatable", func(v string) error {
		guests = append(guests, v)
		return nil
	})
	tiebreak := flag.String("tiebreak", "arrival", "fair: how a tie at the lowest ask is broken — arrival (the earlier bid wins) or lot (a seeded draw among the tied names)")
	book := flag.String("book", "sealed", "fair: what each bidder is told of the auction book with its result — sealed (your ask, the clearing price, the winner, the head-count) or open (every name and every ask)")
	days := flag.Int("days", 1, "town: how many simulated days to run")
	tick := flag.Duration("tick", 700*time.Millisecond, "town: wall clock per ten simulated minutes")
	rounds := flag.Int("rounds", 8, "rounds in the episode")
	seed := flag.Int64("seed", 1, "episode seed; same seed, same episode")
	tracePath := flag.String("trace", "demo-trace.jsonl", "trace output path")
	dbPath := flag.String("db", "", "ledger database path (default: temp file offline, "+liveDB+" live)")
	listen := flag.String("listen", defaultListen, "live: control-plane address for phylumctl")
	genDir := flag.String("generators", "generators", "path to the generators directory")
	imported := flag.Bool("imported", false, "also draw bounties from the imported suites in <generators>/suites — ranked, with an asterisk")
	latency := flag.Duration("latency", 0, "per-call stub latency, for believable pacing")
	post := flag.Duration("post", 900*time.Millisecond, "sim: how often the clock ticks and a bounty appears")
	window := flag.Duration("window", 1500*time.Millisecond, "sim: how long an auction takes bids")
	runFor := flag.Duration("for", 0, "sim: stop after this long (0 = until the deck is spent)")
	deck := flag.Int("deck", 16, "sim: how many bounties the world has to give")
	serve := flag.Bool("serve", false, "run the service: built agents answering over HTTP — on the stub unless OPENROUTER_API_KEY is set, and then real spend behind each user's grant")
	var agents []string
	flag.Func("agent", "serve: `path` to an agent spec (JSON) to run from boot — repeatable", func(v string) error {
		agents = append(agents, v)
		return nil
	})
	serveListen := flag.String("serve-listen", defaultServeListen, "serve: address for the control surface (/v1/agents) and the agents' public endpoints (/a/{id})")
	grant := flag.Int64("grant", 1_000_000, "serve: credits minted to each new user's wallet at first sign-in, in micro-USD (1000000 is $1); every agent they build spends from it")
	serveModel := flag.String("serve-model", defaultServeModel, "serve: the one real model offered, as id=input,output in nano-USD per token")
	serveLocked := flag.String("serve-locked", defaultServeLocked, "serve: models shown in the catalogue and not offered, comma-separated; a builder's click on one joins the waitlist")
	flag.Parse()

	// The trace path's default names the arena. A run on another track that was
	// not given one gets its own file, so neither a town nor a live world ever
	// truncates a demo trace by omission.
	//
	// The sim is deliberately not in here. Like the demo it replays from a seed,
	// so overwriting its output is regeneration rather than loss; `make sim-demo`
	// names its own file anyway.
	traceSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "trace" {
			traceSet = true
		}
	})
	if !traceSet {
		switch {
		case *serve:
			// Its own file, and one `make clean` leaves alone: a service trace
			// is the conversations strangers had with an agent, an audit
			// record rather than a replayable artefact.
			*tracePath = "service-trace.jsonl"
		case *fairMode:
			*tracePath = "fair-trace.jsonl"
		case *townMode && *mindFlag:
			// Its own file: town-trace.jsonl is a pinned artefact, and a
			// thinking day writes a different stream than a silent one.
			*tracePath = "town-mind-trace.jsonl"
		case *townMode:
			*tracePath = "town-trace.jsonl"
		case !*demo && !*sim:
			*tracePath = "live-trace.jsonl"
		}
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	opts := options{
		demo: *demo, sim: *sim, town: *townMode, fair: *fairMode, mind: *mindFlag, rounds: *rounds, seed: *seed,
		tracePath: *tracePath, dbPath: *dbPath, genDir: *genDir, latency: *latency,
		imported: *imported, listen: *listen,
		post: *post, window: *window, runFor: *runFor, deck: *deck,
		days: *days, tick: *tick, guests: guests, tiebreak: *tiebreak, book: *book,
		serve: *serve, agents: agents, serveListen: *serveListen, grant: *grant, serveModel: *serveModel,
		serveLocked: *serveLocked,
	}
	if err := run(ctx, log, opts); err != nil {
		log.Error("phylumd failed", "err", err)
		os.Exit(1)
	}
}

// options is the parsed command line, kept in one place so the three modes do
// not each grow their own argument list.
type options struct {
	demo, sim, town bool
	fair, mind      bool
	rounds          int
	seed            int64
	tracePath       string
	dbPath          string
	genDir          string
	latency         time.Duration
	// imported gates the whole imported path at once — load, register,
	// declare, and deal. One switch, because a world that declares a suite it
	// never posts from would print a provenance note explaining nothing, and a
	// world that posts from one it never declared would be the asterisk
	// missing from the board.
	imported bool
	listen   string

	post, window, runFor time.Duration
	deck                 int

	days int

	// guests are user-authored agents joining the fair; empty everywhere else.
	guests []string
	tick   time.Duration
	// tiebreak names the fair's policy for a tie at the lowest ask:
	// "arrival" or "lot". Arrival is the default on every track; lot is a
	// fair thing, refused elsewhere the way -guest is.
	tiebreak string
	// book names what a bidder is told of the auction book: "sealed" or
	// "open". Sealed is the default on every track; open is a fair thing,
	// refused elsewhere the way -tiebreak lot is.
	book string

	// serve runs built agents instead of a world: no board, no ladder, no
	// containers. agents are spec files to run from boot; serveListen is where
	// they answer; grant is each new user's whole bankroll; serveModel is the
	// one real model on the price table, with its price; serveLocked are the
	// models the catalogue shows behind a lock. accountsPath is the users'
	// database and agentsPath the built agents', both chosen next to the
	// ledger.
	serve        bool
	agents       []string
	serveListen  string
	grant        int64
	serveModel   string
	serveLocked  string
	accountsPath string
	agentsPath   string
}

const (
	// The control plane binds to loopback and carries no authentication: it is
	// an operator's console on the machine running the arena, not a public API.
	defaultListen = "127.0.0.1:8141"
	liveDB        = "phylum-live.db"

	// The service binds to loopback too, for now: an agent's public endpoint
	// is public in shape, not yet in reach. Accounts and rate limits are here;
	// a public bind and TLS are the job of the milestone that puts it online.
	defaultServeListen = "127.0.0.1:8151"
	// The fixed model every built agent runs on, at OpenRouter's price for it
	// ($1 per million input tokens, $5 per million output) in nano-USD per
	// token. One row, set by hand; syncing the table from OpenRouter's
	// catalogue comes with the milestone that unlocks other models.
	defaultServeModel = "anthropic/claude-haiku-4.5=1000,5000"
	// The models a builder sees and cannot pick yet. Display only: the price
	// table never holds them, so a spec naming one is refused. Corrected the
	// same way -serve-model is, by hand, until the catalogue syncs.
	defaultServeLocked = "anthropic/claude-sonnet-5,anthropic/claude-opus-5,openai/gpt-5,google/gemini-2.5-pro"
	// A live service keeps its books: users' grants must survive a restart,
	// so with a key set the ledger goes to a file, and the users next to it.
	serviceDB       = "phylum-service.db"
	serviceAccounts = "phylum-accounts.db"
	serviceAgents   = "phylum-agents.db"
)

func run(ctx context.Context, log *slog.Logger, opt options) error {
	// A guest is a fair thing: the demo and the sim have no map for a body to
	// stand on, and the plain town has no board for a trader to read.
	if len(opt.guests) > 0 && !opt.fair {
		return fmt.Errorf("-guest belongs to the fair; run it with -fair")
	}
	switch opt.tiebreak {
	case "", "arrival":
		// The default everywhere, and the only policy the other tracks have.
	case "lot":
		if !opt.fair {
			return fmt.Errorf("-tiebreak lot belongs to the fair; run it with -fair")
		}
	default:
		return fmt.Errorf("-tiebreak %q is not a policy; it is arrival or lot", opt.tiebreak)
	}
	switch opt.book {
	case "", "sealed":
		// The default everywhere, and the only policy the other tracks have.
	case "open":
		if !opt.fair {
			return fmt.Errorf("-book open belongs to the fair; run it with -fair")
		}
	default:
		return fmt.Errorf("-book %q is not a policy; it is sealed or open", opt.book)
	}
	// The service is not a track: it runs built agents and no world at all,
	// so every flag that shapes a world is refused alongside it.
	if opt.serve && (opt.fair || opt.sim || opt.town || opt.imported || len(opt.guests) > 0) {
		return fmt.Errorf("-serve runs built agents and nothing else; drop -fair, -sim, -town, -imported and -guest")
	}
	if len(opt.agents) > 0 && !opt.serve {
		return fmt.Errorf("-agent belongs to the service; run it with -serve")
	}

	// The town is a different genre, not a fourth arena mode: no ledger, no
	// board, no agents, no money. It shares only the trace and the viewer, so
	// it branches off before any of the economy is built.
	if opt.town {
		tw, err := trace.NewWriter(opt.tracePath)
		if err != nil {
			return fmt.Errorf("open trace: %w", err)
		}
		defer tw.Close()
		return runTown(ctx, tw, opt)
	}

	// The service is not an episode either. Its books are people's grants and
	// its trace is every conversation they had, and both must outlive the
	// process: a service that will not restart over its own record is not a
	// service. So it takes neither of the live guards below — a used ledger
	// is its normal state — and appends to its trace instead of refusing it.
	// Offline, with no key, it is still a demo: temp books, gone at exit.
	if opt.serve {
		return serveBooks(ctx, log, opt)
	}

	dbPath := opt.dbPath
	if dbPath == "" {
		// The offline modes run one episode and print it; their ledger is
		// scaffolding and goes away with them. A live world is the opposite —
		// its balances and permadeaths are the thing being accumulated — so it
		// defaults to a file that survives the process.
		if !opt.demo && !opt.sim {
			dbPath = liveDB
		} else {
			dir, err := os.MkdirTemp("", "phylum-*")
			if err != nil {
				return err
			}
			defer os.RemoveAll(dir)
			dbPath = filepath.Join(dir, "ledger.db")
		}
	}
	// Both live guards run here, in this order, and both before the trace is
	// opened. trace.NewWriter is os.Create, so a boot that is going to be
	// refused must be refused before it can destroy anything on its way out —
	// and the ledger is read before anything in this process can write to it,
	// so the question stays "has someone used this world" rather than "has this
	// binary touched this file".
	l, err := ledger.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open ledger: %w", err)
	}
	defer l.Close()

	if !opt.demo && !opt.sim {
		if err := refuseUsedLedger(ctx, l, dbPath); err != nil {
			return err
		}
		if err := refuseUsedTrace(opt.tracePath); err != nil {
			return err
		}
	}
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
	case opt.fair:
		// Like the sim: nil ladder, and NewFair refuses any other kind.
		return runFair(ctx, log, l, board, tw, notes, opt)
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

// refuseUsedLedger stops a live world from booting onto a book some other
// process has already run.
//
// Only the money is on disk. The roster, the board's numbering and the epoch
// counter are in memory, so a second process over the same book cannot re-admit
// its own living agents — their accounts exist, so intake refuses them, and
// their credits are stranded where nothing can reach them. Its first episode
// then reaches for an attempt wallet the previous run retired, and retired
// accounts are never recreated, which latches the world broken. Both are pinned
// by TestRestartStrandsExistingAgents and TestRestartCollidesOnAttemptWallets.
//
// Refusing at boot is that same outcome, said immediately and in words instead
// of several minutes later as a conservation fault. What it does not do is make
// restart work: that would need the roster on disk too, and an agent's image is
// only ever held in memory.
func refuseUsedLedger(ctx context.Context, l *ledger.Ledger, dbPath string) error {
	used, err := l.HasHistory(ctx)
	if err != nil {
		return err
	}
	if !used {
		return nil
	}
	return fmt.Errorf("ledger %s has already run a world, and a live world cannot be resumed: "+
		"only the money is on disk, so the agents it belongs to cannot be brought back and "+
		"their balances are not recoverable by restarting. Move or delete %s to start fresh, "+
		"or pass -db <path> to leave it alone and run a separate world", dbPath, dbPath)
}

// refuseUsedTrace stops a live world from truncating a trace that already holds
// a run. trace.NewWriter is os.Create, so merely opening one is destructive.
//
// This is live-only, and the asymmetry is the whole point. An offline trace is
// reproducible — regenerating an identical one from a seed is what `make demo`
// is for — so overwriting it costs nothing. A live trace is the only record of a
// run that cannot be run again, so it is never overwritten without being asked.
// serveBooks opens what the service keeps — the ledger, the users, the trace
// — and runs it. With a key the books are files that persist across runs;
// without one they are a temp directory, since the stub's money is not money.
func serveBooks(ctx context.Context, log *slog.Logger, opt options) error {
	live := os.Getenv(openRouterKeyEnv) != ""
	dbPath := opt.dbPath
	if dbPath == "" {
		if live {
			dbPath = serviceDB
		} else {
			dir, err := os.MkdirTemp("", "phylum-*")
			if err != nil {
				return err
			}
			defer os.RemoveAll(dir)
			dbPath = filepath.Join(dir, "ledger.db")
		}
	}
	opt.accountsPath = filepath.Join(filepath.Dir(dbPath), serviceAccounts)
	opt.agentsPath = filepath.Join(filepath.Dir(dbPath), serviceAgents)

	l, err := ledger.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open ledger: %w", err)
	}
	defer l.Close()

	tw, err := trace.OpenWriter(opt.tracePath)
	if err != nil {
		return err
	}
	defer tw.Close()
	return runServe(ctx, log, l, tw, opt)
}

func refuseUsedTrace(path string) error {
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check trace %s: %w", path, err)
	}
	if fi.Size() == 0 {
		return nil
	}
	return fmt.Errorf("trace %s already holds a run and live mode will not overwrite it: "+
		"move it aside, or pass -trace <path> to write somewhere else", path)
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
	cfg := orchestrator.Config{
		StepTimeout: 30 * time.Second,
		// Dust: roughly the cost of one real model call. An agent below it
		// can no longer play — its holds get refused, its attempts burn
		// nothing, and it would haunt the board forever. It dies instead.
		Dust: 200,
	}
	if opt.book == "open" {
		// Only ever true under -fair — run() refuses it anywhere else — so
		// the demo and the sim always build the zero value, sealed.
		cfg.Book = orchestrator.OpenBook
	}
	orch := orchestrator.New(l, board, table, &proxy.StubProvider{Latency: opt.latency},
		tw, steps, ladder, cfg, log)
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
	fmt.Printf("\ntrace: %s (replayable; inspect with phylumctl)\n", tw.Path())
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
	fmt.Printf("\ntrace: %s (replayable; inspect with phylumctl)\n", tw.Path())
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
	fmt.Printf("phylumd listening on %s — submit agents with `phylumctl submit`, run with `phylumctl run`\n", listen)

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
