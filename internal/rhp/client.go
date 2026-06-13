// Package rhp is a minimal Go client for the pdn Radio Host Protocol v2
// (RHPv2 / PWP-0222), the JSON-over-TCP host API a pdn app uses to open and
// accept AX.25 connections through the node's packet engine. It implements the
// subset pdn-bpqchat needs — auth, hello, the BSD socket/bind/listen/accept
// listener path (inbound RF users), and active open/send/close (outbound peer
// dials) — exactly the surface the wire spec in docs/rhp2-server.md pins.
//
// It is the W0 deliverable: a working, host-free RHPv2 client, the Go analogue
// of pdn-bbs/pdn-convers's .NET RhpClient. Higher layers (the chat domain, the
// RF user interface, peering) build on it in later waves.
package rhp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Handler receives the server-initiated pushes the client cannot correlate to
// a request: a new inbound connection on a listener (accept), data (recv), a
// link-down close, and connection-status changes. All callbacks run on the
// client's single read goroutine, so they must not block on another RHP call —
// hand work to a channel and return.
type Handler interface {
	// OnAccept fires when an inbound connection arrives on a listening socket.
	// child is the new per-connection handle; remote is the caller's callsign.
	OnAccept(listener, child int, remote, local, port string)
	// OnRecv delivers data that arrived on a connected socket.
	OnRecv(handle int, data []byte)
	// OnStatus reports a connection-status change (StatusFlags bits).
	OnStatus(handle, flags int)
	// OnClose fires when the peer (or node) closed a socket.
	OnClose(handle int)
}

// nopHandler ignores every push — a safe default for a client that only dials
// out and never listens.
type nopHandler struct{}

func (nopHandler) OnAccept(int, int, string, string, string) {}
func (nopHandler) OnRecv(int, []byte)                        {}
func (nopHandler) OnStatus(int, int)                         {}
func (nopHandler) OnClose(int)                               {}

// Client is one RHPv2 attachment to the node over a single TCP connection.
// It is safe for concurrent use by multiple goroutines.
type Client struct {
	conn    net.Conn
	w       *bufio.Writer
	handler Handler

	writeMu sync.Mutex // serialises frame writes

	mu      sync.Mutex
	nextID  int
	pending map[int]chan *Message
	closed  bool

	done chan struct{} // closed when the read loop exits
	err  error         // the read loop's terminal error
}

// Connect dials the RHPv2 server at host:port and starts the read loop. The
// dial honours ctx's deadline. handler may be nil for a dial-only client.
func Connect(ctx context.Context, host string, p int, handler Handler) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, fmt.Sprintf("%d", p)))
	if err != nil {
		return nil, fmt.Errorf("rhp: connect %s:%d: %w", host, p, err)
	}
	if handler == nil {
		handler = nopHandler{}
	}
	c := &Client{
		conn:    conn,
		w:       bufio.NewWriter(conn),
		handler: handler,
		pending: make(map[int]chan *Message),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// Done returns a channel closed when the read loop exits (connection lost or
// Close called). Err, after Done is closed, gives the reason.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err returns the read loop's terminal error once Done is closed.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close shuts the connection and fails every in-flight request.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	return c.conn.Close()
}

func (c *Client) readLoop() {
	var loopErr error
	defer func() {
		c.mu.Lock()
		c.err = loopErr
		pending := c.pending
		c.pending = map[int]chan *Message{}
		c.closed = true
		c.mu.Unlock()
		for _, ch := range pending {
			close(ch) // a closed channel without a value signals "link lost"
		}
		close(c.done)
		_ = c.conn.Close()
	}()

	r := bufio.NewReader(c.conn)
	for {
		payload, err := readFrame(r)
		if err != nil {
			if err == io.EOF {
				loopErr = nil // clean close on a frame boundary
			} else {
				loopErr = err
			}
			return
		}
		var msg Message
		if err := json.Unmarshal(payload, &msg); err != nil {
			loopErr = fmt.Errorf("rhp: decode frame: %w", err)
			return
		}
		c.dispatch(&msg)
	}
}

// dispatch routes a decoded frame: replies (carrying the request id) wake the
// waiter; pushes (no id, a seqno or a server-initiated type) go to the handler
// (wire-fidelity row 6).
func (c *Client) dispatch(msg *Message) {
	if msg.ID != nil {
		c.mu.Lock()
		ch, ok := c.pending[*msg.ID]
		if ok {
			delete(c.pending, *msg.ID)
		}
		c.mu.Unlock()
		if ok {
			ch <- msg
			close(ch)
		}
		return
	}

	switch msg.Type {
	case TypeAccept:
		listener, child := derefHandle(msg.Handle), derefHandle(msg.Child)
		c.handler.OnAccept(listener, child, msg.Remote, msg.Local, msg.Port.string())
	case TypeRecv:
		c.handler.OnRecv(derefHandle(msg.Handle), DecodeData(deref(msg.Data)))
	case TypeStatus:
		c.handler.OnStatus(derefHandle(msg.Handle), msg.statusFlags())
	case TypeClose:
		c.handler.OnClose(derefHandle(msg.Handle))
	default:
		// An unsolicited reply-shaped frame with no id we can correlate, or an
		// unknown push type — nothing actionable; drop it.
	}
}

