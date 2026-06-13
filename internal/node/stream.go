package node

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/m0lte/pdn-bpqchat/internal/rhp"
)

// rhpStream adapts an RHP child handle (a connected AX.25 session) to an
// io.ReadWriteCloser, turning the client's push-based recv callbacks into a
// blocking Read. This is the unification point that lets BOTH a user session and
// a peer Link run over an RHP child with ordinary stream I/O.
type rhpStream struct {
	client *rhp.Client
	handle int

	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
}

func newRhpStream(client *rhp.Client, handle int) *rhpStream {
	s := &rhpStream{client: client, handle: handle}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// feed delivers bytes that arrived on the child (called from the RHP read loop).
func (s *rhpStream) feed(data []byte) {
	s.mu.Lock()
	if !s.closed {
		s.buf = append(s.buf, data...)
		s.cond.Signal()
	}
	s.mu.Unlock()
}

// markClosed signals EOF to readers (the peer or node closed the child).
func (s *rhpStream) markClosed() {
	s.mu.Lock()
	s.closed = true
	s.cond.Broadcast()
	s.mu.Unlock()
}

// Read blocks until data is buffered or the stream closes (then io.EOF once the
// buffer drains).
func (s *rhpStream) Read(p []byte) (int, error) {
	s.mu.Lock()
	for len(s.buf) == 0 && !s.closed {
		s.cond.Wait()
	}
	if len(s.buf) == 0 && s.closed {
		s.mu.Unlock()
		return 0, io.EOF
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	s.mu.Unlock()
	return n, nil
}

// Write sends bytes on the child over RHP.
func (s *rhpStream) Write(p []byte) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.client.Send(ctx, s.handle, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close closes the child over RHP and signals EOF to readers.
func (s *rhpStream) Close() error {
	s.markClosed()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.client.CloseHandle(ctx, s.handle)
}
