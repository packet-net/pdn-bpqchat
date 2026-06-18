// Command pdn-bpqchat is the supervised daemon for the BPQ-Chat-compatible chat
// node. It wires the layers: the SQLite store and the host-free chat hub, the
// resilient RHP attachment that serves inbound RF users as chat sessions, and
// the loopback web tile. Peering (linking to other chat nodes) lands in W5/W6.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/m0lte/pdn-bpqchat/internal/chat"
	"github.com/m0lte/pdn-bpqchat/internal/config"
	"github.com/m0lte/pdn-bpqchat/internal/node"
	"github.com/m0lte/pdn-bpqchat/internal/peer"
	"github.com/m0lte/pdn-bpqchat/internal/store/sqlite"
	"github.com/m0lte/pdn-bpqchat/internal/web"
)

// version is stamped by the release workflow via -ldflags "-X main.version=…";
// "dev" for local builds.
var version = "dev"

func sprintfMain(format string, a ...any) string { return fmt.Sprintf(format, a...) }

// servePeerListener accepts inbound IP peer links until ctx is cancelled. Each
// accepted connection is handed to peer.ServeInboundIP, which runs the BPQ chat
// node-link handshake, enforces the inbound-peer allow-list, and bridges an
// allow-listed peer to the shared router/hub.
func servePeerListener(ctx context.Context, addr, ourNode string, router *peer.Router, hub *chat.Hub, allow *peer.AllowList, log *slog.Logger) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		log.Error("peer listener failed to bind", "addr", addr, "err", err)
		return
	}
	log.Info("inbound peer listener listening", "addr", ln.Addr().String())
	go func() { <-ctx.Done(); _ = ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return // listener closed on shutdown
			}
			log.Warn("peer accept failed", "err", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go func() {
			log.Info("inbound IP peer connection", "from", conn.RemoteAddr().String())
			if err := peer.ServeInboundIP(ctx, conn, router, hub, ourNode, allow,
				func(f string, a ...any) { log.Info("peer", "msg", sprintfMain(f, a...)) }); err != nil {
				log.Info("inbound IP peer ended", "from", conn.RemoteAddr().String(), "err", err)
			}
		}()
	}
}

// logLevel maps PDN_LOG_LEVEL (debug|info|warn|error) to a slog level; defaults
// to info. Debug surfaces the peer/connect-script trace.
func logLevel() slog.Level {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PDN_LOG_LEVEL"))) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()}))

	cfg, err := config.Load()
	if err != nil {
		log.Error("configuration error", "err", err)
		os.Exit(1)
	}
	log.Info("pdn-bpqchat starting",
		"version", version,
		"chatCallsign", cfg.BoundCallsign(),
		"rhp", cfg.RHPHost, "rhpPort", cfg.RHPPort,
		"webPort", cfg.WebPort, "state", cfg.StateDir)

	// Persistent store under the app state dir.
	if err := os.MkdirAll(cfg.StateDir, 0o750); err != nil {
		log.Error("cannot create state dir", "dir", cfg.StateDir, "err", err)
		os.Exit(1)
	}
	store, err := sqlite.Open(filepath.Join(cfg.StateDir, "bpqchat.db"))
	if err != nil {
		log.Error("cannot open database", "err", err)
		os.Exit(1)
	}
	defer store.Close()

	hub := chat.NewHub(cfg.BoundCallsign(), store, nil)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup

	// Loopback web tile (full chat UI in W4).
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := web.New(cfg.WebPort, cfg.BoundCallsign(), hub, store, log).Run(ctx); err != nil {
			log.Error("web tile stopped", "err", err)
			stop()
		}
	}()

	// Peering: the relay router (shared by RF and IP peers).
	router := peer.NewRouter(hub)
	defer router.Close()

	// Inbound-peer allow-list (design.md §4.1): the single default-deny gate every
	// inbound federation link is checked against — explicit PDN_BPQCHAT_PEER_ALLOW
	// entries plus the callsigns of peers we dial out to (we already trust those).
	// With nothing configured the list is empty and admits NO inbound peer.
	allow := peer.NewAllowList(cfg.EffectiveAllow()...)
	log.Info("inbound peer allow-list (default-deny)",
		"allowed", allow.Entries(), "count", len(allow.Entries()))

	// RHP attachment: bind the callsign; serve inbound RF users and inbound peer
	// links; dial configured RF peers over AX.25.
	wg.Add(1)
	go func() {
		defer wg.Done()
		link := node.New(node.Options{
			Host:             cfg.RHPHost,
			Port:             cfg.RHPPort,
			User:             cfg.RHPUser,
			Pass:             cfg.RHPPass,
			ChatCallsign:     cfg.BoundCallsign(),
			NodeOwnsCallsign: cfg.NodeOwnsCallsign(),
			RFPeers:          cfg.RFPeers,
			Allow:            allow,
		}, hub, router, log)
		link.Run(ctx)
	}()

	// Outbound IP/telnet peer links.
	for _, p := range cfg.Peers {
		p := p
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Info("starting peer link", "peer", p.Call, "addr", p.Addr)
			peer.DialAndServe(ctx, p.Addr, p.Call, cfg.BoundCallsign(), router, hub,
				func(f string, a ...any) { log.Info("peer", "msg", sprintfMain(f, a...)) })
		}()
	}

	// Inbound IP peer listener — the accept side of the pdn↔pdn IP transport,
	// enabling pdn-to-pdn mesh links (and the docs/LAB.md tier-2 cycle test)
	// without an intervening BPQ node. Off unless PDN_BPQCHAT_PEER_LISTEN is set.
	if cfg.PeerListen != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			servePeerListener(ctx, cfg.PeerListen, cfg.BoundCallsign(), router, hub, allow, log)
		}()
	}

	wg.Wait()
	log.Info("pdn-bpqchat stopped")
}
