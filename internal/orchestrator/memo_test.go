package orchestrator

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/metahumansworld/aiphylum/internal/proxy"
	"github.com/metahumansworld/aiphylum/internal/trace"
)

// twoRounds posts one bounty per round, so an agent gets two bid steps and can
// be asked what it remembers of the first at the second.
func twoRounds() Episode {
	return Episode{Rounds: [][]Posting{{post(1, 1)}, {post(2, 1)}}}
}

// The whole promise, in one test: what an agent writes is what it is handed
// back. Nothing else in the protocol carries agent-chosen state across the gap
// between two containers.
func TestMemoComesBackNextStep(t *testing.T) {
	w := newWorld(t, &proxy.StubProvider{}, nil, Config{})
	w.add(t, "keeper", 1000)

	var seen []string
	w.steps.fns["keeper"] = script(func(_ StepRequest, in StepInput) StepResult {
		seen = append(seen, in.Observation.Memo)
		return out(Action{Type: ActionMemo, Text: "round " + string(rune('0'+in.Observation.Round))})
	}, solve)

	if err := w.orch.RunEpisode(w.ctx, twoRounds()); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("bid steps = %d, want 2", len(seen))
	}
	// First step: nothing written yet, so nothing handed back.
	if seen[0] != "" {
		t.Errorf("first observation memo = %q, want empty", seen[0])
	}
	if seen[1] != "round 1" {
		t.Errorf("second observation memo = %q, want what round 1 wrote", seen[1])
	}
}

