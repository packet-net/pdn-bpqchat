package chat

// Event is something that happened in the chat domain, fanned out to every
// subscriber (an RF session, a web SSE stream, the peer relay). Concrete events
// below; consumers type-switch. The set mirrors the BPQ record types
// (design.md §3.3) so the peer relay can map events ↔ wire records directly.
type Event interface{ isEvent() }

// UserJoined: a user became present (locally or learned from a peer).
type UserJoined struct{ User User }

// UserLeft: a user left chat.
type UserLeft struct{ User User }

// TopicMessage: a message was posted to a topic (BPQ id_data).
type TopicMessage struct{ Message Message }

// PrivateMessage: a directed message to one user (BPQ id_send).
type PrivateMessage struct{ Message Message }

// TopicChanged: a user moved to a different topic. From is the topic they left.
type TopicChanged struct {
	User User
	From string
}

// UserInfoChanged: a user's name/QTH changed (BPQ id_user).
type UserInfoChanged struct{ User User }

// NodeLinked / NodeUnlinked: the mesh node graph changed (BPQ id_link/id_unlink).
type NodeLinked struct{ Node Node }
type NodeUnlinked struct{ Node Node }

func (UserJoined) isEvent()      {}
func (UserLeft) isEvent()        {}
func (TopicMessage) isEvent()    {}
func (PrivateMessage) isEvent()  {}
func (TopicChanged) isEvent()    {}
func (UserInfoChanged) isEvent() {}
func (NodeLinked) isEvent()      {}
func (NodeUnlinked) isEvent()    {}
