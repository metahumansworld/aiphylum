package town

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/metahumansworld/soscitea/internal/trace"
)

// The seam's contract: one call per tick, everyone named in roster order,
// the street spelled "", and the clock agreeing with the minute.
func TestVisitSeam(t *testing.T) {
	m, people := Ashmere()
	type call struct {
		day, mod  int
		clock     string
		standings []Standing
	}
	var calls []call
	cfg := fast
	cfg.Visit = func(day, mod int, clock string, standings []Standing) error {
		cp := make([]Standing, len(standings))
		copy(cp, standings)
		calls = append(calls, call{day, mod, clock, cp})
		return nil
	}
	tw, err := trace.NewWriter(filepath.Join(t.TempDir(), "t.jsonl"))
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

	if len(calls) != rep.Ticks {
		t.Fatalf("visited %d times over %d ticks, want one call per tick", len(calls), rep.Ticks)
	}
	inPlace, inStreet := false, false
	for _, c := range calls {
		if len(c.standings) != len(people) {
			t.Fatalf("at %s the seam named %d people, want all %d", c.clock, len(c.standings), len(people))
		}
		for j, st := range c.standings {
			if st.ID != people[j].ID {
				t.Fatalf("at %s standing %d is %q, want roster order (%q)", c.clock, j, st.ID, people[j].ID)
			}
			if st.Place == "" {
				inStreet = true
			} else {
				inPlace = true
			}
		}
		if c.clock != HHMM(c.mod) {
			t.Fatalf("clock %q disagrees with minute %d", c.clock, c.mod)
		}
	}
	// A day where nobody was ever indoors, or nobody ever walked, would
	// mean the standings are wired to nothing.
	if !inPlace || !inStreet {
		t.Errorf("all day: someone indoors %v, someone in the street %v — want both", inPlace, inStreet)
	}
}

// A visitor's error halts the run where it stood: the seam is a participant,
// not a spectator, because the fair's money must be able to stop the world
// rather than drift out of balance behind it.
func TestVisitErrorHaltsTheRun(t *testing.T) {
	m, people := Ashmere()
	boom := errors.New("boom")
	n := 0
	cfg := fast
	cfg.Visit = func(int, int, string, []Standing) error {
		n++
		if n == 3 {
			return boom
		}
		return nil
	}
	tw, err := trace.NewWriter(filepath.Join(t.TempDir(), "t.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Run(context.Background(), tw, m, people, cfg)
	if !errors.Is(err, boom) {
		t.Fatalf("Run = %v, want the visitor's error", err)
	}
	if n != 3 {
		t.Fatalf("visited %d times after erroring on the 3rd", n)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
}

// A visitor that only watches leaves no mark: the stream with a no-op Visit
// is the stream TestRunDeterministic pinned, byte for byte modulo the
// wall-clock stamps. The seam observes the town; it does not disturb it.
func TestVisitNoopLeavesTheStreamAlone(t *testing.T) {
	m, people := Ashmere()
	dir := t.TempDir()
	a := runInto(t, filepath.Join(dir, "nil.jsonl"), m, people, fast)
	cfg := fast
	cfg.Visit = func(int, int, string, []Standing) error { return nil }
	b := runInto(t, filepath.Join(dir, "noop.jsonl"), m, people, cfg)
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

// spot is one resident's frame in one tick, read back out of the stream —
// the only account of the day a viewer ever gets, which is why the tests
// below read it rather than the walker structs that produced it.
type spot struct {
	X, Y     int
	Place    string
	Activity string
	Goal     string
	Path     [][2]int
}

// frames returns one resident's spot for every tick of the run, in order.
func frames(t *testing.T, lines []trace.Line, who string) []spot {
	t.Helper()
	var out []spot
	for _, l := range lines {
		if l.Type != trace.EventTown {
			continue
		}
		var p struct {
			Action    string `json:"action"`
			Residents []struct {
				ID string `json:"id"`
				spot
			} `json:"residents"`
		}
		if err := json.Unmarshal(l.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Action != "tick" {
			continue
		}
		for _, r := range p.Residents {
			if r.ID == who {
				out = append(out, r.spot)
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("no frames for %q in %d lines", who, len(lines))
	}
	return out
}

// The inward seam: Hold keeps a body where it stands. The window is found in
// a free run rather than guessed, so the test holds a resident who was
// demonstrably walking — holding someone already standing still would pass
// against a Hold that did nothing at all.
func TestHoldKeepsABodyStill(t *testing.T) {
	m, people := Ashmere()
	dir := t.TempDir()
	who := people[0].ID
	free := frames(t, runInto(t, filepath.Join(dir, "free.jsonl"), m, people, fast), who)

	const span = 4
	from := -1
	for i := 1; i < len(free)-span-1; i++ {
		if free[i].X != free[i-1].X || free[i].Y != free[i-1].Y {
			from = i
			break
		}
	}
	if from < 0 {
		t.Fatalf("%s never moved all day: nothing to hold", who)
	}

	n := 0
	cfg := fast
	cfg.Hold = func(id string) bool {
		if id != who {
			return false
		}
		n++ // Hold is asked once per resident per tick, so this counts ticks
		return n > from && n <= from+span
	}
	held := frames(t, runInto(t, filepath.Join(dir, "held.jsonl"), m, people, cfg), who)

	if len(held) != len(free) {
		t.Fatalf("held run has %d ticks, free run %d — a hold must not shorten the day", len(held), len(free))
	}
	stood := held[from-1]
	for i := from; i < from+span; i++ {
		got := held[i]
		if got.X != stood.X || got.Y != stood.Y || got.Place != stood.Place {
			t.Errorf("tick %d: held at (%d,%d,%q) but stood at (%d,%d,%q)",
				i, got.X, got.Y, got.Place, stood.X, stood.Y, stood.Place)
		}
		// The frame must not repeat a schedule the resident is visibly not
		// keeping: a held body is waiting, and its goal is its own feet.
		if got.Activity != "waiting" || got.Goal != got.Place {
			t.Errorf("tick %d: held frame says %q toward %q, want waiting at %q",
				i, got.Activity, got.Goal, got.Place)
		}
		if len(got.Path) != 0 {
			t.Errorf("tick %d: held frame walked %v", i, got.Path)
		}
	}

	// And it ends. A hold that never expires is a body the town has lost.
	moved := false
	for i := from + span; i < len(held); i++ {
		if held[i].X != stood.X || held[i].Y != stood.Y {
			moved = true
			break
		}
	}
	if !moved {
		t.Errorf("%s never moved again after the hold lifted", who)
	}
}

// A Hold that holds nobody leaves no mark, and neither does no Hold at all:
// both streams are the one TestRunDeterministic pinned, byte for byte. The
// fair's price for standing still cannot shift the town's own account of a
// day in which nobody bought any.
func TestHoldNobodyLeavesTheStreamAlone(t *testing.T) {
	m, people := Ashmere()
	dir := t.TempDir()
	a := runInto(t, filepath.Join(dir, "nil.jsonl"), m, people, fast)
	cfg := fast
	cfg.Hold = func(string) bool { return false }
	b := runInto(t, filepath.Join(dir, "never.jsonl"), m, people, cfg)
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
