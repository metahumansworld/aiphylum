package account

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/metahumansworld/aiphylum/internal/ledger"
)

// memMailer keeps the tokens it was asked to send, which is what a test
// needs and what a real mailer must never do.
type memMailer struct {
	mu   sync.Mutex
	sent map[string][]string
}

func (m *memMailer) Send(_ context.Context, to, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sent == nil {
		m.sent = map[string][]string{}
	}
	m.sent[to] = append(m.sent[to], token)
	return nil
}

func (m *memMailer) last(t *testing.T, to string) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	toks := m.sent[to]
	if len(toks) == 0 {
		t.Fatalf("nothing mailed to %s", to)
	}
	return toks[len(toks)-1]
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newStore(t *testing.T) (*Store, *ledger.Ledger, *memMailer, *clock) {
	t.Helper()
	dir := t.TempDir()
	l, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	mail := &memMailer{}
	clk := &clock{t: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)}
	s, err := Open(filepath.Join(dir, "accounts.db"), Config{
		Ledger: l, Grant: 1_000_000, Mailer: mail, Now: clk.now,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Reasons: []string{"arena", "model:anthropic/claude-opus-5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, l, mail, clk
}

func signIn(t *testing.T, s *Store, mail *memMailer, clk *clock, email string) Session {
	t.Helper()
	// Each helper sign-in is a later visit: step past the cooldown rather
	// than switching it off.
	clk.advance(time.Minute)
	ctx := context.Background()
	if err := s.Request(ctx, email); err != nil {
		t.Fatalf("request: %v", err)
	}
	// The mail goes to the address as the store keeps it, not as typed.
	normalised, _ := normalizeEmail(email)
	sess, err := s.Verify(ctx, mail.last(t, normalised))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	return sess
}

func TestFirstSignInIsTheSignUpAndMintsTheGrantOnce(t *testing.T) {
	s, l, mail, clk := newStore(t)
	ctx := context.Background()

	first := signIn(t, s, mail, clk, " Ada@Example.com ")
	if first.User.Email != "ada@example.com" {
		t.Errorf("email was not normalised: %q", first.User.Email)
	}
	if bal, _ := l.Balance(ctx, first.User.Wallet); bal != 1_000_000 {
		t.Errorf("wallet holds %d after sign-up, want the grant", bal)
	}
	acct, _ := l.Get(ctx, first.User.Wallet)
	if acct.Kind != ledger.KindUser || acct.Owner != first.User.ID {
		t.Errorf("wallet = %+v, want a user wallet owned by the user", acct)
	}

	// The same address again is the same user, and no second grant.
	again := signIn(t, s, mail, clk, "ada@example.com")
	if again.User.ID != first.User.ID || again.Token == first.Token {
		t.Errorf("second sign-in: user %s→%s, tokens equal: %v", first.User.ID, again.User.ID, again.Token == first.Token)
	}
	if bal, _ := l.Balance(ctx, first.User.Wallet); bal != 1_000_000 {
		t.Errorf("wallet holds %d after a second sign-in; the grant was minted twice", bal)
	}
	// And the books still balance with the new kind of wallet on them.
	if err := l.Verify(ctx); err != nil {
		t.Errorf("ledger audit: %v", err)
	}
}

func TestALinkIsSingleUseAndExpires(t *testing.T) {
	s, _, mail, clk := newStore(t)
	ctx := context.Background()

	if err := s.Request(ctx, "bo@example.com"); err != nil {
		t.Fatal(err)
	}
	token := mail.last(t, "bo@example.com")
	if _, err := s.Verify(ctx, token); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if _, err := s.Verify(ctx, token); !errors.Is(err, ErrBadToken) {
		t.Errorf("a used link was accepted again: %v", err)
	}

	clk.advance(time.Minute) // past the cooldown, not the link's TTL
	if err := s.Request(ctx, "bo@example.com"); err != nil {
		t.Fatal(err)
	}
	stale := mail.last(t, "bo@example.com")
	clk.advance(16 * time.Minute)
	if _, err := s.Verify(ctx, stale); !errors.Is(err, ErrBadToken) {
		t.Errorf("a sixteen-minute-old link was accepted: %v", err)
	}
	if _, err := s.Verify(ctx, "not-a-token"); !errors.Is(err, ErrBadToken) {
		t.Errorf("a made-up token was accepted: %v", err)
	}
}

func TestSessionsExpireAndSignOut(t *testing.T) {
	s, _, mail, clk := newStore(t)
	ctx := context.Background()
	sess := signIn(t, s, mail, clk, "cy@example.com")

	u, err := s.Authenticate(ctx, sess.Token)
	if err != nil || u.ID != sess.User.ID {
		t.Fatalf("authenticate: %v, %+v", err, u)
	}
	if err := s.SignOut(ctx, sess.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, sess.Token); !errors.Is(err, ErrNoSession) {
		t.Errorf("a signed-out session still works: %v", err)
	}

	sess = signIn(t, s, mail, clk, "cy@example.com")
	clk.advance(31 * 24 * time.Hour)
	if _, err := s.Authenticate(ctx, sess.Token); !errors.Is(err, ErrNoSession) {
		t.Errorf("a month-old session still works: %v", err)
	}
	if _, err := s.Authenticate(ctx, ""); !errors.Is(err, ErrNoSession) {
		t.Errorf("an empty token was a session: %v", err)
	}
}

// A sign-up is two books with no transaction across them. If the process
// dies after the user row exists but the ledger never heard of the wallet —
// the order signUp uses makes this the unlikely side, but a ledger restored
// from an older backup produces the same state — the next sign-in funds it.
func TestAUserWithoutAWalletIsFundedOnNextSignIn(t *testing.T) {
	s, l, mail, clk := newStore(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id, email, wallet, created_at) VALUES ('u_orphan', 'di@example.com', 'usr:orphan', ?)`,
		clk.now().Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Balance(ctx, "usr:orphan"); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("precondition: wallet exists: %v", err)
	}
	sess := signIn(t, s, mail, clk, "di@example.com")
	if sess.User.ID != "u_orphan" {
		t.Errorf("sign-in made a new user %s instead of finding the orphan", sess.User.ID)
	}
	if bal, _ := l.Balance(ctx, "usr:orphan"); bal != 1_000_000 {
		t.Errorf("orphan wallet holds %d, want the grant", bal)
	}
}

func TestBadEmailsAreRefusedBeforeAnyMail(t *testing.T) {
	s, _, mail, _ := newStore(t)
	ctx := context.Background()
	for _, e := range []string{"", "nope", "@x", "x@", "a b@c", "a@b@c"} {
		if err := s.Request(ctx, e); !errors.Is(err, ErrBadEmail) {
			t.Errorf("Request(%q) = %v, want ErrBadEmail", e, err)
		}
	}
	if len(mail.sent) != 0 {
		t.Errorf("mail went out for a refused address: %v", mail.sent)
	}
}

func TestWaitlistTakesOnlyNamedReasonsOnce(t *testing.T) {
	s, _, mail, clk := newStore(t)
	ctx := context.Background()
	u := signIn(t, s, mail, clk, "ed@example.com").User

	for _, r := range []string{"arena", "model:anthropic/claude-opus-5", "arena"} {
		if err := s.Waitlist(ctx, u.ID, r); err != nil {
			t.Errorf("Waitlist(%q): %v", r, err)
		}
	}
	if err := s.Waitlist(ctx, u.ID, "model:something-else"); !errors.Is(err, ErrBadReason) {
		t.Errorf("an unlisted reason was accepted: %v", err)
	}
	got, _ := s.Waiting(ctx, u.ID)
	if len(got) != 2 {
		t.Errorf("waiting = %v, want two distinct reasons", got)
	}
}

func TestRechargeCreditsAPaymentOnceAndUnlocks(t *testing.T) {
	ctx := context.Background()
	s, l, mail, clk := newStore(t)
	u := signIn(t, s, mail, clk, "ada@example.com").User
	if paid, _ := s.Paid(ctx, u.ID); paid {
		t.Fatal("a fresh user has not paid")
	}
	for range 2 { // Stripe announces the same payment twice
		if err := s.Recharge(ctx, u.ID, "cs_1", 5_000_000); err != nil {
			t.Fatal(err)
		}
	}
	if bal, _ := l.Balance(ctx, u.Wallet); bal != 6_000_000 {
		t.Fatalf("balance = %d, want the grant plus one $5 topup", bal)
	}
	if paid, _ := s.Paid(ctx, u.ID); !paid {
		t.Fatal("a topup unlocks")
	}
	// The mint landed but the row did not: the next announcement finishes
	// the job without minting again.
	if _, _, err := l.MintOnce(ctx, u.Wallet, 1_000_000, "topup", "cs_2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Recharge(ctx, u.ID, "cs_2", 1_000_000); err != nil {
		t.Fatal(err)
	}
	if bal, _ := l.Balance(ctx, u.Wallet); bal != 7_000_000 {
		t.Fatalf("balance = %d, want no second mint for cs_2", bal)
	}
	if err := s.Recharge(ctx, "u_nobody", "cs_3", 1); !errors.Is(err, ErrNoSuchUser) {
		t.Fatalf("recharge of a stranger: %v, want ErrNoSuchUser", err)
	}
}

func TestARequestCoolsDownPerAddress(t *testing.T) {
	s, _, _, clk := newStore(t)
	ctx := context.Background()
	if err := s.Request(ctx, "gil@example.com"); err != nil {
		t.Fatal(err)
	}
	var retry *RetryError
	if err := s.Request(ctx, "gil@example.com"); !errors.As(err, &retry) || retry.Seconds() != 60 {
		t.Fatalf("second request = %v, want a 60s RetryError", err)
	}
	if err := s.Request(ctx, "hal@example.com"); err != nil {
		t.Errorf("another address was caught in gil's cooldown: %v", err)
	}
	clk.advance(61 * time.Second)
	if err := s.Request(ctx, "gil@example.com"); err != nil {
		t.Errorf("request after the cooldown: %v", err)
	}
}

func TestRequestAnswers429WithRetryAfterInsideTheCooldown(t *testing.T) {
	s, _, _, _ := newStore(t)
	h := s.Handler()
	post := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/auth/request", strings.NewReader(`{"email":"fi@example.com"}`)))
		return rec
	}
	if rec := post(); rec.Code != http.StatusAccepted {
		t.Fatalf("first request: %d", rec.Code)
	}
	rec := post()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request inside the cooldown: %d", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %q, want 60", got)
	}
	// The page shows only the error field, so the wait must be in the words.
	if !strings.Contains(rec.Body.String(), "60 seconds") {
		t.Errorf("the person is not told how long: %s", rec.Body.String())
	}
}

func TestSignInMailCarriesTheLink(t *testing.T) {
	mail := string(signInMail("soscitea <hi@soscitea.example>", "io@example.com", "https://soscitea.example/", "tok123"))
	for _, want := range []string{
		"To: io@example.com\r\n",
		"Subject: ",
		"\r\n\r\n", // a blank line ends the headers
		"https://soscitea.example/?token=tok123",
	} {
		if !strings.Contains(mail, want) {
			t.Errorf("mail lacks %q:\n%s", want, mail)
		}
	}
}
