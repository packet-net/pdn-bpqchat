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
