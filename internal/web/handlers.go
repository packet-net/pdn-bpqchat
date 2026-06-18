package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/m0lte/pdn-bpqchat/internal/chat"
)

// wireEvent is the JSON shape the browser receives over SSE and from REST
// snapshots — a flattened, render-ready view of a chat event.
type wireEvent struct {
	Type  string `json:"type"`            // msg | private | join | leave | topicchange | node
	From  string `json:"from,omitempty"`  // sender callsign
	Node  string `json:"node,omitempty"`  // sender's home node (for off-node users)
	To    string `json:"to,omitempty"`    // private target
	With  string `json:"with,omitempty"`  // DM correspondent from the viewer's POV (the other party)
	Mine  bool   `json:"mine,omitempty"`  // DM the viewer sent (vs received) — for thread alignment
	Topic string `json:"topic,omitempty"` // topic the event belongs to
	Text  string `json:"text,omitempty"`
	Time  int64  `json:"time,omitempty"` // unix ms
}

// handleEvents is the SSE stream for one viewer: an initial snapshot (their
// topic, the user list, recent history) followed by live events filtered to
// what they should see.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	call, ok := s.requireViewer(w, r)
	if !ok {
		return
	}
	key := s.presence.enter(call)
	defer s.presence.leave(call)

	sub, cancel := s.hub.Subscribe()
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Initial snapshot: who you are, then the recent history of your topic.
	u, _ := s.hub.User(key)
	writeSSE(w, "you", map[string]string{"call": key.Call, "topic": u.Topic})
	for _, m := range s.historyFor(r.Context(), u.Topic) {
		writeSSE(w, "event", topicEvent(m))
	}
	writeSSE(w, "users", s.usersFor(u.Topic))
	flusher.Flush()

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-sub:
			if !ok {
				return
			}
			we, send := s.filter(key, ev)
			if !send {
				continue
			}
			writeSSE(w, "event", we)
			flusher.Flush()
		}
	}
}

// filter decides whether a hub event is visible to this viewer and converts it
// to the wire shape. Mirrors the RF session's render rules (topic isolation,
// private addressing).
func (s *Server) filter(key chat.UserKey, ev chat.Event) (wireEvent, bool) {
	myTopic := func() string {
		if u, ok := s.hub.User(key); ok {
			return u.Topic
		}
		return ""
	}
	switch e := ev.(type) {
	case chat.TopicMessage:
		if strings.EqualFold(e.Message.Topic, myTopic()) {
			return topicEvent(e.Message), true
		}
	case chat.PrivateMessage:
		// A DM is visible to BOTH ends: the recipient (its inbox) and the sender
		// (so the compose visibly threads in their own DM pane, S6). Each end's
		// correspondent is the OTHER party — privateEvent computes that from the
		// viewer's callsign so the browser can bucket DMs by correspondent.
		if strings.EqualFold(e.Message.ToCall, key.Call) || strings.EqualFold(e.Message.FromCall, key.Call) {
			return privateEvent(e.Message, key.Call), true
		}
	case chat.UserJoined:
		if e.User.Key() != key && strings.EqualFold(e.User.Topic, myTopic()) {
			return wireEvent{Type: "join", From: e.User.Call, Node: offNode(e.User, s.hub.OurNode()), Topic: e.User.Topic}, true
		}
	case chat.UserLeft:
		if e.User.Key() != key {
			return wireEvent{Type: "leave", From: e.User.Call}, true
		}
	case chat.TopicChanged:
		if e.User.Key() != key && strings.EqualFold(e.User.Topic, myTopic()) {
			return wireEvent{Type: "join", From: e.User.Call, Topic: e.User.Topic}, true
		}
	case chat.NodeLinked:
		return wireEvent{Type: "node", Text: "Link to node " + e.Node.Call + " established"}, true
	case chat.NodeUnlinked:
		return wireEvent{Type: "node", Text: "Link to node " + e.Node.Call + " lost"}, true
	}
	return wireEvent{}, false
}

// handleSend posts a message (or runs a /command) as the viewer.
func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireWrite(w, r) { // read-scope viewers are lurkers (S3): no posting
		return
	}
	call, ok := s.requireViewer(w, r)
	if !ok {
		return
	}
	key := chat.UserKey{Call: call, Node: s.hub.OurNode()}
	text := strings.TrimSpace(readField(r, "text"))
	if text == "" {
		http.Error(w, "empty", http.StatusBadRequest)
		return
	}
	if _, ok := s.hub.User(key); !ok {
		// No live stream yet (e.g. a curl client) — make them present first.
		key = s.presence.enter(call)
		defer s.presence.leave(call)
	}
	if _, err := s.hub.Post(r.Context(), key, text); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTopic switches the viewer's topic.
func (s *Server) handleTopic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireWrite(w, r) { // a topic switch is a write — lurkers stay put (S3)
		return
	}
	call, ok := s.requireViewer(w, r)
	if !ok {
		return
	}
	key := chat.UserKey{Call: call, Node: s.hub.OurNode()}
	name := strings.TrimSpace(readField(r, "topic"))
	if name == "" {
		http.Error(w, "empty topic", http.StatusBadRequest)
		return
	}
	if _, err := s.hub.SetTopic(key, name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	topic := r.URL.Query().Get("topic")
	if topic == "" {
		topic = chat.DefaultTopic
	}
	writeJSON(w, s.usersFor(topic))
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	topic := r.URL.Query().Get("topic")
	if topic == "" {
		topic = chat.DefaultTopic
	}
	out := make([]wireEvent, 0)
	for _, m := range s.historyFor(r.Context(), topic) {
		out = append(out, topicEvent(m))
	}
	writeJSON(w, out)
}

