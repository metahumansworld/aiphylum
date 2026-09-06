// Package account is who is signed in and what they have to spend.
//
// A person signs in by email and nothing else: they ask for a link, the link
// is mailed to them, and presenting its token back is the proof. There is no
// password to lose, and the first successful proof is also the sign-up — it
// mints the user's wallet in the ledger with the grant, once, and every agent
// they build spends from that one wallet. Sessions and links are random
// tokens; only their hashes are stored, so the tables are worthless to anyone
// who reads them without the mail.
package account

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/metahumansworld/aiphylum/internal/ledger"
	_ "modernc.org/sqlite"
)

var (
	ErrBadEmail    = errors.New("that is not an email address")
	ErrBadToken    = errors.New("sign-in link is invalid, used, or expired")
	ErrNoSession   = errors.New("not signed in")
	ErrBadReason   = errors.New("that is not something to wait for")
	ErrNoSuchUser  = errors.New("no such user")
	errUnfunded    = errors.New("wallet is unfunded")
	tokenBytes     = 32
	maxEmailLength = 254
)

// Mailer delivers a sign-in token to an address. The service has one real
// mail concern — this one — and no provider chosen for it yet, so the
// interface is the whole of the commitment: a provider is a Config line, not
// a rewrite. Offline, LogMailer prints the token to the log instead.
type Mailer interface {
	Send(ctx context.Context, to, token string) error
}

// LogMailer is the offline mailer: it writes the token to the log, as the
// link the builder page opens with and as a verify call for the terminal.
// Addr is the service's listen address, for both.
type LogMailer struct {
	Log  *slog.Logger
	Addr string
}

func (m LogMailer) Send(_ context.Context, to, token string) error {
	m.Log.Info("sign-in link (log mailer: nothing was sent)", "to", to,
		"open", "http://"+m.Addr+"/?token="+token,
		"verify", `curl -s `+m.Addr+`/auth/verify -d '{"token":"`+token+`"}'`)
	return nil
}

// Config wires a Store to the ledger that holds its users' money.
type Config struct {
	Ledger *ledger.Ledger
	Grant  ledger.Credits // minted to each new user's wallet at first sign-in
	Mailer Mailer
	Log    *slog.Logger
	// Reasons a signed-in user may join the waitlist for: "arena", and one
	// "model:<id>" per locked model. Anything else is refused.
	Reasons []string

	// LinkTTL bounds how long a mailed link is good for; SessionTTL how long
	// a session lasts from sign-in, with no extension on use. Zero means the
	// defaults: fifteen minutes and thirty days.
	LinkTTL, SessionTTL time.Duration

	// Cooldown is the least time between two links to one address, so the
	// mail cannot be used to flood a person. Zero means a minute.
	Cooldown time.Duration

	// Now is the clock, replaceable for tests. Nil means time.Now.
	Now func() time.Time
}

const (
	defaultLinkTTL    = 15 * time.Minute
	defaultSessionTTL = 30 * 24 * time.Hour
	defaultCooldown   = time.Minute
)

// RetryError is Request refusing to mail the same address twice inside the
// cooldown. It carries how long to wait, for the Retry-After header and for
// the person reading the message.
type RetryError struct{ After time.Duration }

func (e *RetryError) Error() string {
	return fmt.Sprintf("a link was already sent; try again in %d seconds", e.Seconds())
}

// Seconds is After rounded up and never zero, as Retry-After wants it.
func (e *RetryError) Seconds() int { return int(math.Ceil(e.After.Seconds())) }

// User is a signed-in person as the rest of the service sees them.
type User struct {
	ID      string    `json:"id"`
	Email   string    `json:"email"`
	Wallet  string    `json:"wallet"`
	Created time.Time `json:"created"`
}

// Session is the result of a verified link: the token the client presents
// from now on, and when it stops working.
type Session struct {
	Token   string    `json:"session"`
	Expires time.Time `json:"expires"`
	User    User      `json:"user"`
}

