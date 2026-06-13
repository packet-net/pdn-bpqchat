// Command pdn-bpqchat is the supervised daemon for the BPQ-Chat-compatible
// chat node. In W0 it is deliberately a do-nothing skeleton: it attaches to the
// node over RHPv2, binds its derived callsign and listens (so inbound RF users
// reach it), and serves an empty loopback web tile. The chat domain, the RF
// command interface, the web chat, and peering land in later waves — gated on
// docs/design.md (HANDOVER.md §7).
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/m0lte/pdn-bpqchat/internal/config"
	"github.com/m0lte/pdn-bpqchat/internal/rhp"
	"github.com/m0lte/pdn-bpqchat/internal/web"
)

// version is the build's informational version, stamped by the release
// workflow via -ldflags "-X main.version=…"; "dev" for local builds.
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
		"rhp", cfg.RHPHost,
		"rhpPort", cfg.RHPPort,
		"webPort", cfg.WebPort,
		"state", cfg.StateDir)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup

	// The loopback web tile.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := web.New(cfg.WebPort, cfg.ChatCallsign(), log).Run(ctx); err != nil {
			log.Error("web tile stopped", "err", err)
			stop() // a fatal web error takes the daemon down
		}
	}()

	// The resilient RHP attachment: bind the callsign and listen.
	wg.Add(1)
	go func() {
		defer wg.Done()
		runLink(ctx, cfg, log)
	}()

	wg.Wait()
	log.Info("pdn-bpqchat stopped")
}

// runLink keeps an RHPv2 attachment up: connect → (auth) → socket/bind/listen
// for the chat callsign, then hold until the link drops, reconnecting with
// exponential backoff (mirrors pdn-convers's RhpNodeLink discipline).
func runLink(ctx context.Context, cfg *config.Config, log *slog.Logger) {
	const (
		initialBackoff = 1 * time.Second
		maxBackoff     = 60 * time.Second
	)
	backoff := initialBackoff

	for ctx.Err() == nil {
		if err := attachOnce(ctx, cfg, log); err != nil && ctx.Err() == nil {
			log.Warn("RHP link down; will reconnect", "err", err, "in", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// attachOnce runs one connection's lifetime and returns when it ends.
func attachOnce(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	h := &skeletonHandler{log: log, accepted: make(chan int, 16)}

	connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	client, err := rhp.Connect(connectCtx, cfg.RHPHost, cfg.RHPPort, h)
	cancel()
	if err != nil {
		return err
	}
	defer client.Close()
	h.client = client

	if cfg.RHPUser != "" {
		if err := client.Authenticate(ctx, cfg.RHPUser, cfg.RHPPass); err != nil {
			return err
		}
	}

	handle, err := client.Socket(ctx)
	if err != nil {
		return err
	}
	// W0 binds the preferred callsign only; the SSID probe-walk (rhp.IsCallsignInUse)
	// lands with the node-link layer in a later wave.
	if err := client.Bind(ctx, handle, cfg.ChatCallsign(), ""); err != nil {
		return err
	}
	if err := client.Listen(ctx, handle); err != nil {
		return err
	}
	log.Info("bound and listening for inbound connects", "callsign", cfg.ChatCallsign())

	// Drain accepted children: in W0 we greet and politely close them (the chat
	// session machinery is W3). Running this off the read loop avoids deadlock.
	go h.drain(ctx)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-client.Done():
		return client.Err()
	}
}

// skeletonHandler is the W0 do-nothing inbound handler: it accepts connections,
// greets them, and closes them. OnAccept/OnRecv run on the client read loop, so
// they only enqueue work — the actual RHP calls happen in drain.
type skeletonHandler struct {
	log      *slog.Logger
	client   *rhp.Client
	accepted chan int
}

func (h *skeletonHandler) OnAccept(_, child int, remote, _, _ string) {
	h.log.Info("inbound connection", "from", remote, "handle", child)
	select {
	case h.accepted <- child:
	default:
		h.log.Warn("accept backlog full; dropping", "handle", child)
	}
}

func (h *skeletonHandler) OnRecv(int, []byte) {}
func (h *skeletonHandler) OnStatus(int, int)  {}
func (h *skeletonHandler) OnClose(handle int) { h.log.Debug("socket closed", "handle", handle) }

func (h *skeletonHandler) drain(ctx context.Context) {
	const notice = "pdn-bpqchat node is online but the chat service is not built yet.\r" +
		"See https://github.com/m0lte/pdn-bpqchat\r"
	for {
		select {
		case <-ctx.Done():
			return
		case child := <-h.accepted:
			if err := h.client.Send(ctx, child, []byte(notice)); err != nil && !errors.Is(err, context.Canceled) {
				h.log.Debug("greet failed", "handle", child, "err", err)
			}
			_ = h.client.CloseHandle(ctx, child)
		}
	}
}
