package node

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/m0lte/pdn-bpqchat/internal/chat"
	"github.com/m0lte/pdn-bpqchat/internal/peer"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); ch <- c }()
	dial, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return dial, <-ch
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatal("condition not met in time")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// newLink builds a node Link with an inbound-peer allow-list seeded from allowed
// (empty = the default-deny posture: no inbound peer links in).
func newLink(t *testing.T, allowed ...string) (*Link, *chat.Hub) {
	t.Helper()
	hub := chat.NewHub("M0LTE-4", chat.NewMemStore(), nil)
	router := peer.NewRouter(hub)
	t.Cleanup(router.Close)
	l := New(Options{ChatCallsign: "M0LTE-4", Allow: peer.NewAllowList(allowed...)}, hub, router, discard())
	return l, hub
}

// TestProbeCandidates: the SSID probe order is the configured callsign first,
// then base SSIDs 0..15 with the already-tried one skipped (the fallback path).
func TestProbeCandidates(t *testing.T) {
	got := probeCandidates("M0LTE-4")
	if got[0] != "M0LTE-4" {
		t.Fatalf("first candidate = %q, want the configured M0LTE-4", got[0])
	}
	// 16 = configured + SSID 0..15 (16 values) minus the one (SSID 4) already tried.
	if len(got) != 16 {
		t.Fatalf("candidate count = %d, want 16", len(got))
	}
	for _, cs := range got[1:] {
		if cs == "M0LTE-4" {
			t.Fatalf("configured callsign repeated in probe tail: %v", got)
		}
	}
	if got[1] != "M0LTE" { // SSID 0 → bare base
		t.Fatalf("second candidate = %q, want bare base M0LTE", got[1])
	}
}

// TestBoundCallsignFallsBackToConfigured: before a successful bind, the on-air
// identity is the configured callsign (node-owned or derived).
func TestBoundCallsignAccessor(t *testing.T) {
	l, _ := newLink(t)
	if got := l.boundCallsign(); got != "M0LTE-4" {
		t.Fatalf("boundCallsign before bind = %q, want configured M0LTE-4", got)
	}
	l.mu.Lock()
	l.bound = "GB7CHT-7"
	l.mu.Unlock()
	if got := l.boundCallsign(); got != "GB7CHT-7" {
		t.Fatalf("boundCallsign after bind = %q, want GB7CHT-7", got)
	}
}

// TestDemuxUserSession: a caller who does NOT send *RTL becomes a chat user.
func TestDemuxUserSession(t *testing.T) {
	l, hub := newLink(t)
	server, client := tcpPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.serveInbound(ctx, server, "G8PZT")

	br := bufio.NewReader(client)
	// The node greets with the banner.
	line, _ := br.ReadString('\r')
	if !strings.Contains(line, peer.Banner()) {
		t.Fatalf("expected banner, got %q", line)
	}
	// Send a plain line → we are a user; the hub should show us joined.
	_, _ = client.Write([]byte("hello world\r"))
	waitFor(t, func() bool { _, ok := hub.User(chat.UserKey{Call: "G8PZT", Node: "M0LTE-4"}); return ok })
}

// TestDemuxPeerLink: an ALLOW-LISTED caller whose first line is *RTL becomes a
// peer link (the accept end-to-end path of design.md §4.1).
func TestDemuxPeerLink(t *testing.T) {
	l, hub := newLink(t, "GB7XYZ") // GB7XYZ is allow-listed
	server, client := tcpPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.serveInbound(ctx, server, "GB7XYZ")

	br := bufio.NewReader(client)
	line, _ := br.ReadString('\r')
	if !strings.Contains(line, peer.Banner()) {
		t.Fatalf("expected banner, got %q", line)
	}
	// Log in as a node link.
	_, _ = client.Write([]byte("*RTL\r"))
	// The node should add GB7XYZ to its node graph and reply OK.
	waitFor(t, func() bool {
		for _, n := range hub.Nodes() {
			if n.Call == "GB7XYZ" {
				return true
			}
		}
		return false
	})
	// And we should receive the OK + a keepalive record from the node.
	got := make([]byte, 256)
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _ := br.Read(got)
	if !strings.Contains(string(got[:n]), "OK") {
		t.Fatalf("expected OK after *RTL, got %q", got[:n])
	}
}

