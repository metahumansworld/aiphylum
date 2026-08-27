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
	"errors"
	"fmt"
	"sync"

	"github.com/singhtushant3-hub/aiphylum/internal/ledger"
)

var (
	ErrBelowReserve = errors.New("auction: bid is below the reserve floor")
	ErrAboveMax     = errors.New("auction: bid exceeds the posted maximum payout")
	ErrClosed       = errors.New("auction: bidding is closed")
	ErrDuplicate    = errors.New("auction: agent already bid on this bounty")
	ErrNoBids       = errors.New("auction: no bids")
)

// Bid is one agent's sealed asking price for one bounty.
type Bid struct {
	Agent string
	Price ledger.Credits
	seq   int // arrival order; the deterministic tie-break
}

// Auction collects sealed bids for a single bounty.
type Auction struct {
	BountyID  string
	MaxPayout ledger.Credits
	Reserve   ledger.Credits

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

// Award closes bidding and returns the winner: lowest asking price, earliest
// arrival breaking ties. Deterministic, so a replayed episode awards
// identically. The full book is returned alongside — sealed until now, public
// after, which is what makes bid patterns auditable.
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
	book = append(book, a.bids...)
	return best, book, nil
}
