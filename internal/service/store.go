package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/metahumansworld/aiphylum/internal/spec"
	_ "modernc.org/sqlite"
)

// Store keeps agents across restarts. The service is the store's only
// writer and reads it once, at boot; between those moments the map in
// memory is the truth and the store is its shadow, written before the map
// on every change so that a crash leaves the store ahead and never behind.
//
// A nil Store keeps agents in memory only, which is what the tests and the
// offline demo want.
type Store interface {
	// Put writes one agent, replacing the record with its ID.
	Put(ctx context.Context, r Record) error
	// Delete removes one agent; a missing ID is not an error.
	Delete(ctx context.Context, id string) error
	// List returns every agent, oldest first.
	List(ctx context.Context) ([]Record, error)
}

// Record is an agent as the store keeps it: the fields that outlive the
// process. Proxy tokens, buckets and conversations are not among them and
// are made afresh at boot.
type Record struct {
	ID      string
	Owner   string
	Wallet  string
	Spec    spec.Agent
	Created time.Time
}

// SQLiteStore is the Store behind the service in production: one file
// beside the ledger's and the accounts', opened the same way. Three files
// for three concerns is plainer than one file two packages own.
type SQLiteStore struct{ db *sql.DB }

// OpenStore opens or creates the agents database at path.
func OpenStore(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open agents: %w", err)
	}
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(context.Background(), `
		CREATE TABLE IF NOT EXISTS agents (
			id         TEXT PRIMARY KEY,
			owner      TEXT NOT NULL,
			wallet     TEXT NOT NULL,
			spec       TEXT NOT NULL,
			created_at INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS agents_owner ON agents (owner, created_at);`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate agents: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) Close() error { return s.db.Close() }

func (s *SQLiteStore) Put(ctx context.Context, r Record) error {
	body, err := json.Marshal(r.Spec)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT OR REPLACE INTO agents (id, owner, wallet, spec, created_at) VALUES (?, ?, ?, ?, ?)`,
		r.ID, r.Owner, r.Wallet, string(body), r.Created.UnixNano())
	return err
}

func (s *SQLiteStore) Delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM agents WHERE id = ?`, id)
	return err
}

// List reads every agent back. A spec is decoded, not validated: the limits
// may have moved since it was written, and a stricter limit must not refuse
// the whole service at boot over one old persona. A record that does not
// decode at all is a corrupt row, and that is an error.
func (s *SQLiteStore) List(ctx context.Context) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, owner, wallet, spec, created_at FROM agents ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		var body string
		var created int64
		if err := rows.Scan(&r.ID, &r.Owner, &r.Wallet, &body, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(body), &r.Spec); err != nil {
			return nil, fmt.Errorf("agent %s: stored spec: %w", r.ID, err)
		}
		r.Created = time.Unix(0, created).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

var _ Store = (*SQLiteStore)(nil)

// errStore wraps a store failure so the HTTP surface can say "the agent was
// not saved" rather than leak a database message.
var errStore = errors.New("service: could not save the agent")
