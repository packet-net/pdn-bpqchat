package web

import (
	"net/http"

	"github.com/m0lte/pdn-bpqchat/internal/chat"
)

// settingsView is the JSON shape of the settings pane: the viewer's name/QTH and
// the BPQ `/` display flags. It is exactly the slice of the hub User that a web
// flip persists, so a GET reflects what the hub holds and a POST writes it back.
// Field names match the SPA's settings form so the round-trip needs no mapping.
type settingsView struct {
	Name      string `json:"name"`
	QTH       string `json:"qth"`
	Echo      bool   `json:"echo"`
	Bells     bool   `json:"bells"`
	Colour    bool   `json:"colour"`
	ShowNames bool   `json:"shownames"`
	ShowTime  bool   `json:"showtime"`
}

func viewFromUser(u chat.User) settingsView {
	return settingsView{
		Name:      u.Name,
		QTH:       u.QTH,
		Echo:      u.Flags.Echo,
		Bells:     u.Flags.Bells,
		Colour:    u.Flags.Colour,
		ShowNames: u.Flags.ShowNames,
		ShowTime:  u.Flags.ShowTime,
	}
}

// handleSettings is the per-user settings pane (S3): GET reads the viewer's
// current name/QTH/flags from the hub; POST applies a change through hub.SetInfo
// + hub.SetFlags so the web flip becomes the ONE persisted identity every plane
// (RF, web, mesh) observes for that user — not a web-only preference.
//
// POST is a write action, so it is gated on operate+ (requireWrite): a read-scope
// lurker may neither chat nor change identity. The GET is observation only and so
// is open to any resolved viewer (the gateway already 403s a non-gateway probe).
//
// The viewer must be present in the hub to have settings to read/write; for a
// client with no live SSE stream (e.g. curl) we transiently enter presence for
// the duration of the request — mirroring handleSend — so the change still lands
// on a real hub user.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodPost:
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Method == http.MethodPost {
		if !s.requireWrite(w, r) { // changing your identity is a write — lurkers can't (S3)
			return
		}
	}
	call, ok := s.requireViewer(w, r)
	if !ok {
		return
	}
	key := chat.UserKey{Call: call, Node: s.hub.OurNode()}
	if _, present := s.hub.User(key); !present {
		// No live stream (e.g. a curl client) — make them present for this request.
		key = s.presence.enter(call)
		defer s.presence.leave(call)
	}

	if r.Method == http.MethodGet {
		u, _ := s.hub.User(key)
		writeJSON(w, viewFromUser(u))
		return
	}

	// POST: read the desired settings, persist name/QTH and flags into the hub
	// user. Either change emits UserInfoChanged so RF/web/mesh subscribers see the
	// one updated identity. Unparseable bodies fall back to empties (a no-op flip).
	var in settingsView
	if err := decodeJSON(r, &in); err != nil {
		http.Error(w, "bad settings body", http.StatusBadRequest)
		return
	}
	if _, err := s.hub.SetInfo(key, in.Name, in.QTH); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	flags := chat.UserFlags{
		Echo:      in.Echo,
		Bells:     in.Bells,
		Colour:    in.Colour,
		ShowNames: in.ShowNames,
		ShowTime:  in.ShowTime,
	}
	if _, err := s.hub.SetFlags(key, flags); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.log.Info("settings updated", "call", call,
		"name", in.Name, "qth", in.QTH, "flags", flags)
	// Echo the now-current settings back so the SPA can confirm the persisted state.
	u, _ := s.hub.User(key)
	writeJSON(w, viewFromUser(u))
}
