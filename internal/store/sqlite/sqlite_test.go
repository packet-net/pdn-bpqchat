package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/m0lte/pdn-bpqchat/internal/chat"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func msg(id, topic, text string, ts time.Time) chat.Message {
	return chat.Message{
		ID: id, OriginNode: "M0LTE-4", FromCall: "G8PZT",
		Kind: chat.KindTopic, Topic: topic, Time: ts, Text: text,
	}
}

func TestSaveAndHistoryOrder(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0)

	// Insert out of order; History must return oldest-first.
	if err := s.SaveMessage(ctx, msg("c", "General", "third", base.Add(2*time.Minute))); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveMessage(ctx, msg("a", "General", "first", base)); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveMessage(ctx, msg("b", "General", "second", base.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	// A message in another topic must not leak in.
	if err := s.SaveMessage(ctx, msg("x", "DX", "elsewhere", base)); err != nil {
		t.Fatal(err)
	}

	got, err := s.History(ctx, "General", time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"first", "second", "third"}
	if len(got) != len(want) {
		t.Fatalf("history len = %d, want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Text != want[i] {
			t.Fatalf("history[%d] = %q, want %q", i, got[i].Text, want[i])
		}
	}
}

func TestHistoryTopicCaseInsensitiveAndSince(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0)
	_ = s.SaveMessage(ctx, msg("a", "General", "old", base))
	_ = s.SaveMessage(ctx, msg("b", "General", "new", base.Add(time.Hour)))

	// Case-insensitive topic match.
	got, err := s.History(ctx, "general", time.Time{}, 0)
	if err != nil || len(got) != 2 {
		t.Fatalf("case-insensitive history: %d msgs, err %v", len(got), err)
	}
	// since filter keeps only the newer one.
	got, err = s.History(ctx, "General", base.Add(time.Minute), 0)
	if err != nil || len(got) != 1 || got[0].Text != "new" {
		t.Fatalf("since-filtered history = %+v err %v", got, err)
	}
}

func TestSaveMessageIdempotent(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	m := msg("dup", "General", "once", time.Unix(1_700_000_000, 0))
	if err := s.SaveMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	// Re-saving the same id (even with different text) is a silent no-op.
	m.Text = "twice"
	if err := s.SaveMessage(ctx, m); err != nil {
		t.Fatalf("re-save should be a no-op, got %v", err)
	}
	got, _ := s.History(ctx, "General", time.Time{}, 0)
	if len(got) != 1 || got[0].Text != "once" {
		t.Fatalf("idempotent save broke: %+v", got)
	}
}

func TestHistoryLimitReturnsNewest(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0)
	for i := 0; i < 5; i++ {
		id := string(rune('a' + i))
		_ = s.SaveMessage(ctx, msg(id, "General", id, base.Add(time.Duration(i)*time.Minute)))
	}
	got, err := s.History(ctx, "General", time.Time{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	// The newest 2, still oldest-first.
	if len(got) != 2 || got[0].Text != "d" || got[1].Text != "e" {
		t.Fatalf("limited history = %+v, want [d e]", got)
	}
}

func TestConfigKV(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if _, ok, _ := s.GetConfig(ctx, "default_topic"); ok {
		t.Fatal("unset key should report not-found")
	}
	if err := s.SetConfig(ctx, "default_topic", "General"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetConfig(ctx, "default_topic", "DX"); err != nil { // upsert
		t.Fatal(err)
	}
	v, ok, err := s.GetConfig(ctx, "default_topic")
	if err != nil || !ok || v != "DX" {
		t.Fatalf("config get = %q ok=%v err=%v, want DX", v, ok, err)
	}
}

// TestClaimRoundTrip: a claim is recorded, read back, and survives a re-open of
// the same db file (the durability the design promises across a reinstall).
func TestClaimRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claims.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	if err := s.Claim(ctx, "alice@pdn", "M0ABC", now); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if cs, ok, err := s.ClaimedCall(ctx, "alice@pdn"); err != nil || !ok || cs != "M0ABC" {
		t.Fatalf("claimed call = %q ok=%v err=%v, want M0ABC", cs, ok, err)
	}
	if _, ok, _ := s.ClaimedCall(ctx, "nobody@pdn"); ok {
		t.Fatal("unclaimed user should report not-found")
	}
	_ = s.Close()

	// Re-open the SAME file: the claim must still be there.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if cs, ok, _ := s2.ClaimedCall(ctx, "alice@pdn"); !ok || cs != "M0ABC" {
		t.Fatalf("claim did not survive re-open: %q ok=%v", cs, ok)
	}
}

// TestClaimCollision: a callsign held by one user cannot be claimed by another
// (ErrCallsignClaimed), case-insensitively; the original owner is unaffected, and
// a user re-claiming their own callsign or changing it is fine.
func TestClaimCollision(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	if err := s.Claim(ctx, "alice@pdn", "M0ABC", now); err != nil {
		t.Fatal(err)
	}
	// Another user, same callsign → collision.
	if err := s.Claim(ctx, "bob@pdn", "M0ABC", now); !errors.Is(err, ErrCallsignClaimed) {
		t.Fatalf("cross-account claim err = %v, want ErrCallsignClaimed", err)
	}
	// Case-folded spelling collides too.
	if err := s.Claim(ctx, "bob@pdn", "m0abc", now); !errors.Is(err, ErrCallsignClaimed) {
		t.Fatalf("case-folded cross-account claim err = %v, want ErrCallsignClaimed", err)
	}
	// Bob recorded nothing; Alice still owns it.
	if _, ok, _ := s.ClaimedCall(ctx, "bob@pdn"); ok {
		t.Fatal("a collided claim must not be recorded")
	}
	if cs, _, _ := s.ClaimedCall(ctx, "alice@pdn"); cs != "M0ABC" {
		t.Fatalf("owner lost callsign: %q", cs)
	}
	// Alice re-claims her own callsign: idempotent success.
	if err := s.Claim(ctx, "alice@pdn", "M0ABC", now); err != nil {
		t.Fatalf("self re-claim err = %v, want nil", err)
	}
	// Alice changes to a free callsign; the old one frees up for Bob.
	if err := s.Claim(ctx, "alice@pdn", "M0DEF", now); err != nil {
		t.Fatalf("change own callsign err = %v", err)
	}
	if err := s.Claim(ctx, "bob@pdn", "M0ABC", now); err != nil {
		t.Fatalf("re-claim of released callsign err = %v, want nil", err)
	}
}
