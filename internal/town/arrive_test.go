package town

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/metahumansworld/soscitea/internal/proxy"
	"github.com/metahumansworld/soscitea/internal/trace"
)

// The second inward seam: a body handed back by Arrive at tick k is on the
// map from tick k on, seated at home, announced by one "joined" line, and
// counted by every later frame — and it is not "met" on the way in.
func TestArriveSeatsANewcomer(t *testing.T) {
	m, people := AshmereFair()
	g := Guest("pilgrim", "Pilgrim", "a guest")
	const at = 5
	n := 0
	cfg := fast
	cfg.Arrive = func(day, mod int, clock string) []Persona {
		n++ // once per tick, so this counts ticks
		if n == at {
			return []Persona{g}
		}
		return nil
	}
	lines := runInto(t, filepath.Join(t.TempDir(), "t.jsonl"), m, people, cfg)

	free := frames(t, runInto(t, filepath.Join(t.TempDir(), "free.jsonl"), m, people, fast), people[0].ID)
	got := frames(t, lines, g.ID)
	if len(got) != len(free)-at+1 {
		t.Fatalf("newcomer has %d frames, want %d (joined at tick %d of %d)", len(got), len(free)-at+1, at, len(free))
	}

	home, _ := m.Place(g.Home)
	hx, hy := home.Anchor()
	joined, metOnArrival := 0, false
	ticks := 0
	for _, l := range lines {
		if l.Type != trace.EventTown {
			continue
		}
		var p struct {
			Action string `json:"action"`
			ID     string `json:"resident"`
			A, B   string
			X, Y   int
			Place  string
			Name   string
		}
		if err := json.Unmarshal(l.Payload, &p); err != nil {
			t.Fatal(err)
		}
		switch p.Action {
		case "tick":
			ticks++
		case "joined":
			joined++
			if ticks != at-1 {
				t.Errorf("joined line landed after %d ticks, want before tick %d", ticks, at)
			}
			if p.ID != g.ID || p.Name != g.Name || p.Place != g.Home || p.X != hx || p.Y != hy {
				t.Errorf("joined %s %q at (%d,%d,%q), want %s %q at (%d,%d,%q)",
					p.ID, p.Name, p.X, p.Y, p.Place, g.ID, g.Name, hx, hy, g.Home)
			}
		case "met":
			if ticks == at-1 && (p.A == g.ID || p.B == g.ID) {
				metOnArrival = true
			}
		}
	}
	if joined != 1 {
		t.Errorf("%d joined lines, want 1", joined)
	}
	if metOnArrival {
		t.Error("the newcomer was met on the tick it arrived; arriving is a founding, not a meeting")
	}
}

// Joined before the first tick, a body tells the same story as one seated at
// boot: the same frames, the same arrivals, the same meetings. The two runs
// differ only in the founded roster and the one joined line.
func TestArriveBeforeTickOneIsBootSeating(t *testing.T) {
	m, people := AshmereFair()
	g := Guest("pilgrim", "Pilgrim", "a guest")
	dir := t.TempDir()

	// With a mind: a met on arrival would open a conversation, and every
	// later reflection would remember it. The stub is enough to tell.
	mind := fast
	mind.Mind = &Minds{Provider: &proxy.StubProvider{}}
	boot := runInto(t, filepath.Join(dir, "boot.jsonl"), m, append(people, g), mind)
	first := true
	cfg := mind
	cfg.Mind = &Minds{Provider: &proxy.StubProvider{}} // a fresh memory, not the boot run's
	cfg.Arrive = func(int, int, string) []Persona {
		if first {
			first = false
			return []Persona{g}
		}
		return nil
	}
	join := runInto(t, filepath.Join(dir, "join.jsonl"), m, people, cfg)

	strip := func(lines []trace.Line) []string {
		var out []string
		for _, l := range lines {
			var p struct {
				Action string `json:"action"`
			}
			_ = json.Unmarshal(l.Payload, &p)
			if p.Action == "founded" || p.Action == "joined" {
				continue
			}
			out = append(out, string(l.Payload))
		}
		return out
	}
	a, b := strip(boot), strip(join)
	if len(a) != len(b) {
		t.Fatalf("boot run has %d lines after the founding, join run %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("line %d differs:\n  boot %s\n  join %s", i, a[i], b[i])
		}
	}
}

// An Arrive that never hands anyone back leaves no mark: the stream is the
// one a run with no seam writes, byte for byte.
func TestArriveNobodyLeavesTheStreamAlone(t *testing.T) {
	m, people := AshmereFair()
	dir := t.TempDir()
	a := runInto(t, filepath.Join(dir, "nil.jsonl"), m, people, fast)
	cfg := fast
	cfg.Arrive = func(int, int, string) []Persona { return nil }
	b := runInto(t, filepath.Join(dir, "never.jsonl"), m, people, cfg)
	if len(a) != len(b) {
		t.Fatalf("run lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Type != b[i].Type || string(a[i].Payload) != string(b[i].Payload) {
			t.Fatalf("line %d differs:\n  %s\n  %s", i, a[i].Payload, b[i].Payload)
		}
	}
}
