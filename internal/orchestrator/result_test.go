package orchestrator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/singhtushant3-hub/aiphylum/internal/ledger"
	"github.com/singhtushant3-hub/aiphylum/internal/proxy"
	"github.com/singhtushant3-hub/aiphylum/internal/trace"
)

// bidFixed asks the same price on every open bounty, so a test can set the
// book by choosing three numbers and be sure which one wins.
func bidFixed(price ledger.Credits) stepFunc {
	return func(_ StepRequest, in StepInput) StepResult {
		var acts []Action
		for _, b := range in.Observation.Bounties {
			acts = append(acts, Action{Type: ActionBid, Bounty: b.ID, Price: price})
		}
		return out(acts...)
	}
}

// collect records what an agent is told at each of its bid steps, and bids the
// given price so that it keeps being told things.
func collect(price ledger.Credits, into *[][]AuctionResult) stepFunc {
	inner := bidFixed(price)
	return func(req StepRequest, in StepInput) StepResult {
		*into = append(*into, in.Observation.Results)
		return inner(req, in)
	}
}

// The gap this closes, stated as a test: an agent that loses an auction is
// told that it lost and what the work went for. Before this it saw the same
// thing whether it had lost, been refused, or watched the bounty be shelved —
// a board with that bounty still on it.
func TestLoserIsToldTheClearingPrice(t *testing.T) {
	w := newWorld(t, &proxy.StubProvider{}, nil, Config{})
	w.add(t, "cheap", 4000)
	w.add(t, "dear", 4000)

	var cheapSaw, dearSaw [][]AuctionResult
	w.steps.fns["cheap"] = script(collect(40, &cheapSaw), solve)
	w.steps.fns["dear"] = script(collect(90, &dearSaw), solve)

	if err := w.orch.RunEpisode(w.ctx, twoRounds()); err != nil {
		t.Fatal(err)
	}
	if len(dearSaw) != 2 {
		t.Fatalf("dear got %d bid steps, want 2", len(dearSaw))
	}
	// Round 1: nothing has closed yet, so there is nothing to report.
	if len(dearSaw[0]) != 0 {
		t.Errorf("first observation carries %d results, want none", len(dearSaw[0]))
	}
	if len(dearSaw[1]) != 1 {
		t.Fatalf("second observation carries %d results, want 1", len(dearSaw[1]))
	}
	got := dearSaw[1][0]
	if got.Won {
		t.Error("dear asked 90 against 40 and was told it won")
	}
	if got.Asked != 90 {
		t.Errorf("Asked = %d, want the 90 it actually bid", got.Asked)
	}
	if got.Clearing != 40 || got.Winner != "cheap" {
		t.Errorf("cleared at %d to %q, want 40 to cheap", got.Clearing, got.Winner)
	}
	if got.Bidders != 2 {
		t.Errorf("Bidders = %d, want 2", got.Bidders)
	}
	// And the winner is told too, because knowing you won is not the same
	// fact as not being told you lost.
	if len(cheapSaw[1]) != 1 || !cheapSaw[1][0].Won {
		t.Fatalf("winner's second observation = %+v, want one result marked won", cheapSaw[1])
	}
}

// Winning a sealed first-price auction tells you that you were lowest and
// nothing at all about by how much. The platform must not quietly improve on
// that: the winner's clearing price is its own ask, and the runner-up's number
// is not in the report anywhere.
func TestWinningTeachesNothingButThatYouWon(t *testing.T) {
	w := newWorld(t, &proxy.StubProvider{}, nil, Config{})
	w.add(t, "cheap", 4000)
	w.add(t, "dear", 4000)

	var cheapSaw [][]AuctionResult
	w.steps.fns["cheap"] = script(collect(40, &cheapSaw), solve)
	w.steps.fns["dear"] = script(bidFixed(90), solve)

	if err := w.orch.RunEpisode(w.ctx, twoRounds()); err != nil {
		t.Fatal(err)
	}
	if len(cheapSaw) != 2 || len(cheapSaw[1]) != 1 {
		t.Fatalf("winner saw %v, want one result at its second step", cheapSaw)
	}
	got := cheapSaw[1][0]
	if got.Clearing != got.Asked {
		t.Errorf("winner told it cleared at %d having asked %d; the winner's clearing price is its own ask", got.Clearing, got.Asked)
	}
	blob, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "90") {
		t.Errorf("the runner-up's ask leaked into the winner's report: %s", blob)
	}
	if !strings.Contains(string(blob), `"won":true`) {
		t.Errorf("the winner's result did not say so on the wire: %s", blob)
	}
}

