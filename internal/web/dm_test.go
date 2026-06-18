package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/m0lte/pdn-bpqchat/internal/chat"
)

// TestDMPaneInShell (S6): the served SPA carries the DM pane scaffolding — a DM
// rail section, a new-DM composer, the /dms backfill fetch, and the /dm compose
// POST — and still routes private (/S) commands through the DM path.
func TestDMPaneInShell(t *testing.T) {
	_, ts := testServer(t)
	body := fetchIndex(t, ts.URL)
	for _, want := range []string{
		"Direct messages", // DM rail heading
		"id=\"dmlist\"",   // DM thread list
		"id=\"newdm\"",    // new-DM composer
		"fetch('dms')",    // DM backfill on (re)connect
		"fetch('dm'",      // /dm compose POST
		"function openDM", // open a DM thread
		"function sendDM", // compose path
		"^\\/[sS]\\s",     // /S CALL text command parsing
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("served shell missing %q", want)
		}
	}
}

// TestDMRendersInRecipientStream (S6): a DM addressed to the viewer's claimed
// callsign is delivered over their SSE stream as a private event carrying the
// correspondent (with) and a not-mine flag — exactly what the DM pane buckets on.
func TestDMRendersInRecipientStream(t *testing.T) {
	s, ts := testServer(t)
	seedClaim(t, s, "G8PZT", "G8PZT")
	seedClaim(t, s, "M0LTE", "M0LTE")

	// The recipient holds an SSE stream (their presence); the sender also needs to
	// be present for hub.Private to find them, so open a stream for them too.
	lines, cancel := openStream(t, ts.URL, "G8PZT")
	defer cancel()
	drainContains(lines, `"call":"G8PZT"`, time.Second)
	slines, scancel := openStream(t, ts.URL, "M0LTE")
	defer scancel()
	drainContains(slines, `"call":"M0LTE"`, time.Second)

	// M0LTE DMs G8PZT through the web compose path (/dm = /S under the hood).
	r := gwPost(t, ts.URL, "/dm", "M0LTE", `{"to":"G8PZT","text":"meet on 40m"}`)
	r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("/dm status = %d", r.StatusCode)
	}

	// The recipient's stream carries the DM as a private event with from=M0LTE.
	if !drainContains(lines, "meet on 40m", 2*time.Second) {
		t.Fatal("recipient did not receive the DM over SSE")
	}
}

// dmEvent finds the first private SSE event whose data contains substr and
// returns the decoded wireEvent. Fails the test if none arrives in d.
func dmEvent(t *testing.T, lines <-chan string, substr string, d time.Duration) wireEvent {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case l := <-lines:
			if strings.HasPrefix(l, "data: ") && strings.Contains(l, substr) {
				var we wireEvent
				if err := json.Unmarshal([]byte(strings.TrimPrefix(l, "data: ")), &we); err == nil && we.Type == "private" {
					return we
				}
			}
		case <-deadline:
			t.Fatalf("no private event containing %q", substr)
		}
	}
}

// TestDMShapeFromBothPOVs (S6): a single DM is delivered to BOTH ends, each from
// its own point of view — the recipient sees with=sender/mine=false, the sender
// sees with=recipient/mine=true, so each browser keys the SAME thread and aligns
// the bubble correctly.
func TestDMShapeFromBothPOVs(t *testing.T) {
	s, ts := testServer(t)
	seedClaim(t, s, "G8PZT", "G8PZT")
	seedClaim(t, s, "M0LTE", "M0LTE")

	rxLines, rxCancel := openStream(t, ts.URL, "G8PZT")
	defer rxCancel()
	drainContains(rxLines, `"call":"G8PZT"`, time.Second)
	txLines, txCancel := openStream(t, ts.URL, "M0LTE")
	defer txCancel()
	drainContains(txLines, `"call":"M0LTE"`, time.Second)

	gwPost(t, ts.URL, "/dm", "M0LTE", `{"to":"G8PZT","text":"73 es gd dx"}`).Body.Close()

	rx := dmEvent(t, rxLines, "73 es gd dx", 2*time.Second)
	if rx.Mine || !strings.EqualFold(rx.With, "M0LTE") || !strings.EqualFold(rx.From, "M0LTE") {
		t.Fatalf("recipient POV wrong: %+v", rx)
	}
	tx := dmEvent(t, txLines, "73 es gd dx", 2*time.Second)
	if !tx.Mine || !strings.EqualFold(tx.With, "G8PZT") || !strings.EqualFold(tx.To, "G8PZT") {
		t.Fatalf("sender POV wrong: %+v", tx)
	}
}

