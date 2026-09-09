package town

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/metahumansworld/soscitea/internal/proxy"
	"github.com/metahumansworld/soscitea/internal/trace"
)

// The third inward seam: an id handed back by Leave at tick k is off the
// map from tick k on — one "left" line saying where they stood, and then
// not a frame, a walk, a meeting or a word with their name on it.
func TestLeaveRemovesAResident(t *testing.T) {
	m, people := AshmereFair()
	who := people[0].ID
	const at = 5
	n := 0
	cfg := fast
	cfg.Leave = func(day, mod int, clock string) []string {
		n++ // once per tick, so this counts ticks
		if n == at {
			return []string{who}
		}
		return nil
	}
	lines := runInto(t, filepath.Join(t.TempDir(), "t.jsonl"), m, people, cfg)

	got := frames(t, lines, who)
	if len(got) != at-1 {
		t.Fatalf("the departed has %d frames, want %d (left at tick %d)", len(got), at-1, at)
	}
	last := got[len(got)-1]

	left, after := 0, 0
	ticks := 0
	for _, l := range lines {
		if l.Type != trace.EventTown {
			continue
		}
		var p struct {
			Action    string `json:"action"`
			ID        string `json:"resident"`
			A, B      string
			X, Y      int
			Place     string
			Name      string
			Residents []struct {
				ID string `json:"id"`
			}
		}
		if err := json.Unmarshal(l.Payload, &p); err != nil {
			t.Fatal(err)
		}
		names := p.ID == who || p.A == who || p.B == who
		for _, r := range p.Residents {
			names = names || r.ID == who
		}
		switch p.Action {
		case "tick":
			ticks++
		case "left":
			left++
			if ticks != at-1 {
				t.Errorf("left line landed after %d ticks, want before tick %d", ticks, at)
			}
			if p.ID != who || p.Name != people[0].Name || p.X != last.X || p.Y != last.Y || p.Place != last.Place {
				t.Errorf("left %s %q at (%d,%d,%q), want %s %q at (%d,%d,%q)",
					p.ID, p.Name, p.X, p.Y, p.Place, who, people[0].Name, last.X, last.Y, last.Place)
			}
			continue
		}
		if left > 0 && names {
			after++
		}
	}
	if left != 1 {
		t.Errorf("%d left lines, want 1", left)
	}
	if after > 0 {
		t.Errorf("%d lines name the departed after the left line; gone is gone", after)
	}
}

// Gone before the first tick, a body tells the same story as one who never
// came: the same frames, the same meetings, the same words. The two runs
// differ only in the founded roster and the one left line.
func TestLeaveBeforeTickOneIsNeverHavingCome(t *testing.T) {
	m, people := AshmereFair()
	g := Guest("pilgrim", "Pilgrim", "a guest")
	dir := t.TempDir()

	// With a mind: a met on the way out would open a conversation, and a
	// stream with the guest in it would change every later prompt.
	mind := fast
	mind.Mind = &Minds{Provider: &proxy.StubProvider{}}
	never := runInto(t, filepath.Join(dir, "never.jsonl"), m, people, mind)
	first := true
	cfg := mind
	cfg.Mind = &Minds{Provider: &proxy.StubProvider{}} // a fresh memory, not the other run's
	cfg.Leave = func(int, int, string) []string {
		if first {
			first = false
			return []string{g.ID}
		}
		return nil
	}
	gone := runInto(t, filepath.Join(dir, "gone.jsonl"), m, append(people, g), cfg)

	strip := func(lines []trace.Line) []string {
		var out []string
		for _, l := range lines {
			var p struct {
				Action string `json:"action"`
			}
			_ = json.Unmarshal(l.Payload, &p)
			if p.Action == "founded" || p.Action == "left" {
				continue
			}
			out = append(out, string(l.Payload))
		}
		return out
	}
	a, b := strip(never), strip(gone)
	if len(a) != len(b) {
		t.Fatalf("the run without the guest has %d lines after the founding, the run they left %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("line %d differs:\n  never %s\n  gone  %s", i, a[i], b[i])
		}
	}
}

// A Leave that names nobody seated — nobody at all, or an id the town has
// never heard of — leaves no mark: the stream is the one a run with no
// seam writes, byte for byte.
func TestLeaveNobodyLeavesTheStreamAlone(t *testing.T) {
	m, people := AshmereFair()
	dir := t.TempDir()
	a := runInto(t, filepath.Join(dir, "nil.jsonl"), m, people, fast)
	cfg := fast
	cfg.Leave = func(day, _ int, _ string) []string {
		if day == 1 {
			return []string{"nobody"}
		}
		return nil
	}
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
