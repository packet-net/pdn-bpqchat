package web

import (
	"bytes"
	"net/http"
	"strings"
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
		"EventSource", // SSE live transport (locked design)
		"new EventSource('events')",
		"fetch('history", // /history backfill on (re)connect
		"post('send'",    // /send POST
		"post('topic'",   // /topic POST (topic switch)
		// New S1 shell structure:
		"nav class=\"channels\"",  // channel/topic switcher rail
		"id=\"chanlist\"",         // channel list target
		"aside class=\"users\"",   // user list
		"form class=\"composer\"", // composer
		"id=\"log\"",              // timeline log
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

// TestShellShipsFederationPanel (S5): the served SPA carries the admin federation
// panel + allow-list editor, wired to the /peers surface (probe to reveal, GET to
// populate, POST /peers/allow to edit). The panel control lives OUTSIDE the appbar
// so it survives the embed-mode appbar hide, keeping it reachable in the pdn slot.
func TestShellShipsFederationPanel(t *testing.T) {
	_, ts := testServer(t)
	page := fetchIndex(t, ts.URL)
	for _, sub := range []string{
		`id="fedbtn"`,         // the floating Federation control
		`id="fedmodal"`,       // the federation panel
		`id="allowlist"`,      // the inbound allow-list editor list
		`id="allowadd"`,       // the add-callsign form
		`id="pinnedlist"`,     // the read-only pinned (dialed) peers
		`id="fednodes"`,       // known mesh-node graph
		`id="fedlinks"`,       // live per-link state/last-seen/RTT
		`id="fedconfigured"`,  // configured outbound peers
		`fetch('peers')`,      // GET /peers (probe + populate)
		`fetch('peers/allow'`, // POST /peers/allow (the editor)
		`probeFed()`,          // admin-scope discovery (reveal the button on 200)
	} {
		if !strings.Contains(string(page), sub) {
			t.Errorf("served shell missing federation-panel marker %q", sub)
		}
	}
	// The control must NOT be nested in the appbar (hidden in embed mode).
	idx := strings.Index(string(page), `id="fedbtn"`)
	headEnd := strings.Index(string(page), `</header>`)
	if idx >= 0 && headEnd >= 0 && idx < headEnd {
		t.Fatal("federation button is inside the appbar; it would be hidden in embed mode")
	}
}

// TestShellRendersOriginBadgesForEveryone (S5, the split decision's everyone-half):
// the SPA renders origin-node badges on off-node users AND off-node messages for
// EVERY viewer (not gated on admin), so a spoofed/foreign-origin identity is always
// visible. The badge is driven by the wire `node` field (offNode()/Origin.Node).
func TestShellRendersOriginBadgesForEveryone(t *testing.T) {
	_, ts := testServer(t)
	page := string(fetchIndex(t, ts.URL))
	// Off-node message badge: the renderer appends @node when e.node is set.
	if !strings.Contains(page, `e.node?'<span class="node"> @'+esc(e.node)`) {
		t.Error("message origin badge (@node) not rendered for off-node messages")
	}
	// Off-node user-list badge.
	if !strings.Contains(page, `u.node?' <span class="node">@'+esc(u.node)`) {
		t.Error("user-list origin badge (@node) not rendered for off-node users")
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
