package session

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/m0lte/pdn-bpqchat/internal/chat"
)

// Conn is the session's output side: bytes back to the user, and a close. The
// daemon adapts an RHP child handle to this; tests use a buffer. Send may block
// on a slow link — the session calls it from its own goroutine, never the RHP
// read loop.
type Conn interface {
	Send(data []byte) error
	Close() error
}

// EndReason explains why a session ended.
type EndReason int

const (
	// EndPeerClosed: the user disconnected (RHP close).
	EndPeerClosed EndReason = iota
	// EndBye: the user typed /B (return to node) or /QUIT (disconnect).
	EndBye
	// EndQuit: the user typed /QUIT — the caller should also drop the link.
	EndQuit
)

// Session is one connected RF user. It assembles input lines, parses the
// BPQ-Chat `/command` set, drives the hub, and renders hub events back to the
// user. Created with New (which joins the hub); fed bytes with Deliver; stopped
// with Close.
type Session struct {
	hub  *chat.Hub
	conn Conn
	key  chat.UserKey
	asm  *LineAssembler
	log  func(string, ...any)

	sub    <-chan chat.Event
	cancel func()

	inbound chan []byte
	done    chan struct{}
	endOnce sync.Once
	ended   chan EndReason
}

// New creates a session for an inbound user identified by callsign, joins them
// to the hub at our node, sends the welcome, and starts the run loop. local is
// true for a directly-connected RF/web user.
func New(hub *chat.Hub, conn Conn, callsign string, logf func(string, ...any)) (*Session, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	sub, cancel := hub.Subscribe()
	s := &Session{
		hub:     hub,
		conn:    conn,
		key:     chat.UserKey{Call: strings.ToUpper(strings.TrimSpace(callsign)), Node: hub.OurNode()},
		asm:     NewLineAssembler(MaxLine),
		log:     logf,
		sub:     sub,
		cancel:  cancel,
		inbound: make(chan []byte, 32),
		done:    make(chan struct{}),
		ended:   make(chan EndReason, 1),
	}
	u, err := hub.Join(chat.User{Call: callsign, Origin: chat.Origin{Node: hub.OurNode(), Local: true}})
	if err != nil {
		cancel()
		return nil, err
	}
	s.key = u.Key()
	s.greet()
	go s.run()
	return s, nil
}

// Deliver hands received bytes to the session (called from the RHP read loop —
// it only enqueues, never blocks on the hub or the conn).
func (s *Session) Deliver(data []byte) {
	select {
	case s.inbound <- append([]byte(nil), data...):
	case <-s.done:
	}
}

// Close ends the session: leaves the hub, unsubscribes, and stops the run loop.
// Idempotent.
func (s *Session) Close() {
	s.end(EndPeerClosed)
}

// Ended returns a channel that delivers the end reason once, when the session
// stops (so the daemon can drop or keep the RHP link accordingly).
func (s *Session) Ended() <-chan EndReason { return s.ended }

func (s *Session) end(reason EndReason) {
	s.endOnce.Do(func() {
		close(s.done)
		s.cancel()
		s.hub.Leave(s.key)
		s.ended <- reason
	})
}

func (s *Session) run() {
	defer s.conn.Close()
	for {
		select {
		case <-s.done:
			return
		case data := <-s.inbound:
			for _, line := range s.asm.Feed(data) {
				if stop := s.processLine(line); stop {
					return
				}
			}
		case ev, ok := <-s.sub:
			if !ok {
				s.end(EndPeerClosed)
				return
			}
			s.render(ev)
		}
	}
}

// processLine handles one input line. Returns true if the session should stop.
func (s *Session) processLine(line string) bool {
	line = sanitise(line)
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	if trimmed[0] == '/' {
		return s.command(trimmed)
	}
	// Plain text → a message to the user's current topic.
	if _, err := s.hub.Post(context.Background(), s.key, trimmed); err != nil {
		s.sendf("*** %s", errText(err))
	}
	return false
}

// command parses and executes a BPQ-Chat `/command` (design.md §3.5). Returns
// true to stop the session (/B, /QUIT).
func (s *Session) command(line string) bool {
	// Split the verb from the rest.
	verb, rest := line, ""
	if i := strings.IndexByte(line, ' '); i >= 0 {
		verb, rest = line[:i], strings.TrimSpace(line[i+1:])
	}
	switch strings.ToUpper(verb) {
	case "/U":
		s.showUsers()
	case "/T":
		s.topic(rest)
	case "/N":
		s.setName(rest)
	case "/Q":
		s.setQTH(rest)
	case "/S":
		s.private(rest)
	case "/P", "/K":
		s.showNodes()
	case "/H", "/?":
		s.help()
	case "/B":
		s.sendLine("*** Returning to node.")
		s.end(EndBye)
		return true
	case "/QUIT":
		s.sendLine("*** 73!")
		s.end(EndQuit)
		return true
	default:
		s.sendLine("*** Unknown command. /H for help.")
	}
	return false
}

func (s *Session) topic(name string) {
	if name == "" {
		var b strings.Builder
		b.WriteString("Topics:\r")
		for _, t := range s.hub.Topics() {
			fmt.Fprintf(&b, "  %s (%d)\r", t.Name, t.Members)
		}
		s.send(b.String())
		return
	}
	changed, err := s.hub.SetTopic(s.key, name)
	if err != nil {
		s.sendf("*** %s", errText(err))
		return
	}
	if u, ok := s.hub.User(s.key); ok {
		if changed {
			s.sendf("*** Now in topic %s.", u.Topic)
		} else {
			s.sendf("*** Already in topic %s.", u.Topic)
		}
	}
}

