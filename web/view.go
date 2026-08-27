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

	"github.com/metahunmei/dungeon/internal/ledger"
	"github.com/metahunmei/dungeon/internal/proxy"
	"github.com/metahunmei/dungeon/internal/rating"
	"github.com/metahunmei/dungeon/internal/trace"
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

	CallCount int
	CallSpend ledger.Credits

	agentByID  map[string]*AgentView
	bountyByID map[string]*BountyView
}

// The two tracks a trace can come from. They share one currency and one money
// path; only the clock and the ranking differ.
const (
	TrackBenchmark = "benchmark"
	TrackSim       = "sim"
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

	Status   string // open, solved, failed, voided, no bids
	SolvedBy string
	Payout   ledger.Credits
	Failures int

	History []BountyEventView
	Calls   []CallView
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
// money, the full bytes stay in the trace file for replay and dungeonctl.
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
				}
				v.Bounties = append(v.Bounties, b)
				v.bountyByID[id] = b
				b.History = append(b.History, BountyEventView{
					Seq: l.Seq, Round: round, Action: "posted",
					Detail: fmt.Sprintf("tier %d, max payout %d, reserve %d", b.Tier, b.MaxPayout, b.Reserve),
				})
			case "awarded":
				if b == nil {
					continue
				}
				winner := str("winner")
				awardee[id] = winner
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
							ag, _ := m["agent"].(string)
							pr, _ := m["price"].(float64)
							ev.Book = append(ev.Book, BookEntry{ag, ledger.Credits(pr)})
						}
					}
				}
				b.History = append(b.History, ev)
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
				ladder.Record(rating.Attempt{
					Agent: agent, Bounty: id, Tier: b.Tier,
					Earned: payout, Burned: burned, Success: true, Time: l.Time,
				})
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
				ladder.Record(rating.Attempt{
					Agent: agent, Bounty: id, Tier: b.Tier,
					Burned: burned, Success: false, Time: l.Time,
				})
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
	if v.Episode.Track == TrackSim {
		// The sim's numbers are a chronicle, not a score. The rows survive —
		// they are what happened — but nothing here is ranked, because who was
		// idle when a bounty appeared is luck, and luck does not sort.
		for i := range v.Ladder {
			v.Ladder[i].Ranked = false
			v.Ladder[i].Efficiency = 0
		}
	}
	return v, nil
}

// Unranked reports whether this view's track forbids a ranking.
func (v *View) Unranked() bool { return v.Episode.Track == TrackSim }
