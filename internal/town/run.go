package town

import (
	"context"
	"sort"
	"time"

	"github.com/metahumansworld/soscitea/internal/trace"
)

// Config paces the town. Simulated time advances TickMinutes per tick and one
// tick costs Interval of wall clock, so a day is watchable in a minute or two
// or stretched to real time by the same two numbers.
type Config struct {
	TickMinutes int           // simulated minutes per tick
	Interval    time.Duration // wall clock per tick
	Days        int           // stop after this many simulated days
	StartMinute int           // minute of the day the world wakes at

	// Mind is the thinking half, and it is optional. Nil is the town exactly
	// as it was before mind.go existed — schedules, walls and a clock, no
	// model and no calls. Set it and the same residents keep a memory stream,
	// say something when they meet, and sleep on the day.
	Mind *Minds

	// Visit, if set, is told once per tick where everyone stands, right after
	// the tick's frame lands on the trace. It is the town's one outward seam,
	// and it is money-free by construction: the town hands over a clock and a
	// list of standings and nothing else, so whatever an observer builds on
	// them — the fair in internal/orchestrator runs its whole bounty economy
	// on exactly this hook — none of it reaches back in. There is still no
	// money here. Nil is the town exactly as it was before the seam existed.
	Visit func(day, mod int, clock string, standings []Standing) error

	// Hold, if set, is asked once per resident per tick whether that resident
	// has been asked to stay where they are. True and they do not move: the
	// schedule is not consulted, no route is planned, and no arrival or
	// departure fires, because nothing about them changed. The visible
	// evidence of a hold is a resident who simply did not leave.
	//
	// It is the inward twin of Visit and money-free in exactly the same way.
	// The town is told *that* a body was asked to stay still, never why or at
	// what price — the fair in internal/orchestrator sells the standing and
	// burns the fee, and none of that reaches in here. There is still no
	// money in this package. Nil is the town as it was before the seam.
	Hold func(id string) bool

	// Arrive, if set, is asked once per tick, before anyone moves, whether
	// anybody new has come to town. Each Persona it hands back is seated at
	// their Home that minute, written to the trace as a "joined" line, and
	// from then on walked, held, visited and counted like everyone founded
	// with the map. Arriving is that resident's founding, not a meeting:
	// whoever they find at home is not "met" on the way in, the same way the
	// opening state fires no met — so a body that joins before the first
	// tick tells the same story as one seated at boot.
	//
	// It is the second inward seam, money-free like Hold: the town learns
	// that somebody came, never who let them in or what they paid for the
	// room. Nil is the town exactly as it was before the seam existed.
	Arrive func(day, mod int, clock string) []Persona

	// Checkpoint, if set, is handed the whole town at the end of every tick,
	// after Visit has returned — the one moment nothing is in flight: no
	// route half-walked, no conversation mid-sentence, no visitor's work
	// half-done. What it does with the state is its own business; an error
	// stops the run, because a world that cannot write itself down should
	// not go on pretending it can be picked up again.
	Checkpoint func(State) error

	// From, if set, is a State an earlier process left at a tick boundary,
	// and the run continues from the tick after it instead of founding the
	// town: no founded line, the residents where the state stood them, the
	// memories they had, and the counts the closed line will need. The
	// people argument is ignored — the roster is the state's — and the
	// map is still the caller's, since a checkpoint carries who stood
	// where and not the walls. The tick after the state is the first one
	// played, so a run resumed from tick T and a run that never stopped
	// write the same lines from T+1 on.
	From *State
}

// State is the town at a tick boundary, complete enough to continue from.
// Everything in it is what Run would otherwise have in hand at the top of
// the next loop: the roster with each body's position and the route it was
// walking, the meeting and speaking counts, and the thinking half's memory.
// The pairs currently sharing a place are not in it — they are recomputed
// from where everyone stands, the same fold the loop makes every tick.
type State struct {
	Tick       int
	Residents  []ResidentState
	Meetings   int
	Utterances int
	Thoughts   int
	Mind       *MindState // nil for a town without one
}

// ResidentState is one resident as the loop holds them: the whole persona,
// because a guest's schedule exists nowhere but here, and the motion.
type ResidentState struct {
	Persona Persona
	X, Y    int
	Place   string
	Goal    string
	Route   []Cell
}

// Standing is one resident's whereabouts as the Visit hook sees them: the
// place they are standing in, or "" for the street. Handed over in roster
// order, every tick.
type Standing struct {
	ID    string
	Place string
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

	// The thinking half's tally — all zero on a run with no Mind. The token
	// figures are what the provider reported, not what the town guessed.
	Utterances int
	Thoughts   int
	Calls      int
	InToks     int64
	OutToks    int64
}