// The seal, in the one place it still has to hold. The whole book goes into
// the trace at award, because the trace is the audit record; it must not go
// back to the bidders, because a repeated auction in which everyone reads
// everyone's ask is one that walks straight down to the reserve floor.
//
// Three asks make the property checkable: each loser must learn the clearing
// price and its own number, and neither must learn the other's.
func TestResultsNeverCarryTheLosingBook(t *testing.T) {
	w := newWorld(t, &proxy.StubProvider{}, nil, Config{})
	for _, id := range []string{"winner", "middle", "highest"} {
		w.add(t, id, 4000)
	}
	var middleSaw, highestSaw [][]AuctionResult
	w.steps.fns["winner"] = script(bidFixed(40), solve)
	w.steps.fns["middle"] = script(collect(90, &middleSaw), solve)
	w.steps.fns["highest"] = script(collect(95, &highestSaw), solve)

	if err := w.orch.RunEpisode(w.ctx, twoRounds()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		who       string
		saw       [][]AuctionResult
		own, hidd string
	}{
		{"middle", middleSaw, "90", "95"},
		{"highest", highestSaw, "95", "90"},
	} {
		if len(tc.saw) != 2 || len(tc.saw[1]) != 1 {
			t.Fatalf("%s saw %v, want one result at its second step", tc.who, tc.saw)
		}
		blob, err := json.Marshal(tc.saw[1][0])
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(blob), tc.own) {
			t.Errorf("%s was not told its own ask %s: %s", tc.who, tc.own, blob)
		}
		if strings.Contains(string(blob), tc.hidd) {
			t.Errorf("%s learned a fellow loser's ask %s: %s", tc.who, tc.hidd, blob)
		}
		// The tag is `omitempty`, so a loss carries no "won" key at all. The
		// docs say so in three places; this is the one that checks it.
		if strings.Contains(string(blob), `"won"`) {
			t.Errorf("%s's loss carried a won key: %s", tc.who, blob)
		}
	}
}

// Participation is the gate. An agent that did not bid is not told how the
// auction went, however loudly it was standing next to it.
func TestOnlyBiddersAreTold(t *testing.T) {
	w := newWorld(t, &proxy.StubProvider{}, nil, Config{})
	w.add(t, "player", 4000)
	w.add(t, "bystander", 4000)

	var bystanderSaw [][]AuctionResult
	w.steps.fns["player"] = script(bidFixed(40), solve)
	w.steps.fns["bystander"] = script(func(_ StepRequest, in StepInput) StepResult {
		bystanderSaw = append(bystanderSaw, in.Observation.Results)
		return out() // watches, never bids
	}, solve)

	if err := w.orch.RunEpisode(w.ctx, twoRounds()); err != nil {
		t.Fatal(err)
	}
	for i, rs := range bystanderSaw {
		if len(rs) != 0 {
			t.Errorf("bystander's step %d was told %d results without bidding: %+v", i, len(rs), rs)
		}
	}
}

// An agent that bid on nothing gets an observation with no results key at all
// — not an empty list, no key. Same property the memo has, and for the same
// reason: it is what lets the mechanism exist on every track without changing
// a byte of what the tracks that ignore it see.
func TestBidlessObservationHasNoResultsKey(t *testing.T) {
	w := newWorld(t, &proxy.StubProvider{}, nil, Config{})
	w.add(t, "quiet", 1000)

	var raw [][]byte
	w.steps.fns["quiet"] = script(func(req StepRequest, in StepInput) StepResult {
		raw = append(raw, req.Input)
		return out()
	}, solve)

	if err := w.orch.RunEpisode(w.ctx, twoRounds()); err != nil {
		t.Fatal(err)
	}
	for i, in := range raw {
		var envelope struct {
			Observation map[string]json.RawMessage `json:"observation"`
		}
		if err := json.Unmarshal(in, &envelope); err != nil {
			t.Fatal(err)
		}
		if _, present := envelope.Observation["results"]; present {
			t.Errorf("step %d carries a results key for an agent that never bid: %s", i, in)
		}
	}
}

