package town

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/singhtushant3-hub/aiphylum/internal/trace"
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
