package config

import "testing"

func TestChatCallsignDerivation(t *testing.T) {
	c := &Config{NodeCallsign: "M0LTE", SSID: 4}
	if got := c.ChatCallsign(); got != "M0LTE-4" {
		t.Fatalf("ChatCallsign = %q, want M0LTE-4", got)
	}
	c.SSID = 0
	if got := c.ChatCallsign(); got != "M0LTE" {
		t.Fatalf("ChatCallsign(ssid 0) = %q, want M0LTE", got)
	}
}

func TestBoundCallsignPrefersAppCallsign(t *testing.T) {
	// PDN_APP_CALLSIGN set → bind it verbatim, skipping the <node>-<ssid> derive.
	c := &Config{AppCallsign: "GB7CHT-7", NodeCallsign: "M0LTE", SSID: 4}
	if got := c.BoundCallsign(); got != "GB7CHT-7" {
		t.Fatalf("BoundCallsign with AppCallsign = %q, want GB7CHT-7", got)
	}
	if !c.NodeOwnsCallsign() {
		t.Fatal("NodeOwnsCallsign = false with AppCallsign set, want true (no SSID probe)")
	}
}

func TestBoundCallsignFallsBackToDerivation(t *testing.T) {
	// PDN_APP_CALLSIGN absent → keep the derived <node>-<ssid> behaviour.
	c := &Config{NodeCallsign: "M0LTE", SSID: 4}
	if got := c.BoundCallsign(); got != "M0LTE-4" {
		t.Fatalf("BoundCallsign without AppCallsign = %q, want M0LTE-4", got)
	}
	if c.NodeOwnsCallsign() {
		t.Fatal("NodeOwnsCallsign = true with AppCallsign empty, want false (probe allowed)")
	}
}

func TestLoadPrefersAppCallsign(t *testing.T) {
	t.Setenv("PDN_NODE_CALLSIGN", "M0LTE")
	t.Setenv("PDN_APP_CALLSIGN", " gb7cht-7 ") // trimmed + upper-cased
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.AppCallsign != "GB7CHT-7" {
		t.Fatalf("AppCallsign = %q, want GB7CHT-7 (trimmed/upper)", c.AppCallsign)
	}
	if got := c.BoundCallsign(); got != "GB7CHT-7" {
		t.Fatalf("BoundCallsign = %q, want GB7CHT-7", got)
	}
	if !c.NodeOwnsCallsign() {
		t.Fatal("NodeOwnsCallsign = false, want true when PDN_APP_CALLSIGN is set")
	}
}

func TestLoadFallsBackWithoutAppCallsign(t *testing.T) {
	t.Setenv("PDN_NODE_CALLSIGN", "M0LTE")
	t.Setenv("PDN_APP_CALLSIGN", "") // explicitly empty → fallback
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.AppCallsign != "" {
		t.Fatalf("AppCallsign = %q, want empty", c.AppCallsign)
	}
	if got := c.BoundCallsign(); got != "M0LTE-4" {
		t.Fatalf("BoundCallsign = %q, want derived M0LTE-4", got)
	}
	if c.NodeOwnsCallsign() {
		t.Fatal("NodeOwnsCallsign = true, want false when PDN_APP_CALLSIGN is absent")
	}
}

func TestLoadAppCallsignWithoutNodeCallsign(t *testing.T) {
	// A node that reserves a callsign need not supply PDN_NODE_CALLSIGN: with a
	// node-owned callsign there is nothing to derive, so Load must not error.
	t.Setenv("PDN_NODE_CALLSIGN", "")
	t.Setenv("PDN_APP_CALLSIGN", "GB7CHT-7")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load with PDN_APP_CALLSIGN and no node callsign errored: %v", err)
	}
	if got := c.BoundCallsign(); got != "GB7CHT-7" {
		t.Fatalf("BoundCallsign = %q, want GB7CHT-7", got)
	}
}

