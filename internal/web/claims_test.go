package web

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// gwGetAs GETs base+path as a gateway-stamped request for the given pdn user
// (empty = anonymous / auth-off). It does NOT follow redirects so a 303 from the
// claim flow can be asserted.
func gwGetAs(t *testing.T, base, path, user string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+path, nil)
	req.Header.Set("X-Pdn-Gateway", "1")
	if user != "" {
		req.Header.Set("X-Pdn-User", user)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// gwPostForm POSTs an x-www-form-urlencoded body as the given pdn user, NOT
// following redirects (the claim success path 303-redirects to the app root).
func gwPostForm(t *testing.T, base, path, user, form string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+path, strings.NewReader(form))
	req.Header.Set("X-Pdn-Gateway", "1")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if user != "" {
		req.Header.Set("X-Pdn-User", user)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestUnmappedViewerSeesClaimForm (S2): an authenticated pdn user with no claim
// yet gets the claim form at the app root instead of the SPA — they have no chat
// identity to post under until they claim a callsign.
func TestUnmappedViewerSeesClaimForm(t *testing.T) {
	_, ts := testServer(t)
	resp := gwGetAs(t, ts.URL, "/", "alice@pdn")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unmapped index status = %d, want 200", resp.StatusCode)
	}
	page := body(t, resp)
	if !strings.Contains(page, "Claim your callsign") {
		t.Fatalf("unmapped viewer did not get the claim form:\n%s", page)
	}
	// It must be the claim page, not the SPA.
	if strings.Contains(page, "new EventSource('events')") {
		t.Fatal("unmapped viewer got the chat SPA, not the claim form")
	}
	// The form action must post to /claim (built through the gateway prefix helper).
	if !strings.Contains(page, `action="/claim"`) {
		t.Fatalf("claim form action not /claim:\n%s", page)
	}
	// The pdn user is shown (and html-escaped — it is untrusted text).
	if !strings.Contains(page, "alice@pdn") {
		t.Fatalf("claim form does not name the pdn user:\n%s", page)
	}
}

// TestClaimFormActionRespectsGatewayPrefix (S2): the claim form posts inside the
// app's public mount — the form action carries the X-Forwarded-Prefix so the
// browser does not POST to pdn's own root (the bbs claim-405 class of bug).
func TestClaimFormActionRespectsGatewayPrefix(t *testing.T) {
	_, ts := testServer(t)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	req.Header.Set("X-Pdn-Gateway", "1")
	req.Header.Set("X-Pdn-User", "bob@pdn")
	req.Header.Set("X-Forwarded-Prefix", "/apps/bpqchat")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)
	if !strings.Contains(page, `action="/apps/bpqchat/claim"`) {
		t.Fatalf("claim form action did not carry the gateway prefix:\n%s", page)
	}
}

// TestClaimedViewerSeesChat (S2): once a pdn user has claimed a callsign, the app
// root serves the chat SPA, and their resolved chat identity is the claimed call.
func TestClaimedViewerSeesChat(t *testing.T) {
	s, ts := testServer(t)
	seedClaim(t, s, "alice@pdn", "M0ABC")

	resp := gwGetAs(t, ts.URL, "/", "alice@pdn")
	page := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claimed index status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(page, "new EventSource('events')") {
		t.Fatalf("claimed viewer did not get the chat SPA:\n%s", page[:min(len(page), 200)])
	}
	// The SSE 'you' snapshot must carry the CLAIMED callsign, not the pdn user.
	lines, cancel := openStream(t, ts.URL, "alice@pdn")
	defer cancel()
	if !drainContains(lines, `"call":"M0ABC"`, 2*time.Second) {
		t.Fatal("claimed viewer's stream did not identify as the claimed callsign M0ABC")
	}
}

