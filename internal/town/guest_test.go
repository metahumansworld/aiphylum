package town

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/metahumansworld/aiphylum/internal/trace"
)

// TestGuestPersona pins the body the town hands a user-authored agent: a room
// at the tavern, a schedule that only names places AshmereFair actually has,
// in strictly ascending order, with the office on it — a guest who never
// reaches the board would be a lodger, not a participant.
func TestGuestPersona(t *testing.T) {
	m, _ := AshmereFair()
	places := map[string]bool{}
	for _, p := range m.Places {
		places[p.ID] = true
	}

	g := Guest("pilgrim", "Pilgrim", "a guest")
	if g.Home != "tavern" {
		t.Errorf("a guest lodges at the tavern, got home %q", g.Home)
	}
	atOffice := false
	for i, s := range g.Schedule {
		if !places[s.Place] {
			t.Errorf("slot %d names %q, which is not on the fair's map", i, s.Place)
		}
		if i > 0 && s.At <= g.Schedule[i-1].At {
			t.Errorf("slot %d (%d) does not come after slot %d (%d)", i, s.At, i-1, g.Schedule[i-1].At)
		}
		if s.Place == "office" {
			atOffice = true
		}
	}
	if !atOffice {
		t.Error("the guest's day never reaches the office; it could never see the board")
	}
}

// TestGuestDayDeterministic runs a fair-map day with a guest in the roster
// twice and requires the same story both times, modulo wall clock — the same
// bar every other roster meets. It also checks the guest actually arrives at
// the office, so the schedule is exercised rather than assumed.
func TestGuestDayDeterministic(t *testing.T) {
	run := func(path string) []trace.Line {
		m, people := AshmereFair()
		people = append(people, Guest("pilgrim", "Pilgrim", "a guest"))
		return runInto(t, path, m, people, fast)
	}
	dir := t.TempDir()
	a := run(filepath.Join(dir, "a.jsonl"))
	b := run(filepath.Join(dir, "b.jsonl"))

	if len(a) != len(b) {
		t.Fatalf("run lengths differ: %d vs %d", len(a), len(b))
	}
	guestAtOffice := false
	for i := range a {
		if a[i].Type != b[i].Type || string(a[i].Payload) != string(b[i].Payload) {
			t.Fatalf("line %d differs:\n  %s %s\n  %s %s", i, a[i].Type, a[i].Payload, b[i].Type, b[i].Payload)
		}
		var ev struct {
			Action   string `json:"action"`
			Resident string `json:"resident"`
			Place    string `json:"place"`
		}
		if json.Unmarshal(a[i].Payload, &ev) == nil &&
			ev.Action == "arrive" && ev.Resident == "pilgrim" && ev.Place == "office" {
			guestAtOffice = true
		}
	}
	if !guestAtOffice {
		t.Error("the guest never arrived at the office")
	}
}