// roundTrip sends req with a fresh id and waits for the matching reply (or
// ctx cancellation, or link loss). A non-zero errCode in the reply becomes a
// *ServerError.
func (c *Client) roundTrip(ctx context.Context, req *Message) (*Message, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("rhp: client is closed")
	}
	c.nextID++
	id := c.nextID
	ch := make(chan *Message, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	req.ID = intPtr(id)
	if err := c.writeMessage(req); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case reply, ok := <-ch:
		if !ok || reply == nil {
			return nil, errors.New("rhp: connection lost while awaiting reply")
		}
		if code := reply.Err(); code != ErrOk {
			return reply, &ServerError{Code: code, Text: reply.ErrText}
		}
		return reply, nil
	}
}

func (c *Client) writeMessage(msg *Message) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("rhp: encode %s: %w", msg.Type, err)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeFrame(c.w, payload)
}

// --- request helpers (the DAPPS/chat subset) ---

// Authenticate sends an auth request; needed only when the node runs with
// requireAuth (docs/rhp2-server.md §D6).
func (c *Client) Authenticate(ctx context.Context, user, pass string) error {
	_, err := c.roundTrip(ctx, &Message{Type: TypeAuth, User: user, Pass: pass})
	return err
}

// Hello performs capability discovery (a pdn extension). A baseline-v2 server
// without it answers errCode 2, which the caller may treat as "no discovery".
func (c *Client) Hello(ctx context.Context) (*Message, error) {
	return c.roundTrip(ctx, &Message{Type: TypeHello})
}

// Socket creates an unbound ax25/stream socket and returns its handle.
func (c *Client) Socket(ctx context.Context) (int, error) {
	reply, err := c.roundTrip(ctx, &Message{Type: TypeSocket, Pfam: FamilyAX25, Mode: ModeStream})
	if err != nil {
		return 0, err
	}
	return derefHandle(reply.Handle), nil
}

// Bind binds a socket to a local callsign. A null port (port=="") listens on
// all node ports — what an app's inbound listener wants.
func (c *Client) Bind(ctx context.Context, handle int, local, p string) error {
	_, err := c.roundTrip(ctx, &Message{Type: TypeBind, Handle: intPtr(handle), Local: local, Port: port(p)})
	return err
}

// Listen puts a bound socket into the listening state; inbound connections
// then arrive as accept pushes (Handler.OnAccept).
func (c *Client) Listen(ctx context.Context, handle int) error {
	_, err := c.roundTrip(ctx, &Message{Type: TypeListen, Handle: intPtr(handle), Flags: intPtr(FlagPassive)})
	return err
}

// Open performs an active open from local to remote and returns the new
// socket handle (the outbound peer-dial path). pdn replies after the connect
// resolves (deviation D4), so a nil error means the link is up.
func (c *Client) Open(ctx context.Context, local, remote, p string) (int, error) {
	reply, err := c.roundTrip(ctx, &Message{
		Type:   TypeOpen,
		Pfam:   FamilyAX25,
		Mode:   ModeStream,
		Port:   port(p),
		Local:  local,
		Remote: remote,
		Flags:  intPtr(FlagActive),
	})
	if err != nil {
		return 0, err
	}
	return derefHandle(reply.Handle), nil
}

// Send writes payload bytes on a connected socket. The data field is always
// present (an empty payload is a legal zero-byte send; wire-fidelity row 13).
func (c *Client) Send(ctx context.Context, handle int, data []byte) error {
	_, err := c.roundTrip(ctx, &Message{Type: TypeSend, Handle: intPtr(handle), Data: strPtr(EncodeData(data))})
	return err
}

// CloseHandle closes one socket (idempotent best-effort — a link already gone
// is not an error).
func (c *Client) CloseHandle(ctx context.Context, handle int) error {
	_, err := c.roundTrip(ctx, &Message{Type: TypeClose, Handle: intPtr(handle)})
	var se *ServerError
	if errors.As(err, &se) && se.Code == ErrInvalidHandle {
		return nil
	}
	return err
}

// SetReadDeadline bounds how long the next frame read may block; used by tests
// and shutdown.
func (c *Client) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

func derefHandle(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func (p *PortValue) string() string {
	if p == nil {
		return ""
	}
	return string(*p)
}