// tally folds the mind's counters into the report. Called on both exits so an
// interrupted run still says what it spent thinking.
func (rep *Report) tally(mn *Minds) {
	if mn == nil {
		return
	}
	rep.Calls, rep.InToks, rep.OutToks = mn.Calls, mn.InToks, mn.OutToks
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
//	joined   a resident arrived after the founding, seated at home; only
//	         with an Arrive seam
//	depart   a resident leaves a place for the street
//	arrive   a resident reaches the place their schedule names
//	met      two residents are newly in the same place — the hook the memory
//	         stream attaches to
//	said     one turn of a conversation; only with a mind
//	reflected what a resident decides the day was; only with a mind
//	tick     the clock and every resident's position, once per tick
//	closed   the run is over
func Run(ctx context.Context, tw *trace.Writer, m Map, people []Persona, cfg Config) (Report, error) {
	cfg = cfg.withDefaults()
	mn := cfg.Mind
	if mn != nil {
		mn.withDefaults()
	}

	// Walls first: the world knows which cells are solid and where the doors
	// are, and it hands back a map with those doors filled in. Everything
	// downstream — the founded event, the routes, the viewer — reads that one.
	w := NewWorld(m)
	m = w.Map

	// Everyone starts where their schedule says they are, standing at the
	// anchor — the town wakes mid-story rather than everyone materialising
	// at home.
	// A presence key is made of ids, so the met fold below deals in ids.
	// Conversation needs the residents themselves; one index costs less than
	// scanning the roster twice per meeting. Indexed, never ranged over —
	// nothing map-ordered may reach a prompt.
	rs := make([]*resident, 0, len(people))
	byID := make(map[string]*resident, len(people))
	seat := func(p Persona, at string) *resident {
		pl, ok := m.Place(at)
		if !ok {
			pl, _ = m.Place(p.Home)
		}
		x, y := pl.Anchor()
		r := &resident{p: p, x: x, y: y, place: pl.ID, goal: pl.ID}
		rs = append(rs, r)
		byID[p.ID] = r
		return r
	}
	if cfg.From == nil {
		for _, p := range people {
			seat(p, p.At(cfg.StartMinute%DayMinutes).Place)
		}
	} else {
		for _, rs := range cfg.From.Residents {
			r := seat(rs.Persona, rs.Place)
			r.x, r.y, r.place, r.goal, r.route = rs.X, rs.Y, rs.Place, rs.Goal, rs.Route
		}
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
	if cfg.From == nil {
		if err := tw.Append(trace.EventTown, map[string]any{
			"action": "founded", "town": m.Name,
			"width": m.Width, "height": m.Height,
			"places": m.Places, "residents": founded,
		}); err != nil {
			return Report{}, err
		}
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
	first := 1
	if st := cfg.From; st != nil {
		first = st.Tick + 1
		rep.Ticks, rep.Meetings, rep.Utterances, rep.Thoughts = st.Tick, st.Meetings, st.Utterances, st.Thoughts
		if mn != nil {
			mn.load(st.Mind)
		}
	}
	total := cfg.Days * DayMinutes / cfg.TickMinutes
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	for t := first; t <= total; t++ {
		select {
		case <-ctx.Done():
			rep.Reason = "interrupted"
			rep.tally(mn)
			return rep, tw.Append(trace.EventTown, map[string]any{
				"action": "closed", "ticks": rep.Ticks, "days": cfg.Days,
				"meetings": rep.Meetings, "reason": rep.Reason,
			})
		case <-ticker.C:
		}

		if mn != nil {
			mn.tick = t // recency is measured in ticks, so stamp it first
		}

		minute := cfg.StartMinute + t*cfg.TickMinutes
		day := minute/DayMinutes + 1
		mod := minute % DayMinutes
		clock := HHMM(mod)

		// Newcomers first, so the rest of the minute — the hold, the walk,
		// the frame, the visit — already counts them. Seated at home: the
		// schedule is theirs from the next step on, but a body arriving in
		// town arrives at its lodgings, not wherever its day says it should
		// have been by now.
		if cfg.Arrive != nil {
			var came []*resident
			for _, p := range cfg.Arrive(day, mod, clock) {
				came = append(came, seat(p, p.Home))
			}
			if len(came) > 0 {
				// Whoever they find at home is not met on the way in — the
				// arrival is their founding — so the pairs they make go
				// straight into the set, as the opening state's did. Nobody
				// else has moved since the last fold, so every other key
				// here is already present.
				for k := range presence(rs) {
					together[k] = true
				}
			}
			for _, r := range came {
				if err := tw.Append(trace.EventTown, map[string]any{
					"action": "joined", "resident": r.p.ID, "name": r.p.Name,
					"blurb": r.p.Blurb, "home": r.p.Home,
					"x": r.x, "y": r.y, "place": r.place,
					"day": day, "clock": clock,
				}); err != nil {
					return rep, err
				}
			}
		}

		// Asked once per resident per tick, before anyone moves, and reused
		// for both the walking and the frame: Hold is somebody else's code,
		// so consulting it twice in one minute invites two different answers
		// and a resident who is held for the walk but not for the picture.
		// Nil Hold leaves this map nil, which reads false for everyone.
		var held map[string]bool
		if cfg.Hold != nil {
			held = make(map[string]bool, len(rs))
			for _, r := range rs {
				held[r.p.ID] = cfg.Hold(r.p.ID)
			}
		}

		for _, r := range rs {
			if held[r.p.ID] {
				r.walked = nil // last tick's trail is not this tick's
				continue
			}
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
				if mn != nil {
					// Place names carry their own article — "The Bell &
					// Bushel", "Mira's Cottage" — so the template adds none.
					text := "came to " + goal.Name
					if slot.Activity != "" {
						text += " — " + slot.Activity
					}
					mn.observe(r.p.ID, Memory{
						Day: day, Clock: clock, Kind: "arrive",
						Importance: impArrive, Text: text,
					})
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
			if mn == nil {
				continue
			}
			a, b := byID[pr[0]], byID[pr[1]]
			pl, ok := m.Place(pr[2])
			if a == nil || b == nil || !ok {
				continue
			}
			mn.observe(a.p.ID, Memory{Day: day, Clock: clock, Kind: "met",
				Importance: impMet, Text: "ran into " + b.p.Name + " at " + pl.Name})
			mn.observe(b.p.ID, Memory{Day: day, Clock: clock, Kind: "met",
				Importance: impMet, Text: "ran into " + a.p.Name + " at " + pl.Name})
			// Counted before the error is checked, so a conversation that dies
			// halfway still reports the lines it managed.
			n, err := mn.converse(ctx, tw, a, b, pl, day, clock)
			rep.Utterances += n
			if err != nil {
				return rep, err
			}
		}
		together = map[string]bool{}
		for k := range now {
			together[k] = true
		}

		// Evening. Everyone who has had a day and not yet slept on it decides
		// what it was, in roster order — the same order on every run. Emitted
		// before the tick frame, so the viewer has the thought in hand by the
		// time it draws the minute it happened in.
		if mn != nil {
			for _, r := range rs {
				if !mn.due(r.p.ID, day, mod) {
					continue
				}
				did, err := mn.reflect(ctx, tw, r, day, clock)
				if did {
					rep.Thoughts++
				}
				if err != nil {
					return rep, err
				}
			}
		}

		frames := make([]frame, 0, len(rs))
		for _, r := range rs {
			slot := r.p.At(mod)
			f := frame{
				ID: r.p.ID, X: r.x, Y: r.y, Place: r.place,
				Activity: slot.Activity, Goal: slot.Place, Path: r.walked,
			}
			// A held resident is not on their way anywhere, so the frame says
			// so instead of repeating a schedule they are visibly not keeping:
			// the goal is where they already stand, and the activity is the
			// waiting itself. Why they are waiting is not the town's business.
			if held[r.p.ID] {
				f.Activity, f.Goal = "waiting", r.place
			}
			frames = append(frames, f)
		}
		rep.Ticks = t
		if err := tw.Append(trace.EventTown, map[string]any{
			"action": "tick", "day": day, "clock": clock, "minute": mod,
			"residents": frames,
		}); err != nil {
			return rep, err
		}

		// The seam fires last, with the frame already written: whatever the
		// visitor records for this minute lands after the minute itself, so a
		// reader always meets the town's own account first.
		if cfg.Visit != nil {
			standings := make([]Standing, 0, len(rs))
			for _, r := range rs {
				standings = append(standings, Standing{ID: r.p.ID, Place: r.place})
			}
			if err := cfg.Visit(day, mod, clock, standings); err != nil {
				return rep, err
			}
		}

		if cfg.Checkpoint != nil {
			st := State{Tick: t, Meetings: rep.Meetings, Utterances: rep.Utterances, Thoughts: rep.Thoughts}
			for _, r := range rs {
				st.Residents = append(st.Residents, ResidentState{
					Persona: r.p, X: r.x, Y: r.y, Place: r.place, Goal: r.goal, Route: r.route,
				})
			}
			if mn != nil {
				st.Mind = mn.save()
			}
			if err := cfg.Checkpoint(st); err != nil {
				return rep, err
			}
		}
	}

	rep.tally(mn)
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
