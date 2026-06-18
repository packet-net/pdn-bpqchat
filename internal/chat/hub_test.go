package chat

import (
	"context"
	"testing"
	"time"
)

func testHub(t *testing.T) *Hub {
	t.Helper()
	return NewHub("M0LTE-4", NewMemStore(), func() time.Time { return time.Unix(1_700_000_000, 0) })
}

// nextEvent reads one event with a timeout so a missing emit fails fast.
func nextEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
		return nil
	}
}

func TestJoinLandsInGeneral(t *testing.T) {
	h := testHub(t)
	ch, cancel := h.Subscribe()
	defer cancel()

	u, err := h.Join(User{Call: "g8pzt", Origin: Origin{Node: "M0LTE-4", Local: true}})
	if err != nil {
		t.Fatal(err)
	}
	if u.Call != "G8PZT" {
		t.Fatalf("call not canonicalised: %q", u.Call)
	}
	if u.Topic != DefaultTopic {
		t.Fatalf("landed in %q, want %q", u.Topic, DefaultTopic)
	}
	ev, ok := nextEvent(t, ch).(UserJoined)
	if !ok || ev.User.Call != "G8PZT" {
		t.Fatalf("want UserJoined G8PZT, got %#v", ev)
	}
	if got := h.UsersInTopic("general"); len(got) != 1 { // case-insensitive
		t.Fatalf("General has %d users, want 1", len(got))
	}
}

