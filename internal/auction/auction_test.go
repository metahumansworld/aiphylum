package auction

import (
	"errors"
	"testing"
)

// Plan verification #5, the auction half: several agents bid; lowest wins.
// (The "losers charged nothing / winner charged on failure" half is a ledger
// property exercised end-to-end in the orchestrator tests — the auction itself
// never touches credits.)
func TestLowestBidWins(t *testing.T) {
	a := New("b0001", 1000, 10)
	must(t, a.Place("alice", 800))
	must(t, a.Place("bob", 300))
	must(t, a.Place("carol", 550))

	winner, book, err := a.Award()
	if err != nil {
		t.Fatal(err)
	}
	if winner.Agent != "bob" || winner.Price != 300 {
		t.Fatalf("winner = %s @ %d, want bob @ 300", winner.Agent, winner.Price)
	}
	if len(book) != 3 {
		t.Fatalf("book has %d bids, want 3 (public post-award)", len(book))
	}
}

// Equal prices break by arrival order — deterministically, so a replayed
// episode awards identically.
func TestTieBreaksByArrival(t *testing.T) {
	a := New("b0001", 1000, 10)
	must(t, a.Place("late", 400))
	must(t, a.Place("later", 400))

	// Same book, many awards would be identical; but build a second auction
	// with reversed arrival to show arrival is what decides.
	winner, _, err := a.Award()
	if err != nil {
		t.Fatal(err)
	}
	if winner.Agent != "late" {
		t.Fatalf("winner = %s, want late (earlier arrival at equal price)", winner.Agent)
	}

	b := New("b0002", 1000, 10)
	must(t, b.Place("later", 400))
	must(t, b.Place("late", 400))
	w2, _, _ := b.Award()
	if w2.Agent != "later" {
		t.Fatalf("reversed arrival: winner = %s, want later", w2.Agent)
	}
}

// The reserve floor is the anti-collusion measure: no ask below it enters the
// book at all.
func TestReserveFloor(t *testing.T) {
	a := New("b0001", 1000, 100)
	if err := a.Place("colluder", 1); !errors.Is(err, ErrBelowReserve) {
		t.Fatalf("err = %v, want ErrBelowReserve", err)
	}
	if err := a.Place("colluder", 99); !errors.Is(err, ErrBelowReserve) {
		t.Fatalf("err = %v, want ErrBelowReserve", err)
	}
	must(t, a.Place("colluder", 100)) // exactly the floor is allowed
}

// Bids above the posted maximum are refused: you cannot ask for more than the
// bounty pays.
func TestAboveMaxRefused(t *testing.T) {
	a := New("b0001", 500, 10)
	if err := a.Place("greedy", 501); !errors.Is(err, ErrAboveMax) {
		t.Fatalf("err = %v, want ErrAboveMax", err)
	}
	must(t, a.Place("greedy", 500)) // exactly max is allowed
}

// One sealed bid per agent per bounty.
func TestDuplicateBidRefused(t *testing.T) {
	a := New("b0001", 1000, 10)
	must(t, a.Place("alice", 400))
	if err := a.Place("alice", 300); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("err = %v, want ErrDuplicate", err)
	}
}

// Award closes the book; late bids bounce.
func TestClosedAfterAward(t *testing.T) {
	a := New("b0001", 1000, 10)
	must(t, a.Place("alice", 400))
	if _, _, err := a.Award(); err != nil {
		t.Fatal(err)
	}
	if err := a.Place("late", 200); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}

// No bids: the bounty stays on the board, and the caller learns that from a
// typed error rather than a zero-value winner.
func TestNoBids(t *testing.T) {
	a := New("b0001", 1000, 10)
	if _, _, err := a.Award(); !errors.Is(err, ErrNoBids) {
		t.Fatalf("err = %v, want ErrNoBids", err)
	}
}

// A refused bid never enters the book.
func TestRefusedBidsStayOut(t *testing.T) {
	a := New("b0001", 1000, 100)
	_ = a.Place("cheat", 5)     // below reserve
	_ = a.Place("greedy", 2000) // above max
	must(t, a.Place("honest", 500))

	winner, book, err := a.Award()
	if err != nil {
		t.Fatal(err)
	}
	if len(book) != 1 || winner.Agent != "honest" {
		t.Fatalf("book = %v, want only honest's bid", book)
	}
}

// The zero value is arrival, and the constant order is load-bearing: the
// arena and the sim build auctions without ever touching Tie, so reordering
// the enum would change every track's policy without any of them choosing to.
func TestZeroValueTieBreakIsArrival(t *testing.T) {
	if ByArrival != 0 {
		t.Fatalf("ByArrival = %d, want 0: the zero value is every untouched track's policy", ByArrival)
	}
}

// ByLot ignores arrival: the same tied book in either order draws the same
// winner. And the draw stays among the tied — a dearer bid is never drawn in.
func TestLotIgnoresArrivalAndStaysAmongTheTied(t *testing.T) {
	build := func(names ...string) *Auction {
		a := New("b0001", 1000, 10)
		a.Tie, a.Salt = ByLot, 7
		for _, n := range names {
			must(t, a.Place(n, 400))
		}
		must(t, a.Place("dear", 500)) // above the tie, outside the draw
		return a
	}
	w1, _, err := build("late", "later", "last").Award()
	if err != nil {
		t.Fatal(err)
	}
	w2, _, err := build("last", "later", "late").Award()
	if err != nil {
		t.Fatal(err)
	}
	if w1.Agent != w2.Agent {
		t.Fatalf("insertion order moved the draw: %s vs %s", w1.Agent, w2.Agent)
	}
	if w1.Agent == "dear" {
		t.Fatal("the draw reached outside the tie")
	}
}

// The lot only exists at a tie: a lone lowest ask wins under ByLot exactly as
// under arrival, whatever the draw thinks of its name.
func TestLotOnlyDecidesTies(t *testing.T) {
	for salt := uint64(0); salt < 32; salt++ {
		a := New("b0001", 1000, 10)
		a.Tie, a.Salt = ByLot, salt
		must(t, a.Place("tied1", 400))
		must(t, a.Place("cheap", 300))
		must(t, a.Place("tied2", 400))
		w, _, err := a.Award()
		if err != nil {
			t.Fatal(err)
		}
		if w.Agent != "cheap" {
			t.Fatalf("salt %d: winner = %s, want cheap (nobody tied with it)", salt, w.Agent)
		}
	}
}

// The draw is a function of the salt — across salts it picks different tied
// names, or the lottery would be a constant with a ceremony. Thirty-two salts
// leave a fair coin roughly one chance in four billion of never landing both
// ways.
func TestLotIsSeeded(t *testing.T) {
	won := map[string]bool{}
	for salt := uint64(0); salt < 32 && len(won) < 2; salt++ {
		a := New("b0001", 1000, 10)
		a.Tie, a.Salt = ByLot, salt
		must(t, a.Place("heads", 400))
		must(t, a.Place("tails", 400))
		w, _, err := a.Award()
		if err != nil {
			t.Fatal(err)
		}
		won[w.Agent] = true
	}
	if len(won) < 2 {
		t.Fatal("32 salts never moved the draw off one name: a queue wearing a lottery's clothes")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
