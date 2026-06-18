package web

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/m0lte/pdn-bpqchat/internal/peer"
)

// The S5 federation surface (admin scope). Two endpoints, both behind the admin
// gate, built ON TOP of the existing #3 inbound-peer allow-list (peer.AllowList)
// and S4's persisted config seam — never a parallel list:
//
//   - GET  /peers        — the federation status panel: the known mesh-node graph
//     (hub.Nodes — call/alias/version/linked-since), the operator-configured
//     outbound peers, the live per-link state/last-seen/RTT, and the current
//     editable allow-list (with the pinned outbound-dialed set shown separately).
//   - POST /peers/allow  — add/remove a callsign on the SHARED allow-list, persist
//     it (hot-reload — both ingresses hold the same pointer), and audit to the app
//     log. A subsequent inbound from a newly-allowed peer is admitted at once.
//
// The origin-node badge half of the split federation decision (§4.2) is rendered
// for EVERYONE by the existing offNode()/Origin.Node plumbing (handlers.go +
// index.go) — it is not gated here. NodeLinked/NodeUnlinked already surface as
// live system lines over SSE (handlers.filter). This file adds only the admin
// panel + editor.

// nodeView is the wire shape of one known mesh node in the federation panel.
type nodeView struct {
	Call        string `json:"call"`
	Alias       string `json:"alias,omitempty"`
	Version     string `json:"version,omitempty"`
	LinkedSince int64  `json:"linkedSince,omitempty"` // unix ms the node was first linked
}

// linkView is the wire shape of one live peer link's telemetry.
type linkView struct {
	Peer     string `json:"peer"`
	Outbound bool   `json:"outbound"`           // true: we dialled; false: it dialled us
	LinkedAt int64  `json:"linkedAt,omitempty"` // unix ms the link came up
	LastSeen int64  `json:"lastSeen,omitempty"` // unix ms of the last record received
	RTTms    int64  `json:"rttMs,omitempty"`    // last keepalive→poll-response round-trip, ms (0 = none yet)
}

// configuredPeerView is the wire shape of one operator-configured outbound peer.
type configuredPeerView struct {
	Call      string `json:"call"`
	Transport string `json:"transport"`        // "ip" | "rf"
	Target    string `json:"target,omitempty"` // dial target
}

// peersView is the full federation panel payload (admin scope).
type peersView struct {
	OurNode    string               `json:"ourNode"`    // our chat callsign (the local node)
	Nodes      []nodeView           `json:"nodes"`      // known mesh nodes (hub.Nodes)
	Links      []linkView           `json:"links"`      // live per-link telemetry
	Configured []configuredPeerView `json:"configured"` // operator-configured outbound peers
	Allow      []string             `json:"allow"`      // editable inbound allow-list (operator-owned)
	Pinned     []string             `json:"pinned"`     // pinned outbound-dialed peers (always admitted)
	Rejected   int64                `json:"rejected"`   // count of refused inbound links (telemetry)
}

// handlePeers serves the admin federation status panel (GET /peers). Behind the
// admin scope: a non-admin viewer is 403 BEFORE any state is read.
func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireAdmin(w, r) {
		return
	}
	if s.fed == nil {
		http.Error(w, "federation administration is not available on this node", http.StatusServiceUnavailable)
		return
	}

	view := peersView{
		OurNode:    s.hub.OurNode(),
		Nodes:      make([]nodeView, 0),
		Links:      make([]linkView, 0),
		Configured: make([]configuredPeerView, 0),
		Allow:      make([]string, 0),
		Pinned:     make([]string, 0),
	}
	for _, n := range s.hub.Nodes() {
		view.Nodes = append(view.Nodes, nodeView{
			Call:        n.Call,
			Alias:       n.Alias,
			Version:     n.Version,
			LinkedSince: msOrZero(n.Linked),
		})
	}
	if s.fed.Router != nil {
		for _, l := range s.fed.Router.LinkStatuses() {
			view.Links = append(view.Links, linkView{
				Peer:     l.PeerCall,
				Outbound: l.Outbound,
				LinkedAt: msOrZero(l.LinkedAt),
				LastSeen: msOrZero(l.LastSeen),
				RTTms:    l.RTT.Milliseconds(),
			})
		}
	}
	for _, p := range s.fed.Peers {
		view.Configured = append(view.Configured, configuredPeerView{
			Call:      p.Call,
			Transport: p.Transport,
			Target:    p.Target,
		})
	}
	if s.fed.Allow != nil {
		view.Allow = s.fed.Allow.Entries()
		// Pinned = the effective admission set minus the editable set (the
		// outbound-dialed peers we trust implicitly but never persist/edit).
		view.Pinned = pinnedOnly(s.fed.Allow)
		view.Rejected = s.fed.Allow.Rejected()
	}
	// Entries()/AllEntries() may return nil; keep the JSON arrays non-null.
	if view.Allow == nil {
		view.Allow = make([]string, 0)
	}
	writeJSON(w, view)
}

