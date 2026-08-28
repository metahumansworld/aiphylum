// Package mind is the wire format for a resident who thinks.
//
// A town resident accumulates memories of what they did and who they ran into,
// reflects on them when the day ends, and says something when they meet
// somebody. All three are one question put to a model, and this package owns
// the shape of that question and nothing else.
//
// It lives alone, importing nothing else in the tree, for the same reason
// internal/judge does: two seams must agree on the format byte for byte — the
// town that writes the prompt, and the stub provider that answers it offline.
// A package that depends on nothing can be imported by both without a cycle.
//
// The stub's answers here are dumb on purpose but never arbitrary. Reflect and
// Say are genuine functions of what they are handed, so a resident who spent
// the day at the bakery really does think about the bakery, and a prompt built
// wrong produces a visibly wrong reply instead of plausible-looking noise. That
// is the same bargain judge.Grade makes: a stupid mind, but an honest one.
package mind

import (
	"sort"
	"strconv"
	"strings"
)

// Marker opens every mind prompt. A model — real or stub — that sees this line
// first knows it is being asked to be somebody, not to be helpful.
const Marker = "phylum-mind/1"

// Section labels, in the order Prompt emits them.
const (
	SecWho      = "WHO"
	SecWhen     = "WHEN"
	SecWhere    = "WHERE"
	SecWith     = "WITH"
	SecHeard    = "HEARD"
	SecMemories = "MEMORIES"
	SecAsk      = "ASK"
)

const secEnd = "END"

// The two questions a mind can be asked. One marker covers both: the ask is a
// section like any other, so adding a third kind costs a constant and a branch.
const (
	AskReflect = "REFLECT"
	AskSay     = "SAY"
)

// Reply line prefixes. A mind answers in one line, the way a judge answers in
// two — rigid enough for the stub to parse and plain enough for a real model
// to obey.
const (
	PrefixThought = "THOUGHT:"
	PrefixSay     = "SAY:"
)

// Request is everything a resident is shown when asked to think. Memories
// arrive already retrieved and already ordered: choosing what to remember is
// the town's job, not the format's.
type Request struct {
	Who      string   // the resident's name
	Blurb    string   // who they are, as authored
	When     string   // simulated day and clock — never wall-clock time
	Where    string   // the place they are standing in
	With     string   // who they are speaking to; empty when reflecting
	Heard    string   // what the other just said; empty on an opening turn
	Memories []string // most salient first
	Ask      string   // AskReflect or AskSay
}

// Prompt renders the question. The shape is rigid on purpose — Answer parses
// it straight back out, so the format is exercised in both directions every
// time the stub replies.
func Prompt(r Request) string {
	var b strings.Builder
	b.WriteString(Marker + "\n")
	if r.Ask == AskSay {
		b.WriteString("You are the resident described below, in the middle of your day.\n")
		b.WriteString("Say one short line to the person you have just run into. Speak as\n")
		b.WriteString("them, not about them. Reply with exactly one line and nothing else:\n")
		b.WriteString(PrefixSay + " <one short sentence>\n")
	} else {
		b.WriteString("You are the resident described below, at the end of the day.\n")
		b.WriteString("Look back over the memories and say what stays with you. Reply with\n")
		b.WriteString("exactly one line and nothing else:\n")
		b.WriteString(PrefixThought + " <one short sentence>\n")
	}
	for _, s := range []struct{ label, body string }{
		{SecWho, strings.TrimSpace(r.Who + "\n" + r.Blurb)},
		{SecWhen, r.When},
		{SecWhere, r.Where},
		{SecWith, r.With},
		{SecHeard, r.Heard},
		{SecMemories, strings.Join(r.Memories, "\n")},
		{SecAsk, r.Ask},
	} {
		b.WriteString("\n--- " + s.label + " ---\n")
		b.WriteString(s.body + "\n")
	}
	b.WriteString("\n--- " + secEnd + " ---\n")
	return b.String()
}

// Section pulls one labelled block back out of a prompt. Returns "" if the
// block is missing or unterminated. Deliberately identical to judge.Section:
// the two formats are siblings and should stay readable as such.
func Section(prompt, label string) string {
	open := "--- " + label + " ---\n"
	i := strings.Index(prompt, open)
	if i < 0 {
		return ""
	}
	rest := prompt[i+len(open):]
	j := strings.Index(rest, "\n--- ")
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}

// Lines splits a memory block back into the lines Prompt joined.
func Lines(block string) []string {
	var out []string
	for _, l := range strings.Split(block, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// ParseReply reads a mind's reply. A reply carrying neither line is not a
// resident with nothing to say — it is a model that did not answer the
// question, and the caller has to be able to tell those apart.
func ParseReply(text string) (string, bool) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		up := strings.ToUpper(line)
		for _, p := range []string{PrefixThought, PrefixSay} {
			if strings.HasPrefix(up, p) {
				return strings.TrimSpace(line[len(p):]), true
			}
		}
	}
	return "", false
}

// Answer is the stub's reply to a prompt this package wrote. The proxy hands
// the whole prompt over rather than pulling sections out at the call site the
// way it does for the judge: two sections inline readably, seven would not, and
// what the sections mean belongs here in any case.
func Answer(prompt string) string {
	mems := Lines(Section(prompt, SecMemories))
	if Section(prompt, SecAsk) == AskSay {
		who, _, _ := strings.Cut(Section(prompt, SecWho), "\n")
		return PrefixSay + " " + Say(who, Section(prompt, SecWith), Section(prompt, SecHeard), mems)
	}
	return PrefixThought + " " + Reflect(mems)
}

