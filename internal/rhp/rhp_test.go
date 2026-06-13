package rhp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestFramingRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	payload := []byte(`{"type":"hello"}`)
	if err := writeFrame(w, payload); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	got, err := readFrame(&buf)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("round trip mismatch: got %q want %q", got, payload)
	}
}

func TestPortValueStringOrInt(t *testing.T) {
	// XRouter emits port as a string in some shapes, a bare number in others
	// (wire-fidelity rows 3, 4); both must decode and re-emit as a string.
	for _, in := range []string{`{"port":"2"}`, `{"port":2}`} {
		var m Message
		if err := json.Unmarshal([]byte(in), &m); err != nil {
			t.Fatalf("unmarshal %s: %v", in, err)
		}
		if m.Port.string() != "2" {
			t.Fatalf("port from %s = %q, want \"2\"", in, m.Port.string())
		}
		out, _ := json.Marshal(&m)
		if !bytes.Contains(out, []byte(`"port":"2"`)) {
			t.Fatalf("re-emit of %s = %s, want quoted port", in, out)
		}
	}
}

func TestErrCodeCaseInsensitive(t *testing.T) {
	// XRouter uses capital errCode/errText, the spec lowercase; reads accept
	// either (wire-fidelity row 1).
	var m Message
	if err := json.Unmarshal([]byte(`{"type":"bindReply","errcode":9,"errtext":"Duplicate socket"}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.Err() != ErrDuplicateSocket {
		t.Fatalf("Err() = %d, want %d", m.Err(), ErrDuplicateSocket)
	}
}

func TestDataEncodingLatin1(t *testing.T) {
	// Every byte 0x00..0xFF must survive the Latin-1 round trip (not base64;
	// wire-fidelity row 7), preserving length.
	raw := make([]byte, 256)
	for i := range raw {
		raw[i] = byte(i)
	}
	if got := DecodeData(EncodeData(raw)); !bytes.Equal(got, raw) {
		t.Fatalf("latin1 round trip lost bytes: got %d bytes, want 256", len(got))
	}
}

// captureHandler records pushes for assertions.
type captureHandler struct {
	mu      sync.Mutex
	accepts []accept
	recvs   [][]byte
	closes  []int
}

type accept struct {
	listener, child int
	remote          string
}

func (h *captureHandler) OnAccept(l, c int, remote, _, _ string) {
	h.mu.Lock()
	h.accepts = append(h.accepts, accept{l, c, remote})
	h.mu.Unlock()
}
func (h *captureHandler) OnRecv(_ int, d []byte) {
	h.mu.Lock()
	h.recvs = append(h.recvs, append([]byte(nil), d...))
	h.mu.Unlock()
}
func (h *captureHandler) OnStatus(int, int) {}
func (h *captureHandler) OnClose(handle int) {
	h.mu.Lock()
	h.closes = append(h.closes, handle)
	h.mu.Unlock()
}

// TestClientListenerPath drives the full inbound listener sequence against an
// in-process fake RHP server: socket → bind → listen, then a server-pushed
// accept + recv, mirroring the BSD path pdn-bpqchat uses for inbound RF users.
func TestClientListenerPath(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	serverReady := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		serverReady <- conn
		fakeServer(t, conn)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	h := &captureHandler{}
	host, p := splitHostPort(t, ln.Addr().String())
	c, err := Connect(ctx, host, p, h)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()

	handle, err := c.Socket(ctx)
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	if handle != 100 {
		t.Fatalf("socket handle = %d, want 100", handle)
	}
	if err := c.Bind(ctx, handle, "M0LTE-7", ""); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := c.Listen(ctx, handle); err != nil {
		t.Fatalf("listen: %v", err)
	}

	// The fake server pushes an accept then a recv after listen; wait for them.
	deadline := time.After(2 * time.Second)
	for {
		h.mu.Lock()
		gotAccept := len(h.accepts) == 1
		gotRecv := len(h.recvs) == 1
		h.mu.Unlock()
		if gotAccept && gotRecv {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for pushes: accepts=%d recvs=%d", len(h.accepts), len(h.recvs))
		case <-time.After(10 * time.Millisecond):
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.accepts[0].child != 101 || h.accepts[0].remote != "G8PZT" {
		t.Fatalf("accept = %+v, want child 101 from G8PZT", h.accepts[0])
	}
	if string(h.recvs[0]) != "hello world" {
		t.Fatalf("recv = %q, want %q", h.recvs[0], "hello world")
	}
}

// TestServerErrorPropagates checks a non-zero errCode reply surfaces as a
// *ServerError the caller can inspect.
func TestServerErrorPropagates(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Reply to any request with a duplicate-socket error.
		r := bufio.NewReader(conn)
		w := bufio.NewWriter(conn)
		for {
			payload, err := readFrame(r)
			if err != nil {
				return
			}
			var m Message
			_ = json.Unmarshal(payload, &m)
			reply := Message{Type: m.Type + "Reply", ID: m.ID, ErrCode: intPtr(ErrDuplicateSocket), ErrText: "Duplicate socket"}
			out, _ := json.Marshal(&reply)
			_ = writeFrame(w, out)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	host, p := splitHostPort(t, ln.Addr().String())
	c, err := Connect(ctx, host, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	err = c.Bind(ctx, 1, "M0LTE-7", "")
	var se *ServerError
	if !errors.As(err, &se) || se.Code != ErrDuplicateSocket {
		t.Fatalf("bind error = %v, want *ServerError code 9", err)
	}
	if !IsCallsignInUse(se.Code) {
		t.Fatalf("IsCallsignInUse(%d) = false, want true", se.Code)
	}
}

// fakeServer answers socket/bind/listen, then pushes accept + recv for a child.
func fakeServer(t *testing.T, conn net.Conn) {
	t.Helper()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	for {
		payload, err := readFrame(r)
		if err != nil {
			return
		}
		var m Message
		if err := json.Unmarshal(payload, &m); err != nil {
			return
		}
		switch m.Type {
		case TypeSocket:
			reply(w, Message{Type: TypeSocketReply, ID: m.ID, Handle: intPtr(100), ErrCode: intPtr(ErrOk)})
		case TypeBind:
			reply(w, Message{Type: TypeBindReply, ID: m.ID, Handle: m.Handle, ErrCode: intPtr(ErrOk)})
		case TypeListen:
			reply(w, Message{Type: TypeListenReply, ID: m.ID, Handle: m.Handle, ErrCode: intPtr(ErrOk)})
			// Push an accept then a recv (no id; seqno-style pushes).
			p2 := PortValue("1")
			reply(w, Message{Type: TypeAccept, Seqno: intPtr(0), Handle: intPtr(100), Child: intPtr(101), Remote: "G8PZT", Local: "M0LTE-7", Port: &p2})
			reply(w, Message{Type: TypeRecv, Seqno: intPtr(1), Handle: intPtr(101), Data: strPtr(EncodeData([]byte("hello world")))})
		default:
			reply(w, Message{Type: m.Type + "Reply", ID: m.ID, ErrCode: intPtr(ErrOk)})
		}
	}
}

func reply(w *bufio.Writer, m Message) {
	out, _ := json.Marshal(&m)
	_ = writeFrame(w, out)
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return host, p
}
