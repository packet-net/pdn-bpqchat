package peer

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// AllowList is the inbound-peer admission control for federation (design.md
// §4.1). AX.25 has no link authentication, so BPQ authorises an inbound node
// link purely by matching the caller's claimed callsign against its configured
// peer list (HanksRT.c:rtloginl) — and pdn-bpqchat keeps that as a FIRST-CLASS
// config concept with a strict DEFAULT-DENY posture: an inbound peer whose
// callsign is not on the list never links in (it is dropped before any hub
// state is mutated), and an EMPTY list admits NO inbound peer at all.
//
// It governs INBOUND links only. Outbound peers we dial are trusted because we
// initiated them; those callsigns are folded in as PINNED entries (Pin) that the
// effective list always admits but that the editable/persisted set never owns —
// so the operator never has to allow-list a peer they already chose to dial, and
// an editor removing a persisted entry can never strip the implicit trust of a
// peer we actively dial out to.
//
// The list is MUTABLE and hot-reloadable (S4): the editable set is held behind a
// lock and can be replaced live (Replace), added to (Add), or removed from
// (Remove) WITHOUT a restart. Both ingresses (node.serveInbound and
// peer.ServeInboundIP) hold the SAME *AllowList pointer, so a config edit applied
// here re-applies at both ingresses immediately — there is exactly ONE list.
//
// Matching is by canonical callsign (trimmed, upper-cased) and is SSID-exact:
// GB7NDH-3 and GB7NDH-1 are distinct peers, exactly as BPQ's OtherChatNodes
// entries are. The zero value is a valid empty (deny-all) list.
type AllowList struct {
	mu       sync.RWMutex
	allowed  map[string]struct{} // editable/persisted set (env seed + config-table + live edits)
	pinned   map[string]struct{} // outbound-dialed peers: implicitly trusted, not editor-owned
	rejected atomic.Int64        // count of inbound links refused (observable telemetry)
}

// NewAllowList builds an allow-list from the given peer callsigns (the editable
// set). Each is canonicalised (trimmed + upper-cased); blanks are ignored. A list
// with no usable entries is a valid deny-all list.
func NewAllowList(callsigns ...string) *AllowList {
	a := &AllowList{
		allowed: make(map[string]struct{}, len(callsigns)),
		pinned:  make(map[string]struct{}),
	}
	for _, c := range callsigns {
		if cc := normCallsign(c); cc != "" {
			a.allowed[cc] = struct{}{}
		}
	}
	return a
}

// Pin marks callsigns as implicitly trusted inbound peers because we dial OUT to
// them (config.Peers/RFPeers). Pinned entries are always admitted but are NOT part
// of the editable/persisted set: the web editor never sees or removes them, and a
// Replace of the editable set leaves them intact. Idempotent.
func (a *AllowList) Pin(callsigns ...string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pinned == nil {
		a.pinned = make(map[string]struct{}, len(callsigns))
	}
	for _, c := range callsigns {
		if cc := normCallsign(c); cc != "" {
			a.pinned[cc] = struct{}{}
		}
	}
}

// Allowed reports whether an inbound peer presenting callsign may link in. The
// posture is default-deny: an unknown, blank, or non-listed callsign is refused,
// and a nil or empty list refuses everything (§4.1). A pinned (outbound-dialed)
// peer is always admitted.
func (a *AllowList) Allowed(callsign string) bool {
	if a == nil {
		return false // nil list admits no inbound peer (§4.1)
	}
	cc := normCallsign(callsign)
	if cc == "" {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if _, ok := a.allowed[cc]; ok {
		return true
	}
	_, ok := a.pinned[cc]
	return ok
}

// Replace swaps the editable/persisted set for callsigns atomically (the hot-edit
// path: a config-table or chat.yaml change re-applies live without a restart).
// Pinned (outbound-dialed) entries are untouched. Each callsign is canonicalised;
// blanks are dropped. An empty replacement restores the deny-all posture for the
// editable set (pinned peers still link).
func (a *AllowList) Replace(callsigns []string) {
	if a == nil {
		return
	}
	next := make(map[string]struct{}, len(callsigns))
	for _, c := range callsigns {
		if cc := normCallsign(c); cc != "" {
			next[cc] = struct{}{}
		}
	}
	a.mu.Lock()
	a.allowed = next
	a.mu.Unlock()
}

// Add inserts a callsign into the editable set live and reports whether it was
// newly added (false if it was already present or blank). Both ingresses see it
// at once — they share this pointer.
func (a *AllowList) Add(callsign string) bool {
	if a == nil {
		return false
	}
	cc := normCallsign(callsign)
	if cc == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.allowed == nil {
		a.allowed = make(map[string]struct{})
	}
	if _, ok := a.allowed[cc]; ok {
		return false
	}
	a.allowed[cc] = struct{}{}
	return true
}

// Remove deletes a callsign from the editable set live and reports whether it was
// present. A pinned (outbound-dialed) peer is NOT in the editable set, so removing
// its callsign here is a no-op that does not revoke its implicit trust.
func (a *AllowList) Remove(callsign string) bool {
	if a == nil {
		return false
	}
	cc := normCallsign(callsign)
	if cc == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.allowed[cc]; !ok {
		return false
	}
	delete(a.allowed, cc)
	return true
}

// Reject records a refused inbound link (for telemetry) and returns the running
// count. Call it on the drop path so the reject behaviour is observable.
func (a *AllowList) Reject() int64 {
	if a == nil {
		return 0
	}
	return a.rejected.Add(1)
}

// Rejected returns how many inbound links have been refused so far.
func (a *AllowList) Rejected() int64 {
	if a == nil {
		return 0
	}
	return a.rejected.Load()
}

// Entries returns the EDITABLE allow-listed callsigns (canonical form), sorted —
// the operator-owned set the web editor manages and that we persist. Pinned
// (outbound-dialed) entries are excluded; use AllEntries for the full effective
// admission set.
func (a *AllowList) Entries() []string {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]string, 0, len(a.allowed))
	for c := range a.allowed {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// AllEntries returns every callsign that may link in — the editable set UNIONed
// with the pinned (outbound-dialed) peers — sorted and de-duplicated. This is the
// full effective admission set, for logging the policy at startup.
func (a *AllowList) AllEntries() []string {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	seen := make(map[string]struct{}, len(a.allowed)+len(a.pinned))
	for c := range a.allowed {
		seen[c] = struct{}{}
	}
	for c := range a.pinned {
		seen[c] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// normCallsign canonicalises a callsign for allow-list comparison: trimmed and
// upper-cased, matching how the peer protocol (proto.go:Decode) and config
// normalise callsigns, so a listed peer matches regardless of input casing.
func normCallsign(c string) string { return strings.ToUpper(strings.TrimSpace(c)) }