// Announced once. The platform says the price when the auction closes and then
// stops saying it; carrying it further is what the memo is for. Three rounds
// is the smallest episode that can tell "delivered once" from "delivered
// always" — round 3 must hear about round 2 and not again about round 1.
func TestResultsAreAnnouncedOnce(t *testing.T) {
	w := newWorld(t, &proxy.StubProvider{}, nil, Config{})
	w.add(t, "cheap", 6000)
	w.add(t, "dear", 6000)

	var dearSaw [][]AuctionResult
	w.steps.fns["cheap"] = script(bidFixed(40), solve)
	w.steps.fns["dear"] = script(collect(90, &dearSaw), solve)

	three := Episode{Rounds: [][]Posting{{post(1, 1)}, {post(2, 1)}, {post(3, 1)}}}
	if err := w.orch.RunEpisode(w.ctx, three); err != nil {
		t.Fatal(err)
	}
	if len(dearSaw) != 3 {
		t.Fatalf("dear got %d bid steps, want 3", len(dearSaw))
	}
	for i, want := range []int{0, 1, 1} {
		if len(dearSaw[i]) != want {
			t.Errorf("step %d was told %d results, want %d: %+v", i, len(dearSaw[i]), want, dearSaw[i])
		}
	}
	// Each round's own auction, once each — not round 1's arriving twice.
	if len(dearSaw[1]) == 1 && len(dearSaw[2]) == 1 && dearSaw[1][0].Bounty == dearSaw[2][0].Bounty {
		t.Errorf("the same auction was announced twice: %+v", dearSaw[1][0])
	}
}

// A bid a bounty carries over rounds is announced when its auction closes, and
// a bounty that drew no bids is announced to nobody — there is no one to tell.
func TestNoBidsAnnouncesNothing(t *testing.T) {
	w := newWorld(t, &proxy.StubProvider{}, nil, Config{})
	w.add(t, "abstainer", 4000)

	var saw [][]AuctionResult
	w.steps.fns["abstainer"] = script(func(_ StepRequest, in StepInput) StepResult {
		saw = append(saw, in.Observation.Results)
		return out()
	}, solve)

	if err := w.orch.RunEpisode(w.ctx, twoRounds()); err != nil {
		t.Fatal(err)
	}
	for i, rs := range saw {
		if len(rs) != 0 {
			t.Errorf("step %d heard %d results from an auction nobody bid in: %+v", i, len(rs), rs)
		}
	}
}

// The claim in AuctionResult's doc comment, checked rather than asserted:
// every result is a function of the "awarded" event plus the identity of the
// bidder being told. That is what buys the right to announce outcomes without
// writing a single extra byte to the trace — a reader can reconstruct exactly
// what each agent was told, so a per-bidder line would only repeat the award.
func TestResultsAreDerivableFromTheTraceAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "announce.jsonl")
	tw, err := trace.NewWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	w := newWorld(t, &proxy.StubProvider{}, tw, Config{})
	for _, id := range []string{"cheap", "middle", "dear"} {
		w.add(t, id, 6000)
	}
	told := map[string][]AuctionResult{}
	for id, price := range map[string]ledger.Credits{"cheap": 40, "middle": 90, "dear": 95} {
		id, inner := id, bidFixed(price)
		w.steps.fns[id] = script(func(req StepRequest, in StepInput) StepResult {
			told[id] = append(told[id], in.Observation.Results...)
			return inner(req, in)
		}, solve)
	}
	three := Episode{Rounds: [][]Posting{{post(1, 1)}, {post(2, 1)}, {post(3, 1)}}}
	if err := w.orch.RunEpisode(w.ctx, three); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	// Rebuild, from the awards alone, what each agent must have been told.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := map[string][]AuctionResult{}
	round := 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var ev struct {
			Type    string `json:"type"`
			Payload struct {
				Action string         `json:"action"`
				ID     string         `json:"id"`
				Round  int            `json:"round"`
				Winner string         `json:"winner"`
				Price  ledger.Credits `json:"price"`
				Book   []struct {
					Agent string
					Price ledger.Credits
				} `json:"book"`
			} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue // model_call events carry a different payload shape
		}
		if ev.Type == string(trace.EventEpisode) && ev.Payload.Action == "round" {
			round = ev.Payload.Round
		}
		if ev.Type != string(trace.EventBounty) || ev.Payload.Action != "awarded" {
			continue
		}
		for _, b := range ev.Payload.Book {
			rebuilt[b.Agent] = append(rebuilt[b.Agent], AuctionResult{
				Bounty: ev.Payload.ID, Round: round, Asked: b.Price,
				Won:      b.Agent == ev.Payload.Winner,
				Clearing: ev.Payload.Price, Winner: ev.Payload.Winner,
				Bidders: len(ev.Payload.Book),
			})
		}
	}
	if len(rebuilt) == 0 {
		t.Fatal("no awards in the trace; the test proves nothing")
	}
	delivered := 0
	for _, rs := range told {
		delivered += len(rs)
	}
	if delivered == 0 {
		t.Fatal("nobody was told anything; a comparison against the trace proves nothing")
	}
	for agent, want := range rebuilt {
		// The last round's outcome is never delivered — the episode ends
		// before there is another bid step to deliver it at.
		got := told[agent]
		if len(got) > len(want) {
			t.Fatalf("%s was told %d results but the trace explains only %d", agent, len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s result %d: told %+v, trace says %+v", agent, i, got[i], want[i])
			}
		}
	}
}
