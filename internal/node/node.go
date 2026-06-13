// Package node wires the RHP client to the chat hub: it owns the resilient RHP
// attachment, binds the chat callsign and listens, and demultiplexes each
// inbound AX.25 child into either a chat user session or an inbound peer link
// (the caller is a peer iff its first line after the banner is *RTL). It also
// dials configured RF peers (peer chat callsigns) over AX.25 via RHP. It is the
// adapter between the transport (internal/rhp) and the host-free domain.
package node

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/m0lte/pdn-bpqchat/internal/chat"
	"github.com/m0lte/pdn-bpqchat/internal/config"
	"github.com/m0lte/pdn-bpqchat/internal/peer"
	"github.com/m0lte/pdn-bpqchat/internal/rhp"
	"github.com/m0lte/pdn-bpqchat/internal/session"
)

func sprintf(format string, a ...any) string { return fmt.Sprintf(format, a...) }

// Options configures the RHP attachment.
type Options struct {
	Host         string
	Port         int
	User         string
	Pass         string
	ChatCallsign string
	RFPeers      []config.RFPeer // peer chat nodes to dial over AX.25 via RHP (optionally via a connect script)
}

// Link is the resilient RHP attachment that serves inbound RF users and peers
// and dials outbound RF peers.
type Link struct {
	opts   Options
	hub    *chat.Hub
	router *peer.Router
	log    *slog.Logger

	mu      sync.Mutex
	client  *rhp.Client
	ctx     context.Context // the current attachment's context (for spawned children)
	streams map[int]*rhpStream
	// pendingRecv / pendingClosed buffer pushes that arrive for a handle BEFORE its
	// stream is registered. An outbound open's first recv can race ahead of stream
	// registration: pdn replies to `open` only after the connect resolves (deviation
	// D4) and a node sends its prompt/banner immediately on connect, so the recv can
	// be dispatched (same read-loop goroutine) before dialRFPeerOnce registers the
	// stream. Without this buffer that first frame — e.g. the node prompt a connect
	// script waits for — is lost.
	pendingRecv   map[int][][]byte
	pendingClosed map[int]bool
}

// New builds the link.
func New(opts Options, hub *chat.Hub, router *peer.Router, log *slog.Logger) *Link {
	return &Link{
		opts: opts, hub: hub, router: router, log: log,
		streams:       map[int]*rhpStream{},
		pendingRecv:   map[int][][]byte{},
		pendingClosed: map[int]bool{},
	}
}

// registerStream binds a freshly-opened/accepted handle to its stream, draining
// any pushes that raced ahead of registration (see Link.pendingRecv).
func (l *Link) registerStream(handle int, s *rhpStream) {
	l.mu.Lock()
	l.streams[handle] = s
	pending := l.pendingRecv[handle]
	delete(l.pendingRecv, handle)
	closed := l.pendingClosed[handle]
	delete(l.pendingClosed, handle)
	l.mu.Unlock()
	for _, d := range pending {
		s.feed(d)
	}
	if closed {
		s.markClosed()
	}
}

