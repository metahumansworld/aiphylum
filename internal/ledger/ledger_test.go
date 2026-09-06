package ledger

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func newTestLedger(t *testing.T) *Ledger {
	t.Helper()
	// An on-disk book in a temp dir, so the tests exercise the same WAL and
	// pragma settings the daemon runs with.
	l, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func mustWallet(t *testing.T, l *Ledger, id string) {
	t.Helper()
	if err := l.CreateAccount(context.Background(), id, KindAgent, "tester"); err != nil {
		t.Fatalf("create wallet %s: %v", id, err)
	}
}

func balance(t *testing.T, l *Ledger, id string) Credits {
	t.Helper()
	b, err := l.Balance(context.Background(), id)
	if err != nil {
		t.Fatalf("balance %s: %v", id, err)
	}
	return b
}

// verify runs the full audit and fails the test if the book is inconsistent.
// Every test calls it, because a passing behavioural assertion on a corrupt
// book is worth nothing.
func verify(t *testing.T, l *Ledger) {
	t.Helper()
	if err := l.Verify(context.Background()); err != nil {
		t.Fatalf("integrity: %v", err)
	}
}

func TestMintIsTheOnlySourceOfCredits(t *testing.T) {
	ctx := context.Background()
	l := newTestLedger(t)
	mustWallet(t, l, "w1")

	if _, err := l.Mint(ctx, "w1", 1_000_000, "starting grant", "agent:1"); err != nil {
		t.Fatalf("mint: %v", err)
	}

	if got := balance(t, l, "w1"); got != 1_000_000 {
		t.Errorf("wallet balance = %d, want 1000000", got)
	}
	c, err := l.Conservation(ctx)
	if err != nil {
		t.Fatalf("conservation: %v", err)
	}
	if c.Minted != 1_000_000 || c.Wallets != 1_000_000 {
		t.Errorf("conservation = %s, want minted and wallets both 1000000", c)
	}
	verify(t, l)
}

func TestSpendingCannotExceedBalance(t *testing.T) {
	ctx := context.Background()
	l := newTestLedger(t)
	mustWallet(t, l, "w1")
	mustWallet(t, l, "w2")
	if _, err := l.Mint(ctx, "w1", 500, "grant", ""); err != nil {
		t.Fatalf("mint: %v", err)
	}

	_, err := l.Transfer(ctx, "w1", "w2", 501, "overdraw", "")
	if !errors.Is(err, ErrInsufficient) {
		t.Fatalf("transfer of 501 from 500 = %v, want ErrInsufficient", err)
	}

	// The refused transfer must leave both sides exactly as they were.
	if got := balance(t, l, "w1"); got != 500 {
		t.Errorf("source balance = %d, want 500 (refusal must not partially apply)", got)
	}
	if got := balance(t, l, "w2"); got != 0 {
		t.Errorf("destination balance = %d, want 0", got)
	}
	verify(t, l)
}

func TestUnbalancedTransactionIsRejected(t *testing.T) {
	ctx := context.Background()
	l := newTestLedger(t)
	mustWallet(t, l, "w1")

	// Credits appearing from nowhere is the failure the whole design guards
	// against, so it must be impossible even through the generic Post path.
	_, err := l.Post(ctx, "bogus", "credits from nowhere", "", Leg{"w1", 100})
	if !errors.Is(err, ErrUnbalanced) {
		t.Fatalf("one-legged post = %v, want ErrUnbalanced", err)
	}
	if got := balance(t, l, "w1"); got != 0 {
		t.Errorf("wallet balance = %d, want 0", got)
	}
	verify(t, l)
}

func TestHoldReservesCreditsAgainstFurtherSpending(t *testing.T) {
	ctx := context.Background()
	l := newTestLedger(t)
	mustWallet(t, l, "w1")
	mustWallet(t, l, "w2")
	if _, err := l.Mint(ctx, "w1", 1000, "grant", ""); err != nil {
		t.Fatalf("mint: %v", err)
	}

	h, err := l.Hold(ctx, "w1", 800, "call:1")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if got := balance(t, l, "w1"); got != 200 {
		t.Errorf("balance after hold = %d, want 200", got)
	}

	// Reserved credits are not available to spend elsewhere.
	if _, err := l.Transfer(ctx, "w1", "w2", 500, "spend the held credits", ""); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("spending reserved credits = %v, want ErrInsufficient", err)
	}
	verify(t, l)

	// Settling at less than the reservation returns the difference.
	if err := l.Settle(ctx, h.ID, 300); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if got := balance(t, l, "w1"); got != 700 {
		t.Errorf("balance after settling 300 of 800 = %d, want 700", got)
	}

	c, err := l.Conservation(ctx)
	if err != nil {
		t.Fatalf("conservation: %v", err)
	}
	if c.Spent != 300 {
		t.Errorf("spent = %d, want 300", c.Spent)
	}
	if c.Held != 0 {
		t.Errorf("held = %d, want 0 after settlement", c.Held)
	}
	verify(t, l)
}

