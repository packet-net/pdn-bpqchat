// Package node wires the RHP client to the chat hub: it owns the resilient RHP
// attachment, binds the chat callsign and listens, and turns each inbound RHP
// child (an AX.25 user) into a chat session. It is the adapter between the
// transport (internal/rhp) and the host-free domain (internal/chat,
// internal/session).
package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/m0lte/pdn-bpqchat/internal/chat"
	"github.com/m0lte/pdn-bpqchat/internal/rhp"
	"github.com/m0lte/pdn-bpqchat/internal/session"
)

var errLinkDown = errors.New("node: RHP link is down")

func sprintf(format string, a ...any) string { return fmt.Sprintf(format, a...) }

// Options configures the RHP attachment.
type Options struct {
	Host         string
	Port         int
	User         string
	Pass         string
	ChatCallsign string
}

// Link is the resilient RHP attachment that serves inbound RF chat users.
type Link struct {
	opts Options
	hub  *chat.Hub
	log  *slog.Logger

	mu       sync.Mutex
	client   *rhp.Client
	sessions map[int]*session.Session // child handle → session
}

// New builds the link.
func New(opts Options, hub *chat.Hub, log *slog.Logger) *Link {
	return &Link{opts: opts, hub: hub, log: log, sessions: map[int]*session.Session{}}
}

// Run keeps the attachment up until ctx is cancelled, reconnecting with
// exponential backoff.
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

func (l *Link) attachOnce(ctx context.Context) error {
	connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	client, err := rhp.Connect(connectCtx, l.opts.Host, l.opts.Port, l)
	cancel()
	if err != nil {
		return err
	}
	defer client.Close()

	l.mu.Lock()
	l.client = client
	l.mu.Unlock()
	defer l.dropAllSessions()

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

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-client.Done():
		return client.Err()
	}
}

// --- rhp.Handler ---

func (l *Link) OnAccept(_, child int, remote, _, _ string) {
	l.log.Info("inbound chat user", "from", remote, "handle", child)
	conn := &rhpConn{link: l, handle: child}
	s, err := session.New(l.hub, conn, remote, func(f string, a ...any) { l.log.Debug("session", "msg", sprintf(f, a...)) })
	if err != nil {
		l.log.Warn("could not start session", "from", remote, "err", err)
		l.closeChild(child)
		return
	}
	l.mu.Lock()
	l.sessions[child] = s
	l.mu.Unlock()

	// When the session ends by /B or /QUIT, drop the RHP child.
	go func() {
		<-s.Ended()
		l.closeChild(child)
		l.mu.Lock()
		delete(l.sessions, child)
		l.mu.Unlock()
	}()
}

func (l *Link) OnRecv(handle int, data []byte) {
	l.mu.Lock()
	s := l.sessions[handle]
	l.mu.Unlock()
	if s != nil {
		s.Deliver(data)
	}
}

func (l *Link) OnStatus(int, int) {}

func (l *Link) OnClose(handle int) {
	l.mu.Lock()
	s := l.sessions[handle]
	delete(l.sessions, handle)
	l.mu.Unlock()
	if s != nil {
		s.Close()
	}
}

func (l *Link) closeChild(handle int) {
	l.mu.Lock()
	client := l.client
	l.mu.Unlock()
	if client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = client.CloseHandle(ctx, handle)
		cancel()
	}
}

func (l *Link) dropAllSessions() {
	l.mu.Lock()
	sessions := l.sessions
	l.sessions = map[int]*session.Session{}
	l.client = nil
	l.mu.Unlock()
	for _, s := range sessions {
		s.Close()
	}
}

// rhpConn adapts an RHP child handle to session.Conn.
type rhpConn struct {
	link   *Link
	handle int
}

func (c *rhpConn) Send(data []byte) error {
	c.link.mu.Lock()
	client := c.link.client
	c.link.mu.Unlock()
	if client == nil {
		return errLinkDown
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return client.Send(ctx, c.handle, data)
}

func (c *rhpConn) Close() error {
	c.link.closeChild(c.handle)
	return nil
}
