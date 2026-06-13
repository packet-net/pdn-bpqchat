package peer

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/m0lte/pdn-bpqchat/internal/chat"
)

// KeepaliveInterval is how often a link sends a keepalive (which doubles as a
// poll, BPQ-style). A link is considered dead if nothing arrives for
// LinkTimeout.
const (
	KeepaliveInterval = 60 * time.Second
	LinkTimeout       = 5 * time.Minute
)

// Link is one node-to-node session over a byte-stream transport (a telnet/IP
// node link in W5; an RHP AX.25 session in W6). It runs the BPQ link handshake,
// bridges records to the Router, and keeps the link alive.
type Link struct {
	peerCall string
	ourNode  string
	version  string
	rw       io.ReadWriteCloser
	router   *Router
	hub      *chat.Hub
	outbound bool
	greeted  bool
	log      func(string, ...any)

	keepaliveEvery time.Duration

	wmu      sync.Mutex
	lastSeen time.Time
}

// Config configures a Link.
type Config struct {
	PeerCall  string               // the peer node's chat callsign
	OurNode   string               // our chat callsign
	Version   string               // our version string (advertised in keepalives)
	Outbound  bool                 // true if we dialled the peer
	Greeted   bool                 // inbound only: the demux already sent the banner and read *RTL
	Log       func(string, ...any) // optional
	Keepalive time.Duration        // optional override (tests use a short value)
}

// NewLink builds a link over rw, bridging it to router/hub.
func NewLink(rw io.ReadWriteCloser, router *Router, hub *chat.Hub, cfg Config) *Link {
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	if cfg.Keepalive <= 0 {
		cfg.Keepalive = KeepaliveInterval
	}
	if cfg.Version == "" {
		cfg.Version = "pdn"
	}
	return &Link{
		peerCall:       strings.ToUpper(cfg.PeerCall),
		ourNode:        strings.ToUpper(cfg.OurNode),
		version:        cfg.Version,
		rw:             rw,
		router:         router,
		hub:            hub,
		outbound:       cfg.Outbound,
		greeted:        cfg.Greeted,
		log:            cfg.Log,
		keepaliveEvery: cfg.Keepalive,
	}
}

func (l *Link) id() string { return l.peerCall }

// sendRaw writes one record line (adding the CR terminator). Serialised so the
// keepalive ticker and the relay never interleave a frame.
func (l *Link) sendRaw(raw string) error {
	l.wmu.Lock()
	defer l.wmu.Unlock()
	_, err := io.WriteString(l.rw, raw+"\r")
	return err
}

// Run performs the handshake, registers with the router, and serves the link
// until ctx is cancelled, the transport closes, or the link times out.
func (l *Link) Run(ctx context.Context) error {
	return l.RunWithReader(ctx, bufio.NewReader(l.rw))
}

// RunWithReader is Run with a caller-supplied reader — used by the inbound demux,
// which has already consumed the banner exchange and must not lose bytes it
// buffered while reading the *RTL line.
func (l *Link) RunWithReader(ctx context.Context, br *bufio.Reader) error {
	if err := l.handshake(br); err != nil {
		return fmt.Errorf("peer %s: handshake: %w", l.peerCall, err)
	}
	return l.serve(ctx, br, nil)
}

// serve runs the post-handshake link lifecycle: register with the router, tell
// the peer our local users, keep the link alive, and relay records until the
// transport closes. pending, if non-nil, is a record already read during the
// handshake (e.g. the identity-bearing first record of an inbound IP link) and
// is processed before the read loop so it is not lost.
func (l *Link) serve(ctx context.Context, br *bufio.Reader, pending *Record) error {
	l.lastSeen = time.Now()
	l.router.Add(l)
	defer l.router.Remove(l.id())
	l.hub.LinkNode(l.peerCall, "", l.version)
	defer l.hub.UnlinkNode(l.peerCall)

	l.stateTell() // bring the peer up to date with our local users

	// Keepalive ticker.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go l.keepaliveLoop(ctx)

	if pending != nil {
		l.handleRecord(*pending)
	}

	// Read loop.
	lines := make(chan string, 64)
	go l.readLines(br, lines, cancel)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case line, ok := <-lines:
			if !ok {
				return io.EOF
			}
			l.handle(line)
		}
	}
}

// ServeInboundIP serves an inbound pdn↔pdn IP peer link over rw (the documented
// telnet/IP node-link transport, design.md §9 — the dial side is DialAndServe).
// A raw TCP accept carries no AX.25 callsign the way an RHP inbound does, so we
// learn the peer's chat callsign from the first control record it sends: the
// dialer emits its keepalive K right after *RTL (before it waits for our OK), and
// that record carries the originating node in field 0 (proto.go). ourNode is our
// own chat callsign.
func ServeInboundIP(ctx context.Context, rw io.ReadWriteCloser, router *Router, hub *chat.Hub, ourNode string, logf func(string, ...any)) error {
	br := bufio.NewReader(rw)
	if _, err := io.WriteString(rw, banner()+"\r"); err != nil {
		return err
	}
	if err := readUntilLine(br, IsRTL); err != nil {
		return err
	}
	first, err := readFirstRecord(br)
	if err != nil {
		return err
	}
	if first.Node == "" {
		return fmt.Errorf("inbound IP peer: first record carried no node callsign")
	}
	l := NewLink(rw, router, hub, Config{
		PeerCall: first.Node, OurNode: ourNode, Outbound: false, Greeted: true, Log: logf,
	})
	// Now that we know who they are, complete the handshake: OK + our keepalive.
	if err := l.sendRaw("OK"); err != nil {
		return err
	}
	if err := l.sendKeepalive(); err != nil {
		return err
	}
	l.log("inbound IP peer linked: %s", first.Node)
	return l.serve(ctx, br, &first)
}

