package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/metahunmei/dungeon/internal/trace"
)

// miniTrace synthesises a two-round episode: agent a solves bounty x, agent b
// fails bounty y, goes broke and dies. Small enough to assert every number.
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

	add(trace.EventEpisode, map[string]any{"action": "end", "conservation": "minted=150 wallets=106 burned=43 spent=4 held=0 (drift=0)"})
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
		{"/", []string{"efficiency ladder", `href="/agent/a"`, `href="/bounty/x"`, "drift=0"}},
		{"/agent/a", []string{"solved", "106", `href="/bounty/x"`}},
		{"/agent/b", []string{"bankrupt", "wrong answer"}},
		{"/bounty/x", []string{"awarded", "stub-1", "arith"}},
		{"/bounty/y", []string{"failed", "oracle"}},
		{"/replay", []string{"window.EVENTS", "replay.js"}},
		{"/static/style.css", []string{"--gold"}},
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