// Store keeps users, sessions, links and the waitlist in one SQLite file,
// separate from the ledger: the ledger is money and must stay a closed book;
// this is everything else about a person.
type Store struct {
	db  *sql.DB
	cfg Config

	// asked is when each address last asked for a link. In memory on
	// purpose: a restart forgiving every cooldown is fine, a table for a
	// one-minute fact is not.
	mu    sync.Mutex
	asked map[string]time.Time
}

func Open(path string, cfg Config) (*Store, error) {
	if cfg.Ledger == nil || cfg.Mailer == nil {
		return nil, errors.New("account: Config needs a Ledger and a Mailer")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.LinkTTL == 0 {
		cfg.LinkTTL = defaultLinkTTL
	}
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = defaultSessionTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Cooldown == 0 {
		cfg.Cooldown = defaultCooldown
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open accounts: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, cfg: cfg, asked: map[string]time.Time{}}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS users (
			id         TEXT PRIMARY KEY,
			email      TEXT NOT NULL UNIQUE,
			wallet     TEXT NOT NULL UNIQUE,
			created_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS links (
			hash       TEXT PRIMARY KEY,
			email      TEXT NOT NULL,
			expires_at INTEGER NOT NULL,
			used_at    INTEGER
		);
		CREATE TABLE IF NOT EXISTS sessions (
			hash       TEXT PRIMARY KEY,
			user_id    TEXT NOT NULL REFERENCES users(id),
			expires_at INTEGER NOT NULL,
			revoked_at INTEGER
		);
		CREATE TABLE IF NOT EXISTS waitlist (
			user_id    TEXT NOT NULL REFERENCES users(id),
			reason     TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (user_id, reason)
		);
		CREATE TABLE IF NOT EXISTS topups (
			ref        TEXT PRIMARY KEY,
			user_id    TEXT NOT NULL REFERENCES users(id),
			credits    INTEGER NOT NULL,
			created_at INTEGER NOT NULL
		);`)
	if err != nil {
		return fmt.Errorf("migrate accounts: %w", err)
	}
	return nil
}

// Request starts a sign-in: a fresh single-use token, good for LinkTTL, is
// mailed to the address. It says the same thing whether or not the address
// has signed in before, so nobody can use it to find out who has; a
// RetryError inside the cooldown says only that someone just asked.
func (s *Store) Request(ctx context.Context, email string) error {
	email, err := normalizeEmail(email)
	if err != nil {
		return err
	}
	now := s.cfg.Now()
	// One link per address per cooldown, counted from the attempt: a
	// failing relay is not a licence to hammer it.
	// ponytail: per-address only — an attacker rotating addresses can still
	// flood the relay; a global bucket on /auth/request if that ever bites.
	s.mu.Lock()
	if last, ok := s.asked[email]; ok && now.Sub(last) < s.cfg.Cooldown {
		wait := s.cfg.Cooldown - now.Sub(last)
		s.mu.Unlock()
		return &RetryError{After: wait}
	}
	if len(s.asked) >= 1024 { // sweep, so the map cannot grow without bound
		for a, at := range s.asked {
			if now.Sub(at) >= s.cfg.Cooldown {
				delete(s.asked, a)
			}
		}
	}
	s.asked[email] = now
	s.mu.Unlock()
	token := randomToken()
	expires := now.Add(s.cfg.LinkTTL)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO links (hash, email, expires_at) VALUES (?, ?, ?)`,
		hashToken(token), email, expires.Unix()); err != nil {
		return fmt.Errorf("record link: %w", err)
	}
	return s.cfg.Mailer.Send(ctx, email, token)
}

// Verify presents a mailed token. The link is spent whether or not anything
// after it succeeds; a person whose first sign-in fails asks for another,
// which is cheaper than a link that can be replayed. A first-time address
// becomes a user with a funded wallet, and every verified link opens a new
// session.
func (s *Store) Verify(ctx context.Context, token string) (Session, error) {
	now := s.cfg.Now()
	res, err := s.db.ExecContext(ctx,
		`UPDATE links SET used_at = ? WHERE hash = ? AND used_at IS NULL AND expires_at > ?`,
		now.Unix(), hashToken(token), now.Unix())
	if err != nil {
		return Session{}, fmt.Errorf("spend link: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Session{}, ErrBadToken
	}
	var email string
	if err := s.db.QueryRowContext(ctx, `SELECT email FROM links WHERE hash = ?`, hashToken(token)).Scan(&email); err != nil {
		return Session{}, fmt.Errorf("read link: %w", err)
	}

	u, err := s.userByEmail(ctx, email)
	if errors.Is(err, ErrNoSuchUser) {
		u, err = s.signUp(ctx, email, now)
	}
	if err != nil {
		return Session{}, err
	}
	// A user whose wallet is missing from the ledger — a sign-up that was cut
	// off between its two books — is funded now rather than left with an
	// account that cannot do anything.
	if err := s.ensureFunded(ctx, u); err != nil {
		return Session{}, err
	}

	sess := randomToken()
	expires := now.Add(s.cfg.SessionTTL)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (hash, user_id, expires_at) VALUES (?, ?, ?)`,
		hashToken(sess), u.ID, expires.Unix()); err != nil {
		return Session{}, fmt.Errorf("open session: %w", err)
	}
	return Session{Token: sess, Expires: expires, User: u}, nil
}

// signUp makes a person a user. The wallet is created and funded before the
// user row exists, so a failure between the two leaves a funded wallet that
// nobody was issued — a few cents of grant idle in the ledger — and never a
// user who signed in to find they cannot spend anything.
func (s *Store) signUp(ctx context.Context, email string, now time.Time) (User, error) {
	u := User{ID: "u_" + randomHex(8), Email: email, Created: now}
	u.Wallet = "usr:" + strings.TrimPrefix(u.ID, "u_")
	if err := s.fund(ctx, u); err != nil {
		return User{}, err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id, email, wallet, created_at) VALUES (?, ?, ?, ?)`,
		u.ID, u.Email, u.Wallet, now.Unix()); err != nil {
		return User{}, fmt.Errorf("create user: %w", err)
	}
	s.cfg.Log.Info("new user", "user", u.ID, "wallet", u.Wallet, "grant", s.cfg.Grant)
	return u, nil
}

func (s *Store) fund(ctx context.Context, u User) error {
	if err := s.cfg.Ledger.CreateAccount(ctx, u.Wallet, ledger.KindUser, u.ID); err != nil {
		return fmt.Errorf("create wallet: %w", err)
	}
	if s.cfg.Grant > 0 {
		if _, err := s.cfg.Ledger.Mint(ctx, u.Wallet, s.cfg.Grant, "grant", u.ID); err != nil {
			return fmt.Errorf("mint grant: %w", err)
		}
	}
	return nil
}

func (s *Store) ensureFunded(ctx context.Context, u User) error {
	_, err := s.cfg.Ledger.Get(ctx, u.Wallet)
	if errors.Is(err, ledger.ErrNotFound) {
		s.cfg.Log.Warn("user had no wallet; funding it now", "user", u.ID)
		return s.fund(ctx, u)
	}
	return err
}

// Authenticate turns a session token into its user, or ErrNoSession.
func (s *Store) Authenticate(ctx context.Context, token string) (User, error) {
	if token == "" {
		return User{}, ErrNoSession
	}
	var u User
	var created int64
	err := s.db.QueryRowContext(ctx, `
		SELECT u.id, u.email, u.wallet, u.created_at FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.hash = ? AND s.revoked_at IS NULL AND s.expires_at > ?`,
		hashToken(token), s.cfg.Now().Unix()).Scan(&u.ID, &u.Email, &u.Wallet, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNoSession
	}
	if err != nil {
		return User{}, fmt.Errorf("read session: %w", err)
	}
	u.Created = time.Unix(created, 0).UTC()
	return u, nil
}

// SignOut ends one session. Other sessions of the same user are untouched.
func (s *Store) SignOut(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET revoked_at = ? WHERE hash = ? AND revoked_at IS NULL`,
		s.cfg.Now().Unix(), hashToken(token))
	return err
}

// Waitlist records that a user wants something the launch does not offer
// yet. Joining twice for the same reason is one entry; the reason must be
// one the Config named.
func (s *Store) Waitlist(ctx context.Context, userID, reason string) error {
	if !s.wantable(reason) {
		return fmt.Errorf("%w: %q", ErrBadReason, reason)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO waitlist (user_id, reason, created_at) VALUES (?, ?, ?)`,
		userID, reason, s.cfg.Now().Unix())
	if err != nil {
		return fmt.Errorf("join waitlist: %w", err)
	}
	return nil
}

