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
	return c, nil
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
