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
