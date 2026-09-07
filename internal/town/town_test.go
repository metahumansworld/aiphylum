package town

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/metahumansworld/soscitea/internal/proxy"
	"github.com/metahumansworld/soscitea/internal/trace"
)

// fast is a test pacing: real events, no real waiting.
var fast = Config{TickMinutes: 10, Interval: time.Microsecond, Days: 1, StartMinute: 7 * 60}

func runInto(t *testing.T, path string, m Map, people []Persona, cfg Config) []trace.Line {
	t.Helper()
	tw, err := trace.NewWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), tw, m, people, cfg); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	lines, err := trace.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

// TestAshmereFootprintsDisjoint: no two places share a cell, so the viewer can
// pave streets and raise walls without one place drawing inside another. It
// also pins every place inside the grid.
func TestAshmereFootprintsDisjoint(t *testing.T) {
	m, _ := Ashmere()
	for i, a := range m.Places {
		if a.X < 0 || a.Y < 0 || a.X+a.W > m.Width || a.Y+a.H > m.Height {
			t.Errorf("%s sticks out of the %dx%d grid", a.ID, m.Width, m.Height)
		}
		for _, b := range m.Places[i+1:] {
			if a.X < b.X+b.W && b.X < a.X+a.W && a.Y < b.Y+b.H && b.Y < a.Y+a.H {
				t.Errorf("%s overlaps %s", a.ID, b.ID)
			}
		}
	}
}

func TestScheduleWraps(t *testing.T) {
	p := Persona{Schedule: []Slot{
		{At: 8 * 60, Place: "work", Activity: "working"},
		{At: 21 * 60, Place: "home", Activity: "asleep"},
	}}
	// Before the first slot of the day, yesterday's last slot is in force.
	if got := p.At(3 * 60); got.Place != "home" {
		t.Fatalf("03:00 should still be yesterday's %q, got %q", "home", got.Place)
	}
	if got := p.At(12 * 60); got.Place != "work" {
		t.Fatalf("12:00 should be %q, got %q", "work", got.Place)
	}
	if got := p.At(22 * 60); got.Place != "home" {
		t.Fatalf("22:00 should be %q, got %q", "home", got.Place)
	}
}

func TestFoundedIsFirstLine(t *testing.T) {
	m, people := Ashmere()
	lines := runInto(t, filepath.Join(t.TempDir(), "t.jsonl"), m, people, fast)
	if len(lines) == 0 {
		t.Fatal("empty trace")
	}
	if lines[0].Type != trace.EventTown {
		t.Fatalf("first line type = %s, want town", lines[0].Type)
	}
	var p map[string]any
	if err := json.Unmarshal(lines[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	// The viewer routes on the first event it folds, so founding must lead.
	if p["action"] != "founded" {
		t.Fatalf("first action = %v, want founded", p["action"])
	}
	last := lines[len(lines)-1]
	var q map[string]any
	json.Unmarshal(last.Payload, &q)
	if q["action"] != "closed" {
		t.Fatalf("last action = %v, want closed", q["action"])
	}
}

// TestRunDeterministic pins the whole day: two runs of the same town write
// identical event streams, byte for byte, modulo the wall-clock timestamps.
func TestRunDeterministic(t *testing.T) {
	m, people := Ashmere()
	dir := t.TempDir()
	a := runInto(t, filepath.Join(dir, "a.jsonl"), m, people, fast)
	b := runInto(t, filepath.Join(dir, "b.jsonl"), m, people, fast)
	if len(a) != len(b) {
		t.Fatalf("run lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Type != b[i].Type || string(a[i].Payload) != string(b[i].Payload) {
			t.Fatalf("line %d differs:\n  %s %s\n  %s %s",
				i, a[i].Type, a[i].Payload, b[i].Type, b[i].Payload)
		}
	}
}

// TestMeetingDedup: sharing a place is one meeting for as long as it lasts;
// parting and rejoining is a second.
func TestMeetingDedup(t *testing.T) {
	m := Map{Name: "pair", Width: 12, Height: 3, Places: []Place{
		{ID: "east", Name: "East", Kind: "public", X: 9, Y: 0, W: 3, H: 3},
		{ID: "west", Name: "West", Kind: "public", X: 0, Y: 0, W: 3, H: 3},
	}}
	people := []Persona{
		{ID: "a", Name: "A", Home: "west", Schedule: []Slot{
			{At: 0, Place: "west", Activity: "waiting"},
			{At: 2 * 60, Place: "east", Activity: "visiting"},  // join b: meeting one
			{At: 6 * 60, Place: "west", Activity: "away"},      // part
			{At: 10 * 60, Place: "east", Activity: "visiting"}, // rejoin: meeting two
		}},
		{ID: "b", Name: "B", Home: "east", Schedule: []Slot{
			{At: 0, Place: "east", Activity: "staying put"},
		}},
	}
	lines := runInto(t, filepath.Join(t.TempDir(), "t.jsonl"), m, people,
		Config{TickMinutes: 10, Interval: time.Microsecond, Days: 1})
	met := 0
	for _, l := range lines {
		var p map[string]any
		json.Unmarshal(l.Payload, &p)
		if p["action"] == "met" {
			met++
		}
	}
	if met != 2 {
		t.Fatalf("met events = %d, want 2 (once per stretch together, not per tick)", met)
	}
}

// TestRunDeterministicWithMind is TestRunDeterministic's sibling and the test
// that actually protects milestone 2: the same town, thinking this time, still
// writes byte-identical streams. A fresh Minds per run — sharing one would let
// the first run's memories leak into the second and prove nothing. The count
// checks keep it honest: byte-identical silence would also pass the diff.
func TestRunDeterministicWithMind(t *testing.T) {
	dir := t.TempDir()
	run := func(path string) ([]trace.Line, Report) {
		m, people := Ashmere()
		cfg := fast
		cfg.Mind = &Minds{Provider: &proxy.StubProvider{}}
		tw, err := trace.NewWriter(path)
		if err != nil {
			t.Fatal(err)
		}
		rep, err := Run(context.Background(), tw, m, people, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		lines, err := trace.Read(path)
		if err != nil {
			t.Fatal(err)
		}
		return lines, rep
	}
	a, ra := run(filepath.Join(dir, "a.jsonl"))
	b, rb := run(filepath.Join(dir, "b.jsonl"))
	if ra.Utterances == 0 || ra.Thoughts == 0 || ra.Calls == 0 {
		t.Fatalf("the mind never spoke: %+v", ra)
	}
	if ra != rb {
		t.Fatalf("reports differ:\n  %+v\n  %+v", ra, rb)
	}
	if len(a) != len(b) {
		t.Fatalf("run lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Type != b[i].Type || string(a[i].Payload) != string(b[i].Payload) {
			t.Fatalf("line %d differs:\n  %s %s\n  %s %s",
				i, a[i].Type, a[i].Payload, b[i].Type, b[i].Payload)
		}
	}
}
