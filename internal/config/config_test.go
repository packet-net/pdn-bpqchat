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
	peers, err := parsePeers(" GB7CHT@127.0.0.1:8010 , gb7rdg@10.0.0.2:8010 ")
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 2 {
		t.Fatalf("got %d peers, want 2", len(peers))
	}
	if peers[0].Call != "GB7CHT" || peers[0].Addr != "127.0.0.1:8010" {
		t.Fatalf("peer[0] = %+v", peers[0])
	}
	if peers[1].Call != "GB7RDG" {
		t.Fatalf("peer[1] call not upper-cased: %q", peers[1].Call)
	}
}

func TestParsePeersEmpty(t *testing.T) {
	if peers, err := parsePeers(""); err != nil || peers != nil {
		t.Fatalf("empty = %v, %v", peers, err)
	}
}

func TestParsePeersBad(t *testing.T) {
	if _, err := parsePeers("nobody-here"); err == nil {
		t.Fatal("entry without @host:port should error")
	}
}
