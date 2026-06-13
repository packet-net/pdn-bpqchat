package session

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/m0lte/pdn-bpqchat/internal/chat"
)

// fakeConn captures everything sent to the user.
type fakeConn struct {
	mu     sync.Mutex
	buf    strings.Builder
	closed bool
}

func (c *fakeConn) Send(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf.Write(data)
	return nil
}
func (c *fakeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}
func (c *fakeConn) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// waitFor polls until cond is true or the deadline passes.
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

func newHub() *chat.Hub { return chat.NewHub("M0LTE-4", chat.NewMemStore(), nil) }

func TestSessionGreetsAndJoins(t *testing.T) {
	h := newHub()
	conn := &fakeConn{}
	s, err := New(h, conn, "g8pzt", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	waitFor(t, func() bool { return strings.Contains(conn.text(), "BPQCHATSERVER") })
	if u := h.Users(); len(u) != 1 || u[0].Call != "G8PZT" {
		t.Fatalf("user not joined: %+v", u)
	}
	if !strings.Contains(conn.text(), "Welcome") {
		t.Fatalf("no welcome in:\n%s", conn.text())
	}
}

func TestSessionPostAndCrossUserDelivery(t *testing.T) {
	h := newHub()
	c1, c2 := &fakeConn{}, &fakeConn{}
	s1, _ := New(h, c1, "G8PZT", nil)
	defer s1.Close()
	s2, _ := New(h, c2, "M0LTE", nil)
	defer s2.Close()
	waitFor(t, func() bool { return len(h.Users()) == 2 })

	// G8PZT types a line; M0LTE (same default topic) must see it.
	s1.Deliver([]byte("hello all\r"))
	waitFor(t, func() bool { return strings.Contains(c2.text(), "<G8PZT> hello all") })
}

func TestSessionTopicSwitchIsolates(t *testing.T) {
	h := newHub()
	c1, c2 := &fakeConn{}, &fakeConn{}
	s1, _ := New(h, c1, "G8PZT", nil)
	defer s1.Close()
	s2, _ := New(h, c2, "M0LTE", nil)
	defer s2.Close()
	waitFor(t, func() bool { return len(h.Users()) == 2 })

	s1.Deliver([]byte("/T DX\r"))
	waitFor(t, func() bool {
		u, ok := h.User(chat.UserKey{Call: "G8PZT", Node: "M0LTE-4"})
		return ok && strings.EqualFold(u.Topic, "DX")
	})
	// A message in DX must NOT reach M0LTE in General.
	s1.Deliver([]byte("only dx\r"))
	// Give it a moment, then assert absence.
	time.Sleep(50 * time.Millisecond)
	if strings.Contains(c2.text(), "only dx") {
		t.Fatalf("topic isolation broken; M0LTE saw DX traffic:\n%s", c2.text())
	}
}

func TestSessionPrivateMessage(t *testing.T) {
	h := newHub()
	c1, c2 := &fakeConn{}, &fakeConn{}
	s1, _ := New(h, c1, "G8PZT", nil)
	defer s1.Close()
	s2, _ := New(h, c2, "M0LTE", nil)
	defer s2.Close()
	waitFor(t, func() bool { return len(h.Users()) == 2 })

	s1.Deliver([]byte("/S M0LTE secret stuff\r"))
	waitFor(t, func() bool { return strings.Contains(c2.text(), "<*G8PZT*> secret stuff") })
}

func TestSessionByeEndsWithReason(t *testing.T) {
	h := newHub()
	conn := &fakeConn{}
	s, _ := New(h, conn, "G8PZT", nil)
	waitFor(t, func() bool { return len(h.Users()) == 1 })

	s.Deliver([]byte("/QUIT\r"))
	select {
	case r := <-s.Ended():
		if r != EndQuit {
			t.Fatalf("end reason = %v, want EndQuit", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session did not end on /QUIT")
	}
	waitFor(t, func() bool { return len(h.Users()) == 0 })
}

func TestSessionUnknownCommand(t *testing.T) {
	h := newHub()
	conn := &fakeConn{}
	s, _ := New(h, conn, "G8PZT", nil)
	defer s.Close()
	s.Deliver([]byte("/ZZZ\r"))
	waitFor(t, func() bool { return strings.Contains(conn.text(), "Unknown command") })
}
