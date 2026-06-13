package web

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/m0lte/pdn-bpqchat/internal/chat"
)

func slogDiscard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	hub := chat.NewHub("M0LTE-4", chat.NewMemStore(), nil)
	s := New(0, "M0LTE-4", hub, slogDiscard())
	ts := httptest.NewServer(s.srv.Handler)
	t.Cleanup(ts.Close)
	return s, ts
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
	lines, cancel := openStream(t, ts.URL, "G8PZT")
	if !drainContains(lines, `"call":"G8PZT"`, 2*time.Second) {
		t.Fatal("no 'you' snapshot for G8PZT")
	}
	waitFor(t, func() bool { return len(s.hub.Users()) == 1 })
	cancel()
	waitFor(t, func() bool { return len(s.hub.Users()) == 0 })
}

func TestSSEDeliversAnotherUsersMessage(t *testing.T) {
	_, ts := testServer(t)
	lines, cancel := openStream(t, ts.URL, "G8PZT")
	defer cancel()
	drainContains(lines, `"call":"G8PZT"`, time.Second)

	// M0LTE posts via /send; G8PZT's stream must receive it.
	resp, err := http.Post(ts.URL+"/send", "application/json", strings.NewReader(`{"text":"hi there"}`))
	if err != nil {
		t.Fatal(err)
	}
	// Send as M0LTE by setting the header through a manual request.
	resp.Body.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/send", strings.NewReader(`{"text":"from m0lte"}`))
	req.Header.Set("X-Pdn-User", "M0LTE")
	req.Header.Set("Content-Type", "application/json")
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()

	if !drainContains(lines, "from m0lte", 2*time.Second) {
		t.Fatal("G8PZT did not receive M0LTE's message over SSE")
	}
}

func TestSendThenHistory(t *testing.T) {
	_, ts := testServer(t)
	// Post as SYSOP (no identity header → owner).
	r, err := http.Post(ts.URL+"/send", "application/json", strings.NewReader(`{"text":"logged message"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("send status = %d", r.StatusCode)
	}
	// History must contain it.
	hr, err := http.Get(ts.URL + "/history?topic=General")
	if err != nil {
		t.Fatal(err)
	}
	defer hr.Body.Close()
	body := make([]byte, 4096)
	n, _ := hr.Body.Read(body)
	if !strings.Contains(string(body[:n]), "logged message") {
		t.Fatalf("history missing message: %s", body[:n])
	}
}

func TestTopicSwitchIsolation(t *testing.T) {
	s, ts := testServer(t)
	// G8PZT stays in General; M0LTE holds a stream (its presence) and moves to DX.
	lines, cancel := openStream(t, ts.URL, "G8PZT")
	defer cancel()
	drainContains(lines, `"call":"G8PZT"`, time.Second)
	mlines, mcancel := openStream(t, ts.URL, "M0LTE")
	defer mcancel()
	drainContains(mlines, `"call":"M0LTE"`, time.Second)

	post := func(path, body string) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
		req.Header.Set("X-Pdn-User", "M0LTE")
		req.Header.Set("Content-Type", "application/json")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
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
