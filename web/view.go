// Package web is the spectator surface: leaderboard, agent pages, bounty
// pages and the trace replay viewer, all served from a single trace file.
//
// Everything on these pages is derived from the public trace — never from the
// ledger database or the agents' source. That is the visibility deal
// ("traces public, source private") made structural: if a page can't be
// rendered from the trace, spectators couldn't have audited it anyway, and it
// doesn't belong on the surface. It also means the web server needs nothing
// but a file: point it at any published trace and the whole episode is
// browsable, byte-for-byte the same story the replay engine sees.
package web

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/singhtushant3-hub/aiphylum/internal/ledger"
	"github.com/singhtushant3-hub/aiphylum/internal/proxy"
	"github.com/singhtushant3-hub/aiphylum/internal/rating"
	"github.com/singhtushant3-hub/aiphylum/internal/trace"
)

// View is the whole episode, reshaped for pages. A published trace is
// immutable, so a static server builds this once and never invalidates it; a
// live server rebuilds it as the trace grows.
type View struct {
	TracePath string
	Live      bool // the trace is still being written; pages show a live badge
	Episode   EpisodeInfo
	Agents    []*AgentView
	Bounties  []*BountyView
	Ladder    []rating.Row
	// Suites is the provenance of the imported benchmark supply this episode
	// drew on, in registration order. Empty for a world of purely generated
	// bounties, which is what makes the asterisk meaningful when it appears.
	Suites []SuiteView
	// Asterisked is the set of agents whose ladder row counts at least one
	// imported attempt. It is a display concern and nothing more: imported
	// work is ranked, so it is in the row's numbers either way. This only
	// marks whose numbers a reader should read with the contamination note in
	// mind. Kept beside the ladder rather than inside rating.Row because the
	// ladder computes a score and this is a footnote about where the score
	// came from — the ranking must not be able to see it.
	Asterisked map[string]bool

	CallCount int
	CallSpend ledger.Credits

	// Town is non-nil when the trace is a town run rather than an arena
	// episode. The map itself lives in the event stream — the page's reducer
	// reads it from the founding event — so this holds only what the server
	// needs to route and headline the page.
	Town *TownInfo

	agentByID  map[string]*AgentView
	bountyByID map[string]*BountyView
}

// TownInfo is the server-side summary of a town trace.
type TownInfo struct {
	Name      string
	Places    int
	Residents int
	Day       int
	Clock     string
	Meetings  int
	// The thinking half's tally — zero on a trace from a town without a mind.
	Utterances  int
	Reflections int
	// The fair's tally — zero on a town with no economy standing in it. The
	// counts live here, not in a second header, because on the fair track the
	// bounty office is just one more place in town.
	Bounties int
	Awards   int
}

// The tracks a trace can come from. Benchmark and sim share one currency and
// one money path; only the clock and the ranking differ. A town trace has no
// money at all — the viewer detects it and swaps the whole surface for the
// map.
const (
	TrackBenchmark = "benchmark"
	TrackSim       = "sim"
	TrackTown      = "town"
	TrackFair      = "fair"
)

// EpisodeInfo summarises the episode lifecycle events.
type EpisodeInfo struct {
	// Track is "benchmark" or "sim". A sim trace has no round count to
	// announce up front and, more importantly, no ranking: the viewer reads
	// this to make sure it never presents one.
	Track        string
	Rounds       int
	Conservation string
	Start        time.Time
	End          time.Time
}

// AgentView is one agent's whole arc.
type AgentView struct {
	ID       string
	Grant    ledger.Credits
	Bankrupt bool

	Earned  ledger.Credits
	Burned  ledger.Credits
	Balance ledger.Credits // reconstructed: grant + payouts − burns − dust

	Attempts []AttemptView
	Bids     []BidView
	Timeline []BalancePoint

	CallCount int
	CallSpend ledger.Credits
}

