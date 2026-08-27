// Package town is a small inhabited place that runs on a clock.
//
// It shares nothing with the arena but the spine: the same trace file, the
// same viewer, the same idea that what happened is a stream of typed events
// rather than a final report. There is no money here, no bidding, no ranking
// and — in this first pass — no model. Residents keep a daily schedule, walk
// between the places on a small map, and are recorded when they end up
// somewhere together.
//
// That last event is the point of the whole package. A "met" is the hook a
// memory stream attaches to: two residents in one room at one hour is the
// smallest fact a mind could later have an opinion about. Everything here is
// deliberately deterministic and unthinking so that when thinking is added,
// it is obvious which half is which.
package town

import "fmt"

// Place is one named area of the map, a rectangle of grid cells. Residents
// are "at" a place when they stand anywhere inside it, and they walk toward
// its anchor — the middle — to get there.
type Place struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Kind tells the viewer what to draw — home and shop are walled buildings,
	// plaza, market, and park are open ground with their own furniture, street
	// is paving. The simulation reads none of it: a street is never a schedule
	// goal, so decorative places cost nothing.
	Kind string `json:"kind"`
	X    int    `json:"x"`
	Y    int    `json:"y"`
	W    int    `json:"w"`
	H    int    `json:"h"`
	// Door is the one cell of a walled place that opens. NewWorld works it out
	// from the map and Run carries it in the founded event, so the viewer hangs
	// its doorway on the wall the residents actually walk through. Nil for
	// places you step straight onto.
	Door *Cell `json:"door,omitempty"`
}

// Contains reports whether a cell is inside this place.
func (p Place) Contains(x, y int) bool {
	return x >= p.X && x < p.X+p.W && y >= p.Y && y < p.Y+p.H
}

// Anchor is the cell residents walk to. Integer division, so it is the same
// cell every time — nothing in this package is allowed to wobble.
func (p Place) Anchor() (int, int) { return p.X + p.W/2, p.Y + p.H/2 }

// Map is the town's ground: a grid, and the places on it.
type Map struct {
	Name   string  `json:"town"`
	Width  int     `json:"width"`
	Height int     `json:"height"`
	Places []Place `json:"places"`
}

// Place looks one up by id.
func (m Map) Place(id string) (Place, bool) {
	for _, p := range m.Places {
		if p.ID == id {
			return p, true
		}
	}
	return Place{}, false
}

// Slot is one entry in a daily schedule: from this minute of the day, be at
// this place doing this. A schedule is a sorted list of slots and it wraps —
// before the first slot of the day, the last one is still in force, which is
// how "asleep at home from 21:30" covers the small hours without a slot at
// midnight.
type Slot struct {
	At       int // minute of the day, 0–1439
	Place    string
	Activity string
}

// Persona is a resident as authored: who they are and what their day looks
// like. Nothing here is generated, and in this pass nothing here is read by a
// model — the blurb exists because it is what a model will be handed next.
type Persona struct {
	ID       string
	Name     string
	Blurb    string
	Home     string
	Schedule []Slot
}

// At returns the slot in force at a minute of the day.
func (p Persona) At(minute int) Slot {
	cur := p.Schedule[len(p.Schedule)-1] // the wrap: yesterday's last slot
	for _, s := range p.Schedule {
		if s.At > minute {
			break
		}
		cur = s
	}
	return cur
}

// HHMM formats a minute of the day as a wall clock.
func HHMM(minute int) string {
	minute = ((minute % DayMinutes) + DayMinutes) % DayMinutes
	return fmt.Sprintf("%02d:%02d", minute/60, minute%60)
}

// DayMinutes is how long a day is. The town keeps human hours because the
// whole surface is meant to be read at a glance by a person.
const DayMinutes = 24 * 60

func hm(h, m int) int { return h*60 + m }

