package mind

import (
	"strings"
	"testing"
)

func reflectReq(mems []string) Request {
	return Request{
		Who: "Mira", Blurb: "the baker", When: "day 1, 22:00",
		Where: "her house", Memories: mems, Ask: AskReflect,
	}
}

// TestPromptRoundTrip pins the format in both directions: every section Prompt
// writes, Section reads back. The stub answers by parsing its own prompt, so a
// format that survives this is a format both seams agree on.
func TestPromptRoundTrip(t *testing.T) {
	r := Request{
		Who: "Osric", Blurb: "the archivist", When: "day 2, 12:20",
		Where: "the bakery", With: "Mira", Heard: "there you are.",
		Memories: []string{"the bakery — buy the morning bread", "Mira, at the bakery"},
		Ask:      AskSay,
	}
	p := Prompt(r)
	if !strings.HasPrefix(p, Marker) {
		t.Fatalf("prompt does not open with the marker:\n%s", p)
	}
	for _, c := range []struct{ label, want string }{
		{SecWhen, r.When}, {SecWhere, r.Where}, {SecWith, r.With},
		{SecHeard, r.Heard}, {SecAsk, r.Ask},
	} {
		if got := Section(p, c.label); got != c.want {
			t.Errorf("Section(%s) = %q, want %q", c.label, got, c.want)
		}
	}
	if got := Lines(Section(p, SecMemories)); len(got) != 2 || got[0] != r.Memories[0] {
		t.Errorf("memories round-tripped as %q", got)
	}
	if !strings.Contains(Section(p, SecWho), "archivist") {
		t.Errorf("WHO lost the blurb: %q", Section(p, SecWho))
	}
}

// TestReflectIsAboutTheDay is the property the whole stub rests on: the answer
// is a function of the memories, not of the prompt's shape. A resident who
// spent the day at the bakery thinks about the bakery.
func TestReflectIsAboutTheDay(t *testing.T) {
	day := []string{
		"the bakery — bake the morning bread",
		"Osric, at the bakery",
		"the bakery — mind the counter",
		"the garden — sit a while",
	}
	got := Answer(Prompt(reflectReq(day)))
	line, ok := ParseReply(got)
	if !ok {
		t.Fatalf("reply carried no THOUGHT line: %q", got)
	}
	if !strings.Contains(line, "bakery") {
		t.Errorf("a day spent at the bakery reflected as %q", line)
	}
	if !strings.Contains(line, "3 times") {
		t.Errorf("three bakery memories should be counted as three: %q", line)
	}
	// The counter-case matters as much: swap the day and the thought must swap
	// with it, or the rule is decoration.
	other := []string{"the library — read", "the library — file the ledgers", "the square — walk"}
	line2, _ := ParseReply(Answer(Prompt(reflectReq(other))))
	if strings.Contains(line2, "bakery") || !strings.Contains(line2, "library") {
		t.Errorf("a day spent at the library reflected as %q", line2)
	}
}

// TestReflectEmptyDay: handed nothing, a mind says nothing happened rather than
// inventing a day. The failure this guards is the one that makes stub output
// useless — plausible text with no relationship to the input.
func TestReflectEmptyDay(t *testing.T) {
	line, ok := ParseReply(Answer(Prompt(reflectReq(nil))))
	if !ok {
		t.Fatalf("no THOUGHT line for an empty day")
	}
	if !strings.Contains(line, "quiet") {
		t.Errorf("empty day reflected as %q", line)
	}
}

// TestSayAnswersWhatItHeard: the second turn of a conversation has to depend on
// the first, or the residents are not talking to each other.
func TestSayAnswersWhatItHeard(t *testing.T) {
	mems := []string{"the library — file the ledgers"}
	opening := Say("Mira", "Osric", "", mems)
	if !strings.Contains(opening, "Osric") {
		t.Errorf("an opening line should address who it is aimed at: %q", opening)
	}
	if !strings.Contains(opening, "library") {
		t.Errorf("an opening line should carry what is on their mind: %q", opening)
	}
	reply := Say("Osric", "Mira", "there you are, Osric — bakery has been on my mind.", mems)
	if !strings.Contains(reply, "bakery") {
		t.Errorf("a reply should pick up what was said: %q", reply)
	}
	if !strings.Contains(reply, "library") {
		t.Errorf("a reply should still carry its own mind: %q", reply)
	}
	// Heard something with nothing in it but glue, and the reply falls back to
	// its own memory rather than echoing nothing.
	if got := Say("Osric", "Mira", "hm.", mems); !strings.Contains(got, "library") {
		t.Errorf("reply to an empty remark = %q", got)
	}
}