// TestDMComposeRoutesThroughPrivate (S6): the compose path drives hub.Private —
// the same engine call the RF /S command makes — rather than posting to a topic.
// We assert the message landed as a KindPrivate in the durable log (not KindTopic)
// and is absent from the topic timeline.
func TestDMComposeRoutesThroughPrivate(t *testing.T) {
	s, ts := testServer(t)
	seedClaim(t, s, "G8PZT", "G8PZT")
	seedClaim(t, s, "M0LTE", "M0LTE")

	// Both present so the private send succeeds (an offline recipient is a 400).
	_, cancel := openStream(t, ts.URL, "G8PZT")
	defer cancel()
	_, scancel := openStream(t, ts.URL, "M0LTE")
	defer scancel()
	waitFor(t, func() bool { return len(s.hub.Users()) == 2 })

	gwPost(t, ts.URL, "/dm", "M0LTE", `{"to":"G8PZT","text":"private only"}`).Body.Close()

	// It is in the private log for G8PZT…
	waitFor(t, func() bool {
		ms, _ := s.hub.PrivateHistory(context.Background(), "G8PZT", time.Time{}, 100)
		for _, m := range ms {
			if m.Kind == chat.KindPrivate && m.Text == "private only" &&
				strings.EqualFold(m.FromCall, "M0LTE") && strings.EqualFold(m.ToCall, "G8PZT") {
				return true
			}
		}
		return false
	})

	// …and NOT in any topic timeline (it never became a KindTopic post).
	hist, _ := s.hub.History(context.Background(), chat.DefaultTopic, time.Time{}, 100)
	for _, m := range hist {
		if m.Text == "private only" {
			t.Fatal("DM leaked into the topic timeline")
		}
	}
}

// TestDMBackfill (S6): GET /dms returns the viewer's persisted DM threads — both
// sent and received — from THEIR point of view, the durable backfill the live SSE
// stream never replays. A returning viewer rebuilds their threads from this.
func TestDMBackfill(t *testing.T) {
	s, ts := testServer(t)
	seedClaim(t, s, "G8PZT", "G8PZT")
	seedClaim(t, s, "M0LTE", "M0LTE")

	// Establish presence for both, exchange a DM each way, then read G8PZT's /dms.
	_, cancel := openStream(t, ts.URL, "G8PZT")
	defer cancel()
	_, scancel := openStream(t, ts.URL, "M0LTE")
	defer scancel()
	waitFor(t, func() bool { return len(s.hub.Users()) == 2 })

	gwPost(t, ts.URL, "/dm", "M0LTE", `{"to":"G8PZT","text":"inbound to pzt"}`).Body.Close()
	gwPost(t, ts.URL, "/dm", "G8PZT", `{"to":"M0LTE","text":"outbound from pzt"}`).Body.Close()
	waitFor(t, func() bool {
		ms, _ := s.hub.PrivateHistory(context.Background(), "G8PZT", time.Time{}, 100)
		return len(ms) == 2
	})

	r := gwGetAs(t, ts.URL, "/dms", "G8PZT")
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("/dms status = %d", r.StatusCode)
	}
	var got []wireEvent
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatalf("decode /dms: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 backfilled DMs, got %d: %+v", len(got), got)
	}
	// Both threads are with M0LTE (the correspondent); the inbound is not-mine, the
	// outbound is mine.
	var sawInbound, sawOutbound bool
	for _, e := range got {
		if e.Type != "private" || !strings.EqualFold(e.With, "M0LTE") {
			t.Fatalf("backfilled DM has wrong correspondent: %+v", e)
		}
		if e.Text == "inbound to pzt" && !e.Mine {
			sawInbound = true
		}
		if e.Text == "outbound from pzt" && e.Mine {
			sawOutbound = true
		}
	}
	if !sawInbound || !sawOutbound {
		t.Fatalf("backfill POV wrong (inbound=%v outbound=%v): %+v", sawInbound, sawOutbound, got)
	}
}

// TestDMLurkerRefused (S6): a read-scope lurker cannot compose a DM — /dm is a
// write, gated by requireWrite exactly like /send (403).
func TestDMLurkerRefused(t *testing.T) {
	s, ts := testServer(t)
	seedClaim(t, s, "G8PZT", "G8PZT")
	seedClaim(t, s, "M0LTE", "M0LTE")

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/dm", strings.NewReader(`{"to":"M0LTE","text":"hush"}`))
	req.Header.Set("X-Pdn-Gateway", "1")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Pdn-User", "G8PZT")
	req.Header.Set("X-Pdn-Scope", "read") // lurker
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("lurker /dm status = %d, want 403", resp.StatusCode)
	}
}

// TestDMOfflineRecipient (S6): a DM to a user who is not connected is a 400 — the
// same outcome the RF /S command gives ("that user is not logged in").
func TestDMOfflineRecipient(t *testing.T) {
	s, ts := testServer(t)
	seedClaim(t, s, "M0LTE", "M0LTE")

	_, scancel := openStream(t, ts.URL, "M0LTE")
	defer scancel()
	waitFor(t, func() bool { return len(s.hub.Users()) == 1 })

	r := gwPost(t, ts.URL, "/dm", "M0LTE", `{"to":"NOBODY","text":"anyone?"}`)
	defer r.Body.Close()
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("/dm to offline user status = %d, want 400", r.StatusCode)
	}
}
