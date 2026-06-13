# pdn-bpqchat — design

**Status:** W0 (Go scaffold + RHPv2 client + do-nothing daemon) **done**; W1
(vendor BPQ source + this spec) **done — awaiting review**. This document is
the **gate before W2 protocol code** (HANDOVER.md §7): no chat-domain or
wire-protocol implementation lands until the wire spec and the loop-control
design below are reviewed.

**Read order:** `HANDOVER.md` → this file → the vendored ground truth under
`reference/linbpq-chat/` (provenance in its `PROVENANCE.md`).

Everything in §3 (the BPQ wire protocols) is **derived from the LinBPQ source**,
not assumed; each claim cites the file/function it came from so the analysis is
reproducible. pdn-bpqchat is **clean-room Go** — it implements *to* this wire,
it does not port the C.

---

## 1. What we are building (recap)

A BPQ-Chat-compatible chat node, shipped as a **default-off pdn app package**,
with four pillars (HANDOVER.md §4): an **RF chat node** (local users over AX.25
through pdn), a **full web chat** (`/apps/bpqchat/`), **peering** with BPQ and
other pdn-bpqchat nodes, and **oracle-first BPQ interop**. RF users and web
users share the same topics.

## 2. Architecture (how it fits pdn — non-negotiable)

Strictly layered, **app-package-only**: the daemon reaches the node **solely
over RHPv2** (`internal/rhp`), never in-process. Mirrors pdn-bbs/pdn-convers
discipline; the language is Go (single static binary — HANDOVER.md language
decision).

```
cmd/pdn-bpqchat            the supervised daemon (wires the layers, owns lifecycle)
internal/config            supervisor env + chat.yaml → resolved Config; derives the callsign
internal/rhp               the RHPv2 client  (W0 ✅ — framing, codec, client, tests)
internal/chat   (W2)       the chat domain: topics, users, presence, message log, SQLite. Pure, host-free.
internal/session (W3)      RF user sessions: BPQ-compatible /command parser (hostile-input-safe), line assembler
internal/web    (W4)       the browser chat UI: SSE stream, channels, presence, history, send; app-gateway identity
internal/peer   (W5/W6)    PeerLink: the BPQ link protocol; loop/dup suppression; link lifecycle; multi-peer
```

**Layering rule:** `internal/chat` is the pure core (no RHP, no HTTP, no
sockets) — the same hub both `session` (RF) and `web` feed into, and that
`peer` propagates to/from. This is the seam that lets RF + web + mesh share one
room and lets the core be unit-tested without a node.

**Callsign:** derived as `<PDN_NODE_CALLSIGN>-<ssid>` (default SSID 4), never
hard-coded (`internal/config`). The supervisor provides `PDN_NODE_CALLSIGN` /
`PDN_RHP_HOST` / `PDN_RHP_PORT` / `PDN_APP_STATE`.

**RHP usage (the two paths the chat node needs):**
- **Inbound RF users / inbound peer links** — `socket`→`bind`(chat callsign, all
  ports)→`listen`; inbound connections arrive as `accept` pushes. One bound
  callsign serves *both* human users and linking nodes; they are told apart by
  their first line (§3.2).
- **Outbound peer dials** — `open`(Active) from the chat callsign to a peer,
  dialling **through the packet network** (§5). pdn replies after the connect
  resolves (deviation D4), so a successful `openReply` means the session is up.

## 3. The BPQ chat wire protocols (derived from `reference/linbpq-chat/`)

BPQ runs **two protocols over one connected-mode AX.25/NET-ROM session**: a
**user line protocol** (what a human types) and an **inter-node link record
protocol** (how two chat nodes sync). A connection is a *user* session by
default and is *promoted* to a *node link* when the first line is `*RTL`.

### 3.1 Framing

A chat session is a stream of **CR-terminated lines** (`\r`, 0x0D). There is
**no length prefix** — a line ends at the first CR; partial lines are
reassembled across packets (`HanksRT.c:ChatDoReceivedData`). This is a
deficiency (see §4.4): line length is unbounded and framing trusts the peer.

A line is an **inter-node control record** iff its **first byte is `FORMAT`
(0x01, Ctrl-A)** (`HanksRT.c:chkctl` — `if (Buffer[FORMAT_O] != FORMAT) return`).
Otherwise it is **user text / a user command**.

### 3.2 Link establishment handshake

Source: `HanksRT.c:ChatConnected`, `ProcessChatConnectScript`, `rtloginl`,
`bpqchat.h`.

On any new connection the node sends a **greeting banner**:

```
[BPQCHATSERVER-<flags>]
```

