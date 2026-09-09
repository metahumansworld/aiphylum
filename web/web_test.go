package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/metahumansworld/soscitea/internal/trace"
)

// miniTrace synthesises a two-round episode: agent a solves bounty x, agent b
// fails bounty y, goes broke and dies, and agent c watches both rounds and
// leaves with its grant. Small enough to assert every number.
func miniTrace(t *testing.T) []trace.Line {
	t.Helper()
	var lines []trace.Line
	at := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	add := func(typ trace.EventType, payload map[string]any) {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		at = at.Add(time.Second)
		lines = append(lines, trace.Line{Seq: int64(len(lines) + 1), Time: at, Type: typ, Payload: raw})
	}

	add(trace.EventAgent, map[string]any{"action": "spawned", "agent": "a", "grant": 100})
	add(trace.EventAgent, map[string]any{"action": "spawned", "agent": "b", "grant": 50})
	add(trace.EventAgent, map[string]any{"action": "spawned", "agent": "c", "grant": 20})
	add(trace.EventEpisode, map[string]any{"action": "start", "rounds": 2})

	add(trace.EventEpisode, map[string]any{"action": "round", "round": 1, "postings": 1})
	add(trace.EventBounty, map[string]any{"action": "posted", "id": "x", "generator": "arith", "seed": 7, "tier": 1, "max_payout": 30, "reserve": 3})
	add(trace.EventBid, map[string]any{"bounty": "x", "agent": "a", "price": 10})
	add(trace.EventBid, map[string]any{"bounty": "x", "agent": "b", "price": 12})
	add(trace.EventBounty, map[string]any{"action": "awarded", "id": "x", "winner": "a", "price": 10,
		"book": []map[string]any{{"agent": "a", "price": 10}, {"agent": "b", "price": 12}}})
	// Mirror the real proxy.Event shape: request/response bodies and a
	// per-payload clock, all of which the replay stream must strip.
	add(trace.EventModelCall, map[string]any{"wallet": "att:x:r1", "model": "stub-1",
		"time":    "2026-08-27T12:00:09Z",
		"request": map[string]any{"model": "stub-1"}, "response": map[string]any{"text": "ok"},
		"usage": map[string]any{"input_tokens": 3, "output_tokens": 1}, "cost": 4, "balance": 96, "outcome": "ok"})
	add(trace.EventBounty, map[string]any{"action": "solved", "id": "x", "agent": "a", "payout": 10, "burned": 4})

	add(trace.EventEpisode, map[string]any{"action": "round", "round": 2, "postings": 1})
	add(trace.EventBounty, map[string]any{"action": "posted", "id": "y", "generator": "oracle", "seed": 8, "tier": 2, "max_payout": 90, "reserve": 9})
	add(trace.EventBid, map[string]any{"bounty": "y", "agent": "b", "price": 20})
	add(trace.EventBounty, map[string]any{"action": "awarded", "id": "y", "winner": "b", "price": 20,
		"book": []map[string]any{{"agent": "b", "price": 20}}})
	add(trace.EventBounty, map[string]any{"action": "failed", "id": "y", "agent": "b", "reason": "wrong answer", "burned": 7})
	add(trace.EventCredit, map[string]any{"action": "dust_burn", "agent": "b", "amount": 43})
	add(trace.EventAgent, map[string]any{"action": "bankrupt", "agent": "b"})
	add(trace.EventAgent, map[string]any{"action": "left", "agent": "c", "balance": 20})

	add(trace.EventEpisode, map[string]any{"action": "end", "conservation": "minted=170 wallets=126 burned=43 spent=4 held=0 (drift=0)"})
	return lines
}

