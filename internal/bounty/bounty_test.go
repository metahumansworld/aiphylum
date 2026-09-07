package bounty

import (
	"errors"
	"strings"
	"testing"

	"github.com/metahumansworld/soscitea/internal/ledger"
)

// echoGen is a deterministic test generator: the task is to repeat a string
// derived from the seed and tier.
type echoGen struct{}

func (echoGen) Name() string { return "echo" }
func (echoGen) Generate(seed int64, tier int) (Task, error) {
	word := strings.Repeat("x", tier) // tier-length answer, trivially seeded
	return Task{
		Prompt:          "repeat after me",
		Answer:          word,
		ReferenceTokens: int64(10 * (seed%3 + 1)),
	}, nil
}

func newBoard(t *testing.T) *Board {
	t.Helper()
	bd := NewBoard()
	bd.RegisterGenerator(echoGen{})
	return bd
}

func TestPostAndVerify(t *testing.T) {
	bd := newBoard(t)
	b, err := bd.Post("echo", 1, 3, 100000, 60)
	if err != nil {
		t.Fatal(err)
	}
	if b.State != StateOpen {
		t.Fatalf("state = %s, want open", b.State)
	}
	if !b.Verify("xxx") {
		t.Fatal("correct answer rejected")
	}
	if b.Verify("xx") {
		t.Fatal("wrong answer accepted")
	}
	// Normalisation: case and whitespace are not part of the answer.
	if !b.Verify("  XXX  ") {
		t.Fatal("normalized-equal answer rejected")
	}
}

// The answer digest is public before the attempt; the answer is not.
func TestAnswerStaysHidden(t *testing.T) {
	bd := newBoard(t)
	b, _ := bd.Post("echo", 1, 2, 100000, 60)
	if d := b.AnswerDigest(); len(d) != 16 {
		t.Fatalf("digest %q, want 16 hex chars", d)
	}
	// The field is unexported; what we can check is that nothing public equals
	// the answer.
	if b.Prompt == "xx" {
		t.Fatal("prompt leaks the answer")
	}
}

// Payouts rise superlinearly with tier: tier 4 pays more than 4x tier 1 for
// the same reference cost. This schedule is half of the anti-sandbagging
// design; the ladder test proves the effect end to end.
func TestPayoutSuperlinear(t *testing.T) {
	ref := ledger.Credits(1000)
	p1 := PayoutFor(ref, 1)
	p4 := PayoutFor(ref, 4)
	if p4 <= 4*p1 {
		t.Fatalf("tier4 payout %d not superlinear vs tier1 %d (4x = %d)", p4, p1, 4*p1)
	}
	if got, want := p1, ledger.Credits(3000); got != want {
		t.Fatalf("tier1 payout = %d, want %d (3x reference)", got, want)
	}
}

func TestAwardLifecycle(t *testing.T) {
	bd := newBoard(t)
	b, _ := bd.Post("echo", 1, 2, 100000, 60)

	if err := bd.Award(b.ID, "alice", 500); err != nil {
		t.Fatal(err)
	}
	if b.State != StateAwarded || b.Winner != "alice" || b.AskedPrice != 500 {
		t.Fatalf("award not recorded: %+v", b)
	}
	// Double-award refused: exclusivity is the whole point of the auction.
	if err := bd.Award(b.ID, "bob", 400); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("err = %v, want ErrNotOpen", err)
	}

	solved, err := bd.Resolve(b.ID, "xx", true)
	if err != nil || !solved {
		t.Fatalf("resolve: solved=%v err=%v, want solved", solved, err)
	}
	if b.State != StateSolved {
		t.Fatalf("state = %s, want solved", b.State)
	}
}