// Ashmere is the town this repository ships with: six named places, four
// cottages, a grid of streets, and four residents whose days overlap in
// specific places at specific hours. The overlaps are the content — the bakery
// at noon and the tavern at seven are where the encounters come from — so they
// are authored, not rolled.
func Ashmere() (Map, []Persona) {
	m := Map{
		Name:   "Ashmere",
		Width:  20,
		Height: 12,
		Places: []Place{
			{ID: "home-mira", Name: "Mira's Cottage", Kind: "home", X: 1, Y: 1, W: 3, H: 2},
			{ID: "home-osric", Name: "Osric's Rooms", Kind: "home", X: 1, Y: 4, W: 3, H: 2},
			{ID: "home-junia", Name: "Junia's Cottage", Kind: "home", X: 1, Y: 7, W: 3, H: 2},
			{ID: "home-pell", Name: "Pell's Loft", Kind: "home", X: 1, Y: 10, W: 3, H: 2},
			{ID: "bakery", Name: "The Ember Oven", Kind: "shop", X: 6, Y: 1, W: 4, H: 3},
			{ID: "library", Name: "The Archive", Kind: "shop", X: 12, Y: 1, W: 4, H: 3},
			{ID: "square", Name: "Town Square", Kind: "plaza", X: 7, Y: 5, W: 6, H: 3},
			{ID: "market", Name: "Market Stalls", Kind: "market", X: 15, Y: 5, W: 4, H: 3},
			{ID: "tavern", Name: "The Bell & Bushel", Kind: "shop", X: 6, Y: 9, W: 5, H: 2},
			{ID: "garden", Name: "Physic Garden", Kind: "park", X: 13, Y: 9, W: 5, H: 2},

			// The streets. Never a schedule goal, so the simulation cannot
			// see them — but the router costs paving lower than lawn, so
			// laying a street is how you decide where people will walk.
			{ID: "north-row", Kind: "street", X: 0, Y: 0, W: 20, H: 1},
			{ID: "the-avenue", Kind: "street", X: 4, Y: 1, W: 2, H: 11},
			{ID: "high-street", Kind: "street", X: 6, Y: 4, W: 14, H: 1},
			{ID: "low-lane", Kind: "street", X: 6, Y: 8, W: 14, H: 1},

			// Front paths: two paved cells from each cottage door to the
			// avenue, and the tavern's yard. Without them residents reach a
			// door by trampling whatever lawn is nearest.
			{ID: "path-mira", Kind: "street", X: 2, Y: 3, W: 2, H: 1},
			{ID: "path-osric", Kind: "street", X: 2, Y: 6, W: 2, H: 1},
			{ID: "path-junia", Kind: "street", X: 2, Y: 9, W: 2, H: 1},
			{ID: "bell-yard", Kind: "street", X: 6, Y: 11, W: 5, H: 1},
		},
	}

	people := []Persona{{
		ID: "mira", Name: "Mira", Home: "home-mira",
		Blurb: "the baker; up before anyone, in bed before anyone, counts the day in loaves",
		Schedule: []Slot{
			{hm(4, 0), "bakery", "firing the oven"},
			{hm(7, 0), "bakery", "serving the morning queue"},
			{hm(12, 30), "square", "eating on the steps"},
			{hm(13, 30), "bakery", "the second bake"},
			{hm(17, 0), "market", "selling off the day's end loaves"},
			{hm(19, 0), "tavern", "supper, and not cooking it"},
			{hm(21, 30), "home-mira", "asleep"},
		},
	}, {
		ID: "osric", Name: "Osric", Home: "home-osric",
		Blurb: "the archivist; believes every argument has already been settled, in writing, somewhere",
		Schedule: []Slot{
			{hm(7, 30), "home-osric", "reading over breakfast"},
			{hm(9, 0), "library", "cataloguing"},
			{hm(12, 30), "garden", "walking, ostensibly for the air"},
			{hm(13, 30), "library", "cataloguing"},
			{hm(18, 0), "tavern", "arguing about precedence"},
			{hm(21, 0), "home-osric", "asleep"},
		},
	}, {
		ID: "junia", Name: "Junia", Home: "home-junia",
		Blurb: "the trader; knows the price of everything on the map and the mood of everyone on it",
		Schedule: []Slot{
			{hm(5, 30), "market", "setting up the stall"},
			{hm(7, 0), "market", "trading"},
			{hm(12, 0), "bakery", "buying bread, hearing the news"},
			{hm(12, 45), "market", "trading"},
			{hm(17, 30), "square", "counting the take"},
			{hm(18, 30), "tavern", "supper"},
			{hm(22, 0), "home-junia", "asleep"},
		},
	}, {
		ID: "pell", Name: "Pell", Home: "home-pell",
		Blurb: "keeps the tavern; awake for the half of the day the others are not",
		Schedule: []Slot{
			{hm(1, 0), "home-pell", "asleep"},
			{hm(11, 0), "market", "buying in"},
			{hm(12, 30), "garden", "cutting herbs"},
			{hm(14, 0), "tavern", "opening up"},
			{hm(23, 30), "tavern", "putting the chairs up"},
		},
	}}

	return m, people
}