// TestClaimFlowUpsertsThenRedirects (S2): a valid claim POST records the mapping
// and 303-redirects back to the app root (so a refresh shows the chat), with the
// Location built inside the gateway mount.
func TestClaimFlowUpsertsThenRedirects(t *testing.T) {
	s, ts := testServer(t)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/claim", strings.NewReader("callsign=g0xyz"))
	req.Header.Set("X-Pdn-Gateway", "1")
	req.Header.Set("X-Pdn-User", "carol@pdn")
	req.Header.Set("X-Forwarded-Prefix", "/apps/bpqchat")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("claim POST status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/apps/bpqchat/" {
		t.Fatalf("claim redirect Location = %q, want /apps/bpqchat/", loc)
	}
	// The mapping is persisted and canonicalised (upper-cased base).
	got, ok, err := s.claims.ClaimedCall(context.Background(), "carol@pdn")
	if err != nil || !ok || got != "G0XYZ" {
		t.Fatalf("claim not recorded as G0XYZ: got=%q ok=%v err=%v", got, ok, err)
	}
}

// TestClaimStripsSSID (S2): a claim is the SSID-less base identity — an SSID
// suffix in the form input is dropped before storage.
func TestClaimStripsSSID(t *testing.T) {
	s, ts := testServer(t)
	resp := gwPostForm(t, ts.URL, "/claim", "dave@pdn", "callsign=M0LTE-7")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("claim with SSID status = %d, want 303", resp.StatusCode)
	}
	got, _, _ := s.claims.ClaimedCall(context.Background(), "dave@pdn")
	if got != "M0LTE" {
		t.Fatalf("SSID not stripped: stored %q, want M0LTE", got)
	}
}

// TestClaimCollisionIs409 (S2): a callsign already claimed by one pdn user cannot
// be claimed by another — the cross-account collision is a 409, and the original
// owner keeps the callsign.
func TestClaimCollisionIs409(t *testing.T) {
	s, ts := testServer(t)
	seedClaim(t, s, "alice@pdn", "M0ABC")

	resp := gwPostForm(t, ts.URL, "/claim", "mallory@pdn", "callsign=M0ABC")
	page := body(t, resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("cross-account claim status = %d, want 409", resp.StatusCode)
	}
	if !strings.Contains(page, "already claimed") {
		t.Fatalf("409 page does not explain the collision:\n%s", page)
	}
	// Case-insensitive collision too: lower-case spelling of the same call.
	resp = gwPostForm(t, ts.URL, "/claim", "mallory@pdn", "callsign=m0abc")
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("case-folded cross-account claim status = %d, want 409", resp.StatusCode)
	}
	// Mallory still has no claim; Alice still owns M0ABC.
	if _, ok, _ := s.claims.ClaimedCall(context.Background(), "mallory@pdn"); ok {
		t.Fatal("collision should not have recorded a claim for mallory")
	}
	if got, _, _ := s.claims.ClaimedCall(context.Background(), "alice@pdn"); got != "M0ABC" {
		t.Fatalf("original owner lost the callsign: %q", got)
	}
}

// TestClaimReclaimSameCallIsIdempotent (S2): a user re-claiming the callsign they
// already hold succeeds (no spurious self-collision 409).
func TestClaimReclaimSameCallIsIdempotent(t *testing.T) {
	s, ts := testServer(t)
	seedClaim(t, s, "alice@pdn", "M0ABC")
	resp := gwPostForm(t, ts.URL, "/claim", "alice@pdn", "callsign=M0ABC")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("self re-claim status = %d, want 303", resp.StatusCode)
	}
}

// TestClaimChangeOwnCallsign (S2): a user may change their own claim to a free
// callsign (upsert), and the old callsign is released for others.
func TestClaimChangeOwnCallsign(t *testing.T) {
	s, ts := testServer(t)
	seedClaim(t, s, "alice@pdn", "M0ABC")
	resp := gwPostForm(t, ts.URL, "/claim", "alice@pdn", "callsign=M0DEF")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("change own callsign status = %d, want 303", resp.StatusCode)
	}
	if got, _, _ := s.claims.ClaimedCall(context.Background(), "alice@pdn"); got != "M0DEF" {
		t.Fatalf("callsign not changed: %q, want M0DEF", got)
	}
	// The released M0ABC can now be claimed by someone else.
	resp = gwPostForm(t, ts.URL, "/claim", "bob@pdn", "callsign=M0ABC")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("re-claim of released callsign status = %d, want 303", resp.StatusCode)
	}
}

