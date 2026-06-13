package web

import (
	"sync"

	"github.com/m0lte/pdn-bpqchat/internal/chat"
)

// presence refcounts a web viewer's open streams so a user with several browser
// tabs is one hub user: the first stream joins them, the last to close leaves
// them. Keyed by callsign (web users are always at our node).
type presence struct {
	hub *chat.Hub
	mu  sync.Mutex
	ref map[string]int
}

func newPresence(hub *chat.Hub) *presence {
	return &presence{hub: hub, ref: map[string]int{}}
}

// enter registers one stream for call, joining the hub on the first. Returns the
// hub user key.
func (p *presence) enter(call string) chat.UserKey {
	key := chat.UserKey{Call: call, Node: p.hub.OurNode()}
	p.mu.Lock()
	first := p.ref[call] == 0
	p.ref[call]++
	p.mu.Unlock()
	if first {
		if u, err := p.hub.Join(chat.User{Call: call, Origin: chat.Origin{Node: p.hub.OurNode(), Local: true}}); err == nil {
			key = u.Key()
		}
	}
	return key
}

// leave deregisters one stream, leaving the hub when the last closes.
func (p *presence) leave(call string) {
	p.mu.Lock()
	if p.ref[call] > 0 {
		p.ref[call]--
	}
	last := p.ref[call] == 0
	if last {
		delete(p.ref, call)
	}
	p.mu.Unlock()
	if last {
		p.hub.Leave(chat.UserKey{Call: call, Node: p.hub.OurNode()})
	}
}
