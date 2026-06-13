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
	// Shortcut form: via:CALL → open to the base node call, then "C CALL".
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
	if len(rf[0].Script) != 1 || rf[0].Script[0] != "C G0BBB-4" {
		t.Fatalf("shortcut script = %v, want [\"C G0BBB-4\"]", rf[0].Script)
	}

	// Multi-hop form: via:PEER|OPEN|CMD|CMD.
	_, rf, err = parsePeers("via:GB7RDG-1|GB7STH|C 1 MB7NCR-2|C RDGCHT")
	if err != nil {
		t.Fatal(err)
	}
	p := rf[0]
	if p.PeerCall != "GB7RDG-1" || p.OpenTo != "GB7STH" {
		t.Fatalf("multihop plan = %+v", p)
	}
	if len(p.Script) != 2 || p.Script[0] != "C 1 MB7NCR-2" || p.Script[1] != "C RDGCHT" {
		t.Fatalf("multihop script = %v", p.Script)
	}

	// Bad: via: with an open target but no command.
	if _, _, err := parsePeers("via:GB7RDG-1|GB7STH"); err == nil {
		t.Fatal("via: with no connect command should error")
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
