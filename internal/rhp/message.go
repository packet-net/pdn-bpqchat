package rhp

import "encoding/json"

// Wire type discriminators (docs/rhp2-server.md). Casing matters: pdn emits
// camelCase replies (and the lowercase-"to" sendto), and async pushes carry
// seqno, never id.
const (
	TypeAuth        = "auth"
	TypeAuthReply   = "authReply"
	TypeHello       = "hello"
	TypeHelloReply  = "helloReply"
	TypeOpen        = "open"
	TypeOpenReply   = "openReply"
	TypeSocket      = "socket"
	TypeSocketReply = "socketReply"
	TypeBind        = "bind"
	TypeBindReply   = "bindReply"
	TypeListen      = "listen"
	TypeListenReply = "listenReply"
	TypeSend        = "send"
	TypeSendReply   = "sendReply"
	TypeRecv        = "recv"
	TypeAccept      = "accept"
	TypeStatus      = "status"
	TypeStatusReply = "statusReply"
	TypeClose       = "close"
	TypeCloseReply  = "closeReply"
)

// Protocol families and socket modes (open sets on the wire — strings, not
// enums; docs/rhp2-server.md). bpqchat only ever needs ax25/stream.
const (
	FamilyAX25 = "ax25"
	ModeStream = "stream"
)

// open/listen flags (PWP-0222). A passive open/listen sets no bits; an
// active open sets bit 0x80.
const (
	FlagPassive = 0x00
	FlagActive  = 0x80
)

// Message is one RHPv2 frame. Every wire field bpqchat needs is represented;
// pointer fields distinguish "absent" from "zero/empty" where the protocol
// keys on it (handle, data, errCode on a push). encoding/json matches field
// names case-insensitively on read, so XRouter's capital errCode/errText and
// the spec's lowercase forms both decode (wire-fidelity row 1).
type Message struct {
	Type string `json:"type"`

	// Correlation: replies echo the request id; async pushes carry seqno and
	// never an id (wire-fidelity row 6).
	ID    *int `json:"id,omitempty"`
	Seqno *int `json:"seqno,omitempty"`

	// Result fields (replies).
	ErrCode *int   `json:"errCode,omitempty"`
	ErrText string `json:"errText,omitempty"`

	// Socket identity.
	Handle *int `json:"handle,omitempty"`
	Child  *int `json:"child,omitempty"`

	// Addressing.
	Pfam   string     `json:"pfam,omitempty"`
	Mode   string     `json:"mode,omitempty"`
	Port   *PortValue `json:"port,omitempty"`
	Local  string     `json:"local,omitempty"`
	Remote string     `json:"remote,omitempty"`

	// open/listen request flags AND status-push flags share the wire field
	// name; the direction disambiguates (a client sets it on open/listen, the
	// server sets it on a status push).
	Flags *int `json:"flags,omitempty"`

	// Payload. Data is a pointer because the field is mandatory-even-when-empty
	// on send: absent draws errCode 12, "" is a legal zero-byte send
	// (wire-fidelity row 13). On the wire it is a Latin-1 string — use
	// EncodeData/DecodeData to cross the byte boundary.
	Data *string `json:"data,omitempty"`

	// auth.
	User string `json:"user,omitempty"`
	Pass string `json:"pass,omitempty"`

	// helloReply capability advertisement.
	Proto   string   `json:"proto,omitempty"`
	Impl    string   `json:"impl,omitempty"`
	Pfams   []string `json:"pfams,omitempty"`
	MaxData *int     `json:"maxData,omitempty"`
	Enc     string   `json:"enc,omitempty"`
}

// Err returns the message's errCode (0 if absent), treating a missing field as
// success — replies to a request that succeeded with no id still carry
// errCode 0 (wire-fidelity row 10).
func (m *Message) Err() int {
	if m.ErrCode == nil {
		return 0
	}
	return *m.ErrCode
}

// PortValue normalises the RHPv2 port field, which XRouter emits as a JSON
// string in some shapes and a JSON number in others (wire-fidelity rows 3, 4).
// It always round-trips back out as a string, matching the live wire.
type PortValue string

func (p PortValue) String() string { return string(p) }

func (p *PortValue) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*p = PortValue(s)
		return nil
	}
	*p = PortValue(string(b)) // a bare JSON number — keep its text form
	return nil
}

func (p PortValue) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(p))
}

// port builds an optional *PortValue from a string ("" → nil/absent).
func port(s string) *PortValue {
	if s == "" {
		return nil
	}
	v := PortValue(s)
	return &v
}

func intPtr(v int) *int { return &v }

func strPtr(s string) *string { return &s }

// statusFlags returns the status-notification flags carried on a status push
// (0 if absent).
func (m *Message) statusFlags() int {
	if m.Flags == nil {
		return 0
	}
	return *m.Flags
}

// EncodeData turns raw payload bytes into the Latin-1 wire string the RHPv2
// data field carries — one code unit per byte, JSON escaping does the rest;
// it is NOT base64 (wire-fidelity row 7).
func EncodeData(b []byte) string {
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return string(r)
}

// DecodeData recovers raw payload bytes from a Latin-1 wire string. Runes
// above 0xFF (which a conformant peer never sends) are truncated to their low
// byte rather than dropped, so length is preserved.
func DecodeData(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		out = append(out, byte(r))
	}
	return out
}
