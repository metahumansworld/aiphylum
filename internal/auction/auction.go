// Package auction implements the sealed-bid reverse auction that awards
// bounties.
//
// A bounty posts a maximum payout. Agents privately bid the price they are
// willing to solve it for; the lowest asking price wins exclusive attempt
// rights, and on success is paid exactly what it asked. Overbidding loses the
// award; underbidding wins work that cannot be done profitably. The reserve
// floor is the anti-collusion measure from the plan: no asking price may fall
// below it, which keeps a colluding pair from parking bounties at one credit.
package auction

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"sync"

	"github.com/metahumansworld/soscitea/internal/ledger"
)

var (
	ErrBelowReserve = errors.New("auction: bid is below the reserve floor")
	ErrAboveMax     = errors.New("auction: bid exceeds the posted maximum payout")
	ErrClosed       = errors.New("auction: bidding is closed")
	ErrDuplicate    = errors.New("auction: agent already bid on this bounty")
	ErrNoBids       = errors.New("auction: no bids")
)

// TieBreak is the policy for the moment the price refuses to decide: two
// bids at the same lowest ask. It is a named choice rather than an accident
// of the data structure, because for eight milestones it was the accident —
// arrival order fell out of a slice append, and at the fair arrival order is
// roster order, which nobody bids for and nobody earns.
type TieBreak int

const (
	// ByArrival awards the earliest of the tied bids. The zero value, and
	// the only behaviour any track had before the policy had a name: the
	// arena and the sim construct auctions without touching Tie and must go
	// on awarding exactly as they always have.
	ByArrival TieBreak = iota
	// ByLot awards by seeded draw among the tied: the tied name with the
	// smallest Draw(Salt, name) wins. Deterministic and replayable — the
	// same salt draws the same name — and a function of the salt and the
	// tied names alone, which is the point: a tie is two bids the price
	// could not tell apart, and a queue position is not evidence. Where
	// the salt comes from is the caller's business, and the honest account
	// of that is the caller's to give.
	ByLot
)

// Draw is ByLot's whole mechanism, exported so a trace reader can rerun it:
// FNV-1a over the salt and the agent's name, smallest value wins. No bid,
// balance or history feeds it — a lot that rewarded anything would be a
// ranking with dice.
func Draw(salt uint64, agent string) uint64 {
	h := fnv.New64a()
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], salt)
	h.Write(b[:])
	h.Write([]byte(agent))
	return h.Sum64()
}

// Bid is one agent's sealed asking price for one bounty.
type Bid struct {
	Agent string
	Price ledger.Credits
	seq   int // arrival order; ByArrival's tie-break, and every policy's last resort
}

// Book is the bids so far, in arrival order, without closing the auction —
// what a checkpoint records of a window still open. Sealed bids stay sealed:
// a checkpoint is the world's own file, not a page anyone bidding is shown.
func (a *Auction) Book() []Bid {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Bid(nil), a.bids...)
}

// Load restores a book taken by Book onto a fresh auction. Arrival order is
// the slice order, so each bid's seq is its index — the number Place would
// have given it — and an award on the restored book falls exactly as it
// would have on the original.
func (a *Auction) Load(bids []Bid) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.bids = a.bids[:0]
	for i, b := range bids {
		b.seq = i
		a.bids = append(a.bids, b)
	}
}

// Auction collects sealed bids for a single bounty.
type Auction struct {
	BountyID  string
	MaxPayout ledger.Credits
	Reserve   ledger.Credits

	// Tie is how a tie at the lowest ask is broken; Salt seeds the draw
	// when Tie is ByLot and is ignored otherwise. Both are set between New
	// and the first Place, by the venue running the auction — a tie-break
	// is the auctioneer's rule, not the bidders'.
	Tie  TieBreak
	Salt uint64

	mu     sync.Mutex
	bids   []Bid
	closed bool
}

func New(bountyID string, maxPayout, reserve ledger.Credits) *Auction {
	return &Auction{BountyID: bountyID, MaxPayout: maxPayout, Reserve: reserve}
}

// Place records a sealed bid. Bids are validated on arrival — a refused bid
// never enters the book — and are not visible to anyone until the award.
func (a *Auction) Place(agent string, price ledger.Credits) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return ErrClosed
	}
	if price < a.Reserve {
		return fmt.Errorf("%w: %d < %d", ErrBelowReserve, price, a.Reserve)
	}
	if price > a.MaxPayout {
		return fmt.Errorf("%w: %d > %d", ErrAboveMax, price, a.MaxPayout)
	}
	for _, b := range a.bids {
		if b.Agent == agent {
			return fmt.Errorf("%w: %s", ErrDuplicate, agent)
		}
	}
	a.bids = append(a.bids, Bid{Agent: agent, Price: price, seq: len(a.bids)})
	return nil
}

// Award closes bidding and returns the winner: lowest asking price, with a
// tie at that price broken by the auction's Tie policy — earliest arrival
// unless the venue chose ByLot. Either way deterministic, so a replayed
// episode awards identically. The full book is returned alongside — sealed
// until now, public after, which is what makes bid patterns auditable.
func (a *Auction) Award() (winner Bid, book []Bid, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	if len(a.bids) == 0 {
		return Bid{}, nil, ErrNoBids
	}
	best := a.bids[0]
	for _, b := range a.bids[1:] {
		if b.Price < best.Price || (b.Price == best.Price && b.seq < best.seq) {
			best = b
		}
	}
	if a.Tie == ByLot {
		// best is the earliest bid at the lowest price; re-decide among
		// everyone tied with it by draw. The scan is in arrival order and
		// replaces only on a strictly smaller key, so in the vanishing case
		// of a key collision the earlier arrival stands — the award is
		// defined for every book, with no error path to invent.
		for _, b := range a.bids {
			if b.Price == best.Price && Draw(a.Salt, b.Agent) < Draw(a.Salt, best.Agent) {
				best = b
			}
		}
	}
	book = append(book, a.bids...)
	return best, book, nil
}
