package town

import (
	"context"
	"sort"
	"time"

	"github.com/singhtushant3-hub/aiphylum/internal/trace"
)

// Config paces the town. Simulated time advances TickMinutes per tick and one
// tick costs Interval of wall clock, so a day is watchable in a minute or two
// or stretched to real time by the same two numbers.
type Config struct {
	TickMinutes int           // simulated minutes per tick
	Interval    time.Duration // wall clock per tick
	Days        int           // stop after this many simulated days
	StartMinute int           // minute of the day the world wakes at
}

func (c Config) withDefaults() Config {
	if c.TickMinutes <= 0 {
		c.TickMinutes = 10
	}
	if c.Interval <= 0 {
		c.Interval = 700 * time.Millisecond
	}
	if c.Days <= 0 {
		c.Days = 1
	}
	return c
}

// Report is what a run amounts to, printed by the caller.
type Report struct {
	Ticks    int
	Days     int
	Meetings int
	Reason   string // "day complete" or "interrupted"
}

// walkSpeed is cells per tick: four cells in ten simulated minutes. Slow
// enough that the viewer has a walk to animate between ticks, quick enough
// that the long way round a building still fits inside the window a schedule
// leaves for it — cut it to three and Junia misses Mira at the bakery.
const walkSpeed = 4

// resident is one persona in motion.
type resident struct {
	p     Persona
	x, y  int
	place string // the place stood in, "" for the street
	goal  string // the place the route leads to
	route []Cell // the cells left to walk, in order

	walked [][2]int // the cells covered this tick, for the viewer to animate
}

