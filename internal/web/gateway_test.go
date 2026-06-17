package web

import (
	"bytes"
	"io"
	"net/http"
	"testing"
)

// TestGatewayTrustRejectsUnstamped: a request that did not arrive through the pdn
// app-gateway (no X-Pdn-Gateway: 1) is refused with 403 — the loopback boundary
// means an unstamped request can only be a direct probe, never a real viewer, so
// we never trust forgeable identity headers on it.
func TestGatewayTrustRejectsUnstamped(t *testing.T) {
	_, ts := testServer(t)
	for _, path := range []string{"/", "/events", "/send", "/history?topic=General", "/users"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s without gateway stamp = %d, want 403", path, resp.StatusCode)
		}
	}
}

// TestHealthzExemptFromGateway: /healthz is the daemon's own loopback liveness
// probe — hit directly, never through the gateway — so it must answer without a
// gateway stamp.
func TestHealthzExemptFromGateway(t *testing.T) {
	_, ts := testServer(t)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz without gateway stamp = %d, want 200", resp.StatusCode)
	}
	// /healthz is exempt from the middleware, so it carries no no-store header.
	if cc := resp.Header.Get("Cache-Control"); cc == "no-store" {
		t.Errorf("/healthz should not be marked no-store, got %q", cc)
	}
}

// TestGatewayResponsesAreNoStore: every gateway-stamped response is marked
// Cache-Control: no-store so a cached server-rendered page can't leak one
// viewer's state to the next.
func TestGatewayResponsesAreNoStore(t *testing.T) {
	_, ts := testServer(t)
	resp := gwGet(t, ts.URL, "/")
	defer resp.Body.Close()
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
}

// TestIndexByteIdenticalWithAndWithoutPrefix: the JS SPA uses relative paths, so
// the served index page must be byte-for-byte identical whether or not the
// gateway sets X-Forwarded-Prefix. (Future server-rendered pages will differ by
// prefix; the SPA never does.)
func TestIndexByteIdenticalWithAndWithoutPrefix(t *testing.T) {
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
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	noPrefix := fetch("")
	withPrefix := fetch("/apps/bpqchat")
	if string(noPrefix) != string(withPrefix) {
		t.Fatalf("index page differs with prefix: %d bytes vs %d bytes", len(noPrefix), len(withPrefix))
	}
	// And it really is the SPA, not an error body.
	if !bytes.Contains(noPrefix, []byte("EventSource")) {
		t.Fatalf("served index is not the SPA: %q", noPrefix[:min(len(noPrefix), 120)])
	}
}

// TestU exercises the absolute-URL helper: empty prefix passes the path through
// (direct loopback), a set prefix mounts it, and slashes are normalised so the
// join always has exactly one.
func TestU(t *testing.T) {
	cases := []struct{ prefix, path, want string }{
		{"", "claim", "/claim"},
		{"", "/claim", "/claim"},
		{"", "", "/"},
		{"/apps/bpqchat", "claim", "/apps/bpqchat/claim"},
		{"/apps/bpqchat", "/claim", "/apps/bpqchat/claim"},
		{"/apps/bpqchat/", "/claim", "/apps/bpqchat/claim"}, // trailing slash on prefix
		{"/apps/bpqchat", "", "/apps/bpqchat/"},             // empty path => mount root
		{"/apps/bpqchat", "admin/peers", "/apps/bpqchat/admin/peers"},
	}
	for _, c := range cases {
		if got := U(c.prefix, c.path); got != c.want {
			t.Errorf("U(%q, %q) = %q, want %q", c.prefix, c.path, got, c.want)
		}
	}
}

// TestPrefixFromContextDefaultsEmpty: with no prefix in the context, the accessor
// returns "" so root-relative URLs render unchanged on direct loopback access.
func TestPrefixFromContextDefaultsEmpty(t *testing.T) {
	if got := PrefixFromContext(nil); got != "" {
		t.Errorf("PrefixFromContext(nil) = %q, want empty", got)
	}
}