where `<flags>` are FBB-style capability characters (captured into
`conn->FBBReplyChars`). Then:

- **A human user** just starts typing — the node runs `rtloginu`, sends the
  `ChatWelcomeMsg`, drops them into the default topic, and shows a prompt.
- **A linking node** sends **`*RTL\r`** ("Round Table Link" login) as its first
  line. The receiver:
  1. checks the caller's callsign is in its configured peer list (`link_hd`,
     from `OtherChatNodes`) — **else refuses** ("does not have … defined as a
     node to link to");
  2. checks the caller isn't *already* a known node — **else refuses to prevent
     a loop** (`rtloginl`: "Refusing link … to prevent a loop");
  3. replies **`OK\r`**, marks the circuit `p_linked`, and runs **`state_tell`**
     (§3.4) to bring the new peer up to date, then sends an initial keepalive.

**Outbound** (we dial a peer) is the mirror: dial through the connect script
(§5), wait for `[BPQCHATSERVER-…]`, send `*RTL` + an initial keepalive, and on
the peer's `OK` run `state_tell` (`ProcessChatConnectScript`).

### 3.3 Inter-node link record protocol

Every control record (`HanksRT.c:chkctl`, builders `*_xmit`/`*_tell`/`put_text`,
constants `bpqchat.h:207-229`):

```
<FORMAT=0x01><TYPE><SP-separated fields…>\r
         byte 0     byte 1   bytes 2…N
```

`ncall` = the originating **node** callsign; `ucall` = the **user** callsign (or
a second node, for link/unlink). Fields are space-separated; the last field
(message text / name+qth) may contain spaces. On receive, bytes < 0x20 (except
BELL 0x07; TAB 0x09 → space) **reject the whole record as corrupt** — the only
input validation BPQ does.

| TYPE | const | meaning | fields after type byte |
|------|-------|---------|------------------------|
| `J` | id_join | user joined RT | `ncall ucall name qth` |
| `L` | id_leave | user left RT | `ncall ucall name qth` |
| `I` | id_user | user changed name/QTH | `ncall ucall name qth` |
| `D` | id_data | message to a whole topic | `ncall ucall text…` |
| `S` | id_send | private message to one user | `ncall ucall targetcall text…` |
| `T` | id_topic | user changed topic | `ncall ucall topicname` |
| `N` | id_link | node `ncall` gained a link to node `ucall` | `ncall ucall alias version` |
| `Q` | id_unlink | node `ncall` lost its link to node `ucall` | `ncall ucall …` |
| `K` | id_keepalive | node-node keepalive (doubles as a poll) | `ncall linkcall [version]` |
| `P` | id_poll | link-validation poll | `ncall linkcall` |
| `R` | id_pollresp | poll response (drives RTT) | `ncall linkcall` |

**Delivery semantics** (from `chkctl`): `id_data` is delivered to local users in
the originating user's **topic** (`text_tellu(..., o_topic)`) and appended to
the in-RAM history ring (`AddtoHistory`). `id_send` is delivered to one user if
they're local, else relayed onward. `id_join`/`id_leave` maintain the network
user table and announce presence. `id_user` updates name/QTH. `id_link`/
`id_unlink` maintain the **node graph** that loop suppression depends on.
`id_keepalive`/`id_poll` both elicit an `id_pollresp` and refresh
`lastMsgReceived`.

### 3.4 State sync on link-up (`state_tell`)

When a link comes up, each side **tells the other its entire known state**:
an `id_link` for the new link (relayed onward via `node_tell`), then the set of
known nodes, users (with name/QTH), and topic memberships it holds on *other*
links. This is a full dump — heavy, and a resync storm risk on a flapping link
(§4.5).

### 3.5 User line protocol (the `/` commands)

Source: `HanksRT.c:rt_cmd` + the built-in help text. Commands are
case-insensitive and start with `/`; **any line not starting with `/` is a
message to the user's current topic**.

| command | effect |
|---------|--------|
| `/U` | show users |
| `/N [name]` | set / show your name |
| `/Q [qth]` | set / show your QTH |
| `/T` | show topics |
| `/T <name>` | join / create a topic (names case-insensitive) |
| `/P` | show ports & links |
| `/K` | show known nodes |
| `/S <call> <text>` | private message to one user |
| `/A` | toggle join-alert (bell) |
| `/C` | toggle colour mode |
| `/E` | toggle echo |
| `/Time` `/ShowNames` `/Auto` `/UTF-8` `/Codepage CPnnnn` | display/encoding toggles |
| `/Keepalive` | toggle 10-min user keepalive |
| `/History <nn>` | show messages from the last `nn` minutes |
| `/F` | force all links up |
| `/B` | leave chat, return to node |
| `/QUIT` | leave chat and disconnect |
| `/H` `/?` | help |

pdn-bpqchat must accept this command set on the RF side for user familiarity
(W3); the web UI (W4) exposes the same actions as UI affordances over the same
core.

## 4. Deficiency analysis (HANDOVER.md §3 list — confirmed and extended)

### 4.1 No transport security / link auth — callsign-only trust
**Confirmed.** `rtloginl` authorises a link purely by matching the caller's
claimed callsign against the configured `OtherChatNodes` list. AX.25 has no
authentication, so any station that can present (or spoof) an allowed callsign
links. Cleartext throughout.
**pdn:** keep an **inbound allow-list as a first-class config concept** (a peer
must be listed to link in); posture is *cleartext-on-the-BPQ-wire for interop*
but **never accept an unlisted inbound link**. Leave room for a per-link shared
secret between two pdn-bpqchat nodes (negotiated via banner caps), without
breaking vanilla-BPQ interop.

### 4.2 User callsign spoofing across the mesh
**Confirmed.** `id_join`/`id_data`/`id_send` carry `ucall ncall` with zero
authentication; a hostile or buggy node can inject any user identity or message.
`user_find`/`user_join` trust them.
**pdn:** record **provenance** (which link a user/message arrived on); treat all
peer-supplied identities as hostile; rate-limit join/leave per link; surface the
origin node in the web UI so spoofing is at least visible. We cannot make the
BPQ wire authenticated, but we can contain blast radius (allow-listed links +
provenance + rate limits).

### 4.3 Loop / duplicate suppression is weak — the load-bearing problem
**Confirmed, and the worst of it.** BPQ uses **three** overlapping mechanisms:
- **Origin checks** (`chkctl`): drop a record from an unknown node, or from
  ourselves (`matchi(ncall, OurNode)`).
- **Spanning-tree relay** (`echo`): relay to every linked circuit *except* the
  ingress circuit and any circuit that already knows the origin node
  (`cn_find`). Correct only while the per-circuit known-node graph is
  consistent.
- **`CheckforDups`** for `id_data` only: a **10-entry, 5-second** window keyed
  on `(callsign, first 99 bytes of text)`. This is lossy (a 10th distinct
  in-flight message evicts; two messages sharing a 99-byte prefix collide;
  same-call different-text within 5 s can false-positive), **time-based not
  id-based**, and **does not cover** `id_send`/`id_topic`/`id_join`/`id_leave`.
- Plus **magic-number storm guards** (ignore a leave if connected < 3 s; don't
  re-report a join within 5 s) — an admission that the graph state is fragile.
**pdn — robust design in §5** (structural spanning-tree relay as the primary
mechanism + a content-hash dedup backstop, since the BPQ-format-only decision
(§9) leaves no wire id/hop field to use).

### 4.4 C string / buffer handling — hostile input assumed well-formed
**Confirmed.** Fixed buffers (`DupText[100]`, `callsign[10]`, `Msg[80/100/256]`),
`strcpy`/`strcat`/`sprintf` without bounds in places, `_strdup` of
attacker-controlled lines, and **`/S` does `strcat(Buffer, "\r")`** onto an
input buffer. Framing trusts an unbounded CR-terminated line; the only check is
the control-char corruption scan — **no length cap**.
**pdn:** a Go **line assembler with an explicit length cap** (drop / truncate
over-long lines), bounded fields, UTF-8-aware parsing, and a parser that treats
every byte from a peer or user as hostile. This is the single biggest
clean-room win.

### 4.5 Keepalive / link-timeout / resync on a flapping link
**Confirmed.** Keepalive doubles as a poll (`id_keepalive`/`id_poll` →
`id_pollresp`, RTT tracked); link liveness is `lastMsgReceived`; on link-up a
**full `state_tell` dump** runs (heavy; a reconnect loop can storm). Partial
messages are reassembled in `ChatDoReceivedData`. If there are **no local
users, all links are torn down** (`node_keepalive`→`node_close`).
**pdn:** explicit keepalive interval + timeout; **reconnect with exponential
backoff** (already the pattern in `cmd` and pdn-convers); **bounded, idempotent
resync** (reconcile state deltas, don't blindly re-dump); keep "drop idle links"
as a config option, not a hard default that surprises web users.

### 4.6 Character encoding & message length
**Confirmed.** Byte/ASCII-era with a UTF-8 *toggle* + codepage fallback and a
`ChatIsUTF8` heuristic; CR-terminated, no explicit length.
**pdn:** **UTF-8 native** internally and on the web; transcode at the BPQ wire
edge per the peer's caps; enforce a max message length.

### 4.7 No persistence / history / web (the headline improvements, not bugs)
**Confirmed.** History is an in-RAM ring (`AddtoHistory`, `/History nn`);
nothing survives a restart; terminal-only.
**pdn:** **SQLite history** + a **full web chat** — the value-add (HANDOVER.md
§4).

## 5. pdn-bpqchat's loop / duplicate suppression design

The acceptance gate is **"a cycle of nodes does not storm"** (HANDOVER.md §6d).
BPQ's heuristic (§4.3) is not good enough — but **we keep the wire byte-identical
to vanilla BPQ** (decision 2026-06-13, §9): **no extended framing, even between
two pdn nodes.** So there is **no wire-carried message id and no hop counter** to
lean on (BPQ's record format has neither, §3.3). That makes the **structural
mechanism load-bearing**, with a **content-hash dedup as the backstop**:

1. **Origin checks (keep BPQ's).** Drop a record from an unknown node, or one
   whose `ncall` is us (`matchi(ncall, OurNode)`) — the basic loop break.
2. **Spanning-tree relay — the primary mechanism.** Keep and *harden* BPQ's
   `echo` rule: relay to every linked circuit **except** the ingress circuit and
   any circuit that already knows the origin node (`cn_find`). This is purely
   **topology-based** — it needs no wire id — so it survives the BPQ-format-only
   constraint. Its correctness depends entirely on the **per-link known-node
   graph** being consistent, so that graph (built from `id_link`/`id_unlink` +
   `state_tell`) becomes a **first-class, unit-tested state machine**, not the
   fragile thing §4.3's 3 s/5 s magic numbers paper over. Re-link loop refusal
   (`rtloginl`: refuse a link from an already-known node) stays.
3. **Content-hash seen-set — the backstop.** A `seen` set keyed by a
   **deterministic synthetic id** computed identically on every node, purely from
   record content that is **stable across the mesh**:
   `synthID = hash(origin-ncall, ucall, type, normalise(text))`. A record whose
   `synthID` is already in `seen` is **dropped before delivery or relay**. This
   **strictly improves on `CheckforDups`**: it covers **all** record types (not
   just `id_data`), uses a full hash (not a 99-byte prefix), and a sized set with
   a TTL ≥ the longest plausible propagation delay (e.g. 10 min) rather than 10
   entries / 5 s. Because the id is content-derived, a record we *originated*
   that loops back to us via a cycle hashes to the same id and is dropped — the
   same property a wire id would give, without touching the wire.

**The residual limitation (stated honestly).** Without a wire id, two
*genuinely distinct* messages with identical `(origin, user, type, text)` inside
the TTL window collide and the second is suppressed — exactly BPQ's class of
false-positive, but far rarer here (full text hash, not a prefix; and identical
text from the same user in minutes is almost always a real duplicate anyway). A
true unique id is impossible on the BPQ wire by construction, so we make the
**structural** mechanism (2) carry correctness and treat (3) as the safety net
for the transient window while the node graph re-converges after a topology
change. If experience shows this is insufficient, the escape hatch is a future
capability-negotiated pdn↔pdn extension — explicitly deferred, not designed in.

**Acceptance test (W5).** Build `pdn ↔ BPQ ↔ pdn` and `pdn ↔ pdn ↔ pdn` cycles,
inject a message at one node, and assert (a) every node delivers it to its local
users **exactly once**, and (b) after the TTL no record is still circulating
(cf. packet.net INP3 "drain-once-per-round" / link-bench storm work). The test
must also **flap a link mid-cycle** to exercise the node-graph state machine (2),
since that — not a wire id — is what prevents the storm.

## 6. Identity, presence, topics across the mesh

- A network user is `(callsign, home-node)` (BPQ's `user_find(call, node)`),
  plus provenance (ingress link). Presence join/leave propagates as
  `id_join`/`id_leave`; name/QTH as `id_user`.
- Topic state reconciles on link-up via `state_tell` (§3.4) — pdn does this as a
  **bounded delta reconcile**, not a blind re-dump (§4.5).
- **Default topic:** **`General`** (decision 2026-06-13, §9) — every user (RF and
  web) lands in the topic named `General` on connect; topic names are
  case-insensitive (§3.5).

## 7. Persistence (SQLite, from W2)

State dir `/var/lib/packetnet/apps/bpqchat`, `bpqchat.db`. Tables (sketch):
`topics`, `users` (call, home-node, name, qth, current-topic, presence,
ingress-link), `messages` (msgid, origin-node, topic, ts, kind, text — the
web-visible **history** BPQ lacks), `links` (peer call, connect-script, state,
last-seen, RTT), `config`. The `seen` set is in-memory (TTL-bounded). The pure
`internal/chat` core owns the in-RAM model; the store is a persistence adapter
behind an interface so the core stays host-free and unit-tested.

## 8. Web chat (W4)

A complete browser UI at `/apps/bpqchat/`: live stream (SSE — already proven
shape; chunked/SSE pass-through is supported by the gateway), topic switching,
presence list, scrollback from SQLite, send box. **Identity** comes from the
app-gateway headers (`X-Pdn-User`/`X-Pdn-Scope`/`X-Pdn-Gateway`) — the app
trusts the gateway, never a separate login, and **binds loopback only** so the
headers are trustworthy (`internal/web` already enforces this). A web user is a
first-class user in the same topics as RF and mesh users.

## 9. Settled decisions & open questions

**Settled** (HANDOVER.md §8): default-off; BPQ interop required (peer wire
matches BPQ, internals clean-room); web chat first-class; peering is the
headline.

Confirmed here:
- **Peering rides the packet network, not a raw TCP socket.** The
  `OtherChatNodes` config is a `|`-separated **node connect script**
  (e.g. `RDGCHT:GB7RDG-1|C STHGTE|C 1 MB7NCR-2|C RDGCHT`) — a chain of node
  `C`(onnect) commands hopping AX.25/NET-ROM to the destination chat callsign.
  pdn dials this via RHP `open`(Active) through the node (HANDOVER.md §5
  vindicated — there is no "direct TCP chat-peering").
- **The bound callsign serves both users and node links;** `*RTL` as the first
  line promotes a session to a link.

**Resolved 2026-06-13 (Tom):**
- **Default topic = `General`.** Every user lands in the topic named `General`
  on connect (§6); topic names are case-insensitive.
- **Default peer transport = telnet node-link.** The dev loop links to the
  docker BPQ oracle over a telnet/IP node connection (the fastest loop, no RF
  sim); RF-via-RHP + net-sim is the realism pass in W6. The **live peer stays
  unset** until a parent chat node is arranged — develop against the oracle
  meanwhile (still blocked on that external prerequisite).
- **Multi-peer, staged.** v1 targets multiple simultaneous peers (peering is the
  point), staged: **W5** proves one peer + the loop-control backstop against the
  oracle; **W6** adds multi-peer + the RF link path. Robust loop suppression
  (§5) is built in from W5, not retrofitted.
- **BPQ-format only — no pdn↔pdn extended framing.** The inter-node wire stays
  byte-identical to vanilla BPQ on *every* link, including pdn↔pdn. Loop control
  therefore uses the structural spanning-tree relay (primary) + a content-hash
  dedup backstop, with **no wire message-id or hop field** (§5). A
  capability-negotiated extension remains a possible future escape hatch if the
  content-hash backstop proves insufficient in practice — explicitly **not**
  designed in now.

**Still open** (flag early; don't block W2):
- **Which real peer(s)** to link to for the live network — blocked on Tom
  arranging a parent/peer chat node (as above).
- **The exact `General`-topic semantics on the BPQ wire** — confirm in W5 that
  landing every user in `General` interoperates cleanly with a BPQ node's own
  default topic (do they reconcile to one shared room, or stay distinct?).
  Derive from the oracle.

## 10. What W0 delivered (this branch)

- `internal/rhp` — a working, tested Go **RHPv2 client**: 2-byte framing, the
  message catalogue, Latin-1 data encoding, request/reply correlation by `id`,
  async push dispatch (`accept`/`recv`/`status`/`close`), and the
  socket/bind/listen/open/send/close + auth/hello surface. Unit-tested against
  an in-process fake server (listener path, port string/int, errCode casing,
  Latin-1 round trip, server-error propagation).
- `cmd/pdn-bpqchat` — the do-nothing supervised daemon: derive the callsign,
  bind+listen over RHP (reconnect with backoff), greet+close inbound, serve the
  loopback web tile.
- `internal/web` — the W0 placeholder tile proving the app-gateway identity
  contract; loopback-bound.
- `internal/config` — supervisor-env → resolved config; derived callsign.
- `reference/linbpq-chat/` — the vendored BPQ chat source (pinned, provenance
  recorded) this spec is derived from.

**Next (gated on review of this doc): W2 — the pure `internal/chat` domain
(topics, users, presence, message log, SQLite), unit-tested, host-free.**
