package web

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/m0lte/pdn-bpqchat/internal/chat"
	"github.com/m0lte/pdn-bpqchat/internal/peer"
)

// testFedServer builds a web server WIRED for federation (S5): a shared allow-list
// (the same pointer the ingresses would hold), an in-memory config store for
// persistence, the relay router for link telemetry, and a configured outbound
// peer. It returns the server, its httptest front, and the shared allow-list so a
// test can assert the live list AND drive an inbound link against it.
func testFedServer(t *testing.T, allowSeed ...string) (*Server, *httptest.Server, *peer.AllowList) {
	t.Helper()
	hub := chat.NewHub("M0LTE-4", chat.NewMemStore(), nil)
	claims := newClaimStore(t)
	allow := peer.NewAllowList(allowSeed...)
	router := peer.NewRouter(hub)
	t.Cleanup(router.Close)
	store := chat.NewMemStore() // the AllowConfigStore persistence seam
	s := New(0, "M0LTE-4", hub, claims, slogDiscard()).WithFederation(&Federation{
		Allow:      allow,
		AllowStore: store,
		Router:     router,
		Peers:      []ConfiguredPeer{{Call: "GB7RDG-1", Transport: "rf", Target: "GB7RDG"}},
	})
	ts := httptest.NewServer(s.srv.Handler)
	t.Cleanup(ts.Close)
	return s, ts, allow
}

// tcpPair returns a connected pair of real TCP sockets. The OS socket buffering
// matters: the BPQ node-link handshake writes (banner, OK, keepalives) must not
// block on a synchronous reader, so a buffered transport — not net.Pipe — is what
// the end-to-end inbound-admission test needs (mirrors the peer package harness).
func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()
	dial, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	return dial, r.c
}

// TestPeersRequiresAdminScope (S5): GET /peers is admin-only. A read-, operate-,
// or unscoped AUTHENTICATED viewer is 403; only admin (or the auth-off owner) gets
// the panel.
func TestPeersRequiresAdminScope(t *testing.T) {
	s, ts, _ := testFedServer(t)
	seedClaim(t, s, "lurker@pdn", "M0RDR")
	seedClaim(t, s, "op@pdn", "M0OPR")
	seedClaim(t, s, "boss@pdn", "M0ADM")

	for _, c := range []struct {
		user, scope string
		want        int
	}{
		{"lurker@pdn", "read", http.StatusForbidden},
		{"op@pdn", "operate", http.StatusForbidden},
		{"boss@pdn", "", http.StatusForbidden}, // authenticated but no scope is NOT admin
		{"boss@pdn", "admin", http.StatusOK},
	} {
		resp := gwGetScoped(t, ts.URL, "/peers", c.user, c.scope)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("GET /peers user=%q scope=%q = %d, want %d", c.user, c.scope, resp.StatusCode, c.want)
		}
	}
}

// TestPeersAnonymousOwnerIsAdmin (S5): with management auth off (no X-Pdn-User) the
// viewer is the node owner on their own loopback node — the degenerate single-user
// operator — and so reaches the federation panel.
func TestPeersAnonymousOwnerIsAdmin(t *testing.T) {
	_, ts, _ := testFedServer(t, "GB7NDH-1")
	resp := gwGetScoped(t, ts.URL, "/peers", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous owner GET /peers = %d, want 200", resp.StatusCode)
	}
	var view peersView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatalf("decode peers view: %v", err)
	}
	if view.OurNode != "M0LTE-4" {
		t.Errorf("ourNode = %q, want M0LTE-4", view.OurNode)
	}
	if len(view.Allow) != 1 || view.Allow[0] != "GB7NDH-1" {
		t.Errorf("allow = %v, want [GB7NDH-1]", view.Allow)
	}
	if len(view.Configured) != 1 || view.Configured[0].Call != "GB7RDG-1" {
		t.Errorf("configured = %v, want one GB7RDG-1 peer", view.Configured)
	}
}

