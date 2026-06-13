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
	// RFPeers are outbound peer chat callsigns dialled over AX.25 via RHP (W6).
	RFPeers []string
	// PeerListen, if set (PDN_BPQCHAT_PEER_LISTEN, e.g. "127.0.0.1:18094"), is the
	// TCP address the node accepts inbound IP peer links on (the accept side of the
	// pdn↔pdn IP transport). Empty disables inbound IP peering.
	PeerListen string
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
// entries, each either an IP/telnet peer "CALLSIGN@host:port" or an RF peer
// "rf:CALLSIGN" (dialled over AX.25 via RHP). E.g.
// "GB7CHT@127.0.0.1:8010,rf:GB7RDG-1".
func parsePeers(s string) ([]Peer, []string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil, nil
	}
	var (
		peers   []Peer
		rfPeers []string
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
			rfPeers = append(rfPeers, call)
			continue
		}
		call, addr, ok := strings.Cut(entry, "@")
		call, addr = strings.TrimSpace(call), strings.TrimSpace(addr)
		if !ok || call == "" || addr == "" {
			return nil, nil, fmt.Errorf("config: PDN_BPQCHAT_PEERS entry %q must be CALLSIGN@host:port or rf:CALLSIGN", entry)
		}
		peers = append(peers, Peer{Call: strings.ToUpper(call), Addr: addr})
	}
	return peers, rfPeers, nil
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
