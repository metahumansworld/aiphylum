// The thinking half of the town.
//
// town.go and run.go are deliberately unthinking: schedules, walls, routes and
// a clock. This file is the other half — residents keep what happened to them,
// say something when they meet, and at the end of the day decide what the day
// was. It is opt-in, and a town with cfg.Mind unset behaves exactly as it did
// before this file existed.
//
// Two rules hold the whole thing to the determinism the track is pinned to:
// nothing here reads the wall clock (simulated day and clock only), and nothing
// that reaches a prompt is ever iterated out of a map. Memory streams are
// slices; the maps below are only ever indexed by a resident id that came from
// the roster slice.
package town

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/metahumansworld/aiphylum/internal/mind"
	"github.com/metahumansworld/aiphylum/internal/proxy"
	"github.com/metahumansworld/aiphylum/internal/trace"
)

// Memory is one thing a resident noticed, in their own stream. Importance is
// fixed by kind rather than judged: running into somebody matters more than
// walking into a room, and a conclusion you drew yourself matters most. A model
// could be asked to rate it instead, and that would be a second call per
// observation for a number this town does not need.
type Memory struct {
	Day        int
	Clock      string
	Kind       string // arrive, met, heard, thought
	Text       string
	Importance int
	tick       int // when it landed, for recency
}

// Importance by kind.
const (
	impArrive  = 1
	impHeard   = 2
	impMet     = 3
	impThought = 5
)

// turnInHour is when residents stop and look back — 22:00. Reflecting on a day
// boundary instead would tie the habit to the run's start minute, and a town
// woken at seven would reflect at seven, which is nobody's idea of an evening.
const turnInHour = 22 * 60

// Minds is the town's thinking, hung off Config. It calls a Provider directly
// rather than going through internal/proxy: the proxy's wallets, holds and
// refusals exist to meter code the platform does not trust — agents in
// containers — and residents are neither. What the proxy would have given us
// that matters here is the usage figure, and that comes from the provider
// itself, so a resident's tokens are still measured rather than self-reported.
// Only the charging is absent, which is the point of a track whose first
// sentence is that there is no money in it.
//
// If the town is ever pointed at a real model, that call belongs back inside
// the proxy on a funded wallet, exactly like the judge's.
type Minds struct {
	Provider  proxy.Provider
	Model     string
	MaxTokens int64
	Turns     int // how many lines an exchange runs to
	Recall    int // how many memories a prompt carries

	streams map[string][]Memory
	spoke   map[string]int // resident → last day they turned in
	tick    int

	Calls   int
	InToks  int64
	OutToks int64
}

func (mn *Minds) withDefaults() {
	if mn.Model == "" {
		mn.Model = "claude-haiku-4-5-20251001"
	}
	if mn.MaxTokens <= 0 {
		mn.MaxTokens = 128
	}
	if mn.Turns <= 0 {
		mn.Turns = 3
	}
	if mn.Recall <= 0 {
		mn.Recall = 6
	}
	mn.streams = map[string][]Memory{}
	mn.spoke = map[string]int{}
}

// observe files a memory. Observations are not traced: every one of them is
// derivable from an arrive or met event already in the stream, and writing them
// again would double the trace to say nothing new.
func (mn *Minds) observe(id string, m Memory) {
	m.tick = mn.tick
	mn.streams[id] = append(mn.streams[id], m)
}

// recall picks the memories worth putting in front of a resident. The score is
// the classic three — how much it mattered, how lately, and how much it has to
// do with what is being asked — with keyword overlap standing in for relevance.
// There are no embeddings here because there is no budget for them, and a
// cosine over hashed words would be the same keyword match wearing a costume.
func (mn *Minds) recall(id, query string, day int, sameDayOnly bool) []Memory {
	var words []string
	for _, w := range strings.Fields(strings.ToLower(query)) {
		if len(w) >= 4 {
			words = append(words, w)
		}
	}
	var pool []Memory
	for _, m := range mn.streams[id] {
		if sameDayOnly && m.Day != day {
			continue
		}
		pool = append(pool, m)
	}
	// Stable, so memories that score the same stay in the order they happened
	// rather than in whatever order the sort felt like.
	sort.SliceStable(pool, func(i, j int) bool {
		return mn.score(pool[i], words) > mn.score(pool[j], words)
	})
	if len(pool) > mn.Recall {
		pool = pool[:mn.Recall]
	}
	return pool
}

func (mn *Minds) score(m Memory, words []string) float64 {
	recency := 1 / float64(1+mn.tick-m.tick)
	relevance := 0.0
	if len(words) > 0 {
		low := strings.ToLower(m.Text)
		hit := 0
		for _, w := range words {
			if strings.Contains(low, w) {
				hit++
			}
		}
		relevance = float64(hit) / float64(len(words))
	}
	return float64(m.Importance) + 2*recency + 3*relevance
}

func lines(ms []Memory) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Text)
	}
	return out
}

