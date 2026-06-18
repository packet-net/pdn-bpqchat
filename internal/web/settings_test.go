package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/m0lte/pdn-bpqchat/internal/chat"
)

// gwPostScoped POSTs a JSON body as a gateway-stamped request for a given pdn
// user AND scope (X-Pdn-Scope) — the management-auth verdict the gateway injects.
// An empty scope mirrors auth being off for that header.
func gwPostScoped(t *testing.T, base, path, user, scope, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
	req.Header.Set("X-Pdn-Gateway", "1")
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.Header.Set("X-Pdn-User", user)
	}
	if scope != "" {
		req.Header.Set("X-Pdn-Scope", scope)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// gwGetScoped GETs a gateway-stamped request for a given pdn user and scope.
func gwGetScoped(t *testing.T, base, path, user, scope string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+path, nil)
	req.Header.Set("X-Pdn-Gateway", "1")
	if user != "" {
		req.Header.Set("X-Pdn-User", user)
	}
	if scope != "" {
		req.Header.Set("X-Pdn-Scope", scope)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestShellShipsSettingsPane (S3): the served SPA carries the settings pane — a
// name/QTH field set plus the BPQ display flags — and wires it to the /settings
// surface (GET to prefill, POST to persist). The settings control lives OUTSIDE
// the appbar so it survives the embed-mode appbar hide (the pdn slot supplies its
// own chrome), keeping the pane reachable in the real embedded deployment.
func TestShellShipsSettingsPane(t *testing.T) {
	_, ts := testServer(t)
	page := fetchIndex(t, ts.URL)
	for _, sub := range []string{
		`id="settingsbtn"`,              // the floating settings control
		`id="settingsform"`,             // the settings form
		`id="set-name"`, `id="set-qth"`, // name + QTH
		`id="set-echo"`, `id="set-bells"`, // BPQ display flags …
		`id="set-colour"`, `id="set-shownames"`, `id="set-showtime"`,
		`fetch('settings')`,                // GET prefill
		`fetch('settings', {method:'POST'`, // POST persist
	} {
		if !strings.Contains(string(page), sub) {
			t.Errorf("served shell missing settings-pane marker %q", sub)
		}
	}
	// The control must NOT be nested in the appbar (which is hidden in embed mode),
	// so it stays usable inside the pdn slot.
	idx := strings.Index(string(page), `id="settingsbtn"`)
	headEnd := strings.Index(string(page), `</header>`)
	if idx >= 0 && headEnd >= 0 && idx < headEnd {
		t.Fatal("settings button is inside the appbar; it would be hidden in embed mode")
	}
}

// TestReadScopeIsLurker (S3): a read-scope viewer is a lurker — every write
// endpoint (/send, /topic, /settings) refuses their POST with 403, even though
// they hold a valid claim. Read scope may observe, never write.
func TestReadScopeIsLurker(t *testing.T) {
	s, ts := testServer(t)
	seedClaim(t, s, "lurker@pdn", "M0RDR")

	for _, c := range []struct{ path, body string }{
		{"/send", `{"text":"shh"}`},
		{"/topic", `{"topic":"DX"}`},
		{"/settings", `{"name":"Nope"}`},
	} {
		resp := gwPostScoped(t, ts.URL, c.path, "lurker@pdn", "read", c.body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("read-scope POST %s = %d, want 403", c.path, resp.StatusCode)
		}
	}
	// Nothing the lurker tried should have reached the hub: no message logged.
	hist := s.historyFor(t.Context(), "General")
	if len(hist) != 0 {
		t.Fatalf("read-scope POST leaked into history: %+v", hist)
	}
}

// TestReadScopeMayObserveSettings (S3): the scope gate is on WRITES only — a
// read-scope viewer may still GET their settings (pure observation, like
// history/users). It is the POST that is gated.
func TestReadScopeMayObserveSettings(t *testing.T) {
	s, ts := testServer(t)
	seedClaim(t, s, "lurker@pdn", "M0RDR")
	resp := gwGetScoped(t, ts.URL, "/settings", "lurker@pdn", "read")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read-scope GET /settings = %d, want 200", resp.StatusCode)
	}
}

// TestOperateScopeMaySend (S3): operate scope is the write threshold — an
// operate-scope viewer's /send succeeds (204) and the message reaches the hub.
func TestOperateScopeMaySend(t *testing.T) {
	s, ts := testServer(t)
	seedClaim(t, s, "op@pdn", "M0OPR")
	resp := gwPostScoped(t, ts.URL, "/send", "op@pdn", "operate", `{"text":"operate can talk"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("operate-scope /send = %d, want 204", resp.StatusCode)
	}
	hist := s.historyFor(t.Context(), "General")
	if len(hist) != 1 || hist[0].Text != "operate can talk" {
		t.Fatalf("operate-scope message not logged: %+v", hist)
	}
}

// TestAdminScopeMaySend (S3): admin scope is operate+ and so may write too.
func TestAdminScopeMaySend(t *testing.T) {
	s, ts := testServer(t)
	seedClaim(t, s, "boss@pdn", "M0ADM")
	resp := gwPostScoped(t, ts.URL, "/send", "boss@pdn", "admin", `{"text":"admin too"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("admin-scope /send = %d, want 204", resp.StatusCode)
	}
}

// TestAnonymousOwnerNotGatedByScope (S3): with management auth off (no X-Pdn-User)
// the viewer is the node owner and may write regardless of scope — the gate is for
// AUTHENTICATED viewers, never the degenerate single-user owner path.
func TestAnonymousOwnerNotGatedByScope(t *testing.T) {
	_, ts := testServer(t)
	// No user, no scope (auth off) — owner posts fine.
	resp := gwPostScoped(t, ts.URL, "/send", "", "", `{"text":"owner speaks"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("anonymous owner /send = %d, want 204", resp.StatusCode)
	}
}

// TestSettingsPersistToHubUser (S3): a settings POST writes name/QTH AND the BPQ
// display flags into the live hub user — the one identity RF/web/mesh peers
// observe — not a web-only preference. The response echoes the persisted state.
func TestSettingsPersistToHubUser(t *testing.T) {
	s, ts := testServer(t)
	seedClaim(t, s, "paula@pdn", "G8PZT")

	// Hold a stream so the user is a live hub member while we change settings.
	lines, cancel := openStream(t, ts.URL, "paula@pdn")
	defer cancel()
	drainContains(lines, `"call":"G8PZT"`, 2*time.Second)
	waitFor(t, func() bool {
		_, ok := s.hub.User(chat.UserKey{Call: "G8PZT", Node: "M0LTE-4"})
		return ok
	})

	payload := `{"name":"Paula","qth":"Kidderminster","echo":true,"bells":false,"colour":true,"shownames":true,"showtime":true}`
	resp := gwPostScoped(t, ts.URL, "/settings", "paula@pdn", "operate", payload)
	got := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("settings POST = %d, want 200; body=%s", resp.StatusCode, got)
	}
	// Response echoes the now-current persisted state.
	for _, want := range []string{`"name":"Paula"`, `"qth":"Kidderminster"`, `"echo":true`, `"colour":true`, `"bells":false`} {
		if !strings.Contains(got, want) {
			t.Fatalf("settings response missing %s:\n%s", want, got)
		}
	}

	// And it landed on the live hub user — what RF/mesh peers see.
	u, ok := s.hub.User(chat.UserKey{Call: "G8PZT", Node: "M0LTE-4"})
	if !ok {
		t.Fatal("user gone from hub after settings change")
	}
	if u.Name != "Paula" || u.QTH != "Kidderminster" {
		t.Fatalf("name/QTH not persisted: name=%q qth=%q", u.Name, u.QTH)
	}
	want := chat.UserFlags{Echo: true, Bells: false, Colour: true, ShowNames: true, ShowTime: true}
	if u.Flags != want {
		t.Fatalf("flags not persisted: got %+v want %+v", u.Flags, want)
	}

	// A subsequent GET reflects the persisted state (round-trip truth).
	gr := gwGetScoped(t, ts.URL, "/settings", "paula@pdn", "operate")
	gb := body(t, gr)
	if !strings.Contains(gb, `"qth":"Kidderminster"`) || !strings.Contains(gb, `"colour":true`) {
		t.Fatalf("settings GET did not reflect persisted state:\n%s", gb)
	}
}

// TestSettingsForCurlClientTransientPresence (S3): a client with no live SSE
// stream (e.g. curl) can still change settings — the handler enters presence for
// the request so the change lands on a real hub user.
func TestSettingsForCurlClientTransientPresence(t *testing.T) {
	s, ts := testServer(t)
	seedClaim(t, s, "curl@pdn", "M0CRL")
	resp := gwPostScoped(t, ts.URL, "/settings", "curl@pdn", "operate", `{"name":"Headless"}`)
	got := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("curl settings POST = %d, want 200; body=%s", resp.StatusCode, got)
	}
	if !strings.Contains(got, `"name":"Headless"`) {
		t.Fatalf("curl settings response missing name:\n%s", got)
	}
}
