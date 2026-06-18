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
	"strings"
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

// Pdn management-auth scopes (X-Pdn-Scope), least → most privileged. A request's
// scope is the gateway's verdict on what the authenticated user may do; the app
// trusts it because only the gateway can reach this loopback upstream (web.go
// package doc). The set mirrors pdn's own read|operate|admin ladder.
const (
	ScopeRead    = "read"    // lurker: may observe (SSE/history/users) but not write
	ScopeOperate = "operate" // may post, switch topics, change own settings
	ScopeAdmin   = "admin"   // operate + admin-only surfaces (peer allow-list, topology)
)

// canWrite reports whether an identity may perform a write action — post a
// message, switch topic, or change settings (the operate+ gate, S3).
//
// The single subtlety is the management-auth-off / anonymous path: with no
// X-Pdn-User the gateway sends no scope either, and that viewer is the node owner
// (resolveViewer's degenerate single-user fallback), who must be able to chat.
// So an EMPTY user means auth is off — full access. Only an AUTHENTICATED viewer
// (X-Pdn-User set) is held to the scope ladder, and a read scope (or any value
// short of operate/admin) is a lurker: write actions return 403.
func canWrite(id Identity) bool {
	if strings.TrimSpace(id.User) == "" {
		return true // management auth off → node owner → full access
	}
	switch strings.ToLower(strings.TrimSpace(id.Scope)) {
	case ScopeOperate, ScopeAdmin:
		return true
	default:
		return false
	}
}

// requireWrite enforces canWrite for an authenticated viewer, writing a 403 (and
// returning false) for a read-scope lurker. Handlers that mutate state call this
// before touching the hub so a read-only viewer's POST never reaches the engine.
func (s *Server) requireWrite(w http.ResponseWriter, r *http.Request) bool {
	if canWrite(IdentityFromRequest(r)) {
		return true
	}
	http.Error(w, "forbidden: your access is read-only (lurker)", http.StatusForbidden)
	return false
}

// baseCall strips the AX.25 SSID suffix (BASE-SSID) to yield the operator's
// bare callsign. A callsign base never contains a hyphen, so cut at the first.
func baseCall(callsign string) string {
	if i := strings.IndexByte(callsign, '-'); i >= 0 {
		return callsign[:i]
	}
	return callsign
}

// Server is the loopback web chat.
type Server struct {
	port      int
	callsign  string
	ownerCall string // node base call (chat callsign minus SSID) — the owner's on-air identity
	hub       *chat.Hub
	claims    ClaimStore // pdn-user → claimed callsign mapping (S2); nil disables claims (owner-only)
	log       *slog.Logger
	srv       *http.Server
	presence  *presence
}

// New builds the web server bound to the chat hub. claims is the durable pdn-user
// → callsign mapping (the multi-user web plane, S2); pass nil for a degenerate
// owner-only run (every viewer takes the node-owner fallback).
func New(port int, callsign string, hub *chat.Hub, claims ClaimStore, log *slog.Logger) *Server {
	s := &Server{
		port:      port,
		callsign:  callsign,
		ownerCall: baseCall(callsign),
		hub:       hub,
		claims:    claims,
		log:       log,
		presence:  newPresence(hub),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/events", s.handleEvents)
	mux.HandleFunc("/send", s.handleSend)
	mux.HandleFunc("/topic", s.handleTopic)
	mux.HandleFunc("/users", s.handleUsers)
	mux.HandleFunc("/history", s.handleHistory)
	mux.HandleFunc("/settings", s.handleSettings)
	mux.HandleFunc("/claim", s.handleClaim)
	mux.HandleFunc("/", s.handleIndex)
	// gatewayTrust fronts the whole mux: 403 anything not gateway-stamped (except
	// the daemon's own /healthz probe), capture X-Forwarded-Prefix into context,
	// and mark responses no-store. Server-rendered pages (claim form, admin
	// editor) added in later slices build absolute URLs through U()/u() so they
	// stay inside the app's /apps/bpqchat/ mount.
	s.srv = &http.Server{Handler: gatewayTrust(mux), ReadHeaderTimeout: 10 * time.Second}
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
	// An authenticated pdn user who has not yet claimed a callsign sees the claim
	// form instead of the chat — they have no chat identity to post under yet.
	// Anonymous (auth-off) viewers resolve to the node owner and skip this.
	if _, mapped := s.resolveViewer(r); !mapped {
		s.renderClaimForm(w, r, "")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}
