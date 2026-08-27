package town

// Walls, doors, and the ways between them.
//
// A Map on its own is a list of rectangles; it does not say which of them you
// can walk through. This file decides that once, in one place, so the
// simulation and the viewer cannot disagree: homes and shops are solid except
// for a single door cell, everything else is ground, and paving is cheaper to
// cross than lawn. Residents then follow routes that go around buildings and
// along streets, because those are the only routes there are.

// Cell is one square of the map's grid.
type Cell struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// What it costs to cross a cell. The numbers only matter against each other:
// paving is cheap, a garden path is nearly as cheap, and open ground is dear
// enough that a resident will walk the long way round on the street rather
// than cut across the grass — but will still cross it to reach a front door.
const (
	costPaved = 2
	costPark  = 3
	costOpen  = 9
)

// Blocks reports whether this place is a building — walls you go around and a
// door you go through — rather than ground you simply walk onto. Only home and
// shop are buildings; every other kind, including kinds this package has never
// heard of, is ground.
func (p Place) Blocks() bool { return p.Kind == "home" || p.Kind == "shop" }

// World is a Map with its walls worked out: which cells are solid, what each
// open cell costs to cross, and where every building's door is. Routes come
// from here, and the doors it finds ride along in the founded event, so the
// viewer draws its doorways on the same walls the residents walk through.
type World struct {
	Map   Map
	solid []bool
	cost  []int
	out   map[string]Cell // place id → the ground cell outside its door
}

// NewWorld raises the walls and cuts the doors. The Map it returns is a copy
// with every reachable building's Door filled in; a building with no way in —
// walled on all four sides, or standing on the edge of the grid — is left as
// open ground instead, because a room nobody can enter is worse than a room
// with no walls.
func NewWorld(m Map) *World {
	w := &World{Map: m, out: map[string]Cell{}}
	w.Map.Places = append([]Place(nil), m.Places...)
	n := m.Width * m.Height
	w.solid = make([]bool, n)
	w.cost = make([]int, n)
	for i := range w.cost {
		w.cost[i] = costOpen
	}
	for _, p := range w.Map.Places {
		c := costPaved
		switch {
		case p.Blocks():
			c = 0
		case p.Kind == "park":
			c = costPark
		}
		w.each(p, func(i int) {
			if p.Blocks() {
				w.solid[i] = true
			} else {
				w.cost[i] = c
			}
		})
	}
	// Doors are found against the fully walled grid, so the order buildings
	// appear in cannot change where they end up.
	doorless := []Place{}
	for i := range w.Map.Places {
		p := &w.Map.Places[i]
		if !p.Blocks() {
			continue
		}
		in, out, ok := w.findDoor(*p)
		if !ok {
			doorless = append(doorless, *p)
			continue
		}
		p.Door = &Cell{in.X, in.Y}
		w.out[p.ID] = out
	}
	for _, p := range doorless {
		w.each(p, func(i int) { w.solid[i] = false })
	}
	return w
}

// findDoor picks the cell of a building's wall that opens and the ground
// outside it. South first — the face the viewer turns toward the camera — then
// east, north, west, so a building backed against the edge of the map still
// gets a door someone can reach. The door shares a column or row with the
// anchor, so the walk from doorway to hearth is a straight line.
func (w *World) findDoor(p Place) (in, out Cell, ok bool) {
	mx, my := p.X+p.W/2, p.Y+p.H/2
	for _, side := range [4][2]Cell{
		{{mx, p.Y + p.H - 1}, {mx, p.Y + p.H}},
		{{p.X + p.W - 1, my}, {p.X + p.W, my}},
		{{mx, p.Y}, {mx, p.Y - 1}},
		{{p.X, my}, {p.X - 1, my}},
	} {
		if i := w.idx(side[1]); i >= 0 && !w.solid[i] {
			return side[0], side[1], true
		}
	}
	return Cell{}, Cell{}, false
}

