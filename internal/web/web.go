// Package web serves pdn-bpqchat's loopback web tile. In W0 it is an empty
// placeholder that proves the app-gateway identity contract end to end; the
// full browser chat UI (live stream, channels, presence, history) lands in W4.
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
	"html"
	"log/slog"
	"net"
	"net/http"
	"time"
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

// Server is the loopback web tile.
type Server struct {
	port     int
	callsign string
	log      *slog.Logger
	srv      *http.Server
}

// New builds the tile server for the given chat callsign on the given loopback
// port.
func New(port int, callsign string, log *slog.Logger) *Server {
	s := &Server{port: port, callsign: callsign, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/", s.handleIndex)
	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// Run serves until ctx is cancelled, then shuts down gracefully. It binds
// loopback only.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", s.port))
	if err != nil {
		return fmt.Errorf("web: bind 127.0.0.1:%d: %w", s.port, err)
	}
	s.log.Info("web tile listening", "addr", ln.Addr().String())

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
	id := IdentityFromRequest(r)
	viewer := id.User
	if viewer == "" {
		viewer = "(anonymous)"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, indexHTML, html.EscapeString(s.callsign), html.EscapeString(viewer))
}

// indexHTML is the W0 placeholder tile — it confirms the app is reachable and
// the gateway identity arrives; W4 replaces it with the live chat UI.
const indexHTML = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>pdn-bpqchat</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>body{font:16px/1.5 system-ui,sans-serif;margin:3rem auto;max-width:40rem;padding:0 1rem;color:#222}</style>
</head>
<body>
<h1>pdn-bpqchat</h1>
<p>BPQ-Chat-compatible chat node <strong>%s</strong>.</p>
<p>Signed in as <strong>%s</strong>.</p>
<p>The web chat UI is not built yet (arrives in W4). This placeholder confirms
the app is running and the pdn app-gateway identity contract is wired.</p>
</body>
</html>
`