// ask puts one question and reads one answer. An unreadable reply is an error
// rather than a shrug: a resident with nothing to say says so in words, so
// silence here means the model did not answer the question that was asked.
func (mn *Minds) ask(ctx context.Context, req mind.Request) (string, proxy.Usage, error) {
	body, err := json.Marshal(map[string]any{
		"model":      mn.Model,
		"max_tokens": mn.MaxTokens,
		"messages": []map[string]string{
			{"role": "user", "content": mind.Prompt(req)},
		},
	})
	if err != nil {
		return "", proxy.Usage{}, err
	}
	raw, usage, err := mn.Provider.Invoke(ctx, mn.Model, body)
	mn.Calls++
	mn.InToks += usage.InputTokens
	mn.OutToks += usage.OutputTokens
	if err != nil {
		return "", usage, fmt.Errorf("mind call: %w", err)
	}
	var reply struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return "", usage, fmt.Errorf("mind call: unreadable reply: %w", err)
	}
	var text string
	for _, c := range reply.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}
	line, ok := mind.ParseReply(text)
	if !ok {
		return "", usage, fmt.Errorf("mind call: no answer line in %q", text)
	}
	return line, usage, nil
}

// converse runs a whole exchange at the tick the two residents met, rather than
// a line per tick. Spreading it out would mean carrying half-finished
// conversations across the loop, and would let two people drift apart mid
// sentence; a meeting is one moment, so it gets one.
//
// Only the listener files what was said. The speaker already has the meeting
// itself in their stream, and a resident who remembered their own remarks would
// spend tomorrow reflecting on their own turns of phrase.
func (mn *Minds) converse(ctx context.Context, tw *trace.Writer, a, b *resident, pl Place, day int, clock string) (int, error) {
	speakers := [2]*resident{a, b}
	heard := ""
	said := 0
	for turn := 0; turn < mn.Turns; turn++ {
		sp := speakers[turn%2]
		to := speakers[(turn+1)%2]
		mems := mn.recall(sp.p.ID, to.p.Name+" "+pl.Name, day, false)
		line, usage, err := mn.ask(ctx, mind.Request{
			Who: sp.p.Name, Blurb: sp.p.Blurb,
			When: fmt.Sprintf("day %d, %s", day, clock), Where: pl.Name,
			With: to.p.Name, Heard: heard,
			Memories: lines(mems), Ask: mind.AskSay,
		})
		if err != nil {
			return said, err
		}
		if err := tw.Append(trace.EventTown, map[string]any{
			"action": "said", "resident": sp.p.ID, "name": sp.p.Name,
			"to": to.p.ID, "place": pl.ID, "text": line, "turn": turn + 1,
			"day": day, "clock": clock,
			"in_toks": usage.InputTokens, "out_toks": usage.OutputTokens,
		}); err != nil {
			return said, err
		}
		mn.observe(to.p.ID, Memory{
			Day: day, Clock: clock, Kind: "heard", Importance: impHeard,
			Text: sp.p.Name + " said: " + line,
		})
		heard = line
		said++
	}
	return said, nil
}

// reflect is the end of a resident's day: what the day was, in one line, filed
// back into the stream as the most important thing in it. Tomorrow's
// conversations can retrieve it, which is the only reason a reflection is worth
// making at all — a thought nobody can recall is a log line.
func (mn *Minds) reflect(ctx context.Context, tw *trace.Writer, r *resident, day int, clock string) (bool, error) {
	// Settled up front: a resident with nothing to say has still had their
	// evening, and leaving the debt open would have due() re-asking every
	// tick until midnight.
	mn.spoke[r.p.ID] = day
	mems := mn.recall(r.p.ID, "", day, true)
	if len(mems) == 0 {
		return false, nil // nothing happened; inventing a day would be worse
	}
	cites := lines(mems)
	line, usage, err := mn.ask(ctx, mind.Request{
		Who: r.p.Name, Blurb: r.p.Blurb,
		When: fmt.Sprintf("day %d, %s", day, clock), Where: "",
		Memories: cites, Ask: mind.AskReflect,
	})
	if err != nil {
		return false, err
	}
	if err := tw.Append(trace.EventTown, map[string]any{
		"action": "reflected", "resident": r.p.ID, "name": r.p.Name,
		"text": line, "cites": cites, "day": day, "clock": clock,
		"in_toks": usage.InputTokens, "out_toks": usage.OutputTokens,
	}); err != nil {
		return false, err
	}
	mn.observe(r.p.ID, Memory{
		Day: day, Clock: clock, Kind: "thought", Importance: impThought,
		Text: line,
	})
	return true, nil
}

// due reports whether this resident still owes the day a thought. Asking by day
// rather than by an exact clock reading keeps the habit intact at any tick
// size: a town on seven-minute ticks never lands on 22:00 exactly.
func (mn *Minds) due(id string, day, mod int) bool {
	return mod >= turnInHour && mn.spoke[id] != day
}
