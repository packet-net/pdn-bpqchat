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

func newLink(t *testing.T) (*Link, *chat.Hub) {
	t.Helper()
	hub := chat.NewHub("M0LTE-4", chat.NewMemStore(), nil)
	router := peer.NewRouter(hub)
	t.Cleanup(router.Close)
	l := New(Options{ChatCallsign: "M0LTE-4"}, hub, router, discard())
	return l, hub
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

// TestDemuxPeerLink: a caller whose first line is *RTL becomes a peer link.
func TestDemuxPeerLink(t *testing.T) {
	l, hub := newLink(t)
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
