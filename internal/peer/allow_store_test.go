package peer

import (
	"context"
	"reflect"
	"testing"

	"github.com/m0lte/pdn-bpqchat/internal/chat"
)

// TestLoadAllowListEnvSeedAndPersist: on a FIRST run (no persisted set) the env
// seed populates the editable list AND is persisted to the config table, so the
// next start (and the S5 editor) reads it back from the store.
func TestLoadAllowListEnvSeedAndPersist(t *testing.T) {
	ctx := context.Background()
	store := chat.NewMemStore()

	a, err := LoadAllowList(ctx, store, []string{"GB7SEED-1", " gb7seed-2 "})
	if err != nil {
		t.Fatalf("LoadAllowList: %v", err)
	}
	if !a.Allowed("GB7SEED-1") || !a.Allowed("GB7SEED-2") {
		t.Fatal("env seed not honoured by the loaded allow-list")
	}
	// The seed must have been written through to the config table.
	raw, ok, err := store.GetConfig(ctx, ConfigKVAllowKey)
	if err != nil || !ok {
		t.Fatalf("seed was not persisted (ok=%v err=%v)", ok, err)
	}
	if want := "GB7SEED-1\nGB7SEED-2"; raw != want {
		t.Fatalf("persisted value = %q, want %q", raw, want)
	}
}

// TestLoadAllowListPersistedWinsOverEnv: once a set is persisted, it is
// authoritative — a different env seed on a later start does NOT override it (so a
// restart never reverts an operator's live web edits).
func TestLoadAllowListPersistedWinsOverEnv(t *testing.T) {
	ctx := context.Background()
	store := chat.NewMemStore()
	if err := store.SetConfig(ctx, ConfigKVAllowKey, "GB7DB-1\nGB7DB-2"); err != nil {
		t.Fatal(err)
	}

	a, err := LoadAllowList(ctx, store, []string{"GB7ENV-9"}) // different env seed
	if err != nil {
		t.Fatalf("LoadAllowList: %v", err)
	}
	if a.Allowed("GB7ENV-9") {
		t.Fatal("env seed overrode the persisted set (persisted must win)")
	}
	if !a.Allowed("GB7DB-1") || !a.Allowed("GB7DB-2") {
		t.Fatal("persisted set was not loaded")
	}
}

// TestLoadAllowListPersistedEmptyWinsOverEnv: an explicitly persisted EMPTY set
// (the operator cleared the list in the editor) wins over the env seed too — the
// presence of the key, not its emptiness, is what marks "already seeded".
func TestLoadAllowListPersistedEmptyWinsOverEnv(t *testing.T) {
	ctx := context.Background()
	store := chat.NewMemStore()
	if err := store.SetConfig(ctx, ConfigKVAllowKey, ""); err != nil {
		t.Fatal(err)
	}
	a, err := LoadAllowList(ctx, store, []string{"GB7ENV-9"})
	if err != nil {
		t.Fatalf("LoadAllowList: %v", err)
	}
	if a.Allowed("GB7ENV-9") {
		t.Fatal("env seed re-applied over an explicitly-cleared persisted set")
	}
	if len(a.Entries()) != 0 {
		t.Fatalf("editable set = %v, want empty", a.Entries())
	}
}

// TestLoadAllowListNilStore: a nil store (standalone / no state dir) seeds from
// the env without persisting, and never panics.
func TestLoadAllowListNilStore(t *testing.T) {
	a, err := LoadAllowList(context.Background(), nil, []string{"GB7ENV-1"})
	if err != nil {
		t.Fatalf("LoadAllowList(nil store): %v", err)
	}
	if !a.Allowed("GB7ENV-1") {
		t.Fatal("env seed not honoured with a nil store")
	}
}

// TestPersistAndReloadRoundTrip: a live edit persisted with PersistAllowList is
// read back by ReloadAllowList onto a DIFFERENT in-memory list (the cross-process
// hot-reload: process A edits + persists, process B reloads).
func TestPersistAndReloadRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := chat.NewMemStore()

	// Process A: load (seeds + persists), then add an entry and persist the edit.
	a, err := LoadAllowList(ctx, store, []string{"GB7BASE-1"})
	if err != nil {
		t.Fatal(err)
	}
	a.Add("GB7HOT-1")
	if err := PersistAllowList(ctx, store, a); err != nil {
		t.Fatalf("PersistAllowList: %v", err)
	}

	// Process B: a fresh allow-list reloads the persisted set.
	b := NewAllowList()
	if err := ReloadAllowList(ctx, store, b); err != nil {
		t.Fatalf("ReloadAllowList: %v", err)
	}
	if !b.Allowed("GB7BASE-1") || !b.Allowed("GB7HOT-1") {
		t.Fatalf("reloaded set missing entries: %v", b.Entries())
	}
}

// TestReloadAllowListSamePointerLive: ReloadAllowList re-applies the persisted set
// onto the SAME pointer both ingresses already hold — proving a hot edit is visible
// without re-wiring. (The cross-ingress, no-restart proof is the end-to-end test
// TestPersistedAndHotEditAtBothIngresses; this is the in-RAM mechanism.)
func TestReloadAllowListSamePointerLive(t *testing.T) {
	ctx := context.Background()
	store := chat.NewMemStore()
	a, err := LoadAllowList(ctx, store, nil) // starts deny-all
	if err != nil {
		t.Fatal(err)
	}
	if a.Allowed("GB7LIVE-1") {
		t.Fatal("deny-all list admitted GB7LIVE-1 before the edit")
	}
	// An out-of-band edit lands in the store (as the S5 editor would write it)…
	if err := store.SetConfig(ctx, ConfigKVAllowKey, "GB7LIVE-1"); err != nil {
		t.Fatal(err)
	}
	// …and a reload applies it to the very same list, with no new allocation.
	if err := ReloadAllowList(ctx, store, a); err != nil {
		t.Fatalf("ReloadAllowList: %v", err)
	}
	if !a.Allowed("GB7LIVE-1") {
		t.Fatal("hot reload did not take effect on the live list")
	}
}

// TestDecodeAllowTolerant: a persisted value with mixed separators (newline,
// comma, space) and casing decodes to canonical callsigns — a hand-edited config
// value still loads.
func TestDecodeAllowTolerant(t *testing.T) {
	got := decodeAllow(" gb7a-1\nGB7B-2 , gb7c-3\t")
	want := []string{"GB7A-1", "GB7B-2", "GB7C-3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decodeAllow = %v, want %v", got, want)
	}
}
