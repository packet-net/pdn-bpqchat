package peer

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/m0lte/pdn-bpqchat/internal/chat"
)

func TestCodecRoundTrip(t *testing.T) {
	raw := encodeData("GB7AAA", "G8PZT", "hello there world")
	rec, ok := Decode(raw)
	if !ok {
		t.Fatal("decode failed")
	}
	if rec.Type != IDData || rec.Node != "GB7AAA" || rec.User != "G8PZT" {
		t.Fatalf("decoded %+v", rec)
	}
	if rec.Tail(0) != "hello there world" {
		t.Fatalf("text = %q", rec.Tail(0))
	}
}

func TestDecodeRejectsNonControlAndCorrupt(t *testing.T) {
	if _, ok := Decode("just user text"); ok {
		t.Fatal("plain text decoded as control")
	}
	if _, ok := Decode(string([]byte{FORMAT, IDData}) + "GB7AAA G8PZT te\x02xt"); ok {
		t.Fatal("corrupt record (control byte) accepted")
	}
}

// --- cycle-no-storm: the W5 acceptance gate (design.md §5, §6) ---

type testNode struct {
	hub    *chat.Hub
	router *Router
}

func newTestNode(call string) *testNode {
	h := chat.NewHub(call, chat.NewMemStore(), nil)
	return &testNode{hub: h, router: NewRouter(h)}
}

// memSink delivers a forwarded raw record straight into a peer node's router,
// tagged with the sending node's callsign as the ingress — an in-memory stand-in
// for a Link, so the relay/loop-control logic is tested without transport timing.
type memSink struct {
	peerID string // the remote node's callsign (this sink's id in the local router)
	from   string // the local node's callsign (ingress tag at the remote)
	to     *testNode
	sends  *int64
}

func (s *memSink) id() string { return s.peerID }
func (s *memSink) sendRaw(raw string) error {
	atomic.AddInt64(s.sends, 1)
	if rec, ok := Decode(raw); ok {
		s.to.router.Ingest(rec, s.from)
	}
	return nil
}

// connect wires a bidirectional edge between two nodes.
func connect(a, b *testNode, sends *int64) {
	a.router.Add(&memSink{peerID: b.hub.OurNode(), from: a.hub.OurNode(), to: b, sends: sends})
	b.router.Add(&memSink{peerID: a.hub.OurNode(), from: b.hub.OurNode(), to: a, sends: sends})
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
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

func historyLen(t *testing.T, n *testNode, topic string) int {
	t.Helper()
	msgs, err := n.hub.History(context.Background(), topic, time.Time{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	return len(msgs)
}

// TestCycleNoStorm forms a 3-node cycle (a triangle), injects one message at one
// node, and asserts every node delivers it exactly once and the total wire
// traffic is bounded — proving a cycle cannot loop-storm (design.md §5).
func TestCycleNoStorm(t *testing.T) {
	var sends int64
	a := newTestNode("GB7AAA")
	b := newTestNode("GB7BBB")
	c := newTestNode("GB7CCC")
	defer a.router.Close()
	defer b.router.Close()
	defer c.router.Close()
	connect(a, b, &sends)
	connect(b, c, &sends)
	connect(c, a, &sends)

	// A local user on A.
	key := chat.UserKey{Call: "G8PZT", Node: "GB7AAA"}
	if _, err := a.hub.Join(chat.User{Call: key.Call, Origin: chat.Origin{Node: "GB7AAA", Local: true}}); err != nil {
		t.Fatal(err)
	}
	// Wait for the join to propagate (the user should appear on B and C).
	waitFor(t, func() bool {
		_, okB := b.hub.User(key)
		_, okC := c.hub.User(key)
		return okB && okC
	})

	atomic.StoreInt64(&sends, 0)
	if _, err := a.hub.Post(context.Background(), key, "ping the mesh"); err != nil {
		t.Fatal(err)
	}

	// Every node must end up with exactly one copy of the message.
	waitFor(t, func() bool {
		return historyLen(t, a, "General") == 1 &&
			historyLen(t, b, "General") == 1 &&
			historyLen(t, c, "General") == 1
	})
	// Give any storm a chance to manifest, then assert it didn't.
	time.Sleep(100 * time.Millisecond)
	for _, n := range []*testNode{a, b, c} {
		if got := historyLen(t, n, "General"); got != 1 {
			t.Fatalf("node %s has %d copies of the message — storm/dup", n.hub.OurNode(), got)
		}
	}
	// One message across a 3-cycle should cost a handful of sends, not unbounded.
	if total := atomic.LoadInt64(&sends); total > 12 {
		t.Fatalf("message caused %d wire sends across a 3-cycle — looks like a storm", total)
	}
}

// TestMeshTwoPathDedup proves the content-hash backstop: a node reachable by two
// paths from the origin still delivers a message exactly once.
func TestMeshTwoPathDedup(t *testing.T) {
	var sends int64
	a := newTestNode("GB7AAA")
	b := newTestNode("GB7BBB")
	c := newTestNode("GB7CCC")
	d := newTestNode("GB7DDD")
	for _, n := range []*testNode{a, b, c, d} {
		defer n.router.Close()
	}
	// Diamond: A-B, A-C, B-D, C-D. D is reachable from A by two paths.
	connect(a, b, &sends)
	connect(a, c, &sends)
	connect(b, d, &sends)
	connect(c, d, &sends)

	key := chat.UserKey{Call: "M0LTE", Node: "GB7AAA"}
	a.hub.Join(chat.User{Call: key.Call, Origin: chat.Origin{Node: "GB7AAA", Local: true}})
	waitFor(t, func() bool { _, ok := d.hub.User(key); return ok })

	a.hub.Post(context.Background(), key, "diamond message")
	waitFor(t, func() bool { return historyLen(t, d, "General") == 1 })
	time.Sleep(100 * time.Millisecond)
	if got := historyLen(t, d, "General"); got != 1 {
		t.Fatalf("D delivered %d copies via two paths — dedup failed", got)
	}
}

// --- a real two-node link over TCP (handshake + propagation) ---

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

func TestLinkHandshakeAndPropagation(t *testing.T) {
	aConn, bConn := tcpPair(t)

	a := newTestNode("GB7AAA")
	b := newTestNode("GB7BBB")
	defer a.router.Close()
	defer b.router.Close()

	// A user already on A, so stateTell carries them to B on link-up.
	akey := chat.UserKey{Call: "G8PZT", Node: "GB7AAA"}
	a.hub.Join(chat.User{Call: akey.Call, Origin: chat.Origin{Node: "GB7AAA", Local: true}})

	la := NewLink(aConn, a.router, a.hub, Config{PeerCall: "GB7BBB", OurNode: "GB7AAA", Outbound: true, Keepalive: time.Hour})
	lb := NewLink(bConn, b.router, b.hub, Config{PeerCall: "GB7AAA", OurNode: "GB7BBB", Outbound: false, Keepalive: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go la.Run(ctx)
	go lb.Run(ctx)

	// After the handshake + stateTell, B should know A's user.
	waitFor(t, func() bool { _, ok := b.hub.User(akey); return ok })

	// A message posted on A must reach B's log.
	a.hub.Post(context.Background(), akey, "over the link")
	waitFor(t, func() bool {
		msgs, _ := b.hub.History(context.Background(), "General", time.Time{}, 10)
		for _, m := range msgs {
			if strings.Contains(m.Text, "over the link") {
				return true
			}
		}
		return false
	})
}
