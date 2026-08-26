package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// A hold reserves credits before a model call and settles them after.
//
// The proxy cannot know what a call costs until the provider answers, but it
// must refuse the call up front if the wallet cannot cover it. So it reserves
// the worst case, makes the call, and settles the measured amount; the unused
// remainder goes straight back. Reserved credits sit in the hold account, which
// keeps them inside the conservation invariant and out of the wallet's spendable
// balance at the same time.

type HoldState string

const (
	HoldOpen     HoldState = "open"
	HoldSettled  HoldState = "settled"
	HoldReleased HoldState = "released"
)

type Hold struct {
	ID       string
	WalletID string
	Amount   Credits // the reserved worst case, not the eventual cost
	State    HoldState
	Ref      string
}

// Hold reserves amount from a wallet. It fails with ErrInsufficient if the
// wallet cannot cover the reservation, which is the proxy's refusal signal.
func (l *Ledger) Hold(ctx context.Context, walletID string, amount Credits, ref string) (Hold, error) {
	if amount <= 0 {
		return Hold{}, ErrNegativeAmt
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return Hold{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := postTx(ctx, tx, "hold", "reserve for model call", ref,
		Leg{walletID, -amount},
		Leg{AcctHold, amount},
	); err != nil {
		return Hold{}, err
	}

	h := Hold{ID: newID("hold"), WalletID: walletID, Amount: amount, State: HoldOpen, Ref: ref}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO holds (id, wallet_id, amount, state, ref, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		h.ID, h.WalletID, h.Amount, string(h.State), h.Ref, now()); err != nil {
		return Hold{}, fmt.Errorf("insert hold: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Hold{}, fmt.Errorf("commit: %w", err)
	}
	return h, nil
}

// Settle closes a hold at its measured cost: that much reaches the provider
// account, and whatever was over-reserved returns to the wallet.
func (l *Ledger) Settle(ctx context.Context, holdID string, actual Credits) error {
	if actual < 0 {
		return ErrNegativeAmt
	}
	return l.resolve(ctx, holdID, HoldSettled, actual)
}

// Release closes a hold and returns every reserved credit to the wallet. This
// is the platform-fault path: the agent is charged nothing because nothing that
// went wrong was its doing.
func (l *Ledger) Release(ctx context.Context, holdID string) error {
	return l.resolve(ctx, holdID, HoldReleased, 0)
}

func (l *Ledger) resolve(ctx context.Context, holdID string, target HoldState, actual Credits) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	var (
		h     Hold
		state string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT id, wallet_id, amount, state, ref FROM holds WHERE id = ?`, holdID).
		Scan(&h.ID, &h.WalletID, &h.Amount, &state, &h.Ref)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: hold %s", ErrNotFound, holdID)
	}
	if err != nil {
		return fmt.Errorf("read hold %s: %w", holdID, err)
	}
	if HoldState(state) != HoldOpen {
		return fmt.Errorf("%w: hold %s is %s", ErrHoldResolved, holdID, state)
	}
	if actual > h.Amount {
		return fmt.Errorf("%w: hold %s reserved %d, settling %d", ErrOverSettle, holdID, h.Amount, actual)
	}

	// Drain the reservation: the measured cost to the provider, the rest home.
	if _, err := postTx(ctx, tx, string(target), "settle model call", h.Ref,
		Leg{AcctHold, -h.Amount},
		Leg{AcctProvider, actual},
		Leg{h.WalletID, h.Amount - actual},
	); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE holds SET state = ?, resolved_at = ? WHERE id = ?`,
		string(target), now(), holdID); err != nil {
		return fmt.Errorf("resolve hold %s: %w", holdID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// OpenHolds lists the unresolved holds against a wallet. A wallet with open
// holds after an episode has ended means a call was abandoned without being
// settled or released, and the credits are still parked.
func (l *Ledger) OpenHolds(ctx context.Context, walletID string) ([]Hold, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT id, wallet_id, amount, state, ref FROM holds WHERE wallet_id = ? AND state = 'open' ORDER BY created_at`,
		walletID)
	if err != nil {
		return nil, fmt.Errorf("list open holds: %w", err)
	}
	defer rows.Close()

	var out []Hold
	for rows.Next() {
		var (
			h     Hold
			state string
		)
		if err := rows.Scan(&h.ID, &h.WalletID, &h.Amount, &state, &h.Ref); err != nil {
			return nil, fmt.Errorf("scan hold: %w", err)
		}
		h.State = HoldState(state)
		out = append(out, h)
	}
	return out, rows.Err()
}
