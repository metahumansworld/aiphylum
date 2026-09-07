// Package ledger is the double-entry book for soscitea credits.
//
// Every credit that exists was minted into a wallet, and every movement since
// is a balanced transaction: the legs of a transaction sum to zero, always.
// That single rule is what makes the conservation invariant checkable rather
// than merely intended — see Conservation.
//
// A credit is one micro-USD. Prices are quoted in nano-USD per token (see
// package pricing) and rounded up to whole credits once per model call, so a
// wallet balance is always an exact integer count of real money spent.
package ledger

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Credits is a quantity of soscitea currency. One credit is one micro-USD.
type Credits int64

// USD returns the credit amount as dollars, for display only.
func (c Credits) USD() float64 { return float64(c) / 1e6 }

func (c Credits) String() string { return fmt.Sprintf("%d cr ($%.4f)", int64(c), c.USD()) }

// AccountKind distinguishes wallets, which may never go negative, from the
// system accounts that are the source and sink of every credit.
type AccountKind string

const (
	KindMint       AccountKind = "system_mint"     // source of all credits; balance is <= 0
	KindBurn       AccountKind = "system_burn"     // credits destroyed, never to return
	KindProvider   AccountKind = "system_provider" // credits spent on real model tokens
	KindHold       AccountKind = "system_hold"     // reserved mid-call, not yet settled
	KindAgent      AccountKind = "agent_wallet"    // a ranked agent's bankroll; bankruptcy is terminal
	KindExperiment AccountKind = "experiment_wallet"
	KindUser       AccountKind = "user_wallet" // a signed-in person's grant; every agent they build spends from it
)

// isSystem reports whether an account may carry a negative balance.
func (k AccountKind) isSystem() bool {
	switch k {
	case KindMint, KindBurn, KindProvider, KindHold:
		return true
	}
	return false
}

// System account identifiers. These exist from the moment the book is opened.
const (
	AcctMint     = "sys:mint"
	AcctBurn     = "sys:burn"
	AcctProvider = "sys:provider"
	AcctHold     = "sys:hold"
)

var (
	ErrNotFound      = errors.New("ledger: account not found")
	ErrInsufficient  = errors.New("ledger: insufficient funds")
	ErrClosed        = errors.New("ledger: account is closed")
	ErrUnbalanced    = errors.New("ledger: transaction legs do not sum to zero")
	ErrHoldResolved  = errors.New("ledger: hold is already resolved")
	ErrHoldOpen      = errors.New("ledger: account has open holds")
	ErrOverSettle    = errors.New("ledger: settlement exceeds held amount")
	ErrNegativeAmt   = errors.New("ledger: amount must be positive")
	ErrAccountExists = errors.New("ledger: account already exists")
)

// Ledger is a credit book backed by SQLite.
//
// Writes are serialised by a mutex rather than left to SQLite's busy handling:
// the daemon is a single process, contention is low, and a plain lock makes
// "insufficient funds" a decision taken under the same lock that applies the
// debit — there is no window in which two calls both see a sufficient balance.
type Ledger struct {
	db *sql.DB
	mu sync.Mutex
}

// Open opens (creating if needed) the credit book at path. Pass ":memory:" for
// an ephemeral book.
func Open(path string) (*Ledger, error) {
	dsn := path
	if path == ":memory:" {
		// A shared cache keeps every connection in the pool looking at the same
		// in-memory database; without it each connection gets its own empty one.
		dsn = "file::memory:?cache=shared"
	} else {
		dsn = "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open ledger: %w", err)
	}
	// One connection keeps the in-memory case coherent and costs nothing in the
	// on-disk case, where writes are serialised by l.mu regardless.
	db.SetMaxOpenConns(1)

	l := &Ledger{db: db}
	if err := l.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return l, nil
}

func (l *Ledger) Close() error { return l.db.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS accounts (
	id         TEXT PRIMARY KEY,
	kind       TEXT    NOT NULL,
	owner      TEXT    NOT NULL DEFAULT '',
	balance    INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	closed_at  INTEGER
);

CREATE TABLE IF NOT EXISTS txns (
	id         TEXT PRIMARY KEY,
	kind       TEXT    NOT NULL,
	memo       TEXT    NOT NULL DEFAULT '',
	ref        TEXT    NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS entries (
	seq        INTEGER PRIMARY KEY AUTOINCREMENT,
	txn_id     TEXT    NOT NULL REFERENCES txns(id),
	account_id TEXT    NOT NULL REFERENCES accounts(id),
	delta      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS entries_by_account ON entries(account_id);
CREATE INDEX IF NOT EXISTS entries_by_txn     ON entries(txn_id);

CREATE TABLE IF NOT EXISTS holds (
	id          TEXT PRIMARY KEY,
	wallet_id   TEXT    NOT NULL REFERENCES accounts(id),
	amount      INTEGER NOT NULL,
	state       TEXT    NOT NULL,
	ref         TEXT    NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	resolved_at INTEGER
);
CREATE INDEX IF NOT EXISTS holds_by_wallet ON holds(wallet_id);
`

func (l *Ledger) migrate(ctx context.Context) error {
	if _, err := l.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	for _, sys := range []struct {
		id   string
		kind AccountKind
	}{
		{AcctMint, KindMint},
		{AcctBurn, KindBurn},
		{AcctProvider, KindProvider},
		{AcctHold, KindHold},
	} {
		_, err := l.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO accounts (id, kind, created_at) VALUES (?, ?, ?)`,
			sys.id, string(sys.kind), now())
		if err != nil {
			return fmt.Errorf("migrate system account %s: %w", sys.id, err)
		}
	}
	return nil
}