func TestLoadRequiresNodeCallsignWhenDeriving(t *testing.T) {
	// No PDN_APP_CALLSIGN and no PDN_NODE_CALLSIGN → nothing to bind, must error.
	t.Setenv("PDN_NODE_CALLSIGN", "")
	t.Setenv("PDN_APP_CALLSIGN", "")
	if _, err := Load(); err == nil {
		t.Fatal("Load with neither callsign should error")
	}
}

func TestParsePeers(t *testing.T) {
	peers, rf, err := parsePeers(" GB7CHT@127.0.0.1:8010 , gb7rdg@10.0.0.2:8010 , rf:gb7xyz-1 ")
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 2 {
		t.Fatalf("got %d telnet peers, want 2", len(peers))
	}
	if peers[0].Call != "GB7CHT" || peers[0].Addr != "127.0.0.1:8010" {
		t.Fatalf("peer[0] = %+v", peers[0])
	}
	if peers[1].Call != "GB7RDG" {
		t.Fatalf("peer[1] call not upper-cased: %q", peers[1].Call)
	}
	if len(rf) != 1 || rf[0].PeerCall != "GB7XYZ-1" || rf[0].OpenTo != "GB7XYZ-1" || len(rf[0].Script) != 0 {
		t.Fatalf("rf peers = %+v, want one direct GB7XYZ-1", rf)
	}
}

func TestParsePeersConnectScript(t *testing.T) {
	// Shortcut form: via:CALL → open to the base node call, expect its prompt, "C CALL".
	_, rf, err := parsePeers("via:g0bbb-4")
	if err != nil {
		t.Fatal(err)
	}
	if len(rf) != 1 {
		t.Fatalf("got %d rf peers, want 1", len(rf))
	}
	if rf[0].PeerCall != "G0BBB-4" || rf[0].OpenTo != "G0BBB" {
		t.Fatalf("shortcut plan = %+v", rf[0])
	}
	if len(rf[0].Script) != 1 || rf[0].Script[0].Expect != "G0BBB>" || rf[0].Script[0].Send != "C G0BBB-4" {
		t.Fatalf("shortcut script = %+v", rf[0].Script)
	}

	// Multi-hop expect/send form: via:PEER|OPEN|EXPECT=SEND|EXPECT=SEND.
	_, rf, err = parsePeers("via:GB7RDG-1|GB7STH|GB7STH>=C GB7RDG|GB7RDG>=C RDGCHT")
	if err != nil {
		t.Fatal(err)
	}
	p := rf[0]
	if p.PeerCall != "GB7RDG-1" || p.OpenTo != "GB7STH" {
		t.Fatalf("multihop plan = %+v", p)
	}
	if len(p.Script) != 2 ||
		p.Script[0].Expect != "GB7STH>" || p.Script[0].Send != "C GB7RDG" ||
		p.Script[1].Expect != "GB7RDG>" || p.Script[1].Send != "C RDGCHT" {
		t.Fatalf("multihop script = %+v", p.Script)
	}

	// Bad: via: with an open target but no step.
	if _, _, err := parsePeers("via:GB7RDG-1|GB7STH"); err == nil {
		t.Fatal("via: with no connect step should error")
	}
}

func TestParsePeersEmpty(t *testing.T) {
	if peers, rf, err := parsePeers(""); err != nil || peers != nil || rf != nil {
		t.Fatalf("empty = %v, %v, %v", peers, rf, err)
	}
}

func TestParsePeersBad(t *testing.T) {
	if _, _, err := parsePeers("nobody-here"); err == nil {
		t.Fatal("entry without @host:port should error")
	}
}

// TestParseAllow: the inbound-peer allow-list parses comma- or space-separated
// callsigns, canonicalises them, and rejects malformed entries (design.md §4.1).
func TestParseAllow(t *testing.T) {
	got, err := parseAllow(" gb7ndh-3, GB7WOD-1  gb7wok-1 ")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"GB7NDH-3", "GB7WOD-1", "GB7WOK-1"}
	if len(got) != len(want) {
		t.Fatalf("parsed %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry[%d] = %q, want %q (canonical, in order)", i, got[i], want[i])
		}
	}
}