func TestReleaseReturnsEveryReservedCredit(t *testing.T) {
	ctx := context.Background()
	l := newTestLedger(t)
	mustWallet(t, l, "w1")
	if _, err := l.Mint(ctx, "w1", 1000, "grant", ""); err != nil {
		t.Fatalf("mint: %v", err)
	}

	h, err := l.Hold(ctx, "w1", 400, "call:1")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	// A platform-side fault charges the agent nothing.
	if err := l.Release(ctx, h.ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got := balance(t, l, "w1"); got != 1000 {
		t.Errorf("balance after release = %d, want 1000", got)
	}

	c, _ := l.Conservation(ctx)
	if c.Spent != 0 {
		t.Errorf("spent = %d, want 0 after a released hold", c.Spent)
	}
	verify(t, l)
}

func TestHoldResolvesExactlyOnce(t *testing.T) {
	ctx := context.Background()
	l := newTestLedger(t)
	mustWallet(t, l, "w1")
	if _, err := l.Mint(ctx, "w1", 1000, "grant", ""); err != nil {
		t.Fatalf("mint: %v", err)
	}
	h, err := l.Hold(ctx, "w1", 400, "call:1")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if err := l.Settle(ctx, h.ID, 100); err != nil {
		t.Fatalf("settle: %v", err)
	}

	if err := l.Settle(ctx, h.ID, 100); !errors.Is(err, ErrHoldResolved) {
		t.Errorf("double settle = %v, want ErrHoldResolved", err)
	}
	if err := l.Release(ctx, h.ID); !errors.Is(err, ErrHoldResolved) {
		t.Errorf("release after settle = %v, want ErrHoldResolved", err)
	}
	if got := balance(t, l, "w1"); got != 900 {
		t.Errorf("balance = %d, want 900 (the settlement applied once)", got)
	}
	verify(t, l)
}

func TestSettlementCannotExceedTheReservation(t *testing.T) {
	ctx := context.Background()
	l := newTestLedger(t)
	mustWallet(t, l, "w1")
	if _, err := l.Mint(ctx, "w1", 1000, "grant", ""); err != nil {
		t.Fatalf("mint: %v", err)
	}
	h, err := l.Hold(ctx, "w1", 400, "call:1")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}

	// A provider reporting more usage than was reserved must not be able to
	// push a wallet negative through the settlement path.
	if err := l.Settle(ctx, h.ID, 500); !errors.Is(err, ErrOverSettle) {
		t.Fatalf("over-settle = %v, want ErrOverSettle", err)
	}
	verify(t, l)
}

func TestBankruptAgentCannotBeResurrected(t *testing.T) {
	ctx := context.Background()
	l := newTestLedger(t)
	mustWallet(t, l, "w1")
	mustWallet(t, l, "rich")
	if _, err := l.Mint(ctx, "w1", 100, "grant", ""); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := l.Mint(ctx, "rich", 10_000, "grant", ""); err != nil {
		t.Fatalf("mint: %v", err)
	}

	// Burn the agent down to nothing, then retire it.
	h, err := l.Hold(ctx, "w1", 100, "call:1")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if err := l.Settle(ctx, h.ID, 100); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if got := balance(t, l, "w1"); got != 0 {
		t.Fatalf("balance = %d, want 0", got)
	}
	if err := l.Retire(ctx, "w1"); err != nil {
		t.Fatalf("retire: %v", err)
	}

	// Death is permanent: no top-up, no mint, no second retirement.
	if _, err := l.Transfer(ctx, "rich", "w1", 5000, "revive", ""); !errors.Is(err, ErrClosed) {
		t.Errorf("transfer into retired wallet = %v, want ErrClosed", err)
	}
	if _, err := l.Mint(ctx, "w1", 5000, "revive", ""); !errors.Is(err, ErrClosed) {
		t.Errorf("mint into retired wallet = %v, want ErrClosed", err)
	}
	if _, err := l.Hold(ctx, "w1", 1, "call:2"); !errors.Is(err, ErrClosed) {
		t.Errorf("hold on retired wallet = %v, want ErrClosed", err)
	}
	if got := balance(t, l, "w1"); got != 0 {
		t.Errorf("retired balance = %d, want 0", got)
	}
	verify(t, l)
}

func TestRetireRefusesWhileCreditsAreReserved(t *testing.T) {
	ctx := context.Background()
	l := newTestLedger(t)
	mustWallet(t, l, "w1")
	if _, err := l.Mint(ctx, "w1", 1000, "grant", ""); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := l.Hold(ctx, "w1", 400, "call:1"); err != nil {
		t.Fatalf("hold: %v", err)
	}

	// Retiring now would strand the reserved credits where nothing can release
	// them, and the audit would fail forever after.
	if err := l.Retire(ctx, "w1"); !errors.Is(err, ErrHoldOpen) {
		t.Fatalf("retire with an open hold = %v, want ErrHoldOpen", err)
	}
	verify(t, l)
}

