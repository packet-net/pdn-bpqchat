// Package sqlite is the SQLite-backed chat.Store (design.md §7): the durable
// message log (the web-visible history BPQ lacks) and a config KV, under the
// app state dir. It uses the pure-Go modernc.org/sqlite driver so the release
// binary stays a static, CGO-free single file (HANDOVER.md language decision).
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/m0lte/pdn-bpqchat/internal/chat"
	_ "modernc.org/sqlite"
)

// Store implements chat.Store over a SQLite database file.
type Store struct {
	db *sql.DB
}

// Open opens (creating and migrating if needed) the database at path. Pass
// ":memory:" for an ephemeral store. WAL is enabled for concurrent readers
// (the web history reads while the chat core writes).
func Open(path string) (*Store, error) {
	// _pragma busy_timeout avoids spurious "database is locked" under the
	// reader/writer mix; journal_mode=WAL is the standard concurrent choice.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	// One writer at a time is the SQLite model; cap connections so the driver
	// doesn't open parallel writers that serialise on the file lock anyway.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS messages (
    id          TEXT PRIMARY KEY,
    origin_node TEXT NOT NULL,
    from_call   TEXT NOT NULL,
    kind        INTEGER NOT NULL,
    topic       TEXT NOT NULL DEFAULT '',
    to_call     TEXT NOT NULL DEFAULT '',
    ts_unix_ns  INTEGER NOT NULL,
    text        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_topic_ts
    ON messages (kind, topic, ts_unix_ns);
-- DM backfill (S6): a returning viewer reads the persisted threads they sent or
-- received. Two indexes — by recipient and by sender — so PrivateHistory's
-- (to_call = ? OR from_call = ?) probe is cheap on the newest rows from each side.
CREATE INDEX IF NOT EXISTS idx_messages_to_ts
    ON messages (kind, to_call, ts_unix_ns);
CREATE INDEX IF NOT EXISTS idx_messages_from_ts
    ON messages (kind, from_call, ts_unix_ns);
CREATE TABLE IF NOT EXISTS config (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS claims (
    pdn_user   TEXT PRIMARY KEY,
    callsign   TEXT NOT NULL,
    claimed_at INTEGER NOT NULL
);
-- One callsign maps to at most one pdn user: the cross-account-collision guard
-- (a callsign can't be claimed by two web identities). Case-insensitive so
-- "m0lte" and "M0LTE" are the same claim — callsigns are case-folded on write.
CREATE UNIQUE INDEX IF NOT EXISTS idx_claims_callsign
    ON claims (callsign COLLATE NOCASE);`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("sqlite: migrate: %w", err)
	}
	return nil
}

// ErrCallsignClaimed is returned by Claim when the callsign is already mapped to
// a DIFFERENT pdn user (the cross-account collision → HTTP 409). It is a sentinel
// so the web layer can distinguish the 409 case from a genuine store failure.
var ErrCallsignClaimed = errors.New("sqlite: callsign already claimed by another user")

// ClaimedCall returns the callsign a pdn user has claimed, or ("", false) when
// the user has no claim yet. The mapping survives reinstall (it lives in the
// state-dir db, not RAM), so a returning user keeps their identity.
func (s *Store) ClaimedCall(ctx context.Context, pdnUser string) (string, bool, error) {
	var cs string
	err := s.db.QueryRowContext(ctx, `SELECT callsign FROM claims WHERE pdn_user = ?`, pdnUser).Scan(&cs)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("sqlite: claimed call: %w", err)
	}
	return cs, true, nil
}

// Claim maps a pdn user to a callsign (upsert: a user may re-claim / change their
// own callsign). The unique index on callsign enforces that one callsign belongs
// to at most one pdn user; an attempt to claim a callsign already held by ANOTHER
// user returns ErrCallsignClaimed (→ 409). Re-claiming the SAME callsign a user
// already holds is an idempotent no-op success.
func (s *Store) Claim(ctx context.Context, pdnUser, callsign string, claimedAt time.Time) error {
	// Idempotent re-claim of the caller's own current callsign: nothing to do
	// (and we must not trip the collision check against the user's own row).
	if cur, ok, err := s.ClaimedCall(ctx, pdnUser); err != nil {
		return err
	} else if ok && cur == callsign {
		return nil
	}
	// Reject a callsign already held by a DIFFERENT pdn user before we attempt the
	// upsert, so the error is the collision sentinel rather than a raw constraint
	// violation (the unique index is the backstop if a concurrent writer races).
	var holder string
	err := s.db.QueryRowContext(ctx,
		`SELECT pdn_user FROM claims WHERE callsign = ? COLLATE NOCASE`, callsign).Scan(&holder)
	if err == nil && holder != pdnUser {
		return ErrCallsignClaimed
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("sqlite: claim lookup: %w", err)
	}
	const q = `INSERT INTO claims (pdn_user, callsign, claimed_at) VALUES (?, ?, ?)
	           ON CONFLICT(pdn_user) DO UPDATE SET callsign = excluded.callsign, claimed_at = excluded.claimed_at`
	if _, err := s.db.ExecContext(ctx, q, pdnUser, callsign, claimedAt.Unix()); err != nil {
		// The unique-index backstop fires if a concurrent writer claimed the same
		// callsign between our check and this insert — surface it as the 409 too.
		if strings.Contains(err.Error(), "UNIQUE") {
			return ErrCallsignClaimed
		}
		return fmt.Errorf("sqlite: claim: %w", err)
	}
	return nil
}

// SaveMessage appends a message; an already-present id is a no-op (idempotent).
func (s *Store) SaveMessage(ctx context.Context, m chat.Message) error {
	if m.ID == "" {
		return errors.New("sqlite: message has no id")
	}
	const q = `INSERT INTO messages (id, origin_node, from_call, kind, topic, to_call, ts_unix_ns, text)
	           VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	           ON CONFLICT(id) DO NOTHING`
	_, err := s.db.ExecContext(ctx, q,
		m.ID, m.OriginNode, m.FromCall, int(m.Kind), m.Topic, m.ToCall, m.Time.UnixNano(), m.Text)
	if err != nil {
		return fmt.Errorf("sqlite: save message: %w", err)
	}
	return nil
}

// History returns up to limit topic messages at or after since, oldest first.
func (s *Store) History(ctx context.Context, topic string, since time.Time, limit int) ([]chat.Message, error) {
	// We over-fetch the newest `limit` matching rows (DESC + LIMIT), then
	// reverse to oldest-first, so "last N messages" is cheap on the index.
	var sinceNs int64
	if !since.IsZero() {
		sinceNs = since.UnixNano()
	}
	if limit <= 0 {
		limit = 1000
	}
	const q = `SELECT id, origin_node, from_call, kind, topic, to_call, ts_unix_ns, text
	           FROM messages
	           WHERE kind = ? AND topic = ? COLLATE NOCASE AND ts_unix_ns >= ?
	           ORDER BY ts_unix_ns DESC
	           LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, int(chat.KindTopic), topic, sinceNs, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: history: %w", err)
	}
	defer rows.Close()

	var out []chat.Message
	for rows.Next() {
		var (
			m    chat.Message
			kind int
			tsNs int64
		)
		if err := rows.Scan(&m.ID, &m.OriginNode, &m.FromCall, &kind, &m.Topic, &m.ToCall, &tsNs, &m.Text); err != nil {
			return nil, fmt.Errorf("sqlite: scan: %w", err)
		}
		m.Kind = chat.MessageKind(kind)
		m.Time = time.Unix(0, tsNs)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Reverse to oldest-first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// PrivateHistory returns up to limit private messages involving call (sent by or
// addressed to it) at or after since, oldest first — the durable backfill for the
// web DM pane (S6). Like History it over-fetches the newest matching rows then
// reverses to oldest-first; the callsign match is case-insensitive.
func (s *Store) PrivateHistory(ctx context.Context, call string, since time.Time, limit int) ([]chat.Message, error) {
	var sinceNs int64
	if !since.IsZero() {
		sinceNs = since.UnixNano()
	}
	if limit <= 0 {
		limit = 1000
	}
	const q = `SELECT id, origin_node, from_call, kind, topic, to_call, ts_unix_ns, text
	           FROM messages
	           WHERE kind = ? AND ts_unix_ns >= ?
	             AND (to_call = ? COLLATE NOCASE OR from_call = ? COLLATE NOCASE)
	           ORDER BY ts_unix_ns DESC
	           LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, int(chat.KindPrivate), sinceNs, call, call, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: private history: %w", err)
	}
	defer rows.Close()

	var out []chat.Message
	for rows.Next() {
		var (
			m    chat.Message
			kind int
			tsNs int64
		)
		if err := rows.Scan(&m.ID, &m.OriginNode, &m.FromCall, &kind, &m.Topic, &m.ToCall, &tsNs, &m.Text); err != nil {
			return nil, fmt.Errorf("sqlite: scan: %w", err)
		}
		m.Kind = chat.MessageKind(kind)
		m.Time = time.Unix(0, tsNs)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Reverse to oldest-first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// GetConfig reads a config value.
func (s *Store) GetConfig(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM config WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("sqlite: get config: %w", err)
	}
	return v, true, nil
}

// SetConfig writes a config value (upsert).
func (s *Store) SetConfig(ctx context.Context, key, value string) error {
	const q = `INSERT INTO config (key, value) VALUES (?, ?)
	           ON CONFLICT(key) DO UPDATE SET value = excluded.value`
	if _, err := s.db.ExecContext(ctx, q, key, value); err != nil {
		return fmt.Errorf("sqlite: set config: %w", err)
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// compile-time assertion that *Store satisfies chat.Store.
var _ chat.Store = (*Store)(nil)