// Run plays the town onto the trace. Everything below is deterministic — the
// only nondeterminism in a run is where the interrupt lands — so the same
// config always writes the same events in the same order.
//
// The event grammar, all trace type "town" (payloads avoid the keys the
// viewer reserves: time, label, seq, type):
//
//	founded  the map and the roster, always the first line
//	depart   a resident leaves a place for the street
//	arrive   a resident reaches the place their schedule names
//	met      two residents are newly in the same place — the hook a memory
//	         stream will attach to
//	tick     the clock and every resident's position, once per tick
//	closed   the run is over
func Run(ctx context.Context, tw *trace.Writer, m Map, people []Persona, cfg Config) (Report, error) {
	cfg = cfg.withDefaults()

	// Walls first: the world knows which cells are solid and where the doors
	// are, and it hands back a map with those doors filled in. Everything
	// downstream — the founded event, the routes, the viewer — reads that one.
	w := NewWorld(m)
	m = w.Map

	// Everyone starts where their schedule says they are, standing at the
	// anchor — the town wakes mid-story rather than everyone materialising
	// at home.
	rs := make([]*resident, 0, len(people))
	for _, p := range people {
		slot := p.At(cfg.StartMinute % DayMinutes)
		pl, ok := m.Place(slot.Place)
		if !ok {
			pl, _ = m.Place(p.Home)
		}
		x, y := pl.Anchor()
		rs = append(rs, &resident{p: p, x: x, y: y, place: pl.ID, goal: pl.ID})
	}

	type frame struct {
		ID       string `json:"id"`
		Name     string `json:"name,omitempty"`
		Blurb    string `json:"blurb,omitempty"`
		Home     string `json:"home,omitempty"`
		X        int    `json:"x"`
		Y        int    `json:"y"`
		Place    string `json:"place"`
		Activity string `json:"activity,omitempty"`
		Goal     string `json:"goal,omitempty"`
		// Path is the cells walked during this tick, in order. The viewer
		// animates along it, so a resident is seen to round the corner rather
		// than to appear on the far side of the wall.
		Path [][2]int `json:"path,omitempty"`
	}

	founded := make([]frame, 0, len(rs))
	for _, r := range rs {
		founded = append(founded, frame{
			ID: r.p.ID, Name: r.p.Name, Blurb: r.p.Blurb, Home: r.p.Home,
			X: r.x, Y: r.y, Place: r.place,
		})
	}
	if err := tw.Append(trace.EventTown, map[string]any{
		"action": "founded", "town": m.Name,
		"width": m.Width, "height": m.Height,
		"places": m.Places, "residents": founded,
	}); err != nil {
		return Report{}, err
	}

	// together holds the pairs currently sharing a place, keyed a|b|place with
	// a < b. A "met" fires when a pair enters the set and the key clears when
	// either leaves, so lunch together is one meeting, not one per tick — and
	// meeting again at supper is a second one, as it should be.
	together := map[string]bool{}
	seed := presence(rs)
	for k := range seed {
		together[k] = true // the opening state is the founding, not an event
	}

	rep := Report{Days: cfg.Days, Reason: "day complete"}
	total := cfg.Days * DayMinutes / cfg.TickMinutes
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	for t := 1; t <= total; t++ {
		select {
		case <-ctx.Done():
			rep.Reason = "interrupted"
			return rep, tw.Append(trace.EventTown, map[string]any{
				"action": "closed", "ticks": rep.Ticks, "days": cfg.Days,
				"meetings": rep.Meetings, "reason": rep.Reason,
			})
		case <-ticker.C:
		}

		minute := cfg.StartMinute + t*cfg.TickMinutes
		day := minute/DayMinutes + 1
		mod := minute % DayMinutes
		clock := HHMM(mod)

		for _, r := range rs {
			slot := r.p.At(mod)
			goal, ok := m.Place(slot.Place)
			if !ok {
				continue
			}
			// A new destination is a new route, worked out once and then
			// walked a few cells at a time. Recomputing every tick would cost
			// nothing here, but walking a route you committed to is what keeps
			// a resident from oscillating between two equal ways round.
			if r.goal != slot.Place {
				r.goal, r.route = slot.Place, w.Route(Cell{r.x, r.y}, slot.Place)
			}
			r.walked = nil
			for i := 0; i < walkSpeed && len(r.route) > 0; i++ {
				c := r.route[0]
				r.route = r.route[1:]
				r.x, r.y = c.X, c.Y
				r.walked = append(r.walked, [2]int{c.X, c.Y})
			}
			// A resident is "at" a place only when it is the one their
			// schedule names: walking through the square on the way to the
			// tavern is passing, not visiting, and fires nothing.
			was := r.place
			if goal.Contains(r.x, r.y) {
				r.place = goal.ID
			} else {
				r.place = ""
			}
			if r.place == was {
				continue
			}
			if was != "" {
				if err := tw.Append(trace.EventTown, map[string]any{
					"action": "depart", "resident": r.p.ID, "place": was,
					"day": day, "clock": clock,
				}); err != nil {
					return rep, err
				}
			}
			if r.place != "" {
				if err := tw.Append(trace.EventTown, map[string]any{
					"action": "arrive", "resident": r.p.ID, "place": r.place,
					"activity": slot.Activity, "day": day, "clock": clock,
				}); err != nil {
					return rep, err
				}
			}
		}

		now := presence(rs)
		for _, k := range sortedKeys(now) {
			if together[k] {
				continue
			}
			pr := now[k]
			rep.Meetings++
			if err := tw.Append(trace.EventTown, map[string]any{
				"action": "met", "a": pr[0], "b": pr[1], "place": pr[2],
				"day": day, "clock": clock,
			}); err != nil {
				return rep, err
			}
		}
		together = map[string]bool{}
		for k := range now {
			together[k] = true
		}

		frames := make([]frame, 0, len(rs))
		for _, r := range rs {
			slot := r.p.At(mod)
			frames = append(frames, frame{
				ID: r.p.ID, X: r.x, Y: r.y, Place: r.place,
				Activity: slot.Activity, Goal: slot.Place, Path: r.walked,
			})
		}
		rep.Ticks = t
		if err := tw.Append(trace.EventTown, map[string]any{
			"action": "tick", "day": day, "clock": clock, "minute": mod,
			"residents": frames,
		}); err != nil {
			return rep, err
		}
	}

	return rep, tw.Append(trace.EventTown, map[string]any{
		"action": "closed", "ticks": rep.Ticks, "days": cfg.Days,
		"meetings": rep.Meetings, "reason": rep.Reason,
	})
}

// presence maps each co-located pair to [a, b, place], keyed a|b|place with
// the ids ordered.
func presence(rs []*resident) map[string][3]string {
	out := map[string][3]string{}
	for i := 0; i < len(rs); i++ {
		for j := i + 1; j < len(rs); j++ {
			a, b := rs[i], rs[j]
			if a.place == "" || a.place != b.place {
				continue
			}
			ia, ib := a.p.ID, b.p.ID
			if ia > ib {
				ia, ib = ib, ia
			}
			out[ia+"|"+ib+"|"+a.place] = [3]string{ia, ib, a.place}
		}
	}
	return out
}

// sortedKeys keeps the met events in one order regardless of map iteration.
func sortedKeys(m map[string][3]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
