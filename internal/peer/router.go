package peer

import (
	"context"
	"strings"
	"sync"

	"github.com/m0lte/pdn-bpqchat/internal/chat"
)

// Router is the central node-to-node relay and the home of loop/duplicate
// suppression (design.md §5). It does two things:
//
//   - Local-origin fan-out: it subscribes to the hub and forwards events that
//     ORIGINATED locally (a local RF/web user) out to every peer link, encoded
//     as BPQ records.
//   - Peer-origin relay: Ingest applies an inbound record to the hub (so local
//     users see it) and forwards it to every peer EXCEPT the one it arrived on
//     (the structural spanning-tree relay), after a content-id de-dup check
//     against the hub's seen-set (the backstop). A record already seen — whether
//     it looped back to its origin or reached a node by two paths — is dropped
//     before any apply or relay, so a cycle cannot storm.
type Router struct {
	hub     *chat.Hub
	ourNode string

	mu    sync.Mutex
	links map[string]sink // peer id (callsign) → link

	cancel func()
	done   chan struct{}
}

// sink is the part of a Link the Router drives: its id and a raw-record writer.
type sink interface {
	id() string
	sendRaw(raw string) error
}

// NewRouter creates a router and starts the local-origin fan-out loop.
func NewRouter(hub *chat.Hub) *Router {
	sub, cancel := hub.Subscribe()
	r := &Router{
		hub:     hub,
		ourNode: hub.OurNode(),
		links:   map[string]sink{},
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	go r.fanOutLocal(sub)
	return r
}

// Close stops the fan-out loop.
func (r *Router) Close() {
	r.cancel()
	<-r.done
}

// Add registers a peer link.
func (r *Router) Add(l sink) {
	r.mu.Lock()
	r.links[l.id()] = l
	r.mu.Unlock()
}

// Remove deregisters a peer link.
func (r *Router) Remove(id string) {
	r.mu.Lock()
	delete(r.links, id)
	r.mu.Unlock()
}

// fanOutLocal forwards locally-originated hub events to every peer. Peer-origin
// events (origin != our node) are ignored here — they are relayed by Ingest with
// ingress exclusion.
func (r *Router) fanOutLocal(sub <-chan chat.Event) {
	defer close(r.done)
	for ev := range sub {
		raw, ok := r.encodeLocal(ev)
		if !ok {
			continue
		}
		r.forward(raw, "")
	}
}

// encodeLocal maps a locally-originated event to its BPQ wire record.
func (r *Router) encodeLocal(ev chat.Event) (string, bool) {
	switch e := ev.(type) {
	case chat.TopicMessage:
		if e.Message.OriginNode == r.ourNode {
			return encodeData(r.ourNode, e.Message.FromCall, e.Message.Text), true
		}
	case chat.PrivateMessage:
		if e.Message.OriginNode == r.ourNode {
			return encodeSend(r.ourNode, e.Message.FromCall, e.Message.ToCall, e.Message.Text), true
		}
	case chat.UserJoined:
		if r.isLocal(e.User) {
			return encodeJoin(r.ourNode, e.User.Call, e.User.Name, e.User.QTH), true
		}
	case chat.UserLeft:
		if r.isLocal(e.User) {
			return encodeLeave(r.ourNode, e.User.Call, e.User.Name, e.User.QTH), true
		}
	case chat.TopicChanged:
		if r.isLocal(e.User) {
			return encodeTopic(r.ourNode, e.User.Call, e.User.Topic), true
		}
	case chat.UserInfoChanged:
		if r.isLocal(e.User) {
			return encode(IDUser, r.ourNode, e.User.Call, orDash(e.User.Name), orDash(e.User.QTH)), true
		}
	}
	return "", false
}

func (r *Router) isLocal(u chat.User) bool { return u.Origin.Node == r.ourNode }

// forward sends raw to every peer link except exceptID ("" = all).
func (r *Router) forward(raw, exceptID string) {
	r.mu.Lock()
	targets := make([]sink, 0, len(r.links))
	for id, l := range r.links {
		if id != exceptID {
			targets = append(targets, l)
		}
	}
	r.mu.Unlock()
	for _, l := range targets {
		_ = l.sendRaw(raw)
	}
}

// Ingest processes one inbound control record from the link fromID. Keepalive
// and poll records are handled by the Link itself and never reach here.
func (r *Router) Ingest(rec Record, fromID string) {
	// Drop a record from ourselves (loop break) or with no origin.
	if rec.Node == "" || strings.EqualFold(rec.Node, r.ourNode) {
		return
	}

	// De-dup backstop: a record already seen (looped back, or reached us by two
	// paths) is dropped before any apply or relay — the cycle-no-storm guarantee.
	id := r.recordID(rec)
	if id != "" && r.hub.Seen(id) {
		return
	}

	r.apply(rec, fromID)
	// Structural spanning-tree relay: forward verbatim to every peer but the
	// ingress (BPQ's echo rule, HanksRT.c:echo).
	r.forward(rec.Raw, fromID)
}

// recordID is the de-dup key. For data/send it equals the hub's SynthID for the
// same message, so a locally-originated message that loops back (its SynthID
// already recorded by hub.Post) is recognised. Presence/topic/link records use a
// stable hash of their content.
func (r *Router) recordID(rec Record) string {
	key := chat.UserKey{Call: rec.User, Node: rec.Node}
	switch rec.Type {
	case IDData:
		topic := r.topicOf(key)
		return chat.SynthID(rec.Node, rec.User, chat.KindTopic, topic, rec.Tail(0))
	case IDSend:
		return chat.SynthID(rec.Node, rec.User, chat.KindPrivate, rec.Field(0), rec.Tail(1))
	default:
		return "rec:" + string(rec.Type) + ":" + rec.Raw
	}
}

func (r *Router) topicOf(key chat.UserKey) string {
	if u, ok := r.hub.User(key); ok {
		return u.Topic
	}
	return chat.DefaultTopic
}

// apply mutates the hub from an inbound record. The hub events this produces
// carry origin == the remote node, so fanOutLocal ignores them (no double-send);
// relay is done by Ingest.
func (r *Router) apply(rec Record, fromID string) {
	key := chat.UserKey{Call: rec.User, Node: rec.Node}
	ctx := context.Background()
	switch rec.Type {
	case IDJoin:
		r.ensureUser(rec, fromID)
		// A join carries name/qth too; apply them even if the user was already
		// made present by an earlier data record (ensureUser early-returns then),
		// so the name still lands. Skip empty ("?") fields so a later bare join
		// can't clobber a known name.
		if name, qth := unDash(rec.Field(0)), unDash(rec.Field(1)); name != "" || qth != "" {
			r.hub.SetInfo(key, name, qth)
		}
	case IDLeave:
		r.hub.Leave(key)
	case IDUser:
		r.ensureUser(rec, fromID)
		r.hub.SetInfo(key, unDash(rec.Field(0)), unDash(rec.Field(1)))
	case IDTopic:
		r.ensureUser(rec, fromID)
		r.hub.SetTopic(key, rec.Field(0))
	case IDData:
		r.ensureUser(rec, fromID)
		_, _ = r.hub.Post(ctx, key, rec.Tail(0))
	case IDSend:
		r.ensureUser(rec, fromID)
		_, _ = r.hub.Private(ctx, key, rec.Field(0), rec.Tail(1))
	case IDLink:
		// node ncall gained a link to newnode (Field0) alias Field1 version Field2.
		r.hub.LinkNode(rec.Field(0), unDash(rec.Field(1)), rec.Field(2))
	case IDUnlink:
		r.hub.UnlinkNode(rec.Field(0))
	}
}

// ensureUser makes a remote user present (idempotent), recording the ingress
// link as provenance (design.md §4.2). For id_join/id_user it also sets name/QTH.
func (r *Router) ensureUser(rec Record, fromID string) {
	if _, ok := r.hub.User(chat.UserKey{Call: rec.User, Node: rec.Node}); ok {
		return
	}
	name, qth := "", ""
	if rec.Type == IDJoin || rec.Type == IDUser {
		name, qth = unDash(rec.Field(0)), unDash(rec.Field(1))
	}
	_, _ = r.hub.Join(chat.User{
		Call:   rec.User,
		Name:   name,
		QTH:    qth,
		Origin: chat.Origin{Node: rec.Node, Link: fromID},
	})
}