func now() int64 { return time.Now().UnixMicro() }

func newID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("ledger: crypto/rand failed: " + err.Error())
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

// Account is a stored account with its current balance.
type Account struct {
	ID      string
	Kind    AccountKind
	Owner   string
	Balance Credits
	Closed  bool
}

// CreateAccount opens a new account. Wallet IDs are chosen by the caller so
// that they can be derived from an agent or episode identifier.
func (l *Ledger) CreateAccount(ctx context.Context, id string, kind AccountKind, owner string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	_, err := l.db.ExecContext(ctx,
		`INSERT INTO accounts (id, kind, owner, created_at) VALUES (?, ?, ?, ?)`,
		id, string(kind), owner, now())
	if err != nil {
		// A duplicate primary key is the only expected failure here, and callers
		// want to distinguish it from a real database fault.
		var exists int
		if l.db.QueryRowContext(ctx, `SELECT count(*) FROM accounts WHERE id = ?`, id).Scan(&exists) == nil && exists > 0 {
			return fmt.Errorf("%w: %s", ErrAccountExists, id)
		}
		return fmt.Errorf("create account %s: %w", id, err)
	}
	return nil
}

// Get returns an account by ID.
func (l *Ledger) Get(ctx context.Context, id string) (Account, error) {
	var (
		a      Account
		kind   string
		closed sql.NullInt64
	)
	err := l.db.QueryRowContext(ctx,
		`SELECT id, kind, owner, balance, closed_at FROM accounts WHERE id = ?`, id).
		Scan(&a.ID, &kind, &a.Owner, &a.Balance, &closed)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Account{}, fmt.Errorf("get account %s: %w", id, err)
	}
	a.Kind = AccountKind(kind)
	a.Closed = closed.Valid
	return a, nil
}

// Balance returns the current balance of an account.
func (l *Ledger) Balance(ctx context.Context, id string) (Credits, error) {
	a, err := l.Get(ctx, id)
	return a.Balance, err
}

// HasHistory reports whether anything has ever used this book: true once any
// account exists beyond the system accounts migrate seeds, false on a book that
// has only just been opened.
//
// Closed accounts count, and that is the point rather than an oversight. A
// retired attempt wallet is still a row, and it is precisely the row a second
// process collides with when it reaches for a name the first one already spent.
// A world that ran and then went bankrupt is still a used world.
//
// The exemption is by prefix rather than by naming the four constants, so a
// system account added later is exempt automatically. The failure direction
// matters here: an unrecognised sys: account would otherwise read as history
// and refuse every boot, including the first.
func (l *Ledger) HasHistory(ctx context.Context) (bool, error) {
	var n int
	err := l.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM accounts WHERE id NOT LIKE 'sys:%'`).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check ledger history: %w", err)
	}
	return n > 0, nil
}

// Leg is one side of a transaction: a signed movement against one account.
type Leg struct {
	Account string
	Delta   Credits
}

// Post applies a balanced set of legs atomically, or applies none of them.
//
// This is the only path by which any balance in the book changes; Mint, Burn,
// Transfer and the hold operations are all thin wrappers over it, so the
// zero-sum rule has exactly one enforcement point.
func (l *Ledger) Post(ctx context.Context, kind, memo, ref string, legs ...Leg) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.post(ctx, kind, memo, ref, legs...)
}

// post assumes l.mu is held.
func (l *Ledger) post(ctx context.Context, kind, memo, ref string, legs ...Leg) (string, error) {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	txnID, err := postTx(ctx, tx, kind, memo, ref, legs...)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return txnID, nil
}

// postTx applies the legs inside an existing transaction, so that a caller with
// more bookkeeping to do — recording a hold, say — can make the whole thing
// atomic rather than leaving a window where credits have moved but the record
// explaining why has not been written.
func postTx(ctx context.Context, tx *sql.Tx, kind, memo, ref string, legs ...Leg) (string, error) {
	if len(legs) == 0 {
		return "", ErrUnbalanced
	}
	var sum Credits
	for _, leg := range legs {
		sum += leg.Delta
	}
	if sum != 0 {
		return "", fmt.Errorf("%w: legs sum to %d", ErrUnbalanced, sum)
	}

	txnID := newID("txn")
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO txns (id, kind, memo, ref, created_at) VALUES (?, ?, ?, ?, ?)`,
		txnID, kind, memo, ref, now()); err != nil {
		return "", fmt.Errorf("insert txn: %w", err)
	}

	for _, leg := range legs {
		if leg.Delta == 0 {
			continue // a zero movement is not a fact worth recording
		}
		var (
			balance Credits
			kindStr string
			closed  sql.NullInt64
		)
		err := tx.QueryRowContext(ctx,
			`SELECT balance, kind, closed_at FROM accounts WHERE id = ?`, leg.Account).
			Scan(&balance, &kindStr, &closed)
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: %s", ErrNotFound, leg.Account)
		}
		if err != nil {
			return "", fmt.Errorf("read account %s: %w", leg.Account, err)
		}
		if closed.Valid {
			return "", fmt.Errorf("%w: %s", ErrClosed, leg.Account)
		}

		updated := balance + leg.Delta
		if updated < 0 && !AccountKind(kindStr).isSystem() {
			return "", fmt.Errorf("%w: %s has %d, needs %d", ErrInsufficient, leg.Account, balance, -leg.Delta)
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE accounts SET balance = ? WHERE id = ?`, updated, leg.Account); err != nil {
			return "", fmt.Errorf("update balance %s: %w", leg.Account, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO entries (txn_id, account_id, delta) VALUES (?, ?, ?)`,
			txnID, leg.Account, leg.Delta); err != nil {
			return "", fmt.Errorf("insert entry %s: %w", leg.Account, err)
		}
	}
	return txnID, nil
}

