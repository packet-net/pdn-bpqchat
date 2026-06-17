package peer

import (
	"context"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/m0lte/pdn-bpqchat/internal/chat"
)

// readLineConn reads one CR/LF-terminated line from a net.Conn one byte at a time
// (no buffering past the terminator), so a subsequent reader on the same conn
// loses nothing.
func readLineConn(c net.Conn) (string, error) {
	var b []byte
	buf := make([]byte, 1)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			if buf[0] == '\r' || buf[0] == '\n' {
				return string(b), nil
			}
			b = append(b, buf[0])
		}
		if err != nil {
			return string(b), err
		}
	}
}

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

func TestBannerDetectionCaseInsensitive(t *testing.T) {
	// Real LinBPQ sends mixed-case "[BPQChatServer-6.0.25.28]" (verified live
	// against m0lte/linbpq); we must detect it as well as the uppercase form.
	for _, s := range []string{
		"[BPQChatServer-6.0.25.28]",
		"[BPQCHATSERVER-pdn]",
		"[bpqchatserver-x]",
	} {
		if !isBanner(s) {
			t.Fatalf("isBanner(%q) = false, want true", s)
		}
	}
	if isBanner("not a banner") {
		t.Fatal("isBanner matched a non-banner line")
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

// TestInboundIPLink exercises the accept side of the pdn↔pdn IP transport: a
// dialer links to a ServeInboundIP listener that has NO callsign from the
// transport and must learn the peer's identity from the first control record.
func TestInboundIPLink(t *testing.T) {
	aConn, bConn := tcpPair(t)

	a := newTestNode("GB7AAA") // the dialer
	b := newTestNode("GB7BBB") // the listener
	defer a.router.Close()
	defer b.router.Close()

	// A local user on the dialer, carried to the listener by stateTell on link-up.
	akey := chat.UserKey{Call: "G8PZT", Node: "GB7AAA"}
	a.hub.Join(chat.User{Call: akey.Call, Origin: chat.Origin{Node: "GB7AAA", Local: true}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	la := NewLink(aConn, a.router, a.hub, Config{PeerCall: "GB7BBB", OurNode: "GB7AAA", Outbound: true, Keepalive: time.Hour})
	go la.Run(ctx)
	// The listener learns "GB7AAA" from the dialer's first record — not from any
	// transport-supplied callsign. The dialer (GB7AAA) is allow-listed so the link
	// is admitted (allow-list reject paths are covered by TestInboundIPLinkRejected).
	go func() { _ = ServeInboundIP(ctx, bConn, b.router, b.hub, "GB7BBB", NewAllowList("GB7AAA"), nil) }()

	// The listener must have learned the peer and received A's user.
	waitFor(t, func() bool { _, ok := b.hub.User(akey); return ok })

	a.hub.Post(context.Background(), akey, "inbound ip works")
	waitFor(t, func() bool {
		msgs, _ := b.hub.History(context.Background(), "General", time.Time{}, 10)
		for _, m := range msgs {
			if strings.Contains(m.Text, "inbound ip works") {
				return true
			}
		}
		return false
	})
}

// TestInboundIPLinkRejected: an inbound IP peer whose learned callsign is NOT on
// the allow-list is refused at the ingress — ServeInboundIP returns an error, the
// peer never enters the listener's node graph or hub, and the reject is counted
// (design.md §4.1, default-deny). It is the IP-transport twin of
// node.TestDemuxPeerLinkRejectedDefaultDeny.
func TestInboundIPLinkRejected(t *testing.T) {
	aConn, bConn := tcpPair(t)

	a := newTestNode("GB7AAA") // the dialer (NOT allow-listed at the listener)
	b := newTestNode("GB7BBB") // the listener
	defer a.router.Close()
	defer b.router.Close()

	akey := chat.UserKey{Call: "G8PZT", Node: "GB7AAA"}
	a.hub.Join(chat.User{Call: akey.Call, Origin: chat.Origin{Node: "GB7AAA", Local: true}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	la := NewLink(aConn, a.router, a.hub, Config{PeerCall: "GB7BBB", OurNode: "GB7AAA", Outbound: true, Keepalive: time.Hour})
	go la.Run(ctx)

	// Allow-list permits only GB7CCC, so the dialing GB7AAA is refused.
	allow := NewAllowList("GB7CCC")
	errc := make(chan error, 1)
	go func() { errc <- ServeInboundIP(ctx, bConn, b.router, b.hub, "GB7BBB", allow, nil) }()

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("ServeInboundIP admitted a non-allow-listed peer (want refusal error)")
		}
		if !strings.Contains(err.Error(), "allow-list") {
			t.Fatalf("refusal error = %v, want it to mention the allow-list", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeInboundIP did not refuse the non-allow-listed peer in time")
	}

	// No state must have been mutated: the dialer's user must NOT appear, and the
	// reject must be observable.
	if _, ok := b.hub.User(akey); ok {
		t.Fatal("refused peer's user reached the hub — state mutated on a rejected link")
	}
	for _, n := range b.hub.Nodes() {
		if n.Call == "GB7AAA" {
			t.Fatal("refused peer entered the listener's node graph")
		}
	}
	if got := allow.Rejected(); got != 1 {
		t.Fatalf("rejected count = %d, want 1", got)
	}
}

// TestConnectScriptDial proves an outbound connect script: the dialer opens to a
// "node prompt" (the server), sends "C GB7BBB-4" to be connected through to the
// chat app, and the BPQ node-link handshake then completes over that session —
// exactly the PDN ≥0.9.0 node-prompt-to-local-app path.
func TestConnectScriptDial(t *testing.T) {
	aConn, bConn := tcpPair(t)

	a := newTestNode("GB7AAA") // dialer
	b := newTestNode("GB7BBB") // the node + its chat app
	defer a.router.Close()
	defer b.router.Close()

	akey := chat.UserKey{Call: "G8PZT", Node: "GB7AAA"}
	a.hub.Join(chat.User{Call: akey.Call, Origin: chat.Origin{Node: "GB7AAA", Local: true}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Server side: act as the node prompt — emit a welcome + the "GB7BBB>" prompt
	// (no line terminator, as a real node prompt has none), wait for the dialer's
	// "C GB7BBB-4" connect command (expect must have matched the prompt first),
	// emit a little chatter, then become the chat node (ServeInboundIP). The
	// handshake's readUntil(banner) must skip the chatter.
	go func() {
		_, _ = io.WriteString(bConn, "Welcome to GB7BBB node\rGB7BBB> ")
		line, err := readLineConn(bConn)
		if err != nil || !strings.EqualFold(strings.TrimSpace(line), "C GB7BBB-4") {
			t.Errorf("server got connect cmd %q, want \"C GB7BBB-4\"", line)
			return
		}
		_, _ = io.WriteString(bConn, "Connected to GB7BBB-4\r")
		_ = ServeInboundIP(ctx, bConn, b.router, b.hub, "GB7BBB-4", NewAllowList("GB7AAA"), nil)
	}()

	// Dialer: PeerCall is the chat callsign GB7BBB-4; the script expects the node
	// prompt, then sends the connect that reaches the chat app.
	la := NewLink(aConn, a.router, a.hub, Config{PeerCall: "GB7BBB-4", OurNode: "GB7AAA", Outbound: true, Keepalive: time.Hour, ExpectTimeout: 3 * time.Second})
	go func() { _ = la.RunWithScript(ctx, []ScriptStep{{Expect: "GB7BBB>", Send: "C GB7BBB-4"}}) }()

	waitFor(t, func() bool { _, ok := b.hub.User(akey); return ok })
	a.hub.Post(context.Background(), akey, "reached via script")
	waitFor(t, func() bool {
		msgs, _ := b.hub.History(context.Background(), "General", time.Time{}, 10)
		for _, m := range msgs {
			if strings.Contains(m.Text, "reached via script") {
				return true
			}
		}
		return false
	})
}

// TestJoinAfterDataSetsName proves a join record's name is applied even when a
// data record already made the user present (the name would otherwise be lost
// because ensureUser early-returns for an existing user).
func TestJoinAfterDataSetsName(t *testing.T) {
	n := newTestNode("GB7AAA")
	defer n.router.Close()
	key := chat.UserKey{Call: "G8PZT", Node: "GB7BBB"}

	// A data record from a remote node arrives first: the user is created with no name.
	dRec, _ := Decode(encodeData("GB7BBB", "G8PZT", "hello mesh"))
	n.router.Ingest(dRec, "GB7BBB")
	if u, ok := n.hub.User(key); !ok || u.Name != "" {
		t.Fatalf("after data: present=%v name=%q (want present, empty name)", ok, u.Name)
	}

	// Then a join carrying the name — the name must now land.
	jRec, _ := Decode(encodeJoin("GB7BBB", "G8PZT", "John Doe", "London"))
	n.router.Ingest(jRec, "GB7BBB")
	waitFor(t, func() bool {
		u, ok := n.hub.User(key)
		return ok && u.Name == "John Doe" && u.QTH == "London"
	})
}
