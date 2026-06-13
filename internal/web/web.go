// Package web serves pdn-bpqchat's loopback web chat (W4): a full browser UI at
// /apps/bpqchat/ — a live SSE message stream, topic switching, presence, history
// from SQLite, and a send box. Web users are first-class chat.Hub users in the
// same topics as RF and (from W5) mesh users.
//
// The server MUST bind loopback only (127.0.0.1): the X-Pdn-* identity headers
// are trustworthy precisely because pdn is the only thing that can reach a
// loopback upstream, and pdn strips any client-supplied copy before injecting
// its own (docs/app-gateway.md §Identity injection).
package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/m0lte/pdn-bpqchat/internal/chat"
)

// Identity is the authenticated viewer the gateway injects per request.
type Identity struct {
	User    string // X-Pdn-User — viewer callsign/username ("" when anonymous)
	Scope   string // X-Pdn-Scope — read | operate | admin
	Gateway bool   // X-Pdn-Gateway — request came through the pdn gateway
}

// IdentityFromRequest reads the gateway-injected identity headers.
func IdentityFromRequest(r *http.Request) Identity {
	return Identity{
		User:    r.Header.Get("X-Pdn-User"),
		Scope:   r.Header.Get("X-Pdn-Scope"),
		Gateway: r.Header.Get("X-Pdn-Gateway") == "1",
	}
}

// viewerCall is the chat callsign for a web request. When auth is off (no
// injected user) the local node owner is the viewer — we use "SYSOP" so the
// owner is a real participant rather than anonymous.
func (id Identity) viewerCall() string {
	if id.User != "" {
		return id.User
	}
	return "SYSOP"
}

// Server is the loopback web chat.
type Server struct {
	port     int
	callsign string
	hub      *chat.Hub
	log      *slog.Logger
	srv      *http.Server
	presence *presence
}

// New builds the web server bound to the chat hub.
func New(port int, callsign string, hub *chat.Hub, log *slog.Logger) *Server {
	s := &Server{
		port:     port,
		callsign: callsign,
		hub:      hub,
		log:      log,
		presence: newPresence(hub),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/events", s.handleEvents)
	mux.HandleFunc("/send", s.handleSend)
	mux.HandleFunc("/topic", s.handleTopic)
	mux.HandleFunc("/users", s.handleUsers)
	mux.HandleFunc("/history", s.handleHistory)
	mux.HandleFunc("/", s.handleIndex)
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return s
}

// Run serves until ctx is cancelled, binding loopback only.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", s.port))
	if err != nil {
		return fmt.Errorf("web: bind 127.0.0.1:%d: %w", s.port, err)
	}
	s.log.Info("web chat listening", "addr", ln.Addr().String())

	serveErr := make(chan error, 1)
	go func() { serveErr <- s.srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.srv.Shutdown(shutCtx)
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = fmt.Fprintln(w, "ok")
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}