// TestPeersReportsLinkedNodes (S5): the panel surfaces the hub's known-node graph
// (call/alias/version/linked-since) — the same offNode()/Origin plumbing that
// renders the per-message origin badges for everyone.
func TestPeersReportsLinkedNodes(t *testing.T) {
	s, ts, _ := testFedServer(t)
	s.hub.LinkNode("GB7XYZ-1", "XYZ", "1.2.3")

	resp := gwGetScoped(t, ts.URL, "/peers", "", "")
	defer resp.Body.Close()
	var view peersView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(view.Nodes) != 1 {
		t.Fatalf("nodes = %v, want one", view.Nodes)
	}
	n := view.Nodes[0]
	if n.Call != "GB7XYZ-1" || n.Alias != "XYZ" || n.Version != "1.2.3" {
		t.Fatalf("node = %+v, want GB7XYZ-1/XYZ/1.2.3", n)
	}
	if n.LinkedSince == 0 {
		t.Error("linkedSince should be set for a linked node")
	}
}

// TestPeersAllowAddRemoveMutatesLiveList (S5): an admin add/remove POST mutates the
// SHARED live allow-list (the very pointer the ingresses hold), persists it through
// the config seam, and echoes the resulting set. A non-admin POST is 403 and
// changes nothing.
func TestPeersAllowAddRemoveMutatesLiveList(t *testing.T) {
	s, ts, allow := testFedServer(t)
	seedClaim(t, s, "op@pdn", "M0OPR")

	// A non-admin (operate) POST is refused and must not touch the list.
	resp := gwPostScoped(t, ts.URL, "/peers/allow", "op@pdn", "operate", `{"action":"add","callsign":"GB7NDH-1"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("operate-scope /peers/allow = %d, want 403", resp.StatusCode)
	}
	if allow.Allowed("GB7NDH-1") {
		t.Fatal("operate-scope edit leaked into the live allow-list")
	}

	// Admin add: the live shared list now admits it, and the response echoes it.
	resp = gwPostScoped(t, ts.URL, "/peers/allow", "", "", `{"action":"add","callsign":"gb7ndh-1"}`)
	got := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin add = %d, want 200; body=%s", resp.StatusCode, got)
	}
	if !allow.Allowed("GB7NDH-1") {
		t.Fatal("admin add did not reach the live shared allow-list")
	}
	if !strings.Contains(got, `"changed":true`) || !strings.Contains(got, `"GB7NDH-1"`) {
		t.Fatalf("add response = %s, want changed=true and the callsign echoed", got)
	}

	// The edit was persisted to the config store (survives a restart).
	raw, ok, err := s.fed.AllowStore.GetConfig(context.Background(), peer.ConfigKVAllowKey)
	if err != nil || !ok || !strings.Contains(raw, "GB7NDH-1") {
		t.Fatalf("add not persisted: raw=%q ok=%v err=%v", raw, ok, err)
	}

	// Admin remove: gone from the live list.
	resp = gwPostScoped(t, ts.URL, "/peers/allow", "", "", `{"action":"remove","callsign":"GB7NDH-1"}`)
	got = body(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(got, `"changed":true`) {
		t.Fatalf("admin remove = %d body=%s", resp.StatusCode, got)
	}
	if allow.Allowed("GB7NDH-1") {
		t.Fatal("admin remove did not strip the entry from the live list")
	}
}

// TestPeersAllowBadInput (S5): a missing callsign or an unknown action is a 400; an
// admin add of an already-present callsign is an idempotent changed=false success.
func TestPeersAllowBadInput(t *testing.T) {
	_, ts, _ := testFedServer(t, "GB7NDH-1")
	for _, c := range []struct {
		body string
		want int
	}{
		{`{"action":"add","callsign":""}`, http.StatusBadRequest},
		{`{"action":"frobnicate","callsign":"GB7X-1"}`, http.StatusBadRequest},
	} {
		resp := gwPostScoped(t, ts.URL, "/peers/allow", "", "", c.body)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("POST %s = %d, want %d", c.body, resp.StatusCode, c.want)
		}
	}
	// Re-adding an existing entry is idempotent: 200 changed=false.
	resp := gwPostScoped(t, ts.URL, "/peers/allow", "", "", `{"action":"add","callsign":"GB7NDH-1"}`)
	got := body(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(got, `"changed":false`) {
		t.Fatalf("idempotent add = %d body=%s, want 200 changed=false", resp.StatusCode, got)
	}
}

// TestNoFederationWiringIs503 (S5): a server built WITHOUT WithFederation (the
// standalone / default posture) has no federation surface — an admin viewer still
// gets 503, not a panic or a misleading 200.
func TestNoFederationWiringIs503(t *testing.T) {
	_, ts := testServer(t) // plain New, no WithFederation
	for _, path := range []string{"/peers", "/peers/allow"} {
		method := http.MethodGet
		var resp *http.Response
		if path == "/peers/allow" {
			method = http.MethodPost
		}
		if method == http.MethodGet {
			resp = gwGetScoped(t, ts.URL, path, "", "")
		} else {
			resp = gwPostScoped(t, ts.URL, path, "", "", `{"action":"add","callsign":"GB7X-1"}`)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s %s without federation wiring = %d, want 503", method, path, resp.StatusCode)
		}
	}
}

// TestAdminAddThenInboundAdmitted (S5, the headline end-to-end): an inbound peer
// NOT on the allow-list is refused; after the admin adds its callsign through the
// web editor, a SUBSEQUENT inbound IP link from that same peer is ADMITTED — the
// hot-edit reaches the live ingress with no restart, because the editor mutated the
// very pointer ServeInboundIP checks.
func TestAdminAddThenInboundAdmitted(t *testing.T) {
	s, ts, allow := testFedServer(t) // starts deny-all
	_ = s

	ourNode := "M0LTE-4"

	// A helper that drives one inbound IP link attempt from peer GB7AAA against the
	// SHARED allow-list, returning whether GB7AAA's user reached the listener hub.
	tryInbound := func() bool {
		// Fresh dialer side each attempt (a refused link tears down its connection).
		dialerHub := chat.NewHub("GB7AAA", chat.NewMemStore(), nil)
		dialerRouter := peer.NewRouter(dialerHub)
		defer dialerRouter.Close()
		akey := chat.UserKey{Call: "G8PZT", Node: "GB7AAA"}
		dialerHub.Join(chat.User{Call: akey.Call, Origin: chat.Origin{Node: "GB7AAA", Local: true}})

		aConn, bConn := tcpPair(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer aConn.Close()
		defer bConn.Close()

		la := peer.NewLink(aConn, dialerRouter, dialerHub, peer.Config{
			PeerCall: ourNode, OurNode: "GB7AAA", Outbound: true, Keepalive: time.Hour,
		})
		go la.Run(ctx)
		// The listener uses the web server's SHARED allow-list — the same one the
		// editor mutates.
		go func() { _ = peer.ServeInboundIP(ctx, bConn, s.fed.Router, s.hub, ourNode, allow, nil) }()

		// Give the link a moment to either admit (the user propagates) or be refused.
		deadline := time.After(1500 * time.Millisecond)
		for {
			if _, ok := s.hub.User(akey); ok {
				return true
			}
			select {
			case <-deadline:
				return false
			case <-time.After(10 * time.Millisecond):
			}
		}
	}

	// Before the edit: deny-all, so the inbound link is refused.
	if tryInbound() {
		t.Fatal("inbound peer admitted before being allow-listed (default-deny violated)")
	}
	if allow.Rejected() == 0 {
		t.Fatal("refused inbound was not counted")
	}

	// Admin allows GB7AAA through the web editor (hot edit on the live pointer).
	resp := gwPostScoped(t, ts.URL, "/peers/allow", "", "", `{"action":"add","callsign":"GB7AAA"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin add GB7AAA = %d, want 200", resp.StatusCode)
	}

	// After the edit: a subsequent inbound from the newly-allowed peer is admitted.
	if !tryInbound() {
		t.Fatal("inbound peer still refused after admin allow-listed it (hot-edit did not reach the live ingress)")
	}
}
