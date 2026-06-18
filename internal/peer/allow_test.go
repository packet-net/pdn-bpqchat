package peer

import "testing"

// TestAllowListDefaultDeny: an empty (or nil) allow-list admits no inbound peer
// — the default-deny posture (design.md §4.1).
func TestAllowListDefaultDeny(t *testing.T) {
	empty := NewAllowList()
	if empty.Allowed("GB7NDH-3") {
		t.Fatal("empty allow-list admitted a peer — default-deny violated")
	}
	var nilList *AllowList
	if nilList.Allowed("GB7NDH-3") {
		t.Fatal("nil allow-list admitted a peer — default-deny violated")
	}
}

// TestAllowListAdmitsListed: a listed callsign is admitted; an unlisted one is not.
func TestAllowListAdmitsListed(t *testing.T) {
	a := NewAllowList("GB7NDH-3", "GB7WOD-1")
	if !a.Allowed("GB7NDH-3") {
		t.Fatal("GB7NDH-3 is listed but was refused")
	}
	if !a.Allowed("GB7WOD-1") {
		t.Fatal("GB7WOD-1 is listed but was refused")
	}
	if a.Allowed("GB7WOK-1") {
		t.Fatal("GB7WOK-1 is NOT listed but was admitted")
	}
}

// TestAllowListCanonicalises: matching is case-insensitive and whitespace-trimmed,
// matching how proto.go decodes peer callsigns (upper-cased, space-split).
func TestAllowListCanonicalises(t *testing.T) {
	a := NewAllowList(" gb7ndh-3 ") // mixed case + surrounding whitespace
	for _, in := range []string{"GB7NDH-3", "gb7ndh-3", "  GB7NDH-3  "} {
		if !a.Allowed(in) {
			t.Fatalf("Allowed(%q) = false, want true (canonicalisation)", in)
		}
	}
	// Blank input is never admitted.
	if a.Allowed("") || a.Allowed("   ") {
		t.Fatal("blank callsign admitted")
	}
}

// TestAllowListSSIDExact: SSID is significant — GB7NDH-3 and GB7NDH-1 are distinct
// peers (BPQ's OtherChatNodes entries are SSID-exact).
func TestAllowListSSIDExact(t *testing.T) {
	a := NewAllowList("GB7NDH-3")
	if a.Allowed("GB7NDH-1") {
		t.Fatal("GB7NDH-1 admitted by a GB7NDH-3 allow-list — SSID must be exact")
	}
	if a.Allowed("GB7NDH") {
		t.Fatal("bare GB7NDH admitted by a GB7NDH-3 allow-list — SSID must be exact")
	}
}

// TestAllowListRejectCounter: Reject() increments an observable counter.
func TestAllowListRejectCounter(t *testing.T) {
	a := NewAllowList("GB7NDH-3")
	if a.Rejected() != 0 {
		t.Fatalf("initial rejected = %d, want 0", a.Rejected())
	}
	if n := a.Reject(); n != 1 {
		t.Fatalf("Reject() returned %d, want 1", n)
	}
	a.Reject()
	if a.Rejected() != 2 {
		t.Fatalf("rejected = %d, want 2", a.Rejected())
	}
	// nil is safe and never panics.
	var nilList *AllowList
	if nilList.Reject() != 0 || nilList.Rejected() != 0 {
		t.Fatal("nil allow-list Reject/Rejected must be 0, not panic")
	}
}

