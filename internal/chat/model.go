// Package chat is the pure, host-free chat domain: topics, users, presence, the
// node graph, and the message model that RF sessions (W3), the web UI (W4), and
// peering (W5/W6) all feed into and observe. It has no dependency on RHP, HTTP,
// or SQLite — persistence is reached through the Store interface, and host I/O
// through the event stream. This is the seam (design.md §2) that lets RF, web,
// and mesh users share one room and lets the core be unit-tested without a node.
package chat

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// DefaultTopic is the room every user lands in on connect — BPQ's own default
// (reference/linbpq-chat/bpqchat.h:202, `#define deftopic "General"`), so a
// pdn-bpqchat user and a BPQ-node user share one room (design.md §6).
const DefaultTopic = "General"

// MaxText bounds a single message's length — the explicit cap BPQ lacks
// (design.md §4.4). Longer text is truncated at the edge before it reaches the
// core.
const MaxText = 2048

// UserKey identifies a network user as (callsign, home-node) — BPQ's
// user_find(call, node) identity (design.md §6). Callsigns and nodes are
// upper-cased on construction so the key is canonical.
type UserKey struct {
	Call string
	Node string
}

// Origin records where a user (or message) entered our view — the provenance
// that lets us contain spoofing blast radius (design.md §4.2).
type Origin struct {
	Node  string // home-node callsign; our chat callsign for a locally-connected user
	Local bool   // connected directly to us (RF or web), not via a peer link
	Link  string // ingress peer-link callsign for a remote user ("" when Local)
}

// UserFlags are the per-user display/behaviour toggles from the BPQ `/` command
// set (design.md §3.5). They are presentation state; the core carries them so
// sessions and the web UI stay consistent for one identity.
type UserFlags struct {
	Echo      bool
	Bells     bool
	Colour    bool
	ShowNames bool
	ShowTime  bool
}

// User is a chat participant — local or learned from a peer.
type User struct {
	Call       string
	Name       string
	QTH        string
	Origin     Origin
	Topic      string // current topic, canonical (as-entered) casing
	Joined     time.Time
	LastActive time.Time
	Flags      UserFlags
}

// Key returns the user's (call, home-node) identity.
func (u *User) Key() UserKey { return UserKey{Call: u.Call, Node: u.Origin.Node} }

// Node is a chat node in the mesh — for /K (show known nodes) and the
// spanning-tree relay's known-node graph (design.md §5).
type Node struct {
	Call    string
	Alias   string
	Version string
	Linked  time.Time
}

// MessageKind distinguishes a topic broadcast from a directed private message.
type MessageKind int

const (
	// KindTopic is a message to everyone in a topic (BPQ id_data).
	KindTopic MessageKind = iota
	// KindPrivate is a message to one user (BPQ id_send).
	KindPrivate
)

// Message is one chat message in the log — the web-visible history BPQ lacks
// (design.md §4.7).
type Message struct {
	ID         string // synthetic content id for de-dup (design.md §5); see SynthID
	OriginNode string // node the message originated at
	FromCall   string // sender callsign
	Kind       MessageKind
	Topic      string // target topic (KindTopic)
	ToCall     string // target user (KindPrivate)
	Time       time.Time
	Text       string
}

// SynthID is the deterministic, content-derived message id the de-dup
// seen-set keys on (design.md §5). Because the BPQ wire carries no message id,
// every node must compute the SAME id from the SAME record, so the id is a hash
// of content that is stable across the mesh — origin node, sender, kind, scope
// (topic or target call), and the normalised text. It deliberately excludes any
// timestamp (each node stamps its own receive time, which would not match). The
// known limitation — two genuinely distinct messages with identical content
// inside the TTL collide — is accepted and documented in design.md §5.
func SynthID(originNode, fromCall string, kind MessageKind, scope, text string) string {
	h := sha256.New()
	// Length-prefix-free but unambiguous: NUL separators can't appear in
	// callsigns/scope, and the kind byte fixes the field count.
	h.Write([]byte(normCall(originNode)))
	h.Write([]byte{0})
	h.Write([]byte(normCall(fromCall)))
	h.Write([]byte{0, byte(kind), 0})
	h.Write([]byte(strings.ToLower(scope)))
	h.Write([]byte{0})
	h.Write([]byte(normText(text)))
	return hex.EncodeToString(h.Sum(nil))
}

// normCall canonicalises a callsign for keys and ids: trimmed, upper-cased.
func normCall(c string) string { return strings.ToUpper(strings.TrimSpace(c)) }

// normTopicKey folds a topic name to its case-insensitive key (BPQ topic names
// are not case-sensitive, design.md §3.5).
func normTopicKey(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// normText normalises message text for the synthetic id: trailing CR/LF and
// surrounding whitespace stripped, so framing differences across the wire don't
// change the id.
func normText(s string) string { return strings.TrimSpace(s) }
