package ledger

import (
	"context"
	"errors"
	"fmt"
)

// Conservation is the state of the economy in one line: where every credit
// ever minted currently is.
//
// The plan's invariant is minted == wallets + burned + spent. Held is the
// fourth place a credit can legitimately be — reserved for a model call that
// has not answered yet — so the checked identity includes it.
type Conservation struct {
	Minted  Credits // total ever created
	Wallets Credits // sitting in agent and experiment wallets
	Burned  Credits // destroyed, unrecoverable
	Spent   Credits // paid out as real model tokens
	Held    Credits // reserved mid-call
}

// Holds reports whether the invariant is satisfied.
func (c Conservation) Holds() bool {
	return c.Minted == c.Wallets+c.Burned+c.Spent+c.Held
}

func (c Conservation) String() string {
	return fmt.Sprintf("minted=%d wallets=%d burned=%d spent=%d held=%d (drift=%d)",
		c.Minted, c.Wallets, c.Burned, c.Spent, c.Held,
		c.Minted-(c.Wallets+c.Burned+c.Spent+c.Held))
}

// Conservation totals the book by account role.
func (l *Ledger) Conservation(ctx context.Context) (Conservation, error) {
	var c Conservation

	// The mint account's balance is the negative of everything ever created.
	minted, err := l.Balance(ctx, AcctMint)
	if err != nil {
		return c, err
	}
	c.Minted = -minted

	if c.Burned, err = l.Balance(ctx, AcctBurn); err != nil {
		return c, err
	}
	if c.Spent, err = l.Balance(ctx, AcctProvider); err != nil {
		return c, err
	}
	if c.Held, err = l.Balance(ctx, AcctHold); err != nil {
		return c, err
	}

	err = l.db.QueryRowContext(ctx,
		`SELECT coalesce(sum(balance), 0) FROM accounts WHERE kind IN (?, ?)`,
		string(KindAgent), string(KindExperiment)).Scan(&c.Wallets)
	if err != nil {
		return c, fmt.Errorf("sum wallets: %w", err)
	}
	return c, nil
}

// ErrCorrupt reports a book that has lost internal consistency. It is not a
// user-facing condition: if Verify returns this, the economy is not
// trustworthy and the daemon should stop rather than keep trading.
var ErrCorrupt = errors.New("ledger: integrity check failed")

// Verify audits the whole book from the entries up. Balances stored on accounts
// are a cache of the entry log; this recomputes them and confirms the cache,
// the zero-sum rule, and the conservation identity all agree.
//
// It is cheap enough to run after every episode, which is where the plan puts
// it — a continuously checked invariant rather than a test that passed once.
func (l *Ledger) Verify(ctx context.Context) error {
	// 1. Every transaction ever posted nets to zero, so the whole log does too.
	var total Credits
	if err := l.db.QueryRowContext(ctx, `SELECT coalesce(sum(delta), 0) FROM entries`).Scan(&total); err != nil {
		return fmt.Errorf("sum entries: %w", err)
	}
	if total != 0 {
		return fmt.Errorf("%w: entries sum to %d, want 0", ErrCorrupt, total)
	}

	// 2. Each stored balance matches the entries that produced it.
	rows, err := l.db.QueryContext(ctx, `
		SELECT a.id, a.kind, a.balance, coalesce(sum(e.delta), 0)
		FROM accounts a LEFT JOIN entries e ON e.account_id = a.id
		GROUP BY a.id`)
	if err != nil {
		return fmt.Errorf("recompute balances: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id, kind        string
			stored, derived Credits
		)
		if err := rows.Scan(&id, &kind, &stored, &derived); err != nil {
			return fmt.Errorf("scan account: %w", err)
		}
		if stored != derived {
			return fmt.Errorf("%w: %s balance is %d but entries total %d", ErrCorrupt, id, stored, derived)
		}
		// 3. No wallet has been overdrawn behind the checks in post.
		if stored < 0 && !AccountKind(kind).isSystem() {
			return fmt.Errorf("%w: wallet %s is negative (%d)", ErrCorrupt, id, stored)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("recompute balances: %w", err)
	}

	// 4. Reserved credits are accounted for by open holds, one for one.
	var openHolds Credits
	if err := l.db.QueryRowContext(ctx,
		`SELECT coalesce(sum(amount), 0) FROM holds WHERE state = 'open'`).Scan(&openHolds); err != nil {
		return fmt.Errorf("sum open holds: %w", err)
	}
	held, err := l.Balance(ctx, AcctHold)
	if err != nil {
		return err
	}
	if openHolds != held {
		return fmt.Errorf("%w: hold account holds %d but open holds total %d", ErrCorrupt, held, openHolds)
	}

	// 5. The invariant the whole economy is judged by.
	c, err := l.Conservation(ctx)
	if err != nil {
		return err
	}
	if !c.Holds() {
		return fmt.Errorf("%w: conservation broken: %s", ErrCorrupt, c)
	}
	return nil
}