// handleDMs backfills the viewer's persisted DM threads (S6): every KindPrivate
// message they sent or received, rendered from THEIR point of view (With =
// correspondent, Mine = sent-by-me) so the browser can rebuild per-correspondent
// threads on (re)connect — the live SSE path carries new DMs, this carries the
// history the live stream never replays.
func (s *Server) handleDMs(w http.ResponseWriter, r *http.Request) {
	call, ok := s.requireViewer(w, r) // viewing your own DMs requires a claimed identity
	if !ok {
		return
	}
	out := make([]wireEvent, 0)
	for _, m := range s.privateHistoryFor(r.Context(), call) {
		out = append(out, privateEvent(m, call))
	}
	writeJSON(w, out)
}

// handleDM composes a direct message — the web compose path for a DM, which is
// exactly the RF `/S CALL text` command under the hood (it drives the same
// hub.Private the session's /S does). Body: {"to":"CALL","text":"…"}. A DM is a
// write, so a read-scope lurker is refused (S3); an unknown/offline recipient is
// a 400 (the same "that user is not logged in" outcome /S gives).
func (s *Server) handleDM(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireWrite(w, r) {
		return
	}
	call, ok := s.requireViewer(w, r)
	if !ok {
		return
	}
	// Decode the body ONCE: {to,text} are two fields, so reading each via readField
	// would consume the body on the first read and lose the second.
	var body struct {
		To   string `json:"to"`
		Text string `json:"text"`
	}
	if err := decodeJSON(r, &body); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	to := strings.ToUpper(strings.TrimSpace(body.To))
	text := strings.TrimSpace(body.Text)
	if to == "" || text == "" {
		http.Error(w, "to and text are required", http.StatusBadRequest)
		return
	}
	key := chat.UserKey{Call: call, Node: s.hub.OurNode()}
	if _, ok := s.hub.User(key); !ok {
		// No live stream yet (e.g. a curl client) — make the sender present first,
		// mirroring handleSend, so hub.Private finds them as a known user.
		key = s.presence.enter(call)
		defer s.presence.leave(call)
	}
	if _, err := s.hub.Private(r.Context(), key, to, text); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers ---

func (s *Server) historyFor(ctx context.Context, topic string) []chat.Message {
	msgs, err := s.hub.History(ctx, topic, time.Time{}, 100)
	if err != nil {
		s.log.Warn("history query failed", "topic", topic, "err", err)
		return nil
	}
	return msgs
}

func (s *Server) privateHistoryFor(ctx context.Context, call string) []chat.Message {
	msgs, err := s.hub.PrivateHistory(ctx, call, time.Time{}, 200)
	if err != nil {
		s.log.Warn("private history query failed", "call", call, "err", err)
		return nil
	}
	return msgs
}

func (s *Server) usersFor(topic string) []map[string]string {
	out := make([]map[string]string, 0)
	for _, u := range s.hub.UsersInTopic(topic) {
		out = append(out, map[string]string{"call": u.Call, "node": offNode(u, s.hub.OurNode()), "name": u.Name})
	}
	return out
}

func topicEvent(m chat.Message) wireEvent {
	return wireEvent{Type: "msg", From: m.FromCall, Topic: m.Topic, Text: m.Text, Time: ms(m.Time)}
}

// privateEvent renders a KindPrivate message for a given viewer (S6 DM pane). It
// fills With (the correspondent — the OTHER party, so a thread is keyed the same
// whether the viewer sent or received it) and Mine (true when the viewer is the
// sender), so the browser can bucket and align DMs without re-deriving identity.
func privateEvent(m chat.Message, viewer string) wireEvent {
	mine := strings.EqualFold(m.FromCall, viewer)
	with := m.FromCall
	if mine {
		with = m.ToCall
	}
	return wireEvent{Type: "private", From: m.FromCall, To: m.ToCall, With: with, Mine: mine, Text: m.Text, Time: ms(m.Time)}
}

func offNode(u chat.User, ourNode string) string {
	if u.Origin.Node == ourNode {
		return ""
	}
	return u.Origin.Node
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func writeSSE(w http.ResponseWriter, event string, data any) {
	b, _ := json.Marshal(data)
	_, _ = w.Write([]byte("event: " + event + "\ndata: "))
	_, _ = w.Write(b)
	_, _ = w.Write([]byte("\n\n"))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// decodeJSON unmarshals a JSON request body into v. An empty body decodes to the
// zero value (so an empty settings POST is a no-op flip, not an error); any other
// malformed body is reported so the caller can 400 it.
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		if err == io.EOF {
			return nil // empty body → leave v at its zero value
		}
		return err
	}
	return nil
}

// readField reads a value from either a JSON body ({"text":"…"}) or a form post.
func readField(r *http.Request, field string) string {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		var m map[string]any
		if err := json.NewDecoder(r.Body).Decode(&m); err == nil {
			if v, ok := m[field]; ok {
				return toString(v)
			}
		}
		return ""
	}
	return r.FormValue(field)
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return ""
	}
}