// AttemptView is one resolved award, from this agent's side.
type AttemptView struct {
	Seq     int64
	Round   int
	Bounty  string
	Tier    int
	Outcome string // solved, failed, voided
	Reason  string
	Payout  ledger.Credits
	Burned  ledger.Credits
}

// BidView is one sealed bid, from this agent's side.
type BidView struct {
	Seq    int64
	Round  int
	Bounty string
	Price  ledger.Credits
	Won    bool
}

// BalancePoint is one step of the reconstructed balance line.
type BalancePoint struct {
	Seq     int64
	Balance ledger.Credits
	Label   string
}

// BountyView is one bounty's whole lifecycle, across however many auctions it
// took: a failed bounty returns to the board, so awards can repeat.
type BountyView struct {
	ID        string
	Generator string
	Seed      int64
	Tier      int
	MaxPayout ledger.Credits
	Reserve   ledger.Credits

	// Judged marks a bounty a model graded against a hidden rubric. The
	// viewer shows it because the difference matters to a reader: a solved
	// judged bounty is an opinion that went the agent's way, not a proof.
	Judged bool

	// Suite names the imported benchmark this instance was drawn from, empty
	// for generated supply. An imported bounty's result counts on the ladder,
	// so the page says where it came from and lets the reader discount it.
	Suite string

	Status   string // open, solved, failed, voided, no bids
	SolvedBy string
	Payout   ledger.Credits
	Failures int

	History []BountyEventView
	Calls   []CallView
}

// SuiteView is one imported suite's provenance, as published in the trace.
// The contamination note is the load-bearing field: it is what turns the
// asterisk from a decoration into a claim a reader can weigh.
type SuiteView struct {
	Name          string
	Source        string
	Licence       string
	Contamination string
}

// BountyEventView is one step in a bounty's history, already worded.
type BountyEventView struct {
	Seq    int64
	Round  int
	Action string
	Detail string
	Book   []BookEntry
}

// BookEntry is one revealed bid in an awarded auction's book.
type BookEntry struct {
	Agent string
	Price ledger.Credits
}

// CallView is one metered model call, bodies elided: the pages show the
// money, the full bytes stay in the trace file for replay and phylumctl.
type CallView struct {
	Seq     int64
	Wallet  string
	Agent   string
	Model   string
	InToks  int64
	OutToks int64
	Cost    ledger.Credits
	Outcome string
}

// Agent looks up one agent by id; nil if unknown.
func (v *View) Agent(id string) *AgentView { return v.agentByID[id] }

// Bounty looks up one bounty by id; nil if unknown.
func (v *View) Bounty(id string) *BountyView { return v.bountyByID[id] }

// attemptWallet parses "att:<bounty>:r<round>"; ok is false for plain agent
// wallets.
func attemptWallet(w string) (bounty string, round int, ok bool) {
	rest, found := strings.CutPrefix(w, "att:")
	if !found {
		return "", 0, false
	}
	i := strings.LastIndex(rest, ":r")
	if i < 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(rest[i+2:])
	if err != nil {
		return "", 0, false
	}
	return rest[:i], n, true
}