// Reflect is the stub mind's end-of-day rule: name what the day was mostly
// made of, and how often. It is a word tally, and the demo says so out loud — a
// real model reads the memories, this one counts them. What matters offline is
// that the answer is a genuine function of the day, so a resident who never
// left the library really does end up thinking about the library, and a
// resident handed no memories says so rather than inventing a day.
func Reflect(memories []string) string {
	top := salient(memories, 2)
	if len(top) == 0 {
		return "a quiet day — nothing in it worth keeping"
	}
	parts := make([]string, 0, len(top))
	for _, t := range top {
		parts = append(parts, t.word+" "+spell(t.n))
	}
	return strings.Join(parts, ", ") + " — that is where the day went"
}

// Say is the stub mind's rule for one turn: answer with what you just heard
// and what is already on your mind. Dumb, but responsive — what a resident says
// depends on what was said to them, so an exchange reads as a conversation
// rather than as two monologues taking turns.
func Say(who, with, heard string, memories []string) string {
	mine := first(salient(memories, 1, who))
	if strings.TrimSpace(heard) == "" {
		greet := "there you are"
		if with != "" {
			greet += ", " + with
		}
		if mine == "" {
			return greet + "."
		}
		return greet + " — " + mine + " has been on my mind."
	}
	// Their own name and the name they are being called by are address, not
	// subject: greeted by name, a resident would otherwise reply about
	// themselves. A third party named in passing stays salient, which is the
	// distinction worth keeping.
	theirs := first(salient([]string{heard}, 1, who, with))
	switch {
	case theirs != "" && mine != "" && theirs != mine:
		return theirs + ", you say — and here I am still on " + mine + "."
	case theirs != "":
		return theirs + ", yes. the same for me."
	case mine != "":
		return "still " + mine + ", for my part."
	}
	return "hm."
}

// tally is one word, how many of the lines carried it, and where it first
// turned up.
type tally struct {
	word string // as first written, so a name keeps its capital
	n    int
	at   int // first appearance, which is how ties are settled
}

// salient returns the n words running through the most of these lines, most
// common first. A word counts once per line, not once per occurrence, so the
// tally measures how much of the day a thing touched rather than how wordy one
// memory of it was.
//
// Ties go to whichever word turned up first. That is not just a coin-toss made
// repeatable: a memory leads with its subject and trails off into what was done
// there, so on a day where everything happened once, "the library — file the
// ledgers" is a day about the library and not about filing. Alphabetical order
// would have picked "file", which is deterministic and useless.
//
// The result is read out of a sorted slice rather than a map because the town
// this feeds is pinned to byte-identical reruns, and Go randomizes map order
// per map — a tie settled by iteration would pass most runs and fail some.
func salient(lines []string, n int, except ...string) []tally {
	skip := map[string]bool{}
	for _, e := range except {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			skip[e] = true
		}
	}
	counts := map[string]int{}
	form := map[string]string{}
	at := map[string]int{}
	for _, l := range lines {
		seen := map[string]bool{}
		for _, w := range strings.Fields(l) {
			w = strings.Trim(w, ".,;:!?'\"()[]—–-")
			key := strings.ToLower(w)
			if len(key) < 4 || stop[key] || skip[key] || seen[key] {
				continue
			}
			seen[key] = true
			counts[key]++
			if _, ok := form[key]; !ok {
				form[key], at[key] = w, len(at)
			}
		}
	}
	out := make([]tally, 0, len(counts))
	for k, c := range counts {
		out = append(out, tally{word: form[k], n: c, at: at[k]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].at < out[j].at
	})
	// "bakery 3 times, bake once" is one observation wearing two hats. Words
	// that share a stem are the same subject as far as a tally can tell, so the
	// second one is passed over for something that says a different thing.
	top := make([]tally, 0, n)
	for _, t := range out {
		if len(top) == n {
			break
		}
		dup := false
		for _, k := range top {
			if sameStem(k.word, t.word) {
				dup = true
				break
			}
		}
		if !dup {
			top = append(top, t)
		}
	}
	return top
}

// sameStem is a poor man's stemmer: two words are the same subject when one
// starts the other and the shorter is long enough to mean something. It knows
// nothing about English and does not need to — it only has to stop a tally
// counting "bake" and "bakery" as two findings.
func sameStem(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	if len(a) > len(b) {
		a, b = b, a
	}
	return len(a) >= 4 && strings.HasPrefix(b, a)
}

func first(t []tally) string {
	if len(t) == 0 {
		return ""
	}
	return t[0].word
}

func spell(n int) string {
	switch n {
	case 1:
		return "once"
	case 2:
		return "twice"
	}
	return strconv.Itoa(n) + " times"
}

// stop holds the words a mind must not mistake for something that happened:
// ordinary English glue, plus the connective tissue of this package's own
// sentences. That second half is load-bearing. Residents remember what they
// said to each other, so today's phrasing is tomorrow's input — without this,
// a few days in, everyone is reflecting on the word "still".
var stop = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`
		about after again against alone along already also although always among
		another anything around away back because been before behind being
		below beside between both came come coming could does doing done down
		during each either else even ever every from further gets getting give
		given goes going gone good have having here hers herself himself
		however into itself just keep keeping kept know known last late later
		least left less like little long made make making many mean might mind mine
		more most much must myself near need never next nothing once only onto
		other ours ourselves over part past perhaps rather really said same says
		seem seen shall should since some something soon still such take taken
		than that their theirs them themselves then there these they thing
		things think this those though three through thus times together took
		toward twice under until upon used using very want was way well went
		were what when where whether which while whom whose will with within
		without worth would yes yet your yours yourself
	`) {
		stop[w] = true
	}
}