func TestDelegationIsATransferIntoTheSubagentWallet(t *testing.T) {
	ctx := context.Background()
	l := newTestLedger(t)
	mustWallet(t, l, "head")
	mustWallet(t, l, "sub")
	if _, err := l.Mint(ctx, "head", 10_000, "grant", ""); err != nil {
		t.Fatalf("mint: %v", err)
	}

	// The head delegates an allowance; the subagent's ceiling is its balance.
	if _, err := l.Transfer(ctx, "head", "sub", 1000, "delegate", "task:1"); err != nil {
		t.Fatalf("delegate: %v", err)
	}

	// A runaway subagent exhausts its allowance and is refused, and the head's
	// remaining bankroll is untouched.
	h, err := l.Hold(ctx, "sub", 1000, "call:1")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if err := l.Settle(ctx, h.ID, 1000); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if _, err := l.Hold(ctx, "sub", 1, "call:2"); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("subagent past its ceiling = %v, want ErrInsufficient", err)
	}
	if got := balance(t, l, "head"); got != 9000 {
		t.Errorf("head balance = %d, want 9000 (a runaway subagent costs only its allowance)", got)
	}
	verify(t, l)
}

func TestBurnRemovesCreditsPermanently(t *testing.T) {
	ctx := context.Background()
	l := newTestLedger(t)
	mustWallet(t, l, "w1")
	if _, err := l.Mint(ctx, "w1", 1000, "grant", ""); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := l.Burn(ctx, "w1", 250, "design market fee", "sale:1"); err != nil {
		t.Fatalf("burn: %v", err)
	}

	c, err := l.Conservation(ctx)
	if err != nil {
		t.Fatalf("conservation: %v", err)
	}
	if c.Burned != 250 || c.Wallets != 750 {
		t.Errorf("conservation = %s, want burned=250 wallets=750", c)
	}
	// Burned credits stay inside the invariant; they are destroyed, not lost.
	verify(t, l)
}

func TestVerifyDetectsATamperedBalance(t *testing.T) {
	ctx := context.Background()
	l := newTestLedger(t)
	mustWallet(t, l, "w1")
	if _, err := l.Mint(ctx, "w1", 1000, "grant", ""); err != nil {
		t.Fatalf("mint: %v", err)
	}
	verify(t, l)

	// Credits written straight into a balance, bypassing the entry log, are
	// exactly what the audit exists to catch.
	if _, err := l.db.ExecContext(ctx, `UPDATE accounts SET balance = 999999 WHERE id = 'w1'`); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	err := l.Verify(ctx)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("verify on a tampered book = %v, want ErrCorrupt", err)
	}
}

// HasHistory is what a live boot asks before it agrees to open a world, so the
// two answers that matter are "nobody has used this" and "somebody has".
func TestHasHistoryIgnoresTheSystemAccountsAndCountsRetiredOnes(t *testing.T) {
	l := newTestLedger(t)
	ctx := context.Background()

	// A book that has only been migrated holds sys:mint, sys:burn, sys:provider
	// and sys:hold. If those read as history, every first boot is refused.
	used, err := l.HasHistory(ctx)
	if err != nil {
		t.Fatalf("HasHistory on a fresh book: %v", err)
	}
	if used {
		t.Fatal("a freshly opened book reports history; the system accounts are being counted")
	}

	mustWallet(t, l, "alpha")
	if used, err = l.HasHistory(ctx); err != nil || !used {
		t.Fatalf("HasHistory after one wallet = %v, %v; want true", used, err)
	}

	// A retired wallet still occupies its name, and that name is exactly what a
	// second process collides with. A world that ran and then died is used.
	if err := l.Retire(ctx, "alpha"); err != nil {
		t.Fatalf("retire alpha: %v", err)
	}
	if used, err = l.HasHistory(ctx); err != nil || !used {
		t.Fatalf("HasHistory after retiring the only wallet = %v, %v; want true", used, err)
	}
	verify(t, l)
}

func TestMintOnceMintsARefOnceHoweverOftenItIsAnnounced(t *testing.T) {
	ctx := context.Background()
	l := newTestLedger(t)
	mustWallet(t, l, "w1")

	id, minted, err := l.MintOnce(ctx, "w1", 500_000, "topup", "cs_1")
	if err != nil || !minted || id == "" {
		t.Fatalf("first: id=%q minted=%v err=%v", id, minted, err)
	}
	id, minted, err = l.MintOnce(ctx, "w1", 500_000, "topup", "cs_1")
	if err != nil || minted || id != "" {
		t.Fatalf("second: id=%q minted=%v err=%v; want nothing minted", id, minted, err)
	}
	if _, minted, _ = l.MintOnce(ctx, "w1", 500_000, "topup", "cs_2"); !minted {
		t.Fatal("a different ref is a different payment")
	}
	if got := balance(t, l, "w1"); got != 1_000_000 {
		t.Errorf("balance = %d, want 1000000: two payments, one of them announced twice", got)
	}
	if _, _, err := l.MintOnce(ctx, "w1", 1, "topup", ""); err == nil {
		t.Error("an empty ref cannot be keyed on")
	}
	verify(t, l)
}
