package bounty

import (
	"errors"
	"testing"
)

// rubricGen posts open-ended work: a rubric, no answer key.
type rubricGen struct{}

func (rubricGen) Name() string { return "rubric" }
func (rubricGen) Generate(seed int64, tier int) (Task, error) {
	return Task{
		Prompt:          "write the summary",
		Rubric:          "names the component",
		ReferenceTokens: 20,
	}, nil
}

// brokenGen emits whatever it is told to, so the board's refusals can be
// tested at the seam that enforces them.
type brokenGen struct{ task Task }

func (brokenGen) Name() string                        { return "broken" }
func (g brokenGen) Generate(int64, int) (Task, error) { return g.task, nil }

func judgedBoard(t *testing.T) *Board {
	t.Helper()
	bd := NewBoard()
	bd.RegisterGenerator(rubricGen{})
	return bd
}

func TestKeyedRequiresExactlyOneKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		task Task
		want bool
	}{
		{"answer only", Task{Answer: "a"}, true},
		{"rubric only", Task{Rubric: "r"}, true},
		{"both", Task{Answer: "a", Rubric: "r"}, false},
		{"neither", Task{}, false},
	} {
		if got := tc.task.Keyed(); got != tc.want {
			t.Errorf("%s: Keyed() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A task the board cannot decide must never become a bounty: with no key
// nothing can resolve it, and with two the platform holds two disagreeing
// notions of correct.
func TestPostRefusesUndecidableTasks(t *testing.T) {
	for _, tc := range []struct {
		name string
		task Task
	}{
		{"neither key", Task{Prompt: "p", ReferenceTokens: 10}},
		{"both keys", Task{Prompt: "p", Answer: "a", Rubric: "r", ReferenceTokens: 10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bd := NewBoard()
			bd.RegisterGenerator(brokenGen{tc.task})
			if _, err := bd.Post("broken", 1, 1, 0, 60); err == nil {
				t.Fatal("posted a task the board cannot decide")
			}
		})
	}
}

// Verification is the ladder's foundation, so it must never say yes to work
// that has no key — not even to a submission that happens to equal the rubric.
func TestJudgedBountyNeverVerifies(t *testing.T) {
	bd := judgedBoard(t)
	b, err := bd.Post("rubric", 1, 1, 0, 60)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Judged {
		t.Fatal("a bounty posted from a rubric is not marked judged")
	}
	if b.Verify("names the component") {
		t.Error("Verify said yes to a judged bounty")
	}
	if b.Verify("") {
		t.Error("Verify said yes to a judged bounty on an empty submission")
	}
	if b.Rubric() != "names the component" {
		t.Errorf("Rubric() = %q, want the hidden criterion", b.Rubric())
	}
	if b.AnswerDigest() == "" {
		t.Error("a judged bounty published no digest; the rubric must still be committed to")
	}
}

// The two resolution paths refuse each other's bounties. Neither refusal is
// cosmetic: a judged bounty decided by a key comparison would always fail,
// and a keyed bounty decided by a verdict would let a grader award a payout
// the answer key never earned.
func TestResolutionPathsRefuseEachOther(t *testing.T) {
	bd := judgedBoard(t)
	bd.RegisterGenerator(echoGen{})

	judged, err := bd.Post("rubric", 1, 1, 0, 60)
	if err != nil {
		t.Fatal(err)
	}
	keyed, err := bd.Post("echo", 1, 1, 0, 60)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []*Bounty{judged, keyed} {
		if err := bd.Award(b.ID, "agent", 100); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := bd.Resolve(judged.ID, "names the component", true); !errors.Is(err, ErrJudged) {
		t.Errorf("Resolve on a judged bounty: err = %v, want ErrJudged", err)
	}
	if err := bd.ResolveJudged(keyed.ID, true); !errors.Is(err, ErrNotJudged) {
		t.Errorf("ResolveJudged on a keyed bounty: err = %v, want ErrNotJudged", err)
	}
	// Both refusals must be inert: neither bounty moved.
	if judged.State != StateAwarded || keyed.State != StateAwarded {
		t.Errorf("a refused resolution changed state: judged %s, keyed %s", judged.State, keyed.State)
	}
}

func TestResolveJudgedClosesLikeAnyOtherOutcome(t *testing.T) {
	bd := judgedBoard(t)
	b, err := bd.Post("rubric", 1, 1, 0, 60)
	if err != nil {
		t.Fatal(err)
	}
	if err := bd.Award(b.ID, "agent", 100); err != nil {
		t.Fatal(err)
	}
	if err := bd.ResolveJudged(b.ID, false); err != nil {
		t.Fatal(err)
	}
	if b.State != StateOpen || b.Failures != 1 || b.Winner != "" {
		t.Fatalf("a failed verdict left %+v; want open, 1 failure, no winner", b)
	}

	if err := bd.Award(b.ID, "agent2", 90); err != nil {
		t.Fatal(err)
	}
	if err := bd.ResolveJudged(b.ID, true); err != nil {
		t.Fatal(err)
	}
	if b.State != StateSolved {
		t.Fatalf("a passing verdict left %s, want solved", b.State)
	}
	// A closed bounty accepts no second verdict.
	if err := bd.ResolveJudged(b.ID, false); !errors.Is(err, ErrNotAwarded) {
		t.Errorf("re-resolving a solved bounty: err = %v, want ErrNotAwarded", err)
	}
}
