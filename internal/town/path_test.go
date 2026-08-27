package town

import "testing"

// TestEveryBuildingHasADoor: a room nobody can enter would strand whoever
// lives in it, so Ashmere is pinned to the property that all four cottages,
// both shops and the tavern open onto ground.
func TestEveryBuildingHasADoor(t *testing.T) {
	m, _ := Ashmere()
	w := NewWorld(m)
	for _, p := range w.Map.Places {
		if !p.Blocks() {
			continue
		}
		if p.Door == nil {
			t.Errorf("%s has no door", p.ID)
			continue
		}
		if !p.Contains(p.Door.X, p.Door.Y) {
			t.Errorf("%s door %v is outside its own walls", p.ID, *p.Door)
		}
		out, ok := w.out[p.ID]
		if !ok || w.solid[w.idx(out)] {
			t.Errorf("%s door opens onto %v, which is not ground", p.ID, out)
		}
	}
}

// TestRoutesKeepOutOfWalls is the whole point of the router: walk every
// resident's day, one destination at a time, and check that no step of any
// route lands inside a building that is not the one being entered or left.
func TestRoutesKeepOutOfWalls(t *testing.T) {
	m, people := Ashmere()
	w := NewWorld(m)
	for _, p := range people {
		at, _ := m.Place(p.At(0).Place)
		x, y := at.Anchor()
		for _, slot := range p.Schedule {
			goal, ok := m.Place(slot.Place)
			if !ok {
				t.Fatalf("%s is scheduled at %q, which is not on the map", p.ID, slot.Place)
			}
			from := Cell{x, y}
			route := w.Route(from, slot.Place)
			for _, c := range route {
				b, inside := w.buildingAt(c)
				if !inside || b.ID == goal.ID || b.Contains(from.X, from.Y) {
					continue
				}
				t.Errorf("%s walking to %s passes through %s at %v",
					p.ID, goal.ID, b.ID, c)
			}
			if len(route) > 0 {
				last := route[len(route)-1]
				x, y = last.X, last.Y
			}
			if !goal.Contains(x, y) {
				t.Errorf("%s never arrives at %s: route ends at %d,%d", p.ID, goal.ID, x, y)
			}
		}
	}
}

// TestRouteIsAFunctionOfItsEndpoints: same journey, same cells, every time.
// The trace is only replayable because of this.
func TestRouteIsAFunctionOfItsEndpoints(t *testing.T) {
	m, _ := Ashmere()
	a, b := NewWorld(m), NewWorld(m)
	for _, goal := range []string{"tavern", "market", "library", "home-pell"} {
		one := a.Route(Cell{2, 2}, goal)
		two := b.Route(Cell{2, 2}, goal)
		if len(one) != len(two) {
			t.Fatalf("%s: route lengths differ, %d vs %d", goal, len(one), len(two))
		}
		for i := range one {
			if one[i] != two[i] {
				t.Fatalf("%s: step %d differs, %v vs %v", goal, i, one[i], two[i])
			}
		}
	}
}

// TestRoutePrefersPaving: the streets are laid where people should walk, and
// the cost model is what makes that true. Mira's walk to the market is mostly
// paved even though cutting the corner across the lawns would be shorter.
func TestRoutePrefersPaving(t *testing.T) {
	m, _ := Ashmere()
	w := NewWorld(m)
	route := w.Route(Cell{2, 2}, "market")
	paved, total := 0, 0
	for _, c := range route {
		if _, inside := w.buildingAt(c); inside {
			continue // the doorways at either end
		}
		total++
		if w.cost[w.idx(c)] <= costPark {
			paved++
		}
	}
	if paved*4 < total*3 {
		t.Errorf("only %d of %d outdoor cells are paved; the router is cutting across the grass", paved, total)
	}
}

// TestUnknownKindsAreGround: only homes and shops have walls. A map that
// invents a kind this package has never heard of stays walkable, so adding
// vocabulary to the viewer can never wall a resident in.
func TestUnknownKindsAreGround(t *testing.T) {
	m := Map{Name: "odd", Width: 9, Height: 3, Places: []Place{
		{ID: "west", Kind: "public", X: 0, Y: 0, W: 2, H: 3},
		{ID: "middle", Kind: "orchard", X: 3, Y: 0, W: 3, H: 3},
		{ID: "east", Kind: "public", X: 7, Y: 0, W: 2, H: 3},
	}}
	w := NewWorld(m)
	route := w.Route(Cell{0, 1}, "east")
	if len(route) == 0 {
		t.Fatal("no route from west to east across an orchard")
	}
	if last := route[len(route)-1]; last != (Cell{8, 1}) {
		t.Fatalf("route ends at %v, want the east anchor 8,1", last)
	}
}
