// Command pdn-bpqchat is the supervised daemon for the BPQ-Chat-compatible chat
// node. It wires the layers: the SQLite store and the host-free chat hub, the
// resilient RHP attachment that serves inbound RF users as chat sessions, and
// the loopback web tile. Peering (linking to other chat nodes) lands in W5/W6.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/m0lte/pdn-bpqchat/internal/chat"
	"github.com/m0lte/pdn-bpqchat/internal/config"
	"github.com/m0lte/pdn-bpqchat/internal/node"
	"github.com/m0lte/pdn-bpqchat/internal/store/sqlite"
	"github.com/m0lte/pdn-bpqchat/internal/web"
)

// version is stamped by the release workflow via -ldflags "-X main.version=…";
// "dev" for local builds.
var version = "dev"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		log.Error("configuration error", "err", err)
		os.Exit(1)
	}
	log.Info("pdn-bpqchat starting",
		"version", version,
		"chatCallsign", cfg.ChatCallsign(),
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

	hub := chat.NewHub(cfg.ChatCallsign(), store, nil)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup

	// Loopback web tile (full chat UI in W4).
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := web.New(cfg.WebPort, cfg.ChatCallsign(), hub, log).Run(ctx); err != nil {
			log.Error("web tile stopped", "err", err)
			stop()
		}
	}()

	// RHP attachment: bind the callsign, serve inbound RF users.
	wg.Add(1)
	go func() {
		defer wg.Done()
		link := node.New(node.Options{
			Host:         cfg.RHPHost,
			Port:         cfg.RHPPort,
			User:         cfg.RHPUser,
			Pass:         cfg.RHPPass,
			ChatCallsign: cfg.ChatCallsign(),
		}, hub, log)
		link.Run(ctx)
	}()

	wg.Wait()
	log.Info("pdn-bpqchat stopped")
}