// TestDeterministic is the one that protects the town. salient counts into a
// map, and Go randomizes map iteration per map — so a tie broken by iteration
// order would pass most runs and fail some. Repeat enough to catch it here
// rather than in a trace diff.
func TestDeterministic(t *testing.T) {
	// Deliberately full of ties: four words, one line each.
	day := []string{"the bakery", "the garden", "the library", "the tavern"}
	want := Reflect(day)
	for i := 0; i < 2000; i++ {
		if got := Reflect(day); got != want {
			t.Fatalf("run %d disagreed:\n %q\n %q", i, want, got)
		}
	}
	heard := "bakery garden library tavern square market"
	wantSay := Say("Osric", "Mira", heard, day)
	for i := 0; i < 2000; i++ {
		if got := Say("Osric", "Mira", heard, day); got != wantSay {
			t.Fatalf("Say run %d disagreed:\n %q\n %q", i, wantSay, got)
		}
	}
}

// TestParseReplyRejectsNonAnswers: a model that did not answer is not a
// resident with nothing to say, and the caller must be able to tell.
func TestParseReplyRejectsNonAnswers(t *testing.T) {
	if _, ok := ParseReply("stub:0badc0de"); ok {
		t.Error("a stub hash parsed as a reply")
	}
	if got, ok := ParseReply("SAY:   mind the counter  "); !ok || got != "mind the counter" {
		t.Errorf("ParseReply = %q, %v", got, ok)
	}
}

// TestReflectionsDoNotCompound: residents remember what was said, so a mind's
// own phrasing comes back as input. If the connective words counted, everyone
// would end up reflecting on "still" and "there".
func TestReflectionsDoNotCompound(t *testing.T) {
	said := []string{
		"there you are, Osric — bakery has been on my mind.",
		"there you are, Junia — bakery has been on my mind.",
		"there you are, Pell — garden has been on my mind.",
	}
	line, _ := ParseReply(Answer(Prompt(reflectReq(said))))
	for _, glue := range []string{"there", "been", "mind"} {
		if strings.Contains(line, glue+" ") {
			t.Errorf("reflected on its own phrasing (%q): %q", glue, line)
		}
	}
	if !strings.Contains(line, "bakery") {
		t.Errorf("lost the actual subject: %q", line)
	}
}

// Samples, so a reader can see what the stub actually sounds like without
// running the town. Not an assertion — the tests above do that.
func TestSamples(t *testing.T) {
	day := []string{
		"the bakery — bake the morning bread",
		"Osric, at the bakery",
		"the bakery — mind the counter",
		"the tavern — a drink before bed",
	}
	t.Log("reflect: " + Reflect(day))
	open := Say("Mira", "Osric", "", day)
	t.Log("say:     " + open)
	t.Log("reply:   " + Say("Osric", "Mira", open, []string{"the library — file the ledgers", "the library — read"}))
}

// TestTownVocabularyIsGlue pins the seam between the town and the stop list.
// The town composes memories in fixed phrasings — "came to the X to ...",
// "ran into X at the Y", "X said: ...", and reflections carrying spell()'s
// "once"/"twice"/"N times" — and every word of that scaffolding must lose to
// the actual subject, or a resident's second day is spent reflecting on the
// word "times". The stream below is shaped exactly like a town day, a
// reflection fed back in included, and the assertion is on salient itself:
// what wins must be a subject, never the glue.
func TestTownVocabularyIsGlue(t *testing.T) {
	day := []string{
		"came to the Bakery to knead the morning loaves",
		"ran into Mira at the Bakery",
		"Mira said: still Bakery, for my part.",
		"Mira said: there you are — ledgers has been on my mind.",
		"came to the Tavern to pour for the evening crowd",
		Reflect([]string{ // yesterday's thought, back in the stream
			"came to the Bakery to knead the morning loaves",
			"ran into Mira at the Bakery",
		}),
	}
	glue := map[string]bool{}
	for _, g := range strings.Fields(`said says still part mind mine times
		once twice came into that where went been there have same here`) {
		glue[g] = true
	}
	for _, top := range salient(day, 3) {
		if glue[strings.ToLower(top.word)] {
			t.Fatalf("glue word %q won salience (n=%d) over a town-shaped day", top.word, top.n)
		}
	}
	if r := Reflect(day); !strings.Contains(strings.ToLower(r), "bakery") {
		t.Fatalf("reflection missed the day's subject: %s", r)
	}
	// And one turn of speech over the same stream: the remark must be about
	// something, not about the scaffolding.
	if s := Say("Junia", "Mira", "still Bakery, for my part.", day); !strings.Contains(strings.ToLower(s), "bakery") {
		t.Fatalf("say answered past the subject: %s", s)
	}
}
