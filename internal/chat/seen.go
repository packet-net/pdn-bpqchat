package chat

import (
	"sync"
	"time"
)

// DefaultSeenTTL is how long a synthetic message id stays in the seen-set —
// comfortably longer than the longest plausible mesh propagation delay so a
// record circulating a cycle is recognised as a duplicate on its second arrival
// (design.md §5). BPQ's equivalent window is a mere 5 seconds (§4.3).
const DefaultSeenTTL = 10 * time.Minute

// SeenSet is the content-hash de-dup backstop (design.md §5): the safety net
// behind the structural spanning-tree relay. It records synthetic message ids
// (SynthID) with a TTL and reports whether one has been seen before — covering
// ALL record types, not just data, which is the improvement over BPQ's
// id_data-only CheckforDups. Safe for concurrent use.
type SeenSet struct {
	ttl   time.Duration
	clock func() time.Time

	mu      sync.Mutex
	entries map[string]time.Time // id → expiry
}

// NewSeenSet builds a seen-set with the given TTL. A zero ttl uses
// DefaultSeenTTL. clock may be nil (uses time.Now) — tests inject a fake clock.
func NewSeenSet(ttl time.Duration, clock func() time.Time) *SeenSet {
	if ttl <= 0 {
		ttl = DefaultSeenTTL
	}
	if clock == nil {
		clock = time.Now
	}
	return &SeenSet{ttl: ttl, clock: clock, entries: map[string]time.Time{}}
}

// Seen records id and reports whether it was already present (and unexpired).
// The first call for an id returns false and remembers it; a repeat within the
// TTL returns true. Expired entries are pruned opportunistically on each call.
func (s *SeenSet) Seen(id string) bool {
	now := s.clock()
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneLocked(now)

	if exp, ok := s.entries[id]; ok && now.Before(exp) {
		s.entries[id] = now.Add(s.ttl) // refresh: keep a still-circulating id alive
		return true
	}
	s.entries[id] = now.Add(s.ttl)
	return false
}

// Len reports the current number of tracked ids (after a prune) — for tests and
// metrics.
func (s *SeenSet) Len() int {
	now := s.clock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	return len(s.entries)
}

func (s *SeenSet) pruneLocked(now time.Time) {
	for id, exp := range s.entries {
		if !now.Before(exp) {
			delete(s.entries, id)
		}
	}
}
