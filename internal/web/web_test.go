package web

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/m0lte/pdn-bpqchat/internal/chat"
	"github.com/m0lte/pdn-bpqchat/internal/store/sqlite"
)

func slogDiscard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newClaimStore opens a fresh on-disk SQLite store (temp dir) for the claims
// table — the real production implementation, so claim tests exercise the actual
// unique-index collision path and the ErrCallsignClaimed sentinel.
func newClaimStore(t *testing.T) *sqlite.Store {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "claims.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func testServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s, ts, _ := testServerWithClaims(t)
	return s, ts
}

// seedClaim records a pdn-user → callsign mapping directly in the store so a test
// viewer is already claimed (mirrors a returning user). The legacy SSE/topic
// tests use the callsign itself as the pdn user, so seeding call→call lets them
// keep posting under that callsign now that an unclaimed viewer must claim first.
func seedClaim(t *testing.T, s *Server, pdnUser, callsign string) {
	t.Helper()
	if err := s.claims.Claim(context.Background(), pdnUser, callsign, time.Now()); err != nil {
		t.Fatalf("seed claim %s→%s: %v", pdnUser, callsign, err)
	}
}

// testServerWithClaims builds the server with a real SQLite claim store and hands
// it back so claim tests can pre-seed and inspect mappings.
func testServerWithClaims(t *testing.T) (*Server, *httptest.Server, *sqlite.Store) {
	t.Helper()
	hub := chat.NewHub("M0LTE-4", chat.NewMemStore(), nil)
	claims := newClaimStore(t)
	s := New(0, "M0LTE-4", hub, claims, slogDiscard())
	ts := httptest.NewServer(s.srv.Handler)
	t.Cleanup(ts.Close)
	return s, ts, claims
}

// gwPost POSTs body to base+path as a gateway-stamped request, optionally as the
// given viewer (call ""), and returns the response. All app requests arrive via
// the gateway (X-Pdn-Gateway: 1), so the tests must mirror that.
func gwPost(t *testing.T, base, path, call, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
	req.Header.Set("X-Pdn-Gateway", "1")
	req.Header.Set("Content-Type", "application/json")
	if call != "" {
		req.Header.Set("X-Pdn-User", call)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// gwGet GETs base+path as a gateway-stamped request and returns the response.
func gwGet(t *testing.T, base, path string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+path, nil)
	req.Header.Set("X-Pdn-Gateway", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatal("condition not met in time")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// openStream opens an SSE stream as `call` and returns a channel of raw SSE
// lines plus a cancel.
func openStream(t *testing.T, base, call string) (<-chan string, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/events", nil)
	req.Header.Set("X-Pdn-Gateway", "1") // requests reach the app only via the gateway
	if call != "" {
		req.Header.Set("X-Pdn-User", call)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	lines := make(chan string, 256)
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-ctx.Done():
				return
			}
		}
	}()
	return lines, cancel
}

func drainContains(lines <-chan string, substr string, d time.Duration) bool {
	deadline := time.After(d)
	for {
		select {
		case l := <-lines:
			if strings.Contains(l, substr) {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func TestSSEPresenceJoinAndLeave(t *testing.T) {
	s, ts := testServer(t)
	seedClaim(t, s, "G8PZT", "G8PZT")
	lines, cancel := openStream(t, ts.URL, "G8PZT")
	if !drainContains(lines, `"call":"G8PZT"`, 2*time.Second) {
		t.Fatal("no 'you' snapshot for G8PZT")
	}
	waitFor(t, func() bool { return len(s.hub.Users()) == 1 })
	cancel()
	waitFor(t, func() bool { return len(s.hub.Users()) == 0 })
}

func TestSSEDeliversAnotherUsersMessage(t *testing.T) {
	s, ts := testServer(t)
	seedClaim(t, s, "G8PZT", "G8PZT")
	seedClaim(t, s, "M0LTE", "M0LTE")
	lines, cancel := openStream(t, ts.URL, "G8PZT")
	defer cancel()
	drainContains(lines, `"call":"G8PZT"`, time.Second)

	// M0LTE posts via /send; G8PZT's stream must receive it.
	gwPost(t, ts.URL, "/send", "", `{"text":"hi there"}`).Body.Close()
	gwPost(t, ts.URL, "/send", "M0LTE", `{"text":"from m0lte"}`).Body.Close()

	if !drainContains(lines, "from m0lte", 2*time.Second) {
		t.Fatal("G8PZT did not receive M0LTE's message over SSE")
	}
}

func TestSendThenHistory(t *testing.T) {
	_, ts := testServer(t)
	// Post as SYSOP (no identity header → owner).
	r := gwPost(t, ts.URL, "/send", "", `{"text":"logged message"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("send status = %d", r.StatusCode)
	}
	// History must contain it.
	hr := gwGet(t, ts.URL, "/history?topic=General")
	defer hr.Body.Close()
	body := make([]byte, 4096)
	n, _ := hr.Body.Read(body)
	if !strings.Contains(string(body[:n]), "logged message") {
		t.Fatalf("history missing message: %s", body[:n])
	}
}

func TestTopicSwitchIsolation(t *testing.T) {
	s, ts := testServer(t)
	seedClaim(t, s, "G8PZT", "G8PZT")
	seedClaim(t, s, "M0LTE", "M0LTE")
	// G8PZT stays in General; M0LTE holds a stream (its presence) and moves to DX.
	lines, cancel := openStream(t, ts.URL, "G8PZT")
	defer cancel()
	drainContains(lines, `"call":"G8PZT"`, time.Second)
	mlines, mcancel := openStream(t, ts.URL, "M0LTE")
	defer mcancel()
	drainContains(mlines, `"call":"M0LTE"`, time.Second)

	post := func(path, body string) {
		gwPost(t, ts.URL, path, "M0LTE", body).Body.Close()
	}
	post("/topic", `{"topic":"DX"}`)
	waitFor(t, func() bool {
		u, ok := s.hub.User(chat.UserKey{Call: "M0LTE", Node: "M0LTE-4"})
		return ok && strings.EqualFold(u.Topic, "DX")
	})
	post("/send", `{"text":"dx only"}`)

	// G8PZT (General) must NOT see the DX message.
	if drainContains(lines, "dx only", 400*time.Millisecond) {
		t.Fatal("topic isolation broken: General viewer saw DX message")
	}
}
