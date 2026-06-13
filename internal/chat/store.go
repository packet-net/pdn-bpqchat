package chat

import (
	"context"
	"sync"
	"time"
)

// Store is the persistence seam (design.md §7). The pure core owns the live
// in-RAM model (presence is ephemeral by nature — a user who isn't connected
// isn't present, so it is not persisted); the Store persists the durable parts:
// the message log (the web-visible history BPQ lacks) and a small config KV.
// Implementations live outside this package (internal/store/sqlite) so the core
// stays host-free; tests use the in-memory store below.
type Store interface {
	// SaveMessage appends a message to the durable log. Saving an id already
	// present is a no-op (idempotent — the relay may present the same record
	// twice before the seen-set catches it).
	SaveMessage(ctx context.Context, m Message) error

	// History returns up to limit messages in a topic at or after since, oldest
	// first. A zero since means "from the beginning".
	History(ctx context.Context, topic string, since time.Time, limit int) ([]Message, error)

	// GetConfig / SetConfig are a small string KV for app config (e.g. the
	// resolved default topic, peer list state) that must survive a restart.
	GetConfig(ctx context.Context, key string) (string, bool, error)
	SetConfig(ctx context.Context, key, value string) error

	// Close releases the store's resources.
	Close() error
}

// MemStore is an in-memory Store for tests and for a run with no state dir. It
// is safe for concurrent use.
type MemStore struct {
	mu     sync.Mutex
	msgs   []Message
	ids    map[string]struct{}
	config map[string]string
}

// NewMemStore builds an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{ids: map[string]struct{}{}, config: map[string]string{}}
}

func (s *MemStore) SaveMessage(_ context.Context, m Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ids[m.ID]; ok && m.ID != "" {
		return nil
	}
	if m.ID != "" {
		s.ids[m.ID] = struct{}{}
	}
	s.msgs = append(s.msgs, m)
	return nil
}

func (s *MemStore) History(_ context.Context, topic string, since time.Time, limit int) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := normTopicKey(topic)
	var out []Message
	for _, m := range s.msgs {
		if m.Kind != KindTopic || normTopicKey(m.Topic) != key {
			continue
		}
		if !since.IsZero() && m.Time.Before(since) {
			continue
		}
		out = append(out, m)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func (s *MemStore) GetConfig(_ context.Context, key string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.config[key]
	return v, ok, nil
}

func (s *MemStore) SetConfig(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config[key] = value
	return nil
}

func (s *MemStore) Close() error { return nil }