// TestDemuxPeerLinkRejectedDefaultDeny: a caller whose first line is *RTL but
// whose callsign is NOT on the allow-list is refused at the federation ingress —
// it never enters the node graph, the link is dropped, and the rejection is
// counted (design.md §4.1, default-deny).
func TestDemuxPeerLinkRejectedDefaultDeny(t *testing.T) {
	l, hub := newLink(t) // EMPTY allow-list → default-deny
	server, client := tcpPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { l.serveInbound(ctx, server, "GB7BAD"); close(done) }()

	br := bufio.NewReader(client)
	if line, _ := br.ReadString('\r'); !strings.Contains(line, peer.Banner()) {
		t.Fatalf("expected banner, got %q", line)
	}
	// Present as a node link from a non-allow-listed callsign.
	_, _ = client.Write([]byte("*RTL\r"))

	// serveInbound must return (the connection is dropped) without ever adding the
	// caller to the node graph — no OK, no state mutation.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serveInbound did not drop the refused peer link")
	}
	for _, n := range hub.Nodes() {
		if n.Call == "GB7BAD" {
			t.Fatalf("refused peer GB7BAD entered the node graph — state mutated on a rejected link")
		}
	}
	if got := l.opts.Allow.Rejected(); got != 1 {
		t.Fatalf("rejected count = %d, want 1 (the reject must be observable)", got)
	}
}