// BuildView folds a trace into the view model. The rules mirror the
// orchestrator's bookkeeping exactly — solved is +payout −burned, failed is
// −burned, voided is a wash because the burn was refunded, dust is burned at
// retirement — so the final reconstructed balances agree with the ledger the
// episode actually ran on. The ladder is recomputed from the same events
// through the real rating package, not copied, so the board a spectator sees
// is auditable from the artefact alone.
func BuildView(path string, lines []trace.Line) (*View, error) {
	v := &View{
		TracePath:  path,
		agentByID:  map[string]*AgentView{},
		bountyByID: map[string]*BountyView{},
	}
	ladder := rating.New(rating.DefaultConfig())

	round := 0
	awardee := map[string]string{} // bounty id → agent currently holding the award
	// Whose ladder numbers include imported work. Filled at the same two
	// places the ladder is fed, so the footnote and the score can never
	// disagree about what was counted.
	asterisked := map[string]bool{}

	if len(lines) > 0 {
		v.Episode.Start = lines[0].Time
		v.Episode.End = lines[len(lines)-1].Time
	}

	for _, l := range lines {
		var p map[string]any
		if err := json.Unmarshal(l.Payload, &p); err != nil {
			return nil, fmt.Errorf("trace seq %d: %w", l.Seq, err)
		}
		str := func(k string) string { s, _ := p[k].(string); return s }
		num := func(k string) int64 {
			f, _ := p[k].(float64)
			return int64(f)
		}
		credits := func(k string) ledger.Credits { return ledger.Credits(num(k)) }
		boolean := func(k string) bool { b, _ := p[k].(bool); return b }

		switch l.Type {
		case trace.EventAgent:
			switch str("action") {
			case "spawned":
				a := &AgentView{ID: str("agent"), Grant: credits("grant")}
				a.Balance = a.Grant
				a.Timeline = append(a.Timeline, BalancePoint{l.Seq, a.Balance, "spawned"})
				v.Agents = append(v.Agents, a)
				v.agentByID[a.ID] = a
			case "bankrupt":
				if a := v.agentByID[str("agent")]; a != nil {
					a.Bankrupt = true
				}
			}

		case trace.EventSuite:
			if str("action") == "registered" {
				v.Suites = append(v.Suites, SuiteView{
					Name: str("suite"), Source: str("source"),
					Licence: str("licence"), Contamination: str("contamination"),
				})
			}

		case trace.EventEpisode:
			switch str("action") {
			case "start":
				v.Episode.Track = str("track")
				if v.Episode.Track == "" {
					v.Episode.Track = TrackBenchmark
				}
				v.Episode.Rounds = int(num("rounds"))
			case "round":
				round = int(num("round"))
				// A real-time world does not know its length in advance, so
				// its round count is whatever the clock has reached.
				if round > v.Episode.Rounds {
					v.Episode.Rounds = round
				}
			case "end":
				v.Episode.Conservation = str("conservation")
			}

		case trace.EventBounty:
			id := str("id")
			b := v.bountyByID[id]
			switch str("action") {
			case "posted":
				b = &BountyView{
					ID: id, Generator: str("generator"), Seed: num("seed"),
					Tier: int(num("tier")), MaxPayout: credits("max_payout"),
					Reserve: credits("reserve"), Status: "open",
					Judged: boolean("judged"), Suite: str("suite"),
				}
				v.Bounties = append(v.Bounties, b)
				v.bountyByID[id] = b
				b.History = append(b.History, BountyEventView{
					Seq: l.Seq, Round: round, Action: "posted",
					Detail: fmt.Sprintf("tier %d, max payout %d, reserve %d", b.Tier, b.MaxPayout, b.Reserve),
				})
				if v.Town != nil {
					v.Town.Bounties++
				}
				if b.Judged {
					b.History[len(b.History)-1].Detail += " — judged against a hidden rubric, unranked"
				}
				if b.Suite != "" {
					b.History[len(b.History)-1].Detail += " — imported from " + b.Suite + ", ranked with an asterisk"
				}
			case "awarded":
				if b == nil {
					continue
				}
				winner := str("winner")
				awardee[id] = winner
				if v.Town != nil {
					v.Town.Awards++
				}
				if a := v.agentByID[winner]; a != nil {
					// The winner's latest bid on this bounty is the one that won.
					for i := len(a.Bids) - 1; i >= 0; i-- {
						if a.Bids[i].Bounty == id {
							a.Bids[i].Won = true
							break
						}
					}
				}
				ev := BountyEventView{
					Seq: l.Seq, Round: round, Action: "awarded",
					Detail: fmt.Sprintf("to %s at %d", winner, num("price")),
				}
				if book, ok := p["book"].([]any); ok {
					for _, e := range book {
						if m, ok := e.(map[string]any); ok {
							// The book is written by marshalling a Go struct with
							// no field tags, so its keys are capitalised. Accept
							// either spelling: the trace bytes are pinned, and a
							// reader that only knew one of them rendered every
							// revealed bid as a blank name at price zero.
							ag, _ := m["agent"].(string)
							if ag == "" {
								ag, _ = m["Agent"].(string)
							}
							pr, ok := m["price"].(float64)
							if !ok {
								pr, _ = m["Price"].(float64)
							}
							ev.Book = append(ev.Book, BookEntry{ag, ledger.Credits(pr)})
						}
					}
				}
				b.History = append(b.History, ev)
			case "judged":
				if b == nil {
					continue
				}
				word := "failed"
				if boolean("pass") {
					word = "passed"
				}
				b.History = append(b.History, BountyEventView{
					Seq: l.Seq, Round: round, Action: "judged",
					Detail: fmt.Sprintf("%s by %s — %s", word, str("grader"), str("reason")),
				})
			case "no_bids":
				if b == nil {
					continue
				}
				b.Status = "no bids"
				b.History = append(b.History, BountyEventView{Seq: l.Seq, Round: round, Action: "no bids"})
			case "solved":
				if b == nil {
					continue
				}
				agent, payout, burned := str("agent"), credits("payout"), credits("burned")
				b.Status, b.SolvedBy, b.Payout = "solved", agent, payout
				b.History = append(b.History, BountyEventView{
					Seq: l.Seq, Round: round, Action: "solved",
					Detail: fmt.Sprintf("by %s — payout %d, burned %d", agent, payout, burned),
				})
				if a := v.agentByID[agent]; a != nil {
					a.Earned += payout
					a.Burned += burned
					a.Balance += payout - burned
					a.Timeline = append(a.Timeline, BalancePoint{l.Seq, a.Balance, "solved " + id})
					a.Attempts = append(a.Attempts, AttemptView{
						Seq: l.Seq, Round: round, Bounty: id, Tier: b.Tier,
						Outcome: "solved", Payout: payout, Burned: burned,
					})
				}
				// Not "if the track allows it" — judged work is never ranked
				// on any track. The viewer rebuilds the ladder from scratch
				// rather than copying one, so the rule has to be restated
				// here or the rebuild would quietly invent a ranking the
				// orchestrator refused to produce.
				if !b.Judged {
					ladder.Record(rating.Attempt{
						Agent: agent, Bounty: id, Tier: b.Tier,
						Earned: payout, Burned: burned, Success: true, Time: l.Time,
					})
					if b.Suite != "" {
						asterisked[agent] = true
					}
				}
				delete(awardee, id)
			case "failed":
				if b == nil {
					continue
				}
				agent, burned, reason := str("agent"), credits("burned"), str("reason")
				b.Status = "failed"
				b.Failures++
				b.History = append(b.History, BountyEventView{
					Seq: l.Seq, Round: round, Action: "failed",
					Detail: fmt.Sprintf("by %s — %s, burned %d", agent, reason, burned),
				})
				if a := v.agentByID[agent]; a != nil {
					a.Burned += burned
					a.Balance -= burned
					a.Timeline = append(a.Timeline, BalancePoint{l.Seq, a.Balance, "failed " + id})
					a.Attempts = append(a.Attempts, AttemptView{
						Seq: l.Seq, Round: round, Bounty: id, Tier: b.Tier,
						Outcome: "failed", Reason: reason, Burned: burned,
					})
				}
				if !b.Judged {
					ladder.Record(rating.Attempt{
						Agent: agent, Bounty: id, Tier: b.Tier,
						Burned: burned, Success: false, Time: l.Time,
					})
					if b.Suite != "" {
						asterisked[agent] = true
					}
				}
				delete(awardee, id)
			case "voided":
				if b == nil {
					continue
				}
				agent, reason := str("agent"), str("reason")
				b.Status = "voided"
				b.History = append(b.History, BountyEventView{
					Seq: l.Seq, Round: round, Action: "voided", Detail: reason,
				})
				// Voided means platform fault: the burn was refunded, so the
				// balance is a wash and the ladder never hears about it.
				if a := v.agentByID[agent]; a != nil {
					a.Attempts = append(a.Attempts, AttemptView{
						Seq: l.Seq, Round: round, Bounty: id, Tier: b.Tier,
						Outcome: "voided", Reason: reason,
					})
				}
				delete(awardee, id)
			}

		case trace.EventBid:
			agent, bountyID := str("agent"), str("bounty")
			if a := v.agentByID[agent]; a != nil {
				a.Bids = append(a.Bids, BidView{
					Seq: l.Seq, Round: round, Bounty: bountyID, Price: credits("price"),
				})
			}

		case trace.EventCredit:
			if str("action") == "dust_burn" {
				if a := v.agentByID[str("agent")]; a != nil {
					a.Balance -= credits("amount")
					a.Timeline = append(a.Timeline, BalancePoint{l.Seq, a.Balance, "dust burned"})
				}
			}

		case trace.EventTown:
			if v.Town == nil {
				v.Town = &TownInfo{}
				// A fair trace announced its track before the town was
				// founded; only a bare town run needs the default.
				if v.Episode.Track == "" {
					v.Episode.Track = TrackTown
				}
			}
			switch str("action") {
			case "founded":
				v.Town.Name = str("town")
				if ps, ok := p["places"].([]any); ok {
					// Streets are scenery; the header counts destinations.
					for _, pl := range ps {
						if m, ok := pl.(map[string]any); ok && m["kind"] == "street" {
							continue
						}
						v.Town.Places++
					}
				}
				if rs, ok := p["residents"].([]any); ok {
					v.Town.Residents = len(rs)
				}
			case "tick":
				v.Town.Day = int(num("day"))
				v.Town.Clock = str("clock")
			case "met":
				v.Town.Meetings++
			case "said":
				v.Town.Utterances++
			case "reflected":
				v.Town.Reflections++
			}

		case trace.EventModelCall:
			var ev proxy.Event
			if err := json.Unmarshal(l.Payload, &ev); err != nil {
				return nil, fmt.Errorf("decode model_call seq %d: %w", l.Seq, err)
			}
			c := CallView{
				Seq: l.Seq, Wallet: ev.Wallet, Model: ev.Model,
				InToks: ev.Usage.InputTokens, OutToks: ev.Usage.OutputTokens,
				Cost: ev.Cost, Outcome: string(ev.Outcome),
			}
			if bountyID, _, ok := attemptWallet(ev.Wallet); ok {
				c.Agent = awardee[bountyID]
				if b := v.bountyByID[bountyID]; b != nil {
					b.Calls = append(b.Calls, c)
				}
			} else {
				c.Agent = ev.Wallet
			}
			if a := v.agentByID[c.Agent]; a != nil {
				a.CallCount++
				a.CallSpend += c.Cost
			}
			v.CallCount++
			v.CallSpend += c.Cost
		}
	}

	v.Ladder = ladder.Board(v.Episode.End.Add(time.Second))
	v.Asterisked = asterisked
	if v.Episode.Track == TrackSim || v.Episode.Track == TrackFair {
		// The sim's numbers are a chronicle, not a score. The rows survive —
		// they are what happened — but nothing here is ranked, because who was
		// idle when a bounty appeared is luck, and luck does not sort. The
		// fair sharpens that: who was *standing at the office* is a schedule.
		for i := range v.Ladder {
			v.Ladder[i].Ranked = false
			v.Ladder[i].Efficiency = 0
		}
	}
	return v, nil
}

// Unranked reports whether this view's track forbids a ranking.
func (v *View) Unranked() bool {
	return v.Episode.Track == TrackSim || v.Episode.Track == TrackFair
}