func TestBuildViewReconstructsTheEpisode(t *testing.T) {
	v, err := BuildView("mini.jsonl", miniTrace(t))
	if err != nil {
		t.Fatal(err)
	}

	a, b := v.Agent("a"), v.Agent("b")
	if a == nil || b == nil {
		t.Fatal("agents missing from view")
	}

	// Balance reconstruction must match the platform's bookkeeping: a solved
	// once (+10 −4), b failed once (−7) then had its dust burned (−43).
	if a.Balance != 106 {
		t.Errorf("a balance = %d, want 106", a.Balance)
	}
	if b.Balance != 0 {
		t.Errorf("b balance = %d, want 0", b.Balance)
	}
	if a.Bankrupt || !b.Bankrupt {
		t.Errorf("fates wrong: a bankrupt=%v b bankrupt=%v", a.Bankrupt, b.Bankrupt)
	}
	// Leaving is neither of those: the wallet keeps its balance and stays open.
	if c := v.Agent("c"); c == nil || !c.Left || c.Bankrupt || c.Balance != 20 {
		t.Errorf("c = %+v, want left with 20 and not bankrupt", c)
	}

	if a.Earned != 10 || a.Burned != 4 {
		t.Errorf("a earned/burned = %d/%d, want 10/4", a.Earned, a.Burned)
	}
	if b.Earned != 0 || b.Burned != 7 {
		t.Errorf("b earned/burned = %d/%d, want 0/7", b.Earned, b.Burned)
	}

	// Attempts carry the round the resolution happened in.
	if len(a.Attempts) != 1 || a.Attempts[0].Round != 1 || a.Attempts[0].Outcome != "solved" {
		t.Errorf("a attempts = %+v", a.Attempts)
	}
	if len(b.Attempts) != 1 || b.Attempts[0].Round != 2 || b.Attempts[0].Reason != "wrong answer" {
		t.Errorf("b attempts = %+v", b.Attempts)
	}

	// Winning bids are marked; losing ones are not.
	if len(a.Bids) != 1 || !a.Bids[0].Won {
		t.Errorf("a bids = %+v, want its x bid marked won", a.Bids)
	}
	if len(b.Bids) != 2 || b.Bids[0].Won || !b.Bids[1].Won {
		t.Errorf("b bids = %+v, want only its y bid marked won", b.Bids)
	}

	// The model call is attributed through the attempt wallet to the awardee
	// and to the bounty.
	x := v.Bounty("x")
	if x == nil || len(x.Calls) != 1 || x.Calls[0].Agent != "a" || x.Calls[0].Cost != 4 {
		t.Fatalf("bounty x calls = %+v", x.Calls)
	}
	if a.CallCount != 1 || a.CallSpend != 4 {
		t.Errorf("a calls = %d/%d, want 1/4", a.CallCount, a.CallSpend)
	}
	if v.CallCount != 1 || v.CallSpend != 4 {
		t.Errorf("view calls = %d/%d, want 1/4", v.CallCount, v.CallSpend)
	}

	if x.Status != "solved" || x.SolvedBy != "a" || x.Payout != 10 {
		t.Errorf("bounty x = %+v", x)
	}
	y := v.Bounty("y")
	if y.Status != "failed" || y.Failures != 1 {
		t.Errorf("bounty y = %+v", y)
	}

	// The ladder is recomputed from the same events through the real rating
	// package; both agents are below the entry gates, so both rows exist and
	// neither is ranked.
	if len(v.Ladder) != 2 {
		t.Fatalf("ladder rows = %d, want 2", len(v.Ladder))
	}
	for _, r := range v.Ladder {
		if r.Ranked {
			t.Errorf("agent %s ranked on 1 attempt; gates not applied", r.Agent)
		}
	}

	if v.Episode.Rounds != 2 || !strings.Contains(v.Episode.Conservation, "drift=0") {
		t.Errorf("episode = %+v", v.Episode)
	}
}

