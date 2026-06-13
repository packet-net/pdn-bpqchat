// Package config loads pdn-bpqchat's runtime configuration: the node-supplied
// supervisor environment (RHP endpoint, node callsign, state dir) plus the
// app's own chat.yaml in the state dir. The on-air callsign is DERIVED from
// the node — never hard-coded (HANDOVER.md §2).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Default values for fields not pinned by the environment or chat.yaml.
const (
	DefaultSSID    = 4           // the app lives at an SSID of the node callsign
	DefaultWebPort = 18093       // MUST match ui.upstream in pdn-app.yaml
	DefaultRHPHost = "127.0.0.1" // loopback is the RHP trust boundary
	DefaultRHPPort = 9000        // the RHPv2 convention
	DefaultState   = "/var/lib/packetnet/apps/bpqchat"
)

// Config is the resolved runtime configuration.
type Config struct {
	// NodeCallsign is the node's own callsign (PDN_NODE_CALLSIGN). The chat
	// node's on-air callsign is derived from it.
	NodeCallsign string
	// SSID is the SSID appended to the node callsign to form the chat callsign.
	SSID int
	// RHPHost / RHPPort locate the node's RHPv2 server.
	RHPHost string
	RHPPort int
	// RHPUser / RHPPass authenticate when the node runs requireAuth.
	RHPUser string
	RHPPass string
	// StateDir is the app's writable state directory (SQLite, chat.yaml).
	StateDir string
	// WebPort is the loopback port the web tile binds.
	WebPort int
	// Peers are the IP/telnet outbound peer chat nodes (PDN_BPQCHAT_PEERS).
	Peers []Peer
	// RFPeers are outbound peer chat nodes dialled over AX.25 via RHP (W6),
	// optionally reached by a node-prompt connect script (multi-hop).
	RFPeers []RFPeer
	// PeerListen, if set (PDN_BPQCHAT_PEER_LISTEN, e.g. "127.0.0.1:18094"), is the
	// TCP address the node accepts inbound IP peer links on (the accept side of the
	// pdn↔pdn IP transport). Empty disables inbound IP peering.
	PeerListen string
}

// RFPeer is an outbound peer chat node reached over AX.25 via RHP. For a directly
// reachable peer, OpenTo == PeerCall and Script is empty (a plain RHP open). For a
// peer across the network, OpenTo is the first hop (a node we can open to) and
// Script is the node-prompt connect commands typed to walk to the peer's chat
// app — e.g. open to G0BBB's node prompt, then "C G0BBB-4" (the SSID its chat app
// is registered to). PeerCall is always the peer's chat callsign — the link
// identity used in the BPQ node-link handshake.
type RFPeer struct {
	PeerCall string   // peer chat callsign (link identity)
	OpenTo   string   // RHP open target (first hop); == PeerCall for a direct dial
	OpenPort string   // RHP open port label ("" = the node's first port)
	Script   []string // node-prompt connect commands sent after the open (multi-hop)
}

// Peer is a configured outbound peer chat node reachable over a TCP node-link
// transport (the telnet/IP dev-loop transport, design.md §9).
type Peer struct {
	Call string
	Addr string // host:port
}

// ChatCallsign is the derived on-air callsign: <node>-<ssid>. SSID 0 yields the
// bare node callsign (AX.25 convention).
func (c *Config) ChatCallsign() string {
	if c.SSID <= 0 {
		return c.NodeCallsign
	}
	return fmt.Sprintf("%s-%d", c.NodeCallsign, c.SSID)
}

// Load resolves configuration from the supervisor environment, applying
// documented defaults. It does not read chat.yaml yet (W2 adds the persistent
// store); the W0 daemon needs only the RHP endpoint, callsign, and web port.
func Load() (*Config, error) {
	c := &Config{
		NodeCallsign: strings.TrimSpace(os.Getenv("PDN_NODE_CALLSIGN")),
		SSID:         DefaultSSID,
		RHPHost:      envOr("PDN_RHP_HOST", DefaultRHPHost),
		RHPPort:      envIntOr("PDN_RHP_PORT", DefaultRHPPort),
		RHPUser:      os.Getenv("PDN_RHP_USER"),
		RHPPass:      os.Getenv("PDN_RHP_PASS"),
		StateDir:     envOr("PDN_APP_STATE", DefaultState),
		WebPort:      envIntOr("PDN_WEB_PORT", DefaultWebPort),
		PeerListen:   strings.TrimSpace(os.Getenv("PDN_BPQCHAT_PEER_LISTEN")),
	}
	if c.NodeCallsign == "" {
		return nil, fmt.Errorf("config: PDN_NODE_CALLSIGN is not set (the supervisor must provide the node callsign)")
	}
	if v := os.Getenv("PDN_BPQCHAT_SSID"); v != "" {
		ssid, err := strconv.Atoi(v)
		if err != nil || ssid < 0 || ssid > 15 {
			return nil, fmt.Errorf("config: PDN_BPQCHAT_SSID %q must be an integer 0–15", v)
		}
		c.SSID = ssid
	}
	peers, rfPeers, err := parsePeers(os.Getenv("PDN_BPQCHAT_PEERS"))
	if err != nil {
		return nil, err
	}
	c.Peers = peers
	c.RFPeers = rfPeers
	return c, nil
}