func (s *Session) setName(name string) {
	if name == "" {
		if u, ok := s.hub.User(s.key); ok {
			s.sendf("Name is %s", u.Name)
		}
		return
	}
	if _, err := s.hub.SetInfo(s.key, name, s.qth()); err == nil {
		s.sendf("Name set to %s", name)
	}
}

func (s *Session) setQTH(qth string) {
	if qth == "" {
		s.sendf("QTH is %s", s.qth())
		return
	}
	if _, err := s.hub.SetInfo(s.key, s.name(), qth); err == nil {
		s.sendf("QTH set to %s", qth)
	}
}

func (s *Session) private(rest string) {
	to, text, ok := strings.Cut(rest, " ")
	if !ok || strings.TrimSpace(text) == "" {
		s.sendLine("*** Usage: /S CALL message")
		return
	}
	if _, err := s.hub.Private(context.Background(), s.key, to, strings.TrimSpace(text)); err != nil {
		s.sendLine("*** That user is not logged in.")
	}
}

func (s *Session) showUsers() {
	u, _ := s.hub.User(s.key)
	var b strings.Builder
	fmt.Fprintf(&b, "Users in %s:\r", u.Topic)
	for _, m := range s.hub.UsersInTopic(u.Topic) {
		who := m.Call
		if m.Origin.Node != s.hub.OurNode() {
			who = fmt.Sprintf("%s @ %s", m.Call, m.Origin.Node)
		}
		fmt.Fprintf(&b, "  %s\r", who)
	}
	s.send(b.String())
}

func (s *Session) showNodes() {
	nodes := s.hub.Nodes()
	if len(nodes) == 0 {
		s.sendLine("No linked nodes.")
		return
	}
	var b strings.Builder
	b.WriteString("Linked nodes:\r")
	for _, n := range nodes {
		fmt.Fprintf(&b, "  %s%s\r", n.Call, alias(n.Alias))
	}
	s.send(b.String())
}

func (s *Session) help() {
	s.send("Commands (upper or lower case):\r" +
		"/U - show users    /T [topic] - show/join topic\r" +
		"/N [name] - name    /Q [qth] - QTH\r" +
		"/S call msg - private message\r" +
		"/P /K - show linked nodes\r" +
		"/B - back to node   /QUIT - disconnect   /H - help\r")
}

// --- rendering hub events to the user ---

func (s *Session) render(ev chat.Event) {
	switch e := ev.(type) {
	case chat.TopicMessage:
		if s.inMyTopic(e.Message.Topic) {
			s.sendLine(formatMsg(e.Message.FromCall, e.Message.Text))
		}
	case chat.PrivateMessage:
		if strings.EqualFold(e.Message.ToCall, s.key.Call) {
			s.sendLine(fmt.Sprintf("<*%s*> %s", e.Message.FromCall, e.Message.Text))
		}
	case chat.UserJoined:
		if e.User.Key() != s.key && s.inMyTopic(e.User.Topic) {
			s.sendf("*** %s has joined %s", e.User.Call, e.User.Topic)
		}
	case chat.UserLeft:
		if e.User.Key() != s.key {
			s.sendf("*** %s has left", e.User.Call)
		}
	case chat.TopicChanged:
		// Announce arrivals into the topic I'm in.
		if e.User.Key() != s.key && s.inMyTopic(e.User.Topic) {
			s.sendf("*** %s has joined %s", e.User.Call, e.User.Topic)
		}
	case chat.NodeLinked:
		s.sendf("*** Link to node %s established", e.Node.Call)
	case chat.NodeUnlinked:
		s.sendf("*** Link to node %s lost", e.Node.Call)
	}
}

func (s *Session) inMyTopic(topic string) bool {
	u, ok := s.hub.User(s.key)
	return ok && strings.EqualFold(u.Topic, topic)
}

func (s *Session) greet() {
	s.send("[BPQCHATSERVER-pdn]\r")
	s.sendf("Welcome to the %s chat node, %s.", s.hub.OurNode(), s.key.Call)
	s.sendLine("You are in topic " + chat.DefaultTopic + ". Type /H for help.")
	s.showUsers()
}

// --- output helpers ---

func (s *Session) send(text string) {
	if err := s.conn.Send([]byte(text)); err != nil {
		s.log("session send failed: %v", err)
		s.end(EndPeerClosed)
	}
}

func (s *Session) sendLine(text string) { s.send(text + "\r") }

func (s *Session) sendf(format string, a ...any) { s.sendLine(fmt.Sprintf(format, a...)) }

func (s *Session) name() string {
	if u, ok := s.hub.User(s.key); ok {
		return u.Name
	}
	return ""
}

func (s *Session) qth() string {
	if u, ok := s.hub.User(s.key); ok {
		return u.QTH
	}
	return ""
}

func formatMsg(from, text string) string { return fmt.Sprintf("<%s> %s", from, text) }

func alias(a string) string {
	if a == "" {
		return ""
	}
	return " (" + a + ")"
}

func errText(err error) string {
	switch err {
	case chat.ErrNoSuchUser:
		return "You are not logged in."
	case chat.ErrEmptyText:
		return "Nothing to send."
	case chat.ErrNoSuchTopic:
		return "No such topic."
	default:
		return err.Error()
	}
}