func TestServerPagesRender(t *testing.T) {
	srv, err := NewServer("mini.jsonl", miniTrace(t))
	if err != nil {
		t.Fatal(err)
	}

	get := func(path string) (int, string) {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	for _, tc := range []struct {
		path string
		want []string
	}{
		{"/", []string{"efficiency ladder", `href="/agent/a"`, `href="/bounty/x"`, "drift=0", `href="/agent/c"`, `class="badge gone">left`}},
		{"/agent/a", []string{"solved", "106", `href="/bounty/x"`}},
		{"/agent/b", []string{"bankrupt", "wrong answer"}},
		{"/agent/c", []string{`class="badge gone">left`, "20"}},
		{"/bounty/x", []string{"awarded", "stub-1", "arith"}},
		{"/bounty/y", []string{"failed", "oracle"}},
		{"/replay", []string{"window.EVENTS", "replay.js"}},
		{"/static/style.css", []string{"--accent"}},
		{"/static/replay.js", []string{"reduce"}},
	} {
		code, body := get(tc.path)
		if code != 200 {
			t.Errorf("GET %s = %d, want 200", tc.path, code)
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(body, w) {
				t.Errorf("GET %s: missing %q", tc.path, w)
			}
		}
	}

	if code, _ := get("/agent/nobody"); code != 404 {
		t.Errorf("GET /agent/nobody = %d, want 404", code)
	}
	if code, _ := get("/bounty/nothing"); code != 404 {
		t.Errorf("GET /bounty/nothing = %d, want 404", code)
	}

	// The replay stream must not leak call bodies: they are the private-ish
	// bulk; the viewer shows money, not bytes. Decode the stream and pin the
	// model_call event — both its fields and its pre-worded label, since a
	// label worded from the uncleaned line leaks the body even after the
	// fields are deleted.
	var events []map[string]any
	if err := json.Unmarshal([]byte(srv.replay), &events); err != nil {
		t.Fatalf("replay stream is not valid JSON: %v", err)
	}
	var call map[string]any
	for _, e := range events {
		if e["type"] == "model_call" {
			call = e
			break
		}
	}
	if call == nil {
		t.Fatal("no model_call event in the replay stream")
	}
	for _, k := range []string{"request", "response", "time"} {
		if _, leaked := call[k]; leaked {
			t.Errorf("replay model_call event carries %q", k)
		}
	}
	want := `balance=96 cost=4 model="stub-1" outcome="ok" usage={"input_tokens":3,"output_tokens":1} wallet="att:x:r1"`
	if call["label"] != want {
		t.Errorf("model_call label = %q, want %q", call["label"], want)
	}
}

// judgedTrace: one agent, three bounties in one round — a keyed one it solves,
// a judged one it passes, and a judged one it fails. Everything about the
// three looks alike to the balance arithmetic; only the ladder should tell
// them apart.
func judgedTrace(t *testing.T) []trace.Line {
	t.Helper()
	var lines []trace.Line
	at := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	add := func(typ trace.EventType, payload map[string]any) {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		at = at.Add(time.Second)
		lines = append(lines, trace.Line{Seq: int64(len(lines) + 1), Time: at, Type: typ, Payload: raw})
	}

	add(trace.EventAgent, map[string]any{"action": "spawned", "agent": "j", "grant": 100})
	add(trace.EventEpisode, map[string]any{"action": "start", "rounds": 1})
	add(trace.EventEpisode, map[string]any{"action": "round", "round": 1, "postings": 3})

	add(trace.EventBounty, map[string]any{"action": "posted", "id": "k", "generator": "arith", "seed": 1, "tier": 1, "max_payout": 30, "reserve": 3})
	add(trace.EventBid, map[string]any{"bounty": "k", "agent": "j", "price": 10})
	add(trace.EventBounty, map[string]any{"action": "awarded", "id": "k", "winner": "j", "price": 10})
	add(trace.EventBounty, map[string]any{"action": "solved", "id": "k", "agent": "j", "payout": 10, "burned": 4})

	// A judged posting carries the flag; the ladder must not hear about it
	// even though it resolves exactly like the keyed one above.
	add(trace.EventBounty, map[string]any{"action": "posted", "id": "g", "generator": "brief", "seed": 2, "tier": 3, "max_payout": 40, "reserve": 4, "judged": true})
	add(trace.EventBid, map[string]any{"bounty": "g", "agent": "j", "price": 12})
	add(trace.EventBounty, map[string]any{"action": "awarded", "id": "g", "winner": "j", "price": 12})
	add(trace.EventBounty, map[string]any{"action": "judged", "id": "g", "agent": "j", "pass": true, "grader": "stub-1", "reason": "covers the standard"})
	add(trace.EventBounty, map[string]any{"action": "solved", "id": "g", "agent": "j", "payout": 12, "burned": 5})

	add(trace.EventBounty, map[string]any{"action": "posted", "id": "h", "generator": "brief", "seed": 3, "tier": 2, "max_payout": 40, "reserve": 4, "judged": true})
	add(trace.EventBid, map[string]any{"bounty": "h", "agent": "j", "price": 12})
	add(trace.EventBounty, map[string]any{"action": "awarded", "id": "h", "winner": "j", "price": 12})
	add(trace.EventBounty, map[string]any{"action": "judged", "id": "h", "agent": "j", "pass": false, "grader": "stub-1", "reason": "ignores the form"})
	add(trace.EventBounty, map[string]any{"action": "failed", "id": "h", "agent": "j", "reason": "judged: ignores the form", "burned": 6})

	add(trace.EventEpisode, map[string]any{"action": "end", "conservation": "minted=122 wallets=107 burned=0 spent=15 held=0 (drift=0)"})
	return lines
}

// The orchestrator refuses to post judged supply into a ranked world, but the
// viewer rebuilds the ladder from the trace rather than copying one — so the
// rule has to hold a second time, here, against a trace that contains judged
// resolutions. A model's opinion must move the wallet and never the ranking.
func TestViewerKeepsJudgedWorkOffTheLadder(t *testing.T) {
	v, err := BuildView("judged.jsonl", judgedTrace(t))
	if err != nil {
		t.Fatal(err)
	}

	j := v.Agent("j")
	if j == nil {
		t.Fatal("agent j missing from view")
	}
	// The wallet counts all three: judged work is paid work.
	if j.Earned != 22 || j.Burned != 15 || j.Balance != 107 {
		t.Errorf("agent j earned/burned/balance = %d/%d/%d, want 22/15/107", j.Earned, j.Burned, j.Balance)
	}
	if len(j.Attempts) != 3 {
		t.Errorf("agent j attempts = %d, want 3", len(j.Attempts))
	}

	// The ladder counts one: the keyed bounty, and only its numbers.
	if len(v.Ladder) != 1 {
		t.Fatalf("ladder rows = %d, want 1", len(v.Ladder))
	}
	row := v.Ladder[0]
	if row.Agent != "j" {
		t.Fatalf("ladder row = %+v, want agent j", row)
	}
	if row.Attempts != 1 || row.Successes != 1 {
		t.Errorf("ladder attempts/successes = %d/%d, want 1/1 — judged work reached the ladder",
			row.Attempts, row.Successes)
	}
	if row.Earned != 10 || row.Burned != 4 {
		t.Errorf("ladder earned/burned = %d/%d, want 10/4 — judged credits reached the ladder",
			row.Earned, row.Burned)
	}
	// Three attempts across three tiers would have cleared the gates; one
	// keyed attempt at one tier must not.
	if row.Ranked {
		t.Error("agent ranked on a single keyed attempt; judged work filled the gates")
	}

	// The flag and the verdict both survive into the bounty page.
	g := v.Bounty("g")
	if g == nil || !g.Judged {
		t.Fatalf("bounty g = %+v, want judged", g)
	}
	if v.Bounty("k").Judged {
		t.Error("keyed bounty k came back judged")
	}
	var verdict string
	for _, e := range g.History {
		if e.Action == "judged" {
			verdict = e.Detail
		}
	}
	if !strings.Contains(verdict, "passed by stub-1") || !strings.Contains(verdict, "covers the standard") {
		t.Errorf("judged history detail = %q", verdict)
	}
}

// importedTrace: one agent, two bounties in one round — one generated, one
// drawn from an imported suite. Structurally identical to judgedTrace, which
// is the point: the same shape must produce the opposite ladder answer.
func importedTrace(t *testing.T) []trace.Line {
	t.Helper()
	var lines []trace.Line
	at := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	add := func(typ trace.EventType, payload map[string]any) {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		at = at.Add(time.Second)
		lines = append(lines, trace.Line{Seq: int64(len(lines) + 1), Time: at, Type: typ, Payload: raw})
	}

	add(trace.EventAgent, map[string]any{"action": "spawned", "agent": "i", "grant": 100})
	add(trace.EventAgent, map[string]any{"action": "spawned", "agent": "n", "grant": 100})
	add(trace.EventEpisode, map[string]any{"action": "start", "rounds": 1})
	add(trace.EventSuite, map[string]any{
		"action": "registered", "suite": "handbook", "source": "a public benchmark",
		"licence": "CC0-1.0", "contamination": "public since 2021; assume exposure",
	})
	add(trace.EventEpisode, map[string]any{"action": "round", "round": 1, "postings": 2})

	add(trace.EventBounty, map[string]any{"action": "posted", "id": "k", "generator": "arith", "seed": 1, "tier": 1, "max_payout": 30, "reserve": 3})
	add(trace.EventBid, map[string]any{"bounty": "k", "agent": "n", "price": 10})
	add(trace.EventBounty, map[string]any{"action": "awarded", "id": "k", "winner": "n", "price": 10})
	add(trace.EventBounty, map[string]any{"action": "solved", "id": "k", "agent": "n", "payout": 10, "burned": 4})

	add(trace.EventBounty, map[string]any{"action": "posted", "id": "m", "generator": "handbook", "seed": 2, "tier": 2, "max_payout": 40, "reserve": 4, "suite": "handbook"})
	add(trace.EventBid, map[string]any{"bounty": "m", "agent": "i", "price": 12})
	add(trace.EventBounty, map[string]any{"action": "awarded", "id": "m", "winner": "i", "price": 12})
	add(trace.EventBounty, map[string]any{"action": "solved", "id": "m", "agent": "i", "payout": 12, "burned": 5})

	add(trace.EventEpisode, map[string]any{"action": "end", "conservation": "minted=222 wallets=213 burned=0 spent=9 held=0 (drift=0)"})
	return lines
}

// Imported work is the deliberate opposite of judged work: it reaches the
// ladder, because a held-out answer key is a held-out answer key wherever it
// came from. What it also does is mark the agent, so a reader weighing the
// score can find the contamination note that explains the mark.
func TestViewerRanksImportedWorkWithAnAsterisk(t *testing.T) {
	v, err := BuildView("imported.jsonl", importedTrace(t))
	if err != nil {
		t.Fatal(err)
	}

	// The suite's provenance survives the trip through the trace intact —
	// without it the asterisk would be a mark with nothing behind it.
	if len(v.Suites) != 1 {
		t.Fatalf("suites = %+v, want one", v.Suites)
	}
	s := v.Suites[0]
	if s.Name != "handbook" || s.Licence != "CC0-1.0" {
		t.Errorf("suite = %+v", s)
	}
	if !strings.Contains(s.Contamination, "assume exposure") {
		t.Errorf("contamination note = %q", s.Contamination)
	}

	// Both agents are on the ladder with their imported/generated work
	// counted. The ranking cannot see the asterisk.
	byAgent := map[string]int{}
	for i, row := range v.Ladder {
		byAgent[row.Agent] = i
		if row.Attempts != 1 || row.Successes != 1 {
			t.Errorf("%s attempts/successes = %d/%d, want 1/1", row.Agent, row.Attempts, row.Successes)
		}
	}
	if len(v.Ladder) != 2 {
		t.Fatalf("ladder rows = %d, want 2 — imported work must be ranked, not dropped", len(v.Ladder))
	}
	if i, ok := byAgent["i"]; !ok {
		t.Fatal("the imported bounty's solver is missing from the ladder")
	} else if v.Ladder[i].Earned != 12 || v.Ladder[i].Burned != 5 {
		t.Errorf("imported earned/burned = %d/%d, want 12/5",
			v.Ladder[i].Earned, v.Ladder[i].Burned)
	}

	// Only the agent who did imported work is marked.
	if !v.Asterisked["i"] {
		t.Error("the agent who solved an imported bounty is not asterisked")
	}
	if v.Asterisked["n"] {
		t.Error("an agent who only did generated work was asterisked")
	}

	// And the label reaches the bounty page.
	m := v.Bounty("m")
	if m == nil || m.Suite != "handbook" {
		t.Fatalf("bounty m = %+v, want suite handbook", m)
	}
	if v.Bounty("k").Suite != "" {
		t.Error("a generated bounty came back with a suite")
	}
	if !strings.Contains(m.History[0].Detail, "imported from handbook") {
		t.Errorf("posting detail = %q", m.History[0].Detail)
	}
}

// A stay is the one purchase in the tree that buys no work and moves no money
// to anybody — it just leaves the wallet. The viewer reconstructs every balance
// from the stream alone, so it has to account for that, and it has to account
// for it in both places a burn shows up. Otherwise the four numbers at the top
// of an agent's page stop adding up and the page quietly lies by the exact
// amount the agent spent standing still.
func TestViewerCountsAStayAsMoneyGone(t *testing.T) {
	var lines []trace.Line
	at := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	add := func(typ trace.EventType, payload map[string]any) {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		at = at.Add(time.Second)
		lines = append(lines, trace.Line{Seq: int64(len(lines) + 1), Time: at, Type: typ, Payload: raw})
	}

	add(trace.EventAgent, map[string]any{"action": "spawned", "agent": "vigil", "grant": 500})
	add(trace.EventEpisode, map[string]any{"action": "start", "track": "fair"})
	add(trace.EventTown, map[string]any{"action": "founded", "town": "Ashmere",
		"places":    []map[string]any{{"id": "office", "kind": "building"}, {"id": "lane", "kind": "street"}},
		"residents": []map[string]any{{"id": "vigil"}}})
	add(trace.EventBounty, map[string]any{"action": "posted", "id": "x", "generator": "arith", "seed": 7, "tier": 1, "max_payout": 60, "reserve": 6})
	add(trace.EventCredit, map[string]any{"action": "stayed", "agent": "vigil", "amount": 24, "place": "office", "ticks": 3, "until": 9})
	add(trace.EventBid, map[string]any{"bounty": "x", "agent": "vigil", "price": 40})
	add(trace.EventBounty, map[string]any{"action": "awarded", "id": "x", "winner": "vigil", "price": 40,
		"book": []map[string]any{{"agent": "vigil", "price": 40}}})
	add(trace.EventBounty, map[string]any{"action": "solved", "id": "x", "agent": "vigil", "payout": 40, "burned": 6})
	add(trace.EventCredit, map[string]any{"action": "stayed", "agent": "vigil", "amount": 24, "place": "office", "ticks": 3, "until": 13})
	add(trace.EventCredit, map[string]any{"action": "bought", "agent": "vigil", "amount": 100, "place": "office", "item": "notebook", "memo_bytes": 4096})
	add(trace.EventEpisode, map[string]any{"action": "end"})

	v, err := BuildView("stay.jsonl", lines)
	if err != nil {
		t.Fatal(err)
	}
	a := v.Agent("vigil")
	if a == nil {
		t.Fatal("vigil missing from view")
	}

	// 500 granted, 40 earned, 6 burned attempting, 48 burned standing still,
	// 100 burned on the notebook.
	if a.Earned != 40 {
		t.Errorf("earned = %d, want 40", a.Earned)
	}
	if a.Burned != 154 {
		t.Errorf("burned = %d, want 154 (6 attempting + 48 standing still + 100 bought)", a.Burned)
	}
	if a.Balance != 386 {
		t.Errorf("balance = %d, want 386", a.Balance)
	}
	// The identity the agent page's four stats are read as.
	if got := a.Grant + a.Earned - a.Burned; got != a.Balance {
		t.Errorf("grant+earned−burned = %d, but balance = %d", got, a.Balance)
	}

	// Both purchases have to be on the line, or the chart draws one step where
	// two credits left. The label is what tells a reader why it stepped.
	var stays []BalancePoint
	for _, p := range a.Timeline {
		if strings.HasPrefix(p.Label, "stayed at ") {
			stays = append(stays, p)
		}
	}
	if len(stays) != 2 || stays[0].Balance != 476 || stays[1].Balance != 486 {
		t.Errorf("stay points on the balance line = %+v, want two, at 476 and 486", stays)
	}
	if stays[0].Label != "stayed at office" {
		t.Errorf("stay label = %q", stays[0].Label)
	}

	// The purchase is its own step on the line, labelled with what was bought.
	last := a.Timeline[len(a.Timeline)-1]
	if last.Balance != 386 || last.Label != "bought a notebook" {
		t.Errorf("last point on the balance line = %+v, want 386 labelled \"bought a notebook\"", last)
	}

	if v.Town == nil || v.Town.Stays != 2 {
		t.Errorf("town stays = %+v, want 2", v.Town)
	}
}

// A memo is the one action an agent can take that the platform stores without
// reading and that costs nothing, settles nothing, and pays nobody. So the
// viewer's job is to leave every number exactly where it was — and the way to
// assert that is not to check a memo-shaped field but to run the same episode
// twice, once with memos threaded through it and once without, and demand the
// money come out the same.
//
// Seq numbers legitimately differ between the two runs, because the memo lines
// take slots in the stream. Everything that is about money must not.
func TestMemosMoveNoMoneyInTheViewer(t *testing.T) {
	build := func(withMemos bool) *View {
		t.Helper()
		var lines []trace.Line
		at := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
		add := func(typ trace.EventType, payload map[string]any) {
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			at = at.Add(time.Second)
			lines = append(lines, trace.Line{Seq: int64(len(lines) + 1), Time: at, Type: typ, Payload: raw})
		}
		memo := func(text string) {
			if withMemos {
				add(trace.EventAgent, map[string]any{"action": "memo", "agent": "scribe", "memo": text})
			}
		}

		add(trace.EventAgent, map[string]any{"action": "spawned", "agent": "scribe", "grant": 500})
		add(trace.EventEpisode, map[string]any{"action": "start", "track": "fair"})
		add(trace.EventBounty, map[string]any{"action": "posted", "id": "x", "generator": "arith", "seed": 7, "tier": 1, "max_payout": 60, "reserve": 6})
		memo(`{"p":0,"w":0,"a":0}`)
		add(trace.EventBid, map[string]any{"bounty": "x", "agent": "scribe", "price": 40})
		add(trace.EventBounty, map[string]any{"action": "awarded", "id": "x", "winner": "scribe", "price": 40,
			"book": []map[string]any{{"agent": "scribe", "price": 40}}})
		memo(`{"p":0,"w":0,"a":1}`)
		add(trace.EventBounty, map[string]any{"action": "solved", "id": "x", "agent": "scribe", "payout": 40, "burned": 6})
		memo(`{"p":0,"w":34,"a":1}`)
		add(trace.EventEpisode, map[string]any{"action": "end"})

		v, err := BuildView("memo.jsonl", lines)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}

	with, without := build(true), build(false)
	a, b := with.Agent("scribe"), without.Agent("scribe")
	if a == nil || b == nil {
		t.Fatal("scribe missing from one of the views")
	}
	if a.Grant != b.Grant || a.Earned != b.Earned || a.Burned != b.Burned || a.Balance != b.Balance {
		t.Errorf("memos moved money: with = %d/%d/%d/%d, without = %d/%d/%d/%d",
			a.Grant, a.Earned, a.Burned, a.Balance, b.Grant, b.Earned, b.Burned, b.Balance)
	}
	if len(a.Timeline) != len(b.Timeline) {
		t.Fatalf("balance line has %d points with memos and %d without", len(a.Timeline), len(b.Timeline))
	}
	for i := range a.Timeline {
		if a.Timeline[i].Balance != b.Timeline[i].Balance || a.Timeline[i].Label != b.Timeline[i].Label {
			t.Errorf("balance point %d = %+v with memos, %+v without", i, a.Timeline[i], b.Timeline[i])
		}
	}
}