// parsePeers parses PDN_BPQCHAT_PEERS — a comma-separated list of outbound peer
// entries. Each is one of:
//
//   - "CALLSIGN@host:port" — an IP/telnet peer (the pdn↔pdn dev transport).
//   - "rf:CALLSIGN"        — an RF peer dialled DIRECTLY over AX.25 via RHP.
//   - "via:CALLSIGN"       — an RF peer reached by a node-prompt connect script:
//     open to the peer's node (its base callsign), then "C CALLSIGN" to connect
//     through to its chat app (the SSID it is registered to). This is the simple
//     two-node case (PDN ≥0.9.0 connects the node prompt to a local app).
//   - "via:PEERCALL|OPENTARGET|CMD|CMD…" — the multi-hop form: PEERCALL is the
//     peer chat callsign (link identity), OPENTARGET is the first node we open to,
//     and each CMD is a node-prompt line typed to walk onward, the last landing on
//     the chat app (mirrors BPQ's OtherChatNodes connect scripts).
//
// E.g. "GB7CHT@127.0.0.1:8010,rf:GB7RDG-1,via:G0BBB-4,via:GB7RDG-1|GB7STH|C RDGCHT".
func parsePeers(s string) ([]Peer, []RFPeer, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil, nil
	}
	var (
		peers   []Peer
		rfPeers []RFPeer
	)
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if call, ok := strings.CutPrefix(entry, "rf:"); ok {
			call = strings.ToUpper(strings.TrimSpace(call))
			if call == "" {
				return nil, nil, fmt.Errorf("config: PDN_BPQCHAT_PEERS entry %q must be rf:CALLSIGN", entry)
			}
			rfPeers = append(rfPeers, RFPeer{PeerCall: call, OpenTo: call})
			continue
		}
		if spec, ok := strings.CutPrefix(entry, "via:"); ok {
			plan, err := parseVia(spec, entry)
			if err != nil {
				return nil, nil, err
			}
			rfPeers = append(rfPeers, plan)
			continue
		}
		call, addr, ok := strings.Cut(entry, "@")
		call, addr = strings.TrimSpace(call), strings.TrimSpace(addr)
		if !ok || call == "" || addr == "" {
			return nil, nil, fmt.Errorf("config: PDN_BPQCHAT_PEERS entry %q must be CALLSIGN@host:port, rf:CALLSIGN or via:CALLSIGN[|…]", entry)
		}
		peers = append(peers, Peer{Call: strings.ToUpper(call), Addr: addr})
	}
	return peers, rfPeers, nil
}

// parseVia parses the "via:" connect-script forms (see parsePeers). spec is the
// text after "via:"; entry is the whole entry, for error messages.
func parseVia(spec, entry string) (RFPeer, error) {
	fields := strings.Split(spec, "|")
	for i := range fields {
		fields[i] = strings.TrimSpace(fields[i])
	}
	peerCall := strings.ToUpper(fields[0])
	if peerCall == "" {
		return RFPeer{}, fmt.Errorf("config: PDN_BPQCHAT_PEERS entry %q: via: needs a peer callsign", entry)
	}
	switch {
	case len(fields) == 1:
		// Shortcut: open to the peer's node (base call), then connect to its app.
		return RFPeer{PeerCall: peerCall, OpenTo: baseCall(peerCall), Script: []string{"C " + peerCall}}, nil
	case len(fields) >= 3:
		openTo := strings.ToUpper(fields[1])
		if openTo == "" {
			return RFPeer{}, fmt.Errorf("config: PDN_BPQCHAT_PEERS entry %q: via: needs an open target", entry)
		}
		var script []string
		for _, cmd := range fields[2:] {
			if cmd != "" {
				script = append(script, cmd)
			}
		}
		if len(script) == 0 {
			return RFPeer{}, fmt.Errorf("config: PDN_BPQCHAT_PEERS entry %q: via: needs at least one connect command", entry)
		}
		return RFPeer{PeerCall: peerCall, OpenTo: openTo, Script: script}, nil
	default:
		return RFPeer{}, fmt.Errorf("config: PDN_BPQCHAT_PEERS entry %q must be via:CALL or via:PEERCALL|OPENTARGET|CMD…", entry)
	}
}

// baseCall strips an AX.25 SSID suffix (BASE-SSID) to yield the bare node call.
func baseCall(callsign string) string {
	if i := strings.IndexByte(callsign, '-'); i >= 0 {
		return callsign[:i]
	}
	return callsign
}

// DBPath is the SQLite path under the state dir (used from W2 on).
func (c *Config) DBPath() string { return filepath.Join(c.StateDir, "bpqchat.db") }

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