// Run keeps the attachment up until ctx is cancelled, reconnecting with backoff.
func (l *Link) Run(ctx context.Context) {
	const initial, max = 1 * time.Second, 60 * time.Second
	backoff := initial
	for ctx.Err() == nil {
		if err := l.attachOnce(ctx); err != nil && ctx.Err() == nil {
			l.log.Warn("RHP link down; reconnecting", "err", err, "in", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > max {
			backoff = max
		}
	}
}

func (l *Link) attachOnce(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	connectCtx, cc := context.WithTimeout(ctx, 15*time.Second)
	client, err := rhp.Connect(connectCtx, l.opts.Host, l.opts.Port, l)
	cc()
	if err != nil {
		return err
	}
	defer client.Close()

	l.mu.Lock()
	l.client = client
	l.ctx = ctx
	l.streams = map[int]*rhpStream{}
	l.mu.Unlock()

	if l.opts.User != "" {
		if err := client.Authenticate(ctx, l.opts.User, l.opts.Pass); err != nil {
			return err
		}
	}
	handle, err := client.Socket(ctx)
	if err != nil {
		return err
	}
	if err := client.Bind(ctx, handle, l.opts.ChatCallsign, ""); err != nil {
		return err
	}
	if err := client.Listen(ctx, handle); err != nil {
		return err
	}
	l.log.Info("chat node bound and listening", "callsign", l.opts.ChatCallsign)

	// Dial RF peers over AX.25 while this attachment is up.
	for _, p := range l.opts.RFPeers {
		go l.dialRFPeer(ctx, client, p)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-client.Done():
		return client.Err()
	}
}

// --- rhp.Handler ---

func (l *Link) OnAccept(_, child int, remote, _, _ string) {
	l.log.Info("inbound connection", "from", remote, "handle", child)
	l.mu.Lock()
	client, ctx := l.client, l.ctx
	l.mu.Unlock()
	stream := newRhpStream(client, child)
	l.registerStream(child, stream)
	if ctx == nil {
		return
	}
	go l.serveInbound(ctx, stream, remote)
}

// serveInbound demultiplexes one inbound connection (io.ReadWriteCloser so it is
// testable without a live RHP client).

func (l *Link) OnRecv(handle int, data []byte) {
	l.mu.Lock()
	s := l.streams[handle]
	if s == nil {
		// Raced ahead of stream registration — buffer it (see Link.pendingRecv).
		l.pendingRecv[handle] = append(l.pendingRecv[handle], append([]byte(nil), data...))
		l.mu.Unlock()
		return
	}
	l.mu.Unlock()
	s.feed(data)
}

func (l *Link) OnStatus(int, int) {}

func (l *Link) OnClose(handle int) {
	l.mu.Lock()
	s := l.streams[handle]
	delete(l.streams, handle)
	if s == nil {
		l.pendingClosed[handle] = true // close raced ahead of registration
	}
	l.mu.Unlock()
	if s != nil {
		s.markClosed()
	}
}

func (l *Link) serveInbound(ctx context.Context, rw io.ReadWriteCloser, remote string) {
	defer rw.Close()
	br := bufio.NewReader(rw)

	// Greet, then read the first line to tell a peer from a user.
	if _, err := rw.Write([]byte(peer.Banner() + "\r")); err != nil {
		return
	}
	first, err := readChildLine(br)
	if err != nil {
		return
	}

	if peer.IsRTL(first) {
		l.log.Info("inbound peer link", "peer", remote)
		link := peer.NewLink(rw, l.router, l.hub, peer.Config{
			PeerCall: remote, OurNode: l.opts.ChatCallsign, Outbound: false, Greeted: true,
			Log: l.slogf(),
		})
		_ = link.RunWithReader(ctx, br)
		return
	}

	// A user. Start a session and feed it the first line, then pump the rest.
	s, err := session.New(l.hub, &streamConn{rw}, remote, l.slogf())
	if err != nil {
		l.log.Warn("could not start session", "from", remote, "err", err)
		return
	}
	go func() { <-s.Ended(); _ = rw.Close() }()

	s.Deliver([]byte(first + "\r"))
	for {
		line, err := readChildLine(br)
		if err != nil {
			s.Close()
			return
		}
		s.Deliver([]byte(line + "\r"))
	}
}

// dialRFPeer maintains an outbound AX.25 peer link via RHP open, with backoff,
// while ctx is live.
func (l *Link) dialRFPeer(ctx context.Context, client *rhp.Client, p config.RFPeer) {
	const initial, max = 5 * time.Second, 120 * time.Second
	backoff := initial
	for ctx.Err() == nil {
		if err := l.dialRFPeerOnce(ctx, client, p); err != nil && ctx.Err() == nil {
			l.log.Info("RF peer link ended", "peer", p.PeerCall, "err", err, "retry", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > max {
			backoff = max
		}
	}
}

func (l *Link) dialRFPeerOnce(ctx context.Context, client *rhp.Client, p config.RFPeer) error {
	// Open to the first hop: the peer itself for a direct dial, or the node we
	// walk a connect script through (p.OpenTo) for a multi-hop peer.
	handle, err := client.Open(ctx, l.opts.ChatCallsign, p.OpenTo, p.OpenPort)
	if err != nil {
		return err
	}
	stream := newRhpStream(client, handle)
	l.registerStream(handle, stream)
	defer func() {
		l.mu.Lock()
		delete(l.streams, handle)
		l.mu.Unlock()
		_ = stream.Close()
	}()

	link := peer.NewLink(stream, l.router, l.hub, peer.Config{
		PeerCall: p.PeerCall, OurNode: l.opts.ChatCallsign, Outbound: true, Log: l.slogf(),
	})
	if len(p.Script) > 0 {
		l.log.Info("dialled RF node; walking connect script to peer", "open", p.OpenTo, "peer", p.PeerCall, "steps", len(p.Script))
		return link.RunWithScript(ctx, p.Script)
	}
	l.log.Info("dialled RF peer; linking", "peer", p.PeerCall)
	return link.Run(ctx)
}

func (l *Link) slogf() func(string, ...any) {
	return func(f string, a ...any) { l.log.Debug("node", "msg", sprintf(f, a...)) }
}

// streamConn adapts an io.ReadWriteCloser to session.Conn.
type streamConn struct{ rw io.ReadWriteCloser }

func (c *streamConn) Send(data []byte) error {
	_, err := c.rw.Write(data)
	return err
}
func (c *streamConn) Close() error { return c.rw.Close() }

// readChildLine reads one CR/LF-terminated line from a child stream (terminator
// stripped), bounded.
func readChildLine(br *bufio.Reader) (string, error) {
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
