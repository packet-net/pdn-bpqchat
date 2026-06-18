package chat

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

// Errors the Hub returns.
var (
	ErrNoSuchUser  = errors.New("chat: no such user")
	ErrEmptyText   = errors.New("chat: empty message")
	ErrNoSuchTopic = errors.New("chat: no such topic")
)

// subBuffer is how many events a subscriber may fall behind before the Hub
// drops events for it (a slow consumer must not stall the whole hub).
const subBuffer = 256

// Hub is the in-RAM authority for live chat state — users, presence, topics,
// and the mesh node graph — and the fan-out point for Events. It is the pure
// core: no RHP, HTTP, or SQLite (persistence is the injected Store). Safe for
// concurrent use.
type Hub struct {
	ourNode string
	store   Store
	seen    *SeenSet
	clock   func() time.Time

	mu     sync.Mutex
	users  map[UserKey]*User
	topics map[string]string               // topic key → canonical display name
	member map[string]map[UserKey]struct{} // topic key → members
	nodes  map[string]*Node                // node call → node

	subMu   sync.Mutex
	subs    map[int]chan Event
	nextSub int
}

// NewHub builds a hub for our chat callsign (ourNode), backed by store. clock
// may be nil (time.Now). The DefaultTopic always exists.
func NewHub(ourNode string, store Store, clock func() time.Time) *Hub {
	if clock == nil {
		clock = time.Now
	}
	if store == nil {
		store = NewMemStore()
	}
	h := &Hub{
		ourNode: normCall(ourNode),
		store:   store,
		seen:    NewSeenSet(DefaultSeenTTL, clock),
		clock:   clock,
		users:   map[UserKey]*User{},
		topics:  map[string]string{},
		member:  map[string]map[UserKey]struct{}{},
		nodes:   map[string]*Node{},
		subs:    map[int]chan Event{},
	}
	h.ensureTopicLocked(DefaultTopic)
	return h
}

// OurNode returns our chat callsign.
func (h *Hub) OurNode() string { return h.ourNode }

// Seen reports whether a synthetic message id has already been processed (and
// records it). The peer relay (W5) gates inbound records on this — the de-dup
// backstop of design.md §5.
func (h *Hub) Seen(id string) bool { return h.seen.Seen(id) }

// Subscribe returns a channel of events and a cancel function. The caller must
// drain promptly; if it falls more than subBuffer events behind, events are
// dropped for it (never for other subscribers).
func (h *Hub) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, subBuffer)
	h.subMu.Lock()
	id := h.nextSub
	h.nextSub++
	h.subs[id] = ch
	h.subMu.Unlock()
	return ch, func() {
		h.subMu.Lock()
		if c, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(c)
		}
		h.subMu.Unlock()
	}
}

func (h *Hub) emit(events ...Event) {
	if len(events) == 0 {
		return
	}
	// Hold subMu across the sends: the sends are non-blocking (the default
	// case), so the lock is held only briefly, and serialising with the cancel
	// func (which closes a channel under the same lock) is what makes a
	// concurrent send-and-close impossible — without this, emit could send on a
	// channel cancel is closing (a race, and a send-on-closed panic).
	h.subMu.Lock()
	defer h.subMu.Unlock()
	for _, ch := range h.subs {
		for _, e := range events {
			select {
			case ch <- e:
			default: // subscriber is too far behind — drop rather than stall the hub
			}
		}
	}
}

// Join makes a user present. A blank Topic lands them in DefaultTopic. Re-joining
// an existing identity refreshes its presence (BPQ closes the old session;
// callers above enforce that). Returns the resulting user.
func (h *Hub) Join(u User) (User, error) {
	u.Call = normCall(u.Call)
	u.Origin.Node = normCall(u.Origin.Node)
	if u.Call == "" || u.Origin.Node == "" {
		return User{}, ErrNoSuchUser
	}
	now := h.clock()
	if u.Topic == "" {
		u.Topic = DefaultTopic
	}
	u.Joined = now
	u.LastActive = now

	h.mu.Lock()
	canon := h.ensureTopicLocked(u.Topic)
	u.Topic = canon
	stored := u // copy
	h.users[u.Key()] = &stored
	h.member[normTopicKey(canon)][u.Key()] = struct{}{}
	h.mu.Unlock()

	h.emit(UserJoined{User: stored})
	return stored, nil
}

// Leave removes a user. Unknown users are ignored (idempotent).
func (h *Hub) Leave(key UserKey) {
	key = canonKey(key)
	h.mu.Lock()
	u, ok := h.users[key]
	if !ok {
		h.mu.Unlock()
		return
	}
	left := *u
	delete(h.users, key)
	if m, ok := h.member[normTopicKey(u.Topic)]; ok {
		delete(m, key)
	}
	h.mu.Unlock()
	h.emit(UserLeft{User: left})
}

