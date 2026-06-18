package web

import (
	"context"
	"encoding/json"
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
		if strings.EqualFold(e.Message.ToCall, key.Call) {
			return wireEvent{Type: "private", From: e.Message.FromCall, To: e.Message.ToCall, Text: e.Message.Text, Time: ms(e.Message.Time)}, true
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

// --- helpers ---

func (s *Server) historyFor(ctx context.Context, topic string) []chat.Message {
	msgs, err := s.hub.History(ctx, topic, time.Time{}, 100)
	if err != nil {
		s.log.Warn("history query failed", "topic", topic, "err", err)
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
