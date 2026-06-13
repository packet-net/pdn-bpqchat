package chat

import (
	"testing"
	"time"
)

func TestSeenSetDeduplicates(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := NewSeenSet(time.Minute, func() time.Time { return now })

	if s.Seen("abc") {
		t.Fatal("first sighting should be false")
	}
	if !s.Seen("abc") {
		t.Fatal("second sighting should be true")
	}
	if s.Seen("def") {
		t.Fatal("a different id should be false")
	}
}

func TestSeenSetExpires(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := NewSeenSet(time.Minute, func() time.Time { return now })

	s.Seen("abc")
	now = now.Add(2 * time.Minute) // past the TTL
	if s.Seen("abc") {
		t.Fatal("expired id should be treated as new")
	}
	if s.Len() != 1 {
		t.Fatalf("Len = %d, want 1 after prune", s.Len())
	}
}

func TestSynthIDStableAndDistinct(t *testing.T) {
	// Same content → same id on any node (the cross-mesh dedup property, §5).
	a := SynthID("M0LTE-4", "G8PZT", KindTopic, "General", "hello")
	b := SynthID("m0lte-4", " g8pzt ", KindTopic, "general", "  hello\r")
	if a != b {
		t.Fatalf("normalised-equal records hashed differently:\n %s\n %s", a, b)
	}
	// Different scope or kind → different id.
	if SynthID("M0LTE-4", "G8PZT", KindTopic, "DX", "hello") == a {
		t.Fatal("different topic must hash differently")
	}
	if SynthID("M0LTE-4", "G8PZT", KindPrivate, "General", "hello") == a {
		t.Fatal("different kind must hash differently")
	}
}

// TestSeenDropsLoopback proves a message a node originates is recognised when it
// loops back via the mesh — the property that replaces a wire message id (§5).
func TestSeenDropsLoopback(t *testing.T) {
	h := testHub(t)
	key := UserKey{Call: "G8PZT", Node: "M0LTE-4"}
	_, _ = h.Join(User{Call: key.Call, Origin: Origin{Node: key.Node, Local: true}})
	m, err := h.Post(nil, key, "round trip")
	if err != nil {
		t.Fatal(err)
	}
	// The same record arriving back from a peer must be recognised as seen.
	if !h.Seen(m.ID) {
		t.Fatal("originated message id not recorded in seen-set; would loop")
	}
}
