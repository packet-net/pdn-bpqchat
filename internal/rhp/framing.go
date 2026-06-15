package rhp

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

// MaxPayloadLength is the largest payload expressible in the 2-byte length
// prefix. It is also the codec frame cap pdn advertises, so a single RHP frame
// never exceeds it.
const MaxPayloadLength = 0xFFFF

// writeFrame writes one length-prefixed RHPv2 frame: a 2-byte big-endian
// length followed by exactly that many JSON payload bytes. The caller is
// responsible for flushing the underlying writer.
func writeFrame(w *bufio.Writer, payload []byte) error {
	if len(payload) > MaxPayloadLength {
		return fmt.Errorf("rhp: payload of %d bytes exceeds the 16-bit length prefix maximum (%d)", len(payload), MaxPayloadLength)
	}
	var header [2]byte
	binary.BigEndian.PutUint16(header[:], uint16(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	return w.Flush()
}

// readFrame reads one length-prefixed RHPv2 frame and returns its JSON
// payload bytes. io.ErrUnexpectedEOF is returned if the stream ends partway
// through a frame; io.EOF only when the stream ends cleanly on a frame
// boundary.
func readFrame(r io.Reader) ([]byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint16(header[:])
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		if err == io.EOF {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return payload, nil
}