// readUntilLine reads lines until pred matches one (package-level twin of the
// Link.readUntil method, for use before a Link exists).
func readUntilLine(br *bufio.Reader, pred func(string) bool) error {
	for {
		line, err := readLine(br)
		if err != nil {
			return err
		}
		if pred(line) {
			return nil
		}
	}
}

// readFirstRecord reads lines until one decodes as a control record.
func readFirstRecord(br *bufio.Reader) (Record, error) {
	for {
		line, err := readLine(br)
		if err != nil {
			return Record{}, err
		}
		if rec, ok := Decode(line); ok {
			return rec, nil
		}
	}
}

func (l *Link) handshake(br *bufio.Reader) error {
	if l.outbound {
		// We dialled: wait for the banner, send *RTL + a keepalive, expect OK.
		if err := l.readUntil(br, isBanner); err != nil {
			return err
		}
		if err := l.sendRaw(rtlLogin); err != nil {
			return err
		}
		if err := l.sendKeepalive(); err != nil {
			return err
		}
		return l.readUntil(br, func(s string) bool { return strings.HasPrefix(strings.ToUpper(s), "OK") })
	}
	// Inbound: send the banner and wait for *RTL — unless the demux already did
	// that (Greeted), in which case we just reply OK + a keepalive and serve.
	if !l.greeted {
		if err := l.sendRaw(banner()); err != nil {
			return err
		}
		if err := l.readUntil(br, func(s string) bool { return IsRTL(s) }); err != nil {
			return err
		}
	}
	if err := l.sendRaw("OK"); err != nil {
		return err
	}
	return l.sendKeepalive()
}

// readUntil reads lines until pred matches one (handshake lines are short and
// plain; control records that arrive early are ignored until the gate passes).
func (l *Link) readUntil(br *bufio.Reader, pred func(string) bool) error {
	for {
		line, err := readLine(br)
		if err != nil {
			return err
		}
		if pred(line) {
			return nil
		}
	}
}

func (l *Link) readLines(br *bufio.Reader, out chan<- string, cancel context.CancelFunc) {
	defer close(out)
	defer cancel()
	for {
		line, err := readLine(br)
		if err != nil {
			return
		}
		out <- line
	}
}

// handle processes one line received after the handshake.
func (l *Link) handle(line string) {
	if !IsControl(line) {
		return // stray text / node chatter — ignore
	}
	rec, ok := Decode(line)
	if !ok {
		return
	}
	l.handleRecord(rec)
}

// handleRecord processes one already-decoded control record.
func (l *Link) handleRecord(rec Record) {
	l.lastSeen = time.Now()
	switch rec.Type {
	case IDKeepalive, IDPoll:
		// Both elicit a poll response; refresh liveness.
		_ = l.sendRaw(encodePollResp(l.ourNode, l.peerCall))
	case IDPollResp:
		// liveness already refreshed above.
	default:
		l.router.Ingest(rec, l.id())
	}
}

func (l *Link) keepaliveLoop(ctx context.Context) {
	t := time.NewTicker(l.keepaliveEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if time.Since(l.lastSeen) > LinkTimeout {
				_ = l.rw.Close() // dead peer — drop the link
				return
			}
			if err := l.sendKeepalive(); err != nil {
				return
			}
		}
	}
}

func (l *Link) sendKeepalive() error {
	return l.sendRaw(encodeKeepalive(l.ourNode, l.peerCall, l.version))
}

// stateTell sends the peer our local users (joins) on link-up — the bounded
// resync of design.md §4.5 (we send our own users, not a full graph dump).
func (l *Link) stateTell() {
	for _, u := range l.hub.Users() {
		if u.Origin.Node != l.ourNode {
			continue // only our local users
		}
		_ = l.sendRaw(encodeJoin(l.ourNode, u.Call, u.Name, u.QTH))
		if !strings.EqualFold(u.Topic, chat.DefaultTopic) {
			_ = l.sendRaw(encodeTopic(l.ourNode, u.Call, u.Topic))
		}
	}
}

// readLine reads one CR- or LF-terminated line (terminator stripped). It must
// NOT peek past the terminator to collapse a CRLF pair: a peek blocks until the
// next byte arrives, which can be a keepalive interval away, stranding the line
// just read. A CRLF therefore yields the real line plus one empty line — and an
// empty line is harmless (handle and readUntil both ignore non-matching lines).
// Bounded so a peer can't make us buffer without limit.
func readLine(br *bufio.Reader) (string, error) {
	const maxLine = 4096
	var b strings.Builder
	for {
		c, err := br.ReadByte()
		if err != nil {
			return "", err
		}
		if c == '\r' || c == '\n' {
			return b.String(), nil
		}
		if b.Len() < maxLine {
			b.WriteByte(c)
		}
	}
}