// allowEditRequest is the POST /peers/allow body: an action ("add"|"remove") and a
// callsign to apply it to.
type allowEditRequest struct {
	Action   string `json:"action"`
	Callsign string `json:"callsign"`
}

// allowEditResponse echoes the resulting editable set (so the editor can refresh
// without a second round-trip) and whether the edit changed anything.
type allowEditResponse struct {
	Changed bool     `json:"changed"`
	Allow   []string `json:"allow"`
}

// handlePeersAllow is the inbound-peer allow-list editor (POST /peers/allow),
// admin scope. It mutates the EXISTING shared peer.AllowList (so the change is live
// at BOTH ingresses — they hold this very pointer), persists the new editable set
// through S4's config seam (surviving a restart), and audits the edit to the app
// log. A subsequent inbound from a newly-allowed peer is admitted at once.
//
// Pinned (outbound-dialed) peers are not editable here: a remove of a pinned-only
// callsign reports changed=false and never strips its implicit trust (AllowList
// keeps pinned separate). An add of a callsign already present is an idempotent
// no-op (changed=false).
func (s *Server) handlePeersAllow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireAdmin(w, r) {
		return
	}
	if s.fed == nil || s.fed.Allow == nil {
		http.Error(w, "federation administration is not available on this node", http.StatusServiceUnavailable)
		return
	}

	var in allowEditRequest
	if err := decodeJSON(r, &in); err != nil {
		http.Error(w, "bad allow-list edit body", http.StatusBadRequest)
		return
	}
	call := strings.ToUpper(strings.TrimSpace(in.Callsign))
	if call == "" {
		http.Error(w, "callsign is required", http.StatusBadRequest)
		return
	}

	admin := strings.TrimSpace(IdentityFromRequest(r).User)
	if admin == "" {
		admin = "(node owner)"
	}

	var changed bool
	switch strings.ToLower(strings.TrimSpace(in.Action)) {
	case "add":
		changed = s.fed.Allow.Add(call)
		s.log.Info("peer allow-list edit", "action", "add", "callsign", call,
			"by", admin, "changed", changed)
	case "remove":
		changed = s.fed.Allow.Remove(call)
		s.log.Info("peer allow-list edit", "action", "remove", "callsign", call,
			"by", admin, "changed", changed)
	default:
		http.Error(w, "action must be add or remove", http.StatusBadRequest)
		return
	}

	// Persist the new editable set so the live edit survives a restart (the same
	// config-table seam LoadAllowList reads on the next start). Only on an actual
	// change; a no-op edit need not rewrite the store.
	if changed && s.fed.AllowStore != nil {
		if err := peer.PersistAllowList(r.Context(), s.fed.AllowStore, s.fed.Allow); err != nil {
			s.log.Warn("peer allow-list persist failed", "callsign", call, "err", err)
			http.Error(w, "the edit was applied live but could not be persisted", http.StatusInternalServerError)
			return
		}
	}

	allow := s.fed.Allow.Entries()
	if allow == nil {
		allow = make([]string, 0)
	}
	writeJSON(w, allowEditResponse{Changed: changed, Allow: allow})
}

// pinnedOnly returns the pinned (outbound-dialed) callsigns of an allow-list: the
// effective admission set (AllEntries) minus the editable set (Entries). These are
// shown read-only in the panel — implicitly trusted because we dial them.
func pinnedOnly(a *peer.AllowList) []string {
	editable := map[string]struct{}{}
	for _, c := range a.Entries() {
		editable[c] = struct{}{}
	}
	var out []string
	for _, c := range a.AllEntries() {
		if _, ok := editable[c]; !ok {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	if out == nil {
		out = make([]string, 0)
	}
	return out
}

// msOrZero converts a time to unix ms, mapping the zero time to 0 (omitted in the
// JSON), so an unset timestamp renders cleanly in the panel.
func msOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