// A failed attempt returns the bounty to auction with the award cleared and
// the failure counted — the winner ate its costs.
func TestFailureReturnsToAuction(t *testing.T) {
	bd := newBoard(t)
	b, _ := bd.Post("echo", 1, 2, 100000, 60)
	_ = bd.Award(b.ID, "alice", 500)

	solved, err := bd.Resolve(b.ID, "wrong", true)
	if err != nil || solved {
		t.Fatalf("resolve: solved=%v err=%v, want failure", solved, err)
	}
	if b.State != StateOpen || b.Winner != "" || b.AskedPrice != 0 {
		t.Fatalf("bounty not returned cleanly: %+v", b)
	}
	if b.Failures != 1 {
		t.Fatalf("failures = %d, want 1", b.Failures)
	}

	// And it is biddable again.
	if err := bd.Award(b.ID, "bob", 400); err != nil {
		t.Fatal(err)
	}
}

// Plan verification #7, the board half: a platform fault voids the award —
// bounty back on the board, no failure counted. (Contrast Resolve, which
// increments Failures; the two paths are what makes attribution auditable.)
func TestVoidDoesNotCountAsFailure(t *testing.T) {
	bd := newBoard(t)
	b, _ := bd.Post("echo", 1, 2, 100000, 60)
	_ = bd.Award(b.ID, "alice", 500)

	if err := bd.Void(b.ID); err != nil {
		t.Fatal(err)
	}
	if b.State != StateOpen || b.Winner != "" || b.AskedPrice != 0 {
		t.Fatalf("bounty not returned cleanly: %+v", b)
	}
	if b.Failures != 0 {
		t.Fatalf("failures = %d, want 0 — a platform fault is not the agent's failure", b.Failures)
	}
	// Voiding requires an award to void.
	if err := bd.Void(b.ID); !errors.Is(err, ErrNotAwarded) {
		t.Fatalf("err = %v, want ErrNotAwarded", err)
	}
}

// A crash or ceiling hit (attempted=false) is a failure too — no answer is no
// answer.
func TestNoAttemptIsFailure(t *testing.T) {
	bd := newBoard(t)
	b, _ := bd.Post("echo", 1, 2, 100000, 60)
	_ = bd.Award(b.ID, "alice", 500)
	solved, err := bd.Resolve(b.ID, "", false)
	if err != nil || solved {
		t.Fatalf("resolve: solved=%v err=%v, want failure", solved, err)
	}
	if b.State != StateOpen || b.Failures != 1 {
		t.Fatalf("state=%s failures=%d, want open/1", b.State, b.Failures)
	}
}

// Same seed, same tier, same task: the generator contract that makes replay
// and re-run possible.
func TestGenerationIsDeterministic(t *testing.T) {
	bd := newBoard(t)
	b1, _ := bd.Post("echo", 42, 3, 100000, 60)
	b2, _ := bd.Post("echo", 42, 3, 100000, 60)
	if b1.Prompt != b2.Prompt || b1.AnswerDigest() != b2.AnswerDigest() || b1.MaxPayout != b2.MaxPayout {
		t.Fatal("same seed+tier produced different bounties")
	}
	if b1.ID == b2.ID {
		t.Fatal("distinct postings share an ID")
	}
}

func TestOpenListing(t *testing.T) {
	bd := newBoard(t)
	a, _ := bd.Post("echo", 1, 1, 100000, 60)
	b, _ := bd.Post("echo", 2, 2, 100000, 60)
	_ = bd.Award(a.ID, "alice", 100)

	open := bd.Open()
	if len(open) != 1 || open[0].ID != b.ID {
		t.Fatalf("open = %v, want just %s", open, b.ID)
	}
}

func TestUnknownsRefused(t *testing.T) {
	bd := newBoard(t)
	if _, err := bd.Post("nope", 1, 1, 0, 0); !errors.Is(err, ErrNoGenerator) {
		t.Fatalf("err = %v, want ErrNoGenerator", err)
	}
	if err := bd.Award("b9999", "alice", 1); !errors.Is(err, ErrUnknown) {
		t.Fatalf("err = %v, want ErrUnknown", err)
	}
	if _, err := bd.Resolve("b9999", "", false); !errors.Is(err, ErrUnknown) {
		t.Fatalf("err = %v, want ErrUnknown", err)
	}
}