// Waiting lists what a user has joined the waitlist for.
func (s *Store) Waiting(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT reason FROM waitlist WHERE user_id = ? ORDER BY created_at, reason`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Recharge credits a payment to a user's wallet, once per ref however many
// times it is announced. The ledger is the receipt: the mint is keyed on the
// ref, and the topups row here is written after it, so a crash between the
// two costs the payer nothing — the next announcement finds the mint, skips
// it, and writes the row. The row is what Paid reads.
func (s *Store) Recharge(ctx context.Context, userID, ref string, credits ledger.Credits) error {
	u, err := s.userByID(ctx, userID)
	if err != nil {
		return err
	}
	_, minted, err := s.cfg.Ledger.MintOnce(ctx, u.Wallet, credits, "topup", ref)
	if err != nil {
		return fmt.Errorf("mint topup: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO topups (ref, user_id, credits, created_at) VALUES (?, ?, ?, ?)`,
		ref, u.ID, credits, s.cfg.Now().Unix()); err != nil {
		return fmt.Errorf("record topup: %w", err)
	}
	if minted {
		s.cfg.Log.Info("topup", "user", u.ID, "credits", credits, "ref", ref)
	}
	return nil
}

// Paid reports whether a user has ever recharged. It is the unlock: a person
// who has put money in may name the models the grant does not cover.
func (s *Store) Paid(ctx context.Context, userID string) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM topups WHERE user_id = ?`, userID).Scan(&n); err != nil {
		return false, fmt.Errorf("read topups: %w", err)
	}
	return n > 0, nil
}

func (s *Store) wantable(reason string) bool {
	for _, r := range s.cfg.Reasons {
		if r == reason {
			return true
		}
	}
	return false
}

func (s *Store) userByEmail(ctx context.Context, email string) (User, error) {
	return s.user(ctx, `email`, email)
}

func (s *Store) userByID(ctx context.Context, id string) (User, error) {
	return s.user(ctx, `id`, id)
}

func (s *Store) user(ctx context.Context, col, key string) (User, error) {
	var u User
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT id, email, wallet, created_at FROM users WHERE `+col+` = ?`, key).
		Scan(&u.ID, &u.Email, &u.Wallet, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNoSuchUser
	}
	if err != nil {
		return User{}, fmt.Errorf("read user: %w", err)
	}
	u.Created = time.Unix(created, 0).UTC()
	return u, nil
}

// normalizeEmail lowercases and trims, and checks the shape loosely: one
// "@" with something either side and no whitespace. The mail that follows is
// the real test; a stricter parser here would only refuse real addresses.
func normalizeEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	at := strings.LastIndex(email, "@")
	if len(email) > maxEmailLength || at < 1 || at == len(email)-1 ||
		strings.ContainsAny(email, " \t\r\n") || strings.Count(email, "@") != 1 {
		return "", ErrBadEmail
	}
	return email, nil
}

func randomToken() string { return randomHex(tokenBytes) }

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
