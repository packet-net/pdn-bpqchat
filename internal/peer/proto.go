// Package peer implements BPQ chat node-to-node linking (W5/W6): the wire
// protocol derived in design.md §3, the link handshake, keepalives, and the
// loop/duplicate-suppression relay (§5). It bridges a remote chat node (BPQ or
// another pdn-bpqchat) to the local chat.Hub so messages, users, and topics
// propagate across the linked network.
//
// The wire is kept byte-identical to vanilla BPQ (decision, design.md §9): a
// control record is FORMAT(0x01) + a TYPE byte + space-separated fields,
// CR-terminated (reference/linbpq-chat/bpqchat.h, HanksRT.c:chkctl).
package peer

import (
	"strings"
)

// FORMAT is the leading byte of every inter-node control record (Ctrl-A);
// a line that does not begin with it is user text, not a control record.
const FORMAT = 0x01

// Record types (reference/linbpq-chat/bpqchat.h:214-224).
const (
	IDJoin      = 'J' // user joins RT:      node user name qth
	IDLeave     = 'L' // user leaves RT:     node user name qth
	IDLink      = 'N' // node gained a link: node newnode alias version
	IDUnlink    = 'Q' // node lost a link:   node lostnode
	IDData      = 'D' // message to a topic: node user text...
	IDSend      = 'S' // private message:    node user target text...
	IDTopic     = 'T' // user changed topic: node user topic
	IDUser      = 'I' // user name/QTH:      node user name qth
	IDKeepalive = 'K' // node-node keepalive: node linkcall [version]
	IDPoll      = 'P' // link-validation poll: node linkcall
	IDPollResp  = 'R' // poll response:        node linkcall
)

// rtlLogin is the line a linking node sends after the banner to log in as a
// node link (design.md §3.2).
const rtlLogin = "*RTL"

// bannerPrefix is the greeting a chat node sends on connect; the suffix is
// FBB-style capability chars. We advertise "pdn".
const bannerPrefix = "[BPQCHATSERVER-"

func banner() string { return bannerPrefix + "pdn]" }

// Banner is the chat-node greeting line (without the trailing CR) that a node
// sends on every inbound connect, so the demux can send it before deciding
// whether the caller is a user or a linking peer.
func Banner() string { return banner() }

// IsRTL reports whether a line is the *RTL node-link login (design.md §3.2).
func IsRTL(line string) bool { return strings.EqualFold(strings.TrimSpace(line), rtlLogin) }

// Record is one decoded inter-node control record. Raw is the exact wire line
// (CR stripped) — the relay forwards Raw verbatim, exactly as BPQ does
// (HanksRT.c:echo relays the received buffer), so re-encoding never drifts.
type Record struct {
	Raw  string
	Type byte
	Node string   // ncall — the originating node
	User string   // ucall — the user (or, for link/unlink, a second node)
	Args []string // remaining space-split fields
}

// Field returns the i-th argument after node+user, or "".
func (r Record) Field(i int) string {
	if i < len(r.Args) {
		return r.Args[i]
	}
	return ""
}

// Tail returns the arguments from i onward rejoined with spaces (the message
// text for data/send, which may contain spaces).
func (r Record) Tail(i int) string {
	if i < len(r.Args) {
		return strings.Join(r.Args[i:], " ")
	}
	return ""
}

// IsControl reports whether a line is an inter-node control record.
func IsControl(line string) bool {
	return len(line) >= 2 && line[0] == FORMAT
}

// Decode parses a control record line (CR already stripped). ok is false if the
// line is not a well-formed control record. Corruption (control bytes other
// than TAB) is rejected, mirroring chkctl's guard.
func Decode(line string) (Record, bool) {
	if !IsControl(line) {
		return Record{}, false
	}
	typ := line[1]
	body := line[2:]
	// Reject corruption: control bytes (except TAB→space) invalidate the record.
	var b strings.Builder
	for _, c := range []byte(body) {
		switch {
		case c == '\t':
			b.WriteByte(' ')
		case c < 0x20 && c != FORMAT:
			return Record{}, false
		default:
			b.WriteByte(c)
		}
	}
	fields := strings.Fields(b.String())
	if len(fields) < 2 {
		return Record{}, false // need at least node + user
	}
	return Record{
		Raw:  line,
		Type: typ,
		Node: strings.ToUpper(fields[0]),
		User: strings.ToUpper(fields[1]),
		Args: fields[2:],
	}, true
}

// encode builds a control record line (no trailing CR; the writer adds it).
func encode(typ byte, node, user string, rest ...string) string {
	var b strings.Builder
	b.WriteByte(FORMAT)
	b.WriteByte(typ)
	b.WriteString(node)
	b.WriteByte(' ')
	b.WriteString(user)
	for _, f := range rest {
		b.WriteByte(' ')
		b.WriteString(f)
	}
	return b.String()
}

// Builders for the records pdn-bpqchat originates.

func encodeData(node, user, text string) string { return encode(IDData, node, user, text) }
func encodeJoin(node, user, name, qth string) string {
	return encode(IDJoin, node, user, orDash(name), orDash(qth))
}
func encodeLeave(node, user, name, qth string) string {
	return encode(IDLeave, node, user, orDash(name), orDash(qth))
}
func encodeTopic(node, user, topic string) string { return encode(IDTopic, node, user, topic) }
func encodeSend(node, user, target, text string) string {
	return encode(IDSend, node, user, target, text)
}
func encodeKeepalive(node, link, version string) string {
	return encode(IDKeepalive, node, link, version)
}
func encodePollResp(node, link string) string { return encode(IDPollResp, node, link) }

// orDash replaces an empty field with BPQ's "?" placeholder so the space-split
// field count stays stable (BPQ uses "?_name"/"?" style placeholders).
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "?"
	}
	return strings.ReplaceAll(s, " ", "_")
}

// unDash reverses orDash for display.
func unDash(s string) string {
	if s == "?" {
		return ""
	}
	return strings.ReplaceAll(s, "_", " ")
}