// Route is the whole journey: out through the door of whatever building you
// are standing in, across the open ground, and in through the door of the one
// you are going to. Cells come back in walking order, excluding the one you
// start on, so a resident can be moved along the list a few cells per tick.
func (w *World) Route(from Cell, goalID string) []Cell {
	goal, ok := w.Map.Place(goalID)
	if !ok {
		return nil
	}
	ax, ay := goal.Anchor()
	anchor := Cell{ax, ay}
	if goal.Contains(from.X, from.Y) {
		return straight(from, anchor) // already inside: just cross the room
	}

	var path []Cell
	cur := from
	if b, ok := w.buildingAt(cur); ok {
		path = append(path, straight(cur, *b.Door)...)
		path = append(path, w.out[b.ID])
		cur = w.out[b.ID]
	}

	aim := anchor
	door, walled := w.out[goal.ID]
	if walled {
		aim = door
	}
	if cur != aim {
		mid := w.route(cur, aim)
		if mid == nil {
			// Nothing open connects the two. Walking straight there looks
			// wrong, which is the point: it makes an unreachable place
			// visible instead of wedging the resident in the road.
			return append(path, straight(cur, anchor)...)
		}
		path = append(path, mid...)
	}
	if walled {
		path = append(path, *goal.Door)
		path = append(path, straight(*goal.Door, anchor)...)
	}
	return path
}

// route is Dijkstra over the open cells: cheapest way, not fewest cells, so
// residents follow the streets when the streets go their way. Neighbours are
// examined in a fixed order and ties break on the lower cell index, so a route
// is a function of its endpoints — the same journey every run.
func (w *World) route(from, to Cell) []Cell {
	src, dst := w.idx(from), w.idx(to)
	if src < 0 || dst < 0 || w.solid[src] || w.solid[dst] {
		return nil
	}
	n := len(w.cost)
	const inf = 1 << 30
	dist := make([]int, n)
	prev := make([]int, n)
	done := make([]bool, n)
	for i := range dist {
		dist[i], prev[i] = inf, -1
	}
	dist[src] = 0
	for {
		cur, best := -1, inf
		for i := 0; i < n; i++ {
			if !done[i] && dist[i] < best {
				cur, best = i, dist[i]
			}
		}
		if cur < 0 || cur == dst {
			break
		}
		done[cur] = true
		cx, cy := cur%w.Map.Width, cur/w.Map.Width
		for _, d := range [4][2]int{{0, 1}, {1, 0}, {0, -1}, {-1, 0}} {
			j := w.idx(Cell{cx + d[0], cy + d[1]})
			if j < 0 || w.solid[j] || done[j] {
				continue
			}
			if alt := dist[cur] + w.cost[j]; alt < dist[j] {
				dist[j], prev[j] = alt, cur
			}
		}
	}
	if dist[dst] == inf {
		return nil
	}
	var back []Cell
	for i := dst; i != src; i = prev[i] {
		back = append(back, Cell{i % w.Map.Width, i / w.Map.Width})
	}
	out := make([]Cell, len(back))
	for i, c := range back {
		out[len(back)-1-i] = c
	}
	return out
}

// buildingAt returns the walled place a cell is inside, if any.
func (w *World) buildingAt(c Cell) (Place, bool) {
	for _, p := range w.Map.Places {
		if p.Door != nil && p.Contains(c.X, c.Y) {
			return p, true
		}
	}
	return Place{}, false
}

// straight steps x then y, excluding the cell you start on and including the
// one you end on. Only used inside a rectangle, where every cell between two
// corners is part of the same room.
func straight(from, to Cell) []Cell {
	var out []Cell
	x, y := from.X, from.Y
	for x != to.X {
		if x < to.X {
			x++
		} else {
			x--
		}
		out = append(out, Cell{x, y})
	}
	for y != to.Y {
		if y < to.Y {
			y++
		} else {
			y--
		}
		out = append(out, Cell{x, y})
	}
	return out
}

func (w *World) idx(c Cell) int {
	if c.X < 0 || c.Y < 0 || c.X >= w.Map.Width || c.Y >= w.Map.Height {
		return -1
	}
	return c.Y*w.Map.Width + c.X
}

func (w *World) each(p Place, fn func(i int)) {
	for y := p.Y; y < p.Y+p.H; y++ {
		for x := p.X; x < p.X+p.W; x++ {
			if i := w.idx(Cell{x, y}); i >= 0 {
				fn(i)
			}
		}
	}
}
