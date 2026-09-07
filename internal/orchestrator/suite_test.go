package orchestrator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/metahumansworld/soscitea/internal/proxy"
	"github.com/metahumansworld/soscitea/internal/suites"
	"github.com/metahumansworld/soscitea/internal/trace"
)

// A suite file shaped exactly like the shipped one, small enough to reason
// about: one instance per tier, answers the fake agent can produce from the
// prompt the way sayGen's agents do.
func testSuite(t *testing.T) *suites.Suite {
	t.Helper()
	path := filepath.Join(t.TempDir(), "imp.jsonl")
	body := `{"name":"imp","source":"a public benchmark","licence":"CC0-1.0","contamination":"public since 2021, so assume every model has seen it"}
{"id":"i1","tier":1,"prompt":"say exactly: w1","answer":"w1","reference_tokens":20}
{"id":"i2","tier":2,"prompt":"say exactly: w2","answer":"w2","reference_tokens":20}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := suites.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The whole point of the milestone, stated as a test: imported work IS ranked.
// Judged work is refused by a ranked world; imported work is not, because a
// held-out answer key is a held-out answer key wherever it came from. What
// changes is that the instance is public, and the trace says so.
func TestRankedWorldAcceptsImportedSuite(t *testing.T) {
	tw, read := simTrace(t)
	w := newWorld(t, &proxy.StubProvider{}, tw, Config{})
	s := testSuite(t)
	w.orch.Board.RegisterGenerator(s)
	w.orch.Suites = []SuiteNote{{
		Name: s.Manifest.Name, Source: s.Manifest.Source,
		Licence: s.Manifest.Licence, Contamination: s.Manifest.Contamination,
	}}
	w.add(t, "solo", 1000)
	w.steps.fns["solo"] = script(bidAll(0.5), solve)

	if w.orch.Ladder == nil {
		t.Fatal("the test world has no ladder; acceptance would prove nothing")
	}
	err := w.orch.RunEpisode(w.ctx, oneRound(
		Posting{Generator: "imp", Seed: 0, Tier: 1, WallClockSec: 30},
		Posting{Generator: "imp", Seed: 0, Tier: 2, WallClockSec: 30},
	))
	if err != nil {
		t.Fatalf("a ranked world refused imported supply: %v", err)
	}

	rows := w.orch.Ladder.Board(time.Now())
	if len(rows) != 1 || rows[0].Agent != "solo" {
		t.Fatalf("ladder = %+v, want one row for solo", rows)
	}
	if rows[0].Attempts != 2 || rows[0].Successes != 2 {
		t.Errorf("ladder attempts/successes = %d/%d, want 2/2 — imported work must count",
			rows[0].Attempts, rows[0].Successes)
	}

	lines := read()

	// The provenance is published once, before any of the supply it explains.
	notes, noteAt, firstPosting := 0, -1, -1
	for i, l := range lines {
		var p map[string]any
		if err := json.Unmarshal(l.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if l.Type == trace.EventSuite {
			notes++
			noteAt = i
			if p["suite"] != "imp" || p["licence"] != "CC0-1.0" {
				t.Errorf("suite note = %v", p)
			}
			if !strings.Contains(p["contamination"].(string), "every model has seen it") {
				t.Errorf("contamination note did not survive: %v", p["contamination"])
			}
		}
		if l.Type == trace.EventBounty && p["action"] == "posted" && firstPosting < 0 {
			firstPosting = i
		}
	}
	if notes != 1 {
		t.Fatalf("suite notes in trace = %d, want exactly 1", notes)
	}
	// A note after the first asterisked bounty would be an explanation the
	// reader meets too late.
	if noteAt > firstPosting {
		t.Errorf("suite note at line %d, first posting at %d — provenance came after the supply",
			noteAt, firstPosting)
	}
	if lines[0].Type == trace.EventSuite {
		t.Error("the suite note preceded the episode start")
	}

	// Every posting carries its suite, so a reader of one line knows.
	posted := 0
	for _, l := range lines {
		var p map[string]any
		if err := json.Unmarshal(l.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if l.Type != trace.EventBounty || p["action"] != "posted" {
			continue
		}
		posted++
		if p["suite"] != "imp" {
			t.Errorf("posting %v carries suite %v, want imp", p["id"], p["suite"])
		}
	}
	if posted != 2 {
		t.Errorf("postings = %d, want 2", posted)
	}
}

// Generated supply must be untouched by all of the above. A trace recorded
// before suites existed has to replay byte for byte, which means the posting
// line for a generated bounty gains no key — not even an empty one.
func TestGeneratedPostingCarriesNoSuiteKey(t *testing.T) {
	tw, read := simTrace(t)
	w := newWorld(t, &proxy.StubProvider{}, tw, Config{})
	w.add(t, "solo", 1000)
	w.steps.fns["solo"] = script(bidAll(0.5), solve)

	if err := w.orch.RunEpisode(w.ctx, oneRound(
		Posting{Generator: "say", Seed: 1, Tier: 1, WallClockSec: 30},
	)); err != nil {
		t.Fatal(err)
	}

	for _, l := range read() {
		if l.Type == trace.EventSuite {
			t.Error("a world with no imported supply published a suite note")
		}
		if l.Type != trace.EventBounty {
			continue
		}
		var p map[string]any
		if err := json.Unmarshal(l.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if _, ok := p["suite"]; ok {
			t.Errorf("generated posting gained a suite key: %v", p)
		}
	}
}