// TestAllowListHotEditAddRemove: Add/Remove mutate the editable set LIVE — the
// hot-edit primitive the S5 web editor calls. Because both ingresses share one
// *AllowList pointer, an Add/Remove here is seen at both without a restart.
func TestAllowListHotEditAddRemove(t *testing.T) {
	a := NewAllowList()
	if a.Allowed("GB7NDH-3") {
		t.Fatal("empty list admitted GB7NDH-3")
	}
	if !a.Add("gb7ndh-3") { // canonicalised on the way in
		t.Fatal("Add reported no change for a new entry")
	}
	if a.Add("GB7NDH-3") {
		t.Fatal("Add reported a change re-adding an existing entry")
	}
	if !a.Allowed("GB7NDH-3") {
		t.Fatal("hot-added GB7NDH-3 is not admitted")
	}
	if !a.Remove("GB7NDH-3") {
		t.Fatal("Remove reported no change for a present entry")
	}
	if a.Remove("GB7NDH-3") {
		t.Fatal("Remove reported a change for an absent entry")
	}
	if a.Allowed("GB7NDH-3") {
		t.Fatal("hot-removed GB7NDH-3 is still admitted")
	}
	if a.Add("") || a.Remove("   ") {
		t.Fatal("blank Add/Remove must report no change")
	}
}

// TestAllowListReplace: Replace swaps the editable set atomically (the hot-reload
// path) and restores deny-all for an empty replacement.
func TestAllowListReplace(t *testing.T) {
	a := NewAllowList("GB7OLD-1")
	a.Replace([]string{"GB7NEW-1", " gb7new-2 "})
	if a.Allowed("GB7OLD-1") {
		t.Fatal("GB7OLD-1 still admitted after Replace dropped it")
	}
	if !a.Allowed("GB7NEW-1") || !a.Allowed("GB7NEW-2") {
		t.Fatal("Replace did not admit the new entries")
	}
	a.Replace(nil)
	if a.Allowed("GB7NEW-1") {
		t.Fatal("empty Replace did not restore deny-all")
	}
}

// TestAllowListPinnedAlwaysAdmitted: pinned (outbound-dialed) peers are admitted
// even though they are NOT in the editable set, and Remove of a pinned callsign
// does NOT revoke its implicit trust (the dialed-peer guarantee, §4.1). Entries()
// shows only the editable set; AllEntries() shows the union.
func TestAllowListPinnedAlwaysAdmitted(t *testing.T) {
	a := NewAllowList("GB7EDIT-1")
	a.Pin("GB7DIAL-1", " gb7dial-2 ")
	if !a.Allowed("GB7DIAL-1") || !a.Allowed("GB7DIAL-2") {
		t.Fatal("pinned outbound-dialed peer was not admitted")
	}
	// Removing a pinned callsign from the editable set is a no-op for trust.
	if a.Remove("GB7DIAL-1") {
		t.Fatal("Remove reported a change for a pinned-only callsign (not in editable set)")
	}
	if !a.Allowed("GB7DIAL-1") {
		t.Fatal("pinned peer lost trust after a Remove of its callsign")
	}
	// Replace clears the editable set but leaves pinned peers admitted.
	a.Replace(nil)
	if a.Allowed("GB7EDIT-1") {
		t.Fatal("editable entry survived Replace(nil)")
	}
	if !a.Allowed("GB7DIAL-1") {
		t.Fatal("Replace(nil) revoked a pinned peer's trust")
	}
	// Entries() is the editable set only; AllEntries() the full admission set.
	if got := a.Entries(); len(got) != 0 {
		t.Fatalf("Entries() = %v, want empty (editable set cleared)", got)
	}
	all := a.AllEntries()
	if len(all) != 2 || all[0] != "GB7DIAL-1" || all[1] != "GB7DIAL-2" {
		t.Fatalf("AllEntries() = %v, want sorted [GB7DIAL-1 GB7DIAL-2]", all)
	}
}

// TestAllowListNilMutationsSafe: every mutator is nil-safe (never panics).
func TestAllowListNilMutationsSafe(t *testing.T) {
	var a *AllowList
	a.Pin("X")
	a.Replace([]string{"X"})
	if a.Add("X") || a.Remove("X") {
		t.Fatal("nil allow-list mutators must report no change")
	}
	if a.Entries() != nil || a.AllEntries() != nil {
		t.Fatal("nil allow-list Entries/AllEntries must be nil")
	}
}
