// Package session is the RF user interface (W3): it turns a raw connected-mode
// byte stream (an RHP child handle for an inbound AX.25 user) into a
// BPQ-Chat-compatible interactive session driving the chat.Hub. It is host-free
// — it speaks to a Conn (output) and a *chat.Hub, never to RHP directly — so it
// is unit-tested with a fake Conn and a real hub. The daemon adapts an RHP
// child to a Conn (cmd/pdn-bpqchat).
package session

import "bytes"

// MaxLine is the longest input line accepted from a user — the explicit cap BPQ
// lacks (design.md §4.4). Bytes past the cap on one line are discarded up to the
// next terminator; the line is never allowed to grow without bound.
const MaxLine = 512

// LineAssembler reassembles CR/LF-terminated lines from a connected-mode byte
// stream where a line may be split across packets and input is hostile
// (unbounded, partial, mixed terminators). It is not safe for concurrent use;
// one assembler belongs to one session goroutine.
type LineAssembler struct {
	max      int
	buf      []byte
	overflow bool // discarding an over-long line until its terminator
}

// NewLineAssembler builds an assembler with the given per-line cap (<=0 uses
// MaxLine).
func NewLineAssembler(max int) *LineAssembler {
	if max <= 0 {
		max = MaxLine
	}
	return &LineAssembler{max: max}
}

// Feed consumes a chunk of received bytes and returns every complete line it
// completes (terminator stripped). A CR, an LF, or a CRLF pair each end a line;
// an empty line (a bare terminator) is returned as "" so the caller can decide
// (a user pressing enter). An over-long line is truncated to the cap and the
// remainder up to the next terminator is dropped.
func (a *LineAssembler) Feed(data []byte) []string {
	var out []string
	for i := 0; i < len(data); i++ {
		c := data[i]
		switch c {
		case '\r', '\n':
			// Collapse a CRLF (or LFCR) pair into one terminator.
			if i+1 < len(data) && (data[i+1] == '\r' || data[i+1] == '\n') && data[i+1] != c {
				i++
			}
			out = append(out, string(a.buf))
			a.buf = a.buf[:0]
			a.overflow = false
		default:
			if a.overflow {
				continue // dropping the tail of an over-long line
			}
			if len(a.buf) >= a.max {
				a.overflow = true // cap reached: emit what we have at the terminator, drop the rest
				continue
			}
			a.buf = append(a.buf, c)
		}
	}
	return out
}

// Pending reports whether a partial (unterminated) line is buffered — for tests.
func (a *LineAssembler) Pending() bool { return len(a.buf) > 0 || a.overflow }

// sanitise strips control bytes (except tab→space) from a line before it is
// shown or stored — the corruption guard BPQ does inline (HanksRT.c:chkctl),
// hardened: we never trust user/peer bytes to be printable.
func sanitise(s string) string {
	var b bytes.Buffer
	for _, r := range s {
		switch {
		case r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f:
			// drop other control characters
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