// Post sends text from a user to their current topic. The message is de-dup
// recorded, persisted, and fanned out. Returns the stored Message.
func (h *Hub) Post(ctx context.Context, key UserKey, text string) (Message, error) {
	text = clip(text)
	if text == "" {
		return Message{}, ErrEmptyText
	}
	key = canonKey(key)
	h.mu.Lock()
	u, ok := h.users[key]
	if !ok {
		h.mu.Unlock()
		return Message{}, ErrNoSuchUser
	}
	topic := u.Topic
	u.LastActive = h.clock()
	h.mu.Unlock()

	m := Message{
		OriginNode: key.Node,
		FromCall:   key.Call,
		Kind:       KindTopic,
		Topic:      topic,
		Time:       h.clock(),
		Text:       text,
	}
	m.ID = SynthID(m.OriginNode, m.FromCall, m.Kind, m.Topic, m.Text)
	h.seen.Seen(m.ID) // record our own origin so a loopback from the mesh is dropped

	if err := h.store.SaveMessage(ctx, m); err != nil {
		return Message{}, err
	}
	h.emit(TopicMessage{Message: m})
	return m, nil
}

// Private sends text from one user to another (any node). Returns the Message.
// ErrNoSuchUser if either party is unknown.
func (h *Hub) Private(ctx context.Context, from UserKey, toCall, text string) (Message, error) {
	text = clip(text)
	if text == "" {
		return Message{}, ErrEmptyText
	}
	from = canonKey(from)
	toCall = normCall(toCall)
	h.mu.Lock()
	if _, ok := h.users[from]; !ok {
		h.mu.Unlock()
		return Message{}, ErrNoSuchUser
	}
	if !h.userPresentLocked(toCall) {
		h.mu.Unlock()
		return Message{}, ErrNoSuchUser
	}
	h.mu.Unlock()

	m := Message{
		OriginNode: from.Node,
		FromCall:   from.Call,
		Kind:       KindPrivate,
		ToCall:     toCall,
		Time:       h.clock(),
		Text:       text,
	}
	m.ID = SynthID(m.OriginNode, m.FromCall, m.Kind, m.ToCall, m.Text)
	h.seen.Seen(m.ID)
	if err := h.store.SaveMessage(ctx, m); err != nil {
		return Message{}, err
	}
	h.emit(PrivateMessage{Message: m})
	return m, nil
}

// SetTopic moves a user to a (possibly new) topic. Returns false (no event) if
// they were already in it. Topic names are case-insensitive.
func (h *Hub) SetTopic(key UserKey, name string) (bool, error) {
	key = canonKey(key)
	if normTopicKey(name) == "" {
		return false, ErrNoSuchTopic
	}
	h.mu.Lock()
	u, ok := h.users[key]
	if !ok {
		h.mu.Unlock()
		return false, ErrNoSuchUser
	}
	if normTopicKey(u.Topic) == normTopicKey(name) {
		h.mu.Unlock()
		return false, nil
	}
	from := u.Topic
	if m, ok := h.member[normTopicKey(from)]; ok {
		delete(m, key)
	}
	canon := h.ensureTopicLocked(name)
	u.Topic = canon
	h.member[normTopicKey(canon)][key] = struct{}{}
	moved := *u
	h.mu.Unlock()

	h.emit(TopicChanged{User: moved, From: from})
	return true, nil
}

// SetInfo updates a user's name and/or QTH. Returns false (no event) if nothing
// changed.
func (h *Hub) SetInfo(key UserKey, name, qth string) (bool, error) {
	key = canonKey(key)
	h.mu.Lock()
	u, ok := h.users[key]
	if !ok {
		h.mu.Unlock()
		return false, ErrNoSuchUser
	}
	if u.Name == name && u.QTH == qth {
		h.mu.Unlock()
		return false, nil
	}
	u.Name = name
	u.QTH = qth
	changed := *u
	h.mu.Unlock()
	h.emit(UserInfoChanged{User: changed})
	return true, nil
}

// SetFlags updates a user's display/behaviour flags (the BPQ `/` toggle set:
// Echo/Bells/Colour/ShowNames/ShowTime). Flags are presentation state the core
// carries so a web flip becomes the one persisted identity every plane — RF, web,
// and mesh — observes for that user (design.md §3.5). Returns false (no event)
// if nothing changed. Mirrors SetInfo: same lock discipline, same UserInfoChanged
// event so subscribers refresh a single identity uniformly.
func (h *Hub) SetFlags(key UserKey, flags UserFlags) (bool, error) {
	key = canonKey(key)
	h.mu.Lock()
	u, ok := h.users[key]
	if !ok {
		h.mu.Unlock()
		return false, ErrNoSuchUser
	}
	if u.Flags == flags {
		h.mu.Unlock()
		return false, nil
	}
	u.Flags = flags
	changed := *u
	h.mu.Unlock()
	h.emit(UserInfoChanged{User: changed})
	return true, nil
}