func TestPostFanOutAndHistory(t *testing.T) {
	h := testHub(t)
	key := UserKey{Call: "G8PZT", Node: "M0LTE-4"}
	if _, err := h.Join(User{Call: key.Call, Origin: Origin{Node: key.Node, Local: true}}); err != nil {
		t.Fatal(err)
	}
	ch, cancel := h.Subscribe()
	defer cancel()

	m, err := h.Post(context.Background(), key, "hello world")
	if err != nil {
		t.Fatal(err)
	}
	if m.Topic != DefaultTopic || m.Text != "hello world" {
		t.Fatalf("message = %+v", m)
	}
	ev, ok := nextEvent(t, ch).(TopicMessage)
	if !ok || ev.Message.ID != m.ID {
		t.Fatalf("want TopicMessage id %s, got %#v", m.ID, ev)
	}

	hist, err := h.History(context.Background(), "General", time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 || hist[0].Text != "hello world" {
		t.Fatalf("history = %+v", hist)
	}
}

func TestPostEmptyAndUnknownUser(t *testing.T) {
	h := testHub(t)
	key := UserKey{Call: "G8PZT", Node: "M0LTE-4"}
	if _, err := h.Post(context.Background(), key, "hi"); err != ErrNoSuchUser {
		t.Fatalf("post by absent user: err = %v, want ErrNoSuchUser", err)
	}
	_, _ = h.Join(User{Call: key.Call, Origin: Origin{Node: key.Node, Local: true}})
	if _, err := h.Post(context.Background(), key, "   "); err != ErrEmptyText {
		t.Fatalf("empty post: err = %v, want ErrEmptyText", err)
	}
}

func TestSetTopicMovesUser(t *testing.T) {
	h := testHub(t)
	key := UserKey{Call: "G8PZT", Node: "M0LTE-4"}
	_, _ = h.Join(User{Call: key.Call, Origin: Origin{Node: key.Node, Local: true}})
	ch, cancel := h.Subscribe()
	defer cancel()

	changed, err := h.SetTopic(key, "DX")
	if err != nil || !changed {
		t.Fatalf("SetTopic: changed=%v err=%v", changed, err)
	}
	ev, ok := nextEvent(t, ch).(TopicChanged)
	if !ok || ev.From != DefaultTopic || ev.User.Topic != "DX" {
		t.Fatalf("want TopicChanged General->DX, got %#v", ev)
	}
	if len(h.UsersInTopic("General")) != 0 || len(h.UsersInTopic("dx")) != 1 {
		t.Fatal("membership not moved")
	}
	// Re-joining the same topic (different case) is a no-op.
	if changed, _ := h.SetTopic(key, "dx"); changed {
		t.Fatal("re-join same topic should not change")
	}
}

func TestPrivateRequiresPresentTarget(t *testing.T) {
	h := testHub(t)
	from := UserKey{Call: "G8PZT", Node: "M0LTE-4"}
	_, _ = h.Join(User{Call: from.Call, Origin: Origin{Node: from.Node, Local: true}})

	if _, err := h.Private(context.Background(), from, "M0LTE", "hi"); err != ErrNoSuchUser {
		t.Fatalf("private to absent target: err = %v, want ErrNoSuchUser", err)
	}
	_, _ = h.Join(User{Call: "M0LTE", Origin: Origin{Node: "GB7XYZ", Link: "GB7XYZ"}})
	ch, cancel := h.Subscribe()
	defer cancel()
	m, err := h.Private(context.Background(), from, "m0lte", "secret")
	if err != nil {
		t.Fatal(err)
	}
	ev, ok := nextEvent(t, ch).(PrivateMessage)
	if !ok || ev.Message.ToCall != "M0LTE" || ev.Message.ID != m.ID {
		t.Fatalf("want PrivateMessage to M0LTE, got %#v", ev)
	}
}

func TestLeaveRemovesUser(t *testing.T) {
	h := testHub(t)
	key := UserKey{Call: "G8PZT", Node: "M0LTE-4"}
	_, _ = h.Join(User{Call: key.Call, Origin: Origin{Node: key.Node, Local: true}})
	ch, cancel := h.Subscribe()
	defer cancel()

	h.Leave(key)
	if _, ok := nextEvent(t, ch).(UserLeft); !ok {
		t.Fatal("want UserLeft")
	}
	if len(h.Users()) != 0 {
		t.Fatal("user still present after leave")
	}
	h.Leave(key) // idempotent
}

func TestUnlinkNodeDropsRemoteUsers(t *testing.T) {
	h := testHub(t)
	h.LinkNode("GB7XYZ", "XYZ", "6.0")
	// A user learned via the GB7XYZ link.
	_, _ = h.Join(User{Call: "M0ABC", Origin: Origin{Node: "GB7XYZ", Link: "GB7XYZ"}})
	// A local user that must survive.
	_, _ = h.Join(User{Call: "G8PZT", Origin: Origin{Node: "M0LTE-4", Local: true}})

	if !h.UnlinkNode("GB7XYZ") {
		t.Fatal("UnlinkNode returned false for known node")
	}
	users := h.Users()
	if len(users) != 1 || users[0].Call != "G8PZT" {
		t.Fatalf("after unlink users = %+v, want only G8PZT", users)
	}
	if len(h.Nodes()) != 0 {
		t.Fatal("node not removed")
	}
}

func TestSetInfoEmitsOnChange(t *testing.T) {
	h := testHub(t)
	key := UserKey{Call: "G8PZT", Node: "M0LTE-4"}
	_, _ = h.Join(User{Call: key.Call, Origin: Origin{Node: key.Node, Local: true}})
	ch, cancel := h.Subscribe()
	defer cancel()

	changed, _ := h.SetInfo(key, "Paula", "Kidderminster")
	if !changed {
		t.Fatal("SetInfo should report change")
	}
	if ev, ok := nextEvent(t, ch).(UserInfoChanged); !ok || ev.User.Name != "Paula" {
		t.Fatalf("want UserInfoChanged Paula, got %#v", ev)
	}
	if changed, _ := h.SetInfo(key, "Paula", "Kidderminster"); changed {
		t.Fatal("no-op SetInfo should report no change")
	}
}

// TestSetFlagsPersistsAndEmits (S3): a flag flip lands on the live hub user (so
// RF/web/mesh observe one identity) and emits UserInfoChanged; re-applying the
// same flags is a no-op (no spurious event).
func TestSetFlagsPersistsAndEmits(t *testing.T) {
	h := testHub(t)
	key := UserKey{Call: "G8PZT", Node: "M0LTE-4"}
	if _, err := h.Join(User{Call: key.Call, Origin: Origin{Node: key.Node, Local: true}}); err != nil {
		t.Fatal(err)
	}
	ch, cancel := h.Subscribe()
	defer cancel()

	want := UserFlags{Echo: true, Bells: true, Colour: false, ShowNames: true, ShowTime: true}
	changed, err := h.SetFlags(key, want)
	if err != nil || !changed {
		t.Fatalf("SetFlags changed=%v err=%v, want true/nil", changed, err)
	}
	if ev, ok := nextEvent(t, ch).(UserInfoChanged); !ok || ev.User.Flags != want {
		t.Fatalf("want UserInfoChanged with %+v, got %#v", want, ev)
	}
	// The change is durable on the live user the RF/mesh plane reads.
	if u, ok := h.User(key); !ok || u.Flags != want {
		t.Fatalf("hub user flags = %+v (ok=%v), want %+v", u.Flags, ok, want)
	}
	if changed, _ := h.SetFlags(key, want); changed {
		t.Fatal("no-op SetFlags should report no change")
	}
}

// TestSetFlagsUnknownUser (S3): setting flags for an absent user is an error, not
// a silent create.
func TestSetFlagsUnknownUser(t *testing.T) {
	h := testHub(t)
	if _, err := h.SetFlags(UserKey{Call: "NOBODY", Node: "M0LTE-4"}, UserFlags{Echo: true}); err == nil {
		t.Fatal("SetFlags on an unknown user should error")
	}
}
