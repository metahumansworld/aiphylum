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
	"strings"
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

// LogMailer is the offline mailer: it writes the token to the log, where an
// operator at the terminal can copy it into a verify call. Addr is the
// service's listen address, for the hint.
type LogMailer struct {
	Log  *slog.Logger
	Addr string
}

func (m LogMailer) Send(_ context.Context, to, token string) error {
	m.Log.Info("sign-in link (log mailer: nothing was sent)", "to", to, "token", token,
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

	// Now is the clock, replaceable for tests. Nil means time.Now.
	Now func() time.Time
}

const (
	defaultLinkTTL    = 15 * time.Minute
	defaultSessionTTL = 30 * 24 * time.Hour
)

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
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open accounts: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, cfg: cfg}
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
		);`)
	if err != nil {
		return fmt.Errorf("migrate accounts: %w", err)
	}
	return nil
}

// Request starts a sign-in: a fresh single-use token, good for LinkTTL, is
// mailed to the address. It says the same thing whether or not the address
// has signed in before, so nobody can use it to find out who has.
func (s *Store) Request(ctx context.Context, email string) error {
	email, err := normalizeEmail(email)
	if err != nil {
		return err
	}
	token := randomToken()
	expires := s.cfg.Now().Add(s.cfg.LinkTTL)
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

func (s *Store) wantable(reason string) bool {
	for _, r := range s.cfg.Reasons {
		if r == reason {
			return true
		}
	}
	return false
}

func (s *Store) userByEmail(ctx context.Context, email string) (User, error) {
	var u User
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT id, email, wallet, created_at FROM users WHERE email = ?`, email).
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