func TestParseAllowEmpty(t *testing.T) {
	// Empty/unset → nil (the default-deny posture: nothing admitted).
	if got, err := parseAllow("   "); err != nil || got != nil {
		t.Fatalf("empty allow-list = %v, %v; want nil, nil", got, err)
	}
}

func TestParseAllowMalformed(t *testing.T) {
	for _, bad := range []string{
		"GB7NDH-99",  // SSID out of range
		"GB7NDH-",    // empty SSID
		"GB7@NDH",    // illegal punctuation
		"GB7NDH-3-1", // double SSID
		"-3",         // no base
	} {
		if _, err := parseAllow(bad); err == nil {
			t.Fatalf("parseAllow(%q) accepted a malformed entry", bad)
		}
	}
}

// TestEffectiveAllowUnionsOutboundPeers: the effective inbound allow-list folds in
// every configured outbound peer callsign (we already trust a peer we dial) and
// de-duplicates, so an operator never has to list a dialed peer twice.
func TestEffectiveAllowUnionsOutboundPeers(t *testing.T) {
	c := &Config{
		PeerAllow: []string{"GB7WOK-1"},
		Peers:     []Peer{{Call: "GB7NDH-3", Addr: "10.0.0.1:8010"}},
		RFPeers:   []RFPeer{{PeerCall: "GB7WOD-1"}, {PeerCall: "GB7WOK-1"}}, // GB7WOK-1 dup
	}
	got := c.EffectiveAllow()
	want := map[string]bool{"GB7WOK-1": true, "GB7NDH-3": true, "GB7WOD-1": true}
	if len(got) != len(want) {
		t.Fatalf("effective allow = %v, want the 3 unique callsigns %v", got, want)
	}
	for _, cs := range got {
		if !want[cs] {
			t.Fatalf("unexpected effective-allow entry %q", cs)
		}
		delete(want, cs)
	}
	if len(want) != 0 {
		t.Fatalf("missing effective-allow entries: %v", want)
	}
}

// TestDialedPeerCallsigns: the pinned (implicitly-trusted) set is exactly the
// outbound-dialed peers (Peers/RFPeers), de-duplicated, and EXCLUDES the
// operator-editable PeerAllow entries — those are persisted/edited separately, not
// pinned (S4).
func TestDialedPeerCallsigns(t *testing.T) {
	c := &Config{
		PeerAllow: []string{"GB7EDIT-1"}, // editable-only: must NOT appear in the pinned set
		Peers:     []Peer{{Call: "GB7NDH-3", Addr: "10.0.0.1:8010"}},
		RFPeers:   []RFPeer{{PeerCall: "GB7WOD-1"}, {PeerCall: "GB7WOD-1"}}, // dup collapses
	}
	got := c.DialedPeerCallsigns()
	want := map[string]bool{"GB7NDH-3": true, "GB7WOD-1": true}
	if len(got) != len(want) {
		t.Fatalf("dialed peers = %v, want the 2 unique dialed callsigns %v", got, want)
	}
	for _, cs := range got {
		if cs == "GB7EDIT-1" {
			t.Fatal("editable-only PeerAllow entry leaked into the pinned dialed set")
		}
		if !want[cs] {
			t.Fatalf("unexpected dialed-peer entry %q", cs)
		}
	}
}

func TestLoadParsesAllow(t *testing.T) {
	t.Setenv("PDN_NODE_CALLSIGN", "M0LTE")
	t.Setenv("PDN_BPQCHAT_PEER_ALLOW", "gb7ndh-3, gb7wod-1")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.PeerAllow) != 2 || c.PeerAllow[0] != "GB7NDH-3" || c.PeerAllow[1] != "GB7WOD-1" {
		t.Fatalf("PeerAllow = %v, want [GB7NDH-3 GB7WOD-1]", c.PeerAllow)
	}
}

func TestLoadRejectsMalformedAllow(t *testing.T) {
	t.Setenv("PDN_NODE_CALLSIGN", "M0LTE")
	t.Setenv("PDN_BPQCHAT_PEER_ALLOW", "GB7NDH-99")
	if _, err := Load(); err == nil {
		t.Fatal("Load with a malformed PDN_BPQCHAT_PEER_ALLOW should error")
	}
}
