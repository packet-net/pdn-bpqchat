package peer

import (
	"strings"
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
// initiated them; a node that dials out to a peer should still list that peer
// here to accept the symmetric inbound link, so the daemon folds the configured
// outbound peer callsigns into the effective list (config.PeerAllowEffective) —
// the operator never has to allow-list a peer they already chose to dial.
//
// Matching is by canonical callsign (trimmed, upper-cased) and is SSID-exact:
// GB7NDH-3 and GB7NDH-1 are distinct peers, exactly as BPQ's OtherChatNodes
// entries are. The zero value is a valid empty (deny-all) list.
type AllowList struct {
	allowed  map[string]struct{}
	rejected atomic.Int64 // count of inbound links refused (observable telemetry)
}

// NewAllowList builds an allow-list from the given peer callsigns. Each is
// canonicalised (trimmed + upper-cased); blanks are ignored. A list with no
// usable entries is a valid deny-all list.
func NewAllowList(callsigns ...string) *AllowList {
	a := &AllowList{allowed: make(map[string]struct{}, len(callsigns))}
	for _, c := range callsigns {
		if cc := normCallsign(c); cc != "" {
			a.allowed[cc] = struct{}{}
		}
	}
	return a
}

// Allowed reports whether an inbound peer presenting callsign may link in. The
// posture is default-deny: an unknown, blank, or non-listed callsign is refused,
// and a nil or empty list refuses everything (§4.1).
func (a *AllowList) Allowed(callsign string) bool {
	if a == nil || len(a.allowed) == 0 {
		return false // deny-all: an empty list admits no inbound peer (§4.1)
	}
	cc := normCallsign(callsign)
	if cc == "" {
		return false
	}
	_, ok := a.allowed[cc]
	return ok
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

// Entries returns the allow-listed callsigns (canonical form), unordered — for
// logging the effective policy at startup.
func (a *AllowList) Entries() []string {
	if a == nil {
		return nil
	}
	out := make([]string, 0, len(a.allowed))
	for c := range a.allowed {
		out = append(out, c)
	}
	return out
}

// normCallsign canonicalises a callsign for allow-list comparison: trimmed and
// upper-cased, matching how the peer protocol (proto.go:Decode) and config
// normalise callsigns, so a listed peer matches regardless of input casing.
func normCallsign(c string) string { return strings.ToUpper(strings.TrimSpace(c)) }
