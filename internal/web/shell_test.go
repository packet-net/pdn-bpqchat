package web

import (
	"bytes"
	"net/http"
	"testing"
)

// fetchIndex GETs the served SPA shell as a gateway-stamped request and returns
// its body.
func fetchIndex(t *testing.T, base string) []byte {
	t.Helper()
	resp := gwGet(t, base, "/")
	defer resp.Body.Close()
	buf := make([]byte, 64*1024)
	n := 0
	for {
		m, err := resp.Body.Read(buf[n:])
		n += m
		if err != nil || n == len(buf) {
			break
		}
	}
	return buf[:n]
}

// TestShellRendersSlotChrome (S1): the redesigned SPA shell ships the slot-mode
// scaffolding — a channel/topic switcher, a user list, a timeline log, and a
// composer — and is still driven by the existing SSE/REST surface (relative
// fetches to events/send/topic/history). It must also handle the pdn slot
// embed: the ?pdn_embed=1 detection that hides its own outer chrome.
func TestShellRendersSlotChrome(t *testing.T) {
	_, ts := testServer(t)
	body := fetchIndex(t, ts.URL)

	// Live transport is still SSE driving an append-only timeline.
	mustContain := []string{
		"EventSource",      // SSE live transport (locked design)
		"new EventSource('events')",
		"fetch('history",   // /history backfill on (re)connect
		"post('send'",      // /send POST
		"post('topic'",     // /topic POST (topic switch)
		// New S1 shell structure:
		"nav class=\"channels\"", // channel/topic switcher rail
		"id=\"chanlist\"",        // channel list target
		"aside class=\"users\"",  // user list
		"form class=\"composer\"", // composer
		"id=\"log\"",             // timeline log
		// Slot mode: chrome-less embed detection.
		"pdn_embed",
		"classList.add('embed')",
	}
	for _, sub := range mustContain {
		if !bytes.Contains(body, []byte(sub)) {
			t.Errorf("served shell missing %q", sub)
		}
	}
}

// TestShellEmbedHidesAppbar (S1): the embed CSS rule must hide the SPA's own
// outer header when running inside the pdn slot, so there is no double chrome.
func TestShellEmbedHidesAppbar(t *testing.T) {
	_, ts := testServer(t)
	body := fetchIndex(t, ts.URL)
	if !bytes.Contains(body, []byte("body.embed header.appbar { display: none; }")) {
		t.Fatal("shell does not hide its appbar in embed mode")
	}
}

// TestShellByteIdenticalAcrossPrefix (S1): the SPA uses only relative fetch
// paths, so the served shell must be byte-for-byte identical whether or not the
// gateway sets X-Forwarded-Prefix (the SPA never depends on the prefix; only
// server-rendered pages in later slices do).
func TestShellByteIdenticalAcrossPrefix(t *testing.T) {
	_, ts := testServer(t)
	fetch := func(prefix string) []byte {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
		req.Header.Set("X-Pdn-Gateway", "1")
		if prefix != "" {
			req.Header.Set("X-Forwarded-Prefix", prefix)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		buf := make([]byte, 64*1024)
		n := 0
		for {
			m, err := resp.Body.Read(buf[n:])
			n += m
			if err != nil || n == len(buf) {
				break
			}
		}
		return buf[:n]
	}
	if !bytes.Equal(fetch(""), fetch("/apps/bpqchat")) {
		t.Fatal("shell differs across X-Forwarded-Prefix; SPA must be prefix-independent")
	}
}