// TestClaimRejectsBadCallsign (S2): the looks-like-a-callsign gate rejects inputs
// that are too short, non-alphanumeric, or carry no digit — re-showing the form
// with a 400 and recording nothing.
func TestClaimRejectsBadCallsign(t *testing.T) {
	s, ts := testServer(t)
	for _, bad := range []string{"AB", "ABCD", "M0 LTE", "M0!E", ""} {
		resp := gwPostForm(t, ts.URL, "/claim", "eve@pdn", "callsign="+bad)
		page := body(t, resp)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("bad callsign %q status = %d, want 400", bad, resp.StatusCode)
		}
		if !strings.Contains(page, "Claim your callsign") {
			t.Fatalf("rejected claim %q did not re-show the form:\n%s", bad, page)
		}
	}
	if _, ok, _ := s.claims.ClaimedCall(context.Background(), "eve@pdn"); ok {
		t.Fatal("a rejected callsign must not record a claim")
	}
}

// TestAnonymousViewerIsOwner (S2): with management auth off (no X-Pdn-User) the
// viewer degrades to the node-owner base callsign — the single-user path — and
// gets the chat directly, never the claim form.
func TestAnonymousViewerIsOwner(t *testing.T) {
	_, ts := testServer(t)
	resp := gwGetAs(t, ts.URL, "/", "")
	page := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous index status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(page, "new EventSource('events')") {
		t.Fatal("anonymous viewer did not get the chat SPA")
	}
	if strings.Contains(page, "Claim your callsign") {
		t.Fatal("anonymous viewer was shown the claim form")
	}
	// The owner identity is the node base callsign (M0LTE-4 → M0LTE).
	lines, cancel := openStream(t, ts.URL, "")
	defer cancel()
	if !drainContains(lines, `"call":"M0LTE"`, 2*time.Second) {
		t.Fatal("anonymous viewer did not resolve to the node-owner base callsign")
	}
}

// TestAnonymousClaimPostRejected (S2): a claim POST with no pdn identity (auth
// off) is a 400 — there is no account to map a callsign to.
func TestAnonymousClaimPostRejected(t *testing.T) {
	_, ts := testServer(t)
	resp := gwPostForm(t, ts.URL, "/claim", "", "callsign=M0ZZZ")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("anonymous claim POST status = %d, want 400", resp.StatusCode)
	}
}

// TestUnclaimedViewerCannotChat (S2): the live/action endpoints refuse an
// authenticated-but-unclaimed viewer with 403 — the defensive guard behind the
// claim-form gate (the SPA never reaches them while unclaimed).
func TestUnclaimedViewerCannotChat(t *testing.T) {
	_, ts := testServer(t)
	for _, c := range []struct {
		method, path, body string
	}{
		{http.MethodPost, "/send", `{"text":"nope"}`},
		{http.MethodPost, "/topic", `{"topic":"DX"}`},
	} {
		resp := gwPost(t, ts.URL, c.path, "frank@pdn", c.body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s %s unclaimed = %d, want 403", c.method, c.path, resp.StatusCode)
		}
	}
	// And the SSE stream refuses too.
	resp := gwGetAs(t, ts.URL, "/events", "frank@pdn")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unclaimed /events = %d, want 403", resp.StatusCode)
	}
}

// TestLooksLikeCallsign (S2): the validator unit — per the spec, at least 3
// alphanumerics including a digit; spaces, punctuation, and SSID hyphens are
// rejected (the SSID is stripped upstream, so a hyphen reaching the validator is
// invalid). Surrounding whitespace is trimmed before the check.
func TestLooksLikeCallsign(t *testing.T) {
	good := []string{"M0LTE", "G8PZT", "2E0ABC", "VK2A", "AB1", " M0LTE ", "lower0case"}
	bad := []string{"", "AB", "ABCD", "M0 LTE", "M0-7", "M0!E", "ABC"}
	for _, c := range good {
		if !looksLikeCallsign(c) {
			t.Errorf("looksLikeCallsign(%q) = false, want true", c)
		}
	}
	for _, c := range bad {
		if looksLikeCallsign(c) {
			t.Errorf("looksLikeCallsign(%q) = true, want false", c)
		}
	}
}
