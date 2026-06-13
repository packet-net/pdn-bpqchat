package peer

import (
	"context"
	"net"
	"time"

	"github.com/m0lte/pdn-bpqchat/internal/chat"
)

// DialAndServe maintains an outbound peer link to a chat node reachable at a
// TCP address (the telnet/IP node-link transport — design.md §9, W5). It dials,
// runs the link until it drops, then reconnects with exponential backoff until
// ctx is cancelled.
func DialAndServe(ctx context.Context, addr string, peerCall, ourNode string, router *Router, hub *chat.Hub, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	const initial, max = 2 * time.Second, 60 * time.Second
	backoff := initial
	for ctx.Err() == nil {
		if err := dialOnce(ctx, addr, peerCall, ourNode, router, hub, logf); err != nil && ctx.Err() == nil {
			logf("peer %s link ended: %v; retry in %s", peerCall, err, backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > max {
			backoff = max
		}
	}
}

func dialOnce(ctx context.Context, addr, peerCall, ourNode string, router *Router, hub *chat.Hub, logf func(string, ...any)) error {
	d := net.Dialer{Timeout: 15 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	logf("dialled peer %s at %s; linking", peerCall, addr)
	link := NewLink(conn, router, hub, Config{
		PeerCall: peerCall,
		OurNode:  ourNode,
		Outbound: true,
		Log:      logf,
	})
	// Close the conn when ctx is cancelled so Run unblocks.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	return link.Run(ctx)
}