// An agent that never writes one is handed an observation with no memo key at
// all — not an empty string, no key. This is what lets the mechanism exist on
// every track without changing a byte of what the tracks that ignore it see.
func TestMemolessObservationHasNoMemoKey(t *testing.T) {
	w := newWorld(t, &proxy.StubProvider{}, nil, Config{})
	w.add(t, "quiet", 1000)

	var raw []byte
	w.steps.fns["quiet"] = script(func(req StepRequest, in StepInput) StepResult {
		raw = req.Input
		return out(bidsFor(in, 0.4)...)
	}, solve)

	if err := w.orch.RunEpisode(w.ctx, oneRound(post(1, 1))); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Observation map[string]json.RawMessage `json:"observation"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if _, present := envelope.Observation["memo"]; present {
		t.Fatalf("observation carries a memo key for an agent that never wrote one: %s", raw)
	}
}

// A memo belongs to the agent that wrote it and to nobody else. There is no
// action for reading someone else's, and the store must not leak one by
// accident either.
func TestMemosAreNotSharedBetweenAgents(t *testing.T) {
	w := newWorld(t, &proxy.StubProvider{}, nil, Config{})
	w.add(t, "alice", 1000)
	w.add(t, "bob", 1000)

	var bobSaw []string
	w.steps.fns["alice"] = script(func(_ StepRequest, in StepInput) StepResult {
		return out(Action{Type: ActionMemo, Text: "alice's private arithmetic"})
	}, solve)
	w.steps.fns["bob"] = script(func(_ StepRequest, in StepInput) StepResult {
		bobSaw = append(bobSaw, in.Observation.Memo)
		return out()
	}, solve)

	if err := w.orch.RunEpisode(w.ctx, twoRounds()); err != nil {
		t.Fatal(err)
	}
	for i, got := range bobSaw {
		if got != "" {
			t.Fatalf("bob's observation %d carried %q — that is alice's memo", i, got)
		}
	}
}

// Two memos in one breath is one memo, and it is the last one: the same rule
// ParseActions applies to the sentinel line itself.
func TestLastMemoInAStepWins(t *testing.T) {
	w := newWorld(t, &proxy.StubProvider{}, nil, Config{})
	w.add(t, "fickle", 1000)

	var second string
	w.steps.fns["fickle"] = script(func(_ StepRequest, in StepInput) StepResult {
		if in.Observation.Round == 2 {
			second = in.Observation.Memo
			return out()
		}
		return out(
			Action{Type: ActionMemo, Text: "first thought"},
			Action{Type: ActionMemo, Text: "second thought"},
		)
	}, solve)

	if err := w.orch.RunEpisode(w.ctx, twoRounds()); err != nil {
		t.Fatal(err)
	}
	if second != "second thought" {
		t.Fatalf("memo = %q, want the last one written", second)
	}
}

// Writing nothing is a way of forgetting, and it is the same action. The key
// disappears again, so a cleared memo and a memo never written are indistin-
// guishable — which is what "cleared" ought to mean.
func TestEmptyMemoClearsIt(t *testing.T) {
	w := newWorld(t, &proxy.StubProvider{}, nil, Config{})
	w.add(t, "forgetful", 1000)

	var third string
	w.steps.fns["forgetful"] = script(func(_ StepRequest, in StepInput) StepResult {
		switch in.Observation.Round {
		case 1:
			return out(Action{Type: ActionMemo, Text: "something to forget"})
		case 2:
			if in.Observation.Memo != "something to forget" {
				t.Errorf("round 2 memo = %q, want it still there", in.Observation.Memo)
			}
			return out(Action{Type: ActionMemo, Text: ""})
		}
		third = in.Observation.Memo
		return out()
	}, solve)

	three := Episode{Rounds: [][]Posting{{post(1, 1)}, {post(2, 1)}, {post(3, 1)}}}
	if err := w.orch.RunEpisode(w.ctx, three); err != nil {
		t.Fatal(err)
	}
	if third != "" {
		t.Fatalf("memo after clearing = %q, want empty", third)
	}
}

// Over the cap the write is refused, not truncated: the agent keeps the memo it
// had, and the refusal is on the record rather than in a silently shortened
// string it would have no way to recognise as shortened.
func TestOversizeMemoIsRefusedAndThePreviousOneStands(t *testing.T) {
	tw, read := simTrace(t)
	w := newWorld(t, &proxy.StubProvider{}, tw, Config{})
	w.add(t, "verbose", 1000)

	huge := strings.Repeat("x", MaxMemoBytes+1)
	var third string
	w.steps.fns["verbose"] = script(func(_ StepRequest, in StepInput) StepResult {
		switch in.Observation.Round {
		case 1:
			return out(Action{Type: ActionMemo, Text: "brief and kept"})
		case 2:
			return out(Action{Type: ActionMemo, Text: huge})
		}
		third = in.Observation.Memo
		return out()
	}, solve)

	three := Episode{Rounds: [][]Posting{{post(1, 1)}, {post(2, 1)}, {post(3, 1)}}}
	if err := w.orch.RunEpisode(w.ctx, three); err != nil {
		t.Fatal(err)
	}
	if third != "brief and kept" {
		t.Fatalf("memo after a refused write = %q, want the previous one intact", third)
	}

	lines := read()
	if n := count(lines, trace.EventNote, "memo refused: over limit"); n != 1 {
		t.Fatalf("refusal notes = %d, want 1", n)
	}
	// Exactly at the cap is fine: the refusal is for exceeding it, and an
	// off-by-one here would quietly cost every agent its last byte.
	if !(&notebook{memos: map[string]string{}}).write("a", strings.Repeat("y", MaxMemoBytes)) {
		t.Error("a memo of exactly MaxMemoBytes was refused")
	}
}

// The attempt phase can write one too, and it is waiting at the next bid step.
// This is the only route by which the outcome of a win reaches the agent's next
// decision: the attempt wallet it spent from is retired before that decision is
// made, so anything the agent noticed while working has to travel this way.
func TestMemoWrittenWhileAttemptingSurvivesToTheNextBid(t *testing.T) {
	w := newWorld(t, &proxy.StubProvider{}, nil, Config{})
	w.add(t, "worker", 1000)

	var secondBid string
	w.steps.fns["worker"] = script(
		func(_ StepRequest, in StepInput) StepResult {
			if in.Observation.Round == 2 {
				secondBid = in.Observation.Memo
				return out()
			}
			return out(bidsFor(in, 0.4)...)
		},
		func(_ StepRequest, in StepInput) StepResult {
			return out(
				Action{Type: ActionSubmit, Bounty: in.Observation.Task.BountyID,
					Answer: answerFrom(in.Observation.Task.Prompt)},
				Action{Type: ActionMemo, Text: "won " + in.Observation.Task.BountyID},
			)
		})

	if err := w.orch.RunEpisode(w.ctx, twoRounds()); err != nil {
		t.Fatal(err)
	}
	if secondBid != "won b0001" {
		t.Fatalf("memo at the second bid = %q, want what the attempt wrote", secondBid)
	}
}

// An accepted memo is traced. The platform does not read it, but it does not
// hide it either — the trace is the published artefact, and an agent's memory
// is part of what happened.
func TestAcceptedMemoIsTraced(t *testing.T) {
	tw, read := simTrace(t)
	w := newWorld(t, &proxy.StubProvider{}, tw, Config{})
	w.add(t, "diarist", 1000)

	w.steps.fns["diarist"] = script(func(_ StepRequest, in StepInput) StepResult {
		return out(Action{Type: ActionMemo, Text: "day one"})
	}, solve)

	if err := w.orch.RunEpisode(w.ctx, oneRound(post(1, 1))); err != nil {
		t.Fatal(err)
	}
	lines := read()
	if n := count(lines, trace.EventAgent, "memo"); n != 1 {
		t.Fatalf("memo events = %d, want 1", n)
	}
	for _, l := range lines {
		if l.Type != trace.EventAgent {
			continue
		}
		var p struct{ Action, Agent, Memo string }
		if err := json.Unmarshal(l.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Action == "memo" && (p.Agent != "diarist" || p.Memo != "day one") {
			t.Fatalf("memo event = %+v, want diarist/day one", p)
		}
	}
}