// LinkNode records a node in the mesh graph. Returns false (no event) if it was
// already known (its link time is refreshed).
func (h *Hub) LinkNode(call, alias, version string) bool {
	call = normCall(call)
	if call == "" {
		return false
	}
	now := h.clock()
	h.mu.Lock()
	if n, ok := h.nodes[call]; ok {
		n.Linked = now
		if version != "" {
			n.Version = version
		}
		h.mu.Unlock()
		return false
	}
	n := &Node{Call: call, Alias: alias, Version: version, Linked: now}
	h.nodes[call] = n
	added := *n
	h.mu.Unlock()
	h.emit(NodeLinked{Node: added})
	return true
}

// UnlinkNode removes a node and drops every user learned via it, emitting a
// UserLeft for each (the link-drop cascade — design.md §4.5). Returns false if
// the node was unknown.
func (h *Hub) UnlinkNode(call string) bool {
	call = normCall(call)
	h.mu.Lock()
	n, ok := h.nodes[call]
	if !ok {
		h.mu.Unlock()
		return false
	}
	removed := *n
	delete(h.nodes, call)
	var left []User
	for key, u := range h.users {
		if u.Origin.Link == call || u.Origin.Node == call {
			left = append(left, *u)
			delete(h.users, key)
			if m, ok := h.member[normTopicKey(u.Topic)]; ok {
				delete(m, key)
			}
		}
	}
	h.mu.Unlock()

	events := make([]Event, 0, len(left)+1)
	for _, u := range left {
		events = append(events, UserLeft{User: u})
	}
	events = append(events, NodeUnlinked{Node: removed})
	h.emit(events...)
	return true
}

// History returns up to limit topic messages at or after since, oldest first.
func (h *Hub) History(ctx context.Context, topic string, since time.Time, limit int) ([]Message, error) {
	return h.store.History(ctx, topic, since, limit)
}

// --- queries (snapshots; never expose internal pointers) ---

// User returns a snapshot of a present user, or false if absent.
func (h *Hub) User(key UserKey) (User, bool) {
	key = canonKey(key)
	h.mu.Lock()
	defer h.mu.Unlock()
	if u, ok := h.users[key]; ok {
		return *u, true
	}
	return User{}, false
}

// Users returns every present user, sorted by callsign then node.
func (h *Hub) Users() []User {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]User, 0, len(h.users))
	for _, u := range h.users {
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Call != out[j].Call {
			return out[i].Call < out[j].Call
		}
		return out[i].Origin.Node < out[j].Origin.Node
	})
	return out
}

// UsersInTopic returns the users currently in a topic, sorted by callsign.
func (h *Hub) UsersInTopic(name string) []User {
	h.mu.Lock()
	defer h.mu.Unlock()
	members := h.member[normTopicKey(name)]
	out := make([]User, 0, len(members))
	for key := range members {
		if u, ok := h.users[key]; ok {
			out = append(out, *u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Call < out[j].Call })
	return out
}

// Topic is a topic snapshot for queries.
type Topic struct {
	Name    string
	Members int
}

// Topics returns every topic and its member count, sorted by name.
func (h *Hub) Topics() []Topic {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Topic, 0, len(h.topics))
	for key, name := range h.topics {
		out = append(out, Topic{Name: name, Members: len(h.member[key])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Nodes returns the known mesh nodes, sorted by callsign.
func (h *Hub) Nodes() []Node {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Node, 0, len(h.nodes))
	for _, n := range h.nodes {
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Call < out[j].Call })
	return out
}

// --- internals (caller holds h.mu) ---

// ensureTopicLocked creates the topic if absent and returns its canonical
// display name (first-seen casing wins).
func (h *Hub) ensureTopicLocked(name string) string {
	key := normTopicKey(name)
	if canon, ok := h.topics[key]; ok {
		return canon
	}
	canon := strings.TrimSpace(name)
	h.topics[key] = canon
	h.member[key] = map[UserKey]struct{}{}
	return canon
}

func (h *Hub) userPresentLocked(call string) bool {
	for key := range h.users {
		if key.Call == call {
			return true
		}
	}
	return false
}

func canonKey(k UserKey) UserKey { return UserKey{Call: normCall(k.Call), Node: normCall(k.Node)} }

// clip trims and length-caps message text (design.md §4.4).
func clip(s string) string {
	s = strings.TrimRight(s, "\r\n")
	if len(s) > MaxText {
		s = s[:MaxText]
	}
	return strings.TrimSpace(s)
}
