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
CREATE TABLE IF NOT EXISTS config (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("sqlite: migrate: %w", err)
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