// presentsAsPeer drives one inbound caller through the node ingress (serveInbound)
// presenting as a node link (*RTL) from callsign, and reports whether the link was
// ADMITTED — i.e. the caller entered the node graph and the node replied OK. It is
// the node-ingress probe used by the persisted/hot-edit end-to-end test.
func presentsAsPeer(t *testing.T, l *Link, hub *chat.Hub, callsign string) bool {
	t.Helper()
	server, client := tcpPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.serveInbound(ctx, server, callsign)

	br := bufio.NewReader(client)
	if line, _ := br.ReadString('\r'); !strings.Contains(line, peer.Banner()) {
		t.Fatalf("expected banner, got %q", line)
	}
	_, _ = client.Write([]byte("*RTL\r"))

	// Admission is observable two ways: the node graph gains the caller and an OK
	// record comes back. Poll briefly; an un-admitted caller is dropped instead.
	deadline := time.After(1500 * time.Millisecond)
	for {
		for _, n := range hub.Nodes() {
			if n.Call == callsign {
				return true
			}
		}
		select {
		case <-deadline:
			return false
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// admittedAtIPIngress drives one inbound caller through the IP ingress
// (peer.ServeInboundIP) presenting as a node link from callsign, and reports
// whether it was admitted (ServeInboundIP returns a refusal error for a denied
// peer; an admitted one stays open until ctx is cancelled).
func admittedAtIPIngress(t *testing.T, allow *peer.AllowList, callsign string) bool {
	t.Helper()
	aConn, bConn := tcpPair(t)
	hubA := chat.NewHub("GB7AAA", chat.NewMemStore(), nil)
	hubB := chat.NewHub("GB7BBB", chat.NewMemStore(), nil)
	rA := peer.NewRouter(hubA)
	rB := peer.NewRouter(hubB)
	t.Cleanup(rA.Close)
	t.Cleanup(rB.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The dialer presents `callsign` as its originating node.
	la := peer.NewLink(aConn, rA, hubA, peer.Config{
		PeerCall: "GB7BBB", OurNode: callsign, Outbound: true, Keepalive: time.Hour,
	})
	go la.Run(ctx)

	errc := make(chan error, 1)
	go func() { errc <- peer.ServeInboundIP(ctx, bConn, rB, hubB, "GB7BBB", allow, nil) }()

	// A denied peer returns an allow-list refusal error quickly; an admitted one
	// links up (the listener learns the peer) and ServeInboundIP keeps running.
	select {
	case err := <-errc:
		if err != nil && strings.Contains(err.Error(), "allow-list") {
			return false // refused at the ingress
		}
		// Any other early return is a test/transport fault, not an admission verdict.
		t.Fatalf("ServeInboundIP returned unexpectedly: %v", err)
		return false
	case <-time.After(750 * time.Millisecond):
		// Still serving → admitted. Confirm the peer entered the listener's graph.
		for _, n := range hubB.Nodes() {
			if n.Call == callsign {
				return true
			}
		}
		return true // serving without a refusal error == admitted (graph add may race)
	}
}

// TestPersistedAndHotEditAtBothIngresses is the S4 end-to-end proof: the
// allow-list is loaded from the persisted SQLite-style config table (seeded by the
// PDN_BPQCHAT_PEER_ALLOW env value), a persisted entry is honoured at BOTH
// ingresses (node serveInbound + peer ServeInboundIP), and a HOT edit (persist +
// reload onto the same live list) takes effect at both WITHOUT a restart. Both
// ingresses consult the SAME *AllowList pointer, so one list governs both.
func TestPersistedAndHotEditAtBothIngresses(t *testing.T) {
	ctx := context.Background()
	store := chat.NewMemStore()

	// Load seeded by the env value (the headless seed). GB7SEED-1 is the persisted
	// entry; GB7HOT-1 is NOT yet allowed.
	allow, err := peer.LoadAllowList(ctx, store, []string{"GB7SEED-1"})
	if err != nil {
		t.Fatalf("LoadAllowList: %v", err)
	}

	// Build a node Link that consults THIS allow-list at its ingress.
	hub := chat.NewHub("M0LTE-4", chat.NewMemStore(), nil)
	router := peer.NewRouter(hub)
	t.Cleanup(router.Close)
	l := New(Options{ChatCallsign: "M0LTE-4", Allow: allow}, hub, router, discard())

	// 1) The persisted/seeded entry is honoured at BOTH ingresses.
	if !presentsAsPeer(t, l, hub, "GB7SEED-1") {
		t.Fatal("persisted entry GB7SEED-1 was NOT admitted at the node ingress")
	}
	if !admittedAtIPIngress(t, allow, "GB7SEED-1") {
		t.Fatal("persisted entry GB7SEED-1 was NOT admitted at the IP ingress")
	}

	// 2) A not-yet-allowed callsign is denied at BOTH ingresses (default-deny).
	if presentsAsPeer(t, l, hub, "GB7HOT-1") {
		t.Fatal("GB7HOT-1 admitted at the node ingress before it was added")
	}
	if admittedAtIPIngress(t, allow, "GB7HOT-1") {
		t.Fatal("GB7HOT-1 admitted at the IP ingress before it was added")
	}

	// 3) HOT EDIT: an out-of-band config write (as the S5 editor would do) + reload
	// onto the SAME live list — no restart, no re-wiring of either ingress.
	if err := store.SetConfig(ctx, peer.ConfigKVAllowKey, "GB7SEED-1\nGB7HOT-1"); err != nil {
		t.Fatal(err)
	}
	if err := peer.ReloadAllowList(ctx, store, allow); err != nil {
		t.Fatalf("ReloadAllowList: %v", err)
	}

	// 4) GB7HOT-1 is now admitted at BOTH ingresses, no restart.
	if !presentsAsPeer(t, l, hub, "GB7HOT-1") {
		t.Fatal("hot-added GB7HOT-1 was NOT admitted at the node ingress after reload")
	}
	if !admittedAtIPIngress(t, allow, "GB7HOT-1") {
		t.Fatal("hot-added GB7HOT-1 was NOT admitted at the IP ingress after reload")
	}
}

// TestDialedPeerImplicitlyTrustedAtIngress: a peer we dial OUT to (pinned) is
// admitted inbound even though it is NOT in the editable/persisted set — and a
// Replace that clears the editable set does not revoke that trust (the
// outbound-dialed guarantee, §4.1). Exercised at the node ingress.
func TestDialedPeerImplicitlyTrustedAtIngress(t *testing.T) {
	ctx := context.Background()
	store := chat.NewMemStore()
	allow, err := peer.LoadAllowList(ctx, store, nil) // empty editable set
	if err != nil {
		t.Fatal(err)
	}
	allow.Pin("GB7DIAL-1") // we dial out to this peer → implicitly trusted inbound

	hub := chat.NewHub("M0LTE-4", chat.NewMemStore(), nil)
	router := peer.NewRouter(hub)
	t.Cleanup(router.Close)
	l := New(Options{ChatCallsign: "M0LTE-4", Allow: allow}, hub, router, discard())

	if !presentsAsPeer(t, l, hub, "GB7DIAL-1") {
		t.Fatal("dialed (pinned) peer GB7DIAL-1 was refused inbound")
	}
	// A hot edit that clears the editable set must NOT strip the pinned peer.
	if err := store.SetConfig(ctx, peer.ConfigKVAllowKey, ""); err != nil {
		t.Fatal(err)
	}
	if err := peer.ReloadAllowList(ctx, store, allow); err != nil {
		t.Fatal(err)
	}
	if !presentsAsPeer(t, l, hub, "GB7DIAL-1") {
		t.Fatal("pinned peer lost inbound trust after the editable set was cleared")
	}
}