// Mint creates credits and places them in an account. This is the faucet: it
// is the only operation that increases the total credits in circulation.
func (l *Ledger) Mint(ctx context.Context, to string, amount Credits, memo, ref string) (string, error) {
	if amount <= 0 {
		return "", ErrNegativeAmt
	}
	return l.Post(ctx, "mint", memo, ref,
		Leg{AcctMint, -amount},
		Leg{to, amount},
	)
}

// MintOnce is Mint keyed by its ref: a second call with the same ref mints
// nothing and reports it. It is for money that arrives by a channel that
// retries — a payment webhook — where the ref is the payment, and the book
// must show it once however many times it is announced. The check and the
// mint share one transaction under the ledger's lock, so two announcements
// at once cannot both pass the check.
func (l *Ledger) MintOnce(ctx context.Context, to string, amount Credits, memo, ref string) (id string, minted bool, err error) {
	if amount <= 0 {
		return "", false, ErrNegativeAmt
	}
	if ref == "" {
		return "", false, errors.New("mint once: a ref is required")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM txns WHERE kind = 'mint' AND ref = ?`, ref).Scan(&n); err != nil {
		return "", false, fmt.Errorf("check ref: %w", err)
	}
	if n > 0 {
		return "", false, nil
	}
	id, err = postTx(ctx, tx, "mint", memo, ref, Leg{AcctMint, -amount}, Leg{to, amount})
	if err != nil {
		return "", false, err
	}
	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("commit: %w", err)
	}
	return id, true, nil
}

// Burn destroys credits. This is the sink that stands against continuous
// minting; a burned credit can never re-enter circulation.
func (l *Ledger) Burn(ctx context.Context, from string, amount Credits, memo, ref string) (string, error) {
	if amount <= 0 {
		return "", ErrNegativeAmt
	}
	return l.Post(ctx, "burn", memo, ref,
		Leg{from, -amount},
		Leg{AcctBurn, amount},
	)
}

// Transfer moves credits between accounts. A delegation to a subagent is a
// transfer into that subagent's own wallet, which is what makes its spending
// ceiling nothing more than its balance.
func (l *Ledger) Transfer(ctx context.Context, from, to string, amount Credits, memo, ref string) (string, error) {
	if amount <= 0 {
		return "", ErrNegativeAmt
	}
	return l.Post(ctx, "transfer", memo, ref,
		Leg{from, -amount},
		Leg{to, amount},
	)
}

// Retire closes an account permanently. A retired account can neither send nor
// receive, which is how a bankrupt agent stays dead.
func (l *Ledger) Retire(ctx context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Retiring an account with credits still reserved would strand them in the
	// hold account, where nothing could ever release them.
	var open int
	if err := l.db.QueryRowContext(ctx,
		`SELECT count(*) FROM holds WHERE wallet_id = ? AND state = 'open'`, id).Scan(&open); err != nil {
		return fmt.Errorf("check holds for %s: %w", id, err)
	}
	if open > 0 {
		return fmt.Errorf("%w: %s has %d open hold(s)", ErrHoldOpen, id, open)
	}

	res, err := l.db.ExecContext(ctx,
		`UPDATE accounts SET closed_at = ? WHERE id = ? AND closed_at IS NULL`, now(), id)
	if err != nil {
		return fmt.Errorf("close account %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Either it does not exist or it was already closed; both are terminal
		// states from the caller's point of view, so report which.
		var exists int
		if err := l.db.QueryRowContext(ctx, `SELECT count(*) FROM accounts WHERE id = ?`, id).Scan(&exists); err != nil {
			return fmt.Errorf("close account %s: %w", id, err)
		}
		if exists == 0 {
			return fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return fmt.Errorf("%w: %s", ErrClosed, id)
	}
	return nil
}
