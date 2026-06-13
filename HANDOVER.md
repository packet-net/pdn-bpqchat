# pdn-bpqchat — handover / build plan

**Purpose:** everything a fresh agent needs to build **`m0lte/pdn-bpqchat`** — a BPQ-Chat-compatible chat node for the pdn (packet.net) platform, with a **full web chat** and **node-to-node peering**, shipped as a pdn app package that's **default-off**. Authored 2026-06-13 for Tom M0LTE.

> Read order: this file → (the agent writes) `docs/design.md` → the vendored BPQ chat source under `reference/`. Mirror the **discipline** of **`m0lte/pdn-bbs`** / **`m0lte/pdn-convers`** (strict layering, app-package-only, oracle-first testing) — but **not the language**: see the language decision below.

## Language: Go (decided 2026-06-13, Tom)

**Build pdn-bpqchat in Go, not C#.** The app-package contract is language-agnostic — an app only needs to speak **RHPv2 over TCP** (the packet plane) and serve a **loopback HTTP** web tile, plus ship a `pdn-app.yaml`. The existing pdn apps are C#/.NET (pdn-bbs/pdn-convers/dapps) and Python (wall/lobby); pdn never branches on language. Go is chosen because the priority is **ubiquity + trivial install**: a **single static binary, no runtime, no deps** (install = drop one file), **trivial cross-compile** to `linux/amd64,arm64,arm` via `GOARCH` (which also sidesteps the crossgen2-R2R OOM that wedges the .NET release pipeline), and stdlib `net/http` + goroutines are an ideal fit for a chat+web daemon. The one new piece is a **small Go RHPv2 client** (can't reuse pdn-bbs's .NET one — but RHPv2 is a simple framed-TCP protocol; spec in packet.net `docs/rhp2-server.md`). Everywhere this doc references C#-isms (`.slnx`, `Directory.Packages.props`, "copy pdn-bbs's bones"), read it as the Go equivalent (Go modules, `go build`, "copy pdn-bbs's *discipline*").

---

## 1. What this is

Give pdn's RF users (and the node owner, via a **web chat tile**) a first-class **multi-user chat** that **interoperates with the BPQ Chat network** — the G8BPQ/LinBPQ "chat node" conference system that hams already run. pdn-bpqchat:

- presents a **chat node** that local RF users connect to (over AX.25, through the pdn node, via RHPv2) and that the node owner uses through a **web UI**;
- **peers** with other chat nodes — both real **BPQ Chat** nodes and other **pdn-bpqchat** nodes — so messages, users and topics propagate across the linked network (this is the headline feature: a chat is only useful if it's connected to the wider network);
- is a **pdn app package**: it talks to the node **only over public interfaces** (RHPv2 for the packet plane, the app-gateway identity contract for the web tile) and is **discovered-but-off** until the owner enables it (so "default off" is the app-package model's natural state — no special-casing).

**This is NOT pdn-convers.** `pdn-convers` is the *convers* (round-table / "Tampa PingPong") network; `pdn-bpqchat` is the *BPQ Chat* network — a different protocol and a different network. They are sibling apps (`id: convers` vs `id: bpqchat`) that can coexist on one node; reuse pdn-convers's *shape and patterns*, not its protocol.

## 2. How it fits pdn (architecture — non-negotiable)

Mirror pdn-bbs/pdn-convers exactly:

- **Go**, strictly layered, **app-package-only**: the daemon reaches the node **solely over RHPv2** (no in-process coupling to packet.net). The node supervises it as a service when enabled. Write a minimal Go RHPv2 client first (W0).
- **Manifest**: a `pdn-app.yaml` with `id: bpqchat`, `capabilities: [network, web]`, a `service:` block (the supervised daemon), and a `ui:` block (loopback web upstream the node reverse-proxies at `/apps/bpqchat/`). See packet.net `docs/app-packages.md` (the contract) + `docs/app-gateway.md` (the web-tile identity injection). The on-air callsign is derived from the node — `<PDN_NODE_CALLSIGN>-<ssid>` — via the supervisor env (`PDN_NODE_CALLSIGN` / `PDN_RHP_HOST` / `PDN_RHP_PORT`); do **not** hard-code a callsign.
- **State**: SQLite under the app state dir (`/var/lib/packetnet/apps/bpqchat`). Channels/topics, message log, user/session table, peer-link state, config.
- **Packaging**: a self-contained `.deb` + a published release (the same release pipeline pdn-bbs/dapps use). packet.net's node `.deb` will **bundle it default-off** (staged from the published release like DAPPS — see packet.net issue #403 for the convers precedent; do the same for bpqchat once it cuts a release).

## 3. Reference the BPQ source — and work *around* its deficiencies, don't copy them

**Mandate from Tom:** reference the BPQ Chat source, **analyse it for deficiencies, and design around them** — do not blindly port C-isms or protocol warts into a clean Go node.

- **Where the source is.** The chat node lives in the **LinBPQ / BPQ32** codebase (John Wiseman, G8BPQ). The interop stack here already runs a containerised build as **`m0lte/linbpq`** (see §7); the upstream source is G8BPQ's LinBPQ (the `BPQChatServer` / chat-node component — locate the chat module, e.g. the `Chat*.c` files / the ChatServer program). **First task: vendor the relevant chat source under `reference/`** (a pinned copy, with provenance recorded — exactly as pdn-convers vendored `conversd-saupp`), so the protocol can be derived from ground truth and the analysis is reproducible.
- **Derive, then judge.** From the source, derive (a) the **user-facing command/line protocol** (the `/` commands users type — name, topic/channel, who, msg, leave, etc.) and (b) the **inter-node link/peer protocol** (how two chat nodes exchange joins/leaves/messages/topic changes/keepalives). Write both up in `docs/design.md` as the wire spec you're implementing to.
- **Deficiencies to scrutinise (starting list — confirm + extend against the source):**
  - **No transport security / auth on links** — chat links are cleartext and trust the peer's claimed callsign. Design: don't *trust* a peer callsign blindly; make link auth/allow-listing a first-class config concept even if interop with vanilla BPQ stays cleartext.
  - **Callsign spoofing** of users propagated across the mesh.
  - **Loop / flood control in the peer mesh** — how does BPQ stop a message looping across a cycle of linked nodes? Confirm its mechanism (message ids? hop limits? spanning tree?) and implement a *robust* one rather than inheriting any gap.
  - **C string/buffer handling** — fixed buffers, unbounded line lengths, parsing that assumes well-formed input. A clean Go parser that treats peer/user input as hostile is the win.
  - **Keepalive / link-timeout / resync** behaviour and partial-message handling on a flapping RF link.
  - **Character encoding** (BPQ is byte/ASCII-era) and message-length limits.
  - **No persistence / no history / no web** — BPQ chat is terminal-only and ephemeral. pdn-bpqchat's **SQLite history + the web chat** are the headline improvements, not deficiencies to copy.
- **Interop is the constraint, not the ceiling.** pdn-bpqchat must **link to a real BPQ chat node and exchange traffic** (§7), so the on-the-wire peer protocol must match BPQ. Everything *internal* (storage, parsing, the mesh model, auth) is yours to do better.

## 4. Scope (all four pillars are in scope)

1. **RF chat node** — local users connect over AX.25 (through pdn, via RHPv2 to the app's bound callsign), get a BPQ-Chat-compatible command interface, join topics/channels, see network-wide users + messages.
2. **Full web chat** (explicitly in scope) — a complete browser chat UI served by the app at `/apps/bpqchat/`: live message stream (SSE or WebSocket), channel/topic switching, user/presence list, scrollback/history (from SQLite), and a send box. Authenticated via the app-gateway identity contract (the node injects the authenticated user; the app trusts the gateway, not a separate login). RF users and web users share the same channels — a web user and an on-air user are in the same room.
3. **Peering** (explicitly in scope — see §5) — node-to-node links to BPQ chat nodes and other pdn-bpqchat nodes; message/user/topic propagation; loop prevention; link lifecycle.
4. **BPQ interop, oracle-first** — develop and test against a **containerised BPQ chat node** (§7) as the ground-truth peer.

## 5. Peering (design depth)

The chat network is a graph of linked chat nodes. **First derive how BPQ chat nodes actually link from the source — do not assume.** To the best of current knowledge BPQ chat-node linking rides the **packet network**, not a bespoke raw-TCP socket: over **RF**, and between internet-linked nodes over BPQ's IP mechanisms — **AXIP / AXUDP** (UDP tunnels carrying AX.25 between BPQ nodes) and/or **telnet** node links. There is **no "direct TCP chat-peering" concept** to assume; confirm the real transport(s) in the BPQ source before building.

Design behind one **`PeerLink`** seam (the Go analogue of pdn-convers's `IUpstreamLink`) with providers matching what BPQ actually uses:

- **RF/RHP peer** — connect to a peer node's chat callsign over AX.25 by issuing an RHP `open` (the node dials it), then speak the BPQ chat link protocol over that connected-mode session. This is the one we *know* exists.
- **Internet peer** — whatever BPQ uses node-to-node over IP (AXIP/AXUDP tunnel, or telnet link) — implement to match, derived from the source. The containerised BPQ node (§6) is reached this way for tests, over whatever link transport it's configured to expose.

Decisions to make explicit in `docs/design.md`:

- **Topology** — start **leaf/link-to-named-peers** (a configured list of peers to link to), not auto-discovery. A full mesh needs loop control (below). Document whether you support multiple simultaneous peers in v1 or one uplink first (pdn-convers chose one-uplink-first; bpqchat's headline is peering, so aim for multi-peer but stage it).
- **Loop / duplicate suppression** — the load-bearing correctness problem. Derive BPQ's approach from the source; implement message de-dup (ids + a seen-set with TTL) and/or hop limits so a message can't circulate forever across a cycle. **Add a test that forms a cycle of nodes and proves no storm** (cf. packet.net's INP3 "drain-once-per-round" and the link-bench storm work — loops are where chat networks melt down).
- **Identity across the mesh** — how a user is represented network-wide (callsign + node), presence join/leave propagation, topic state reconciliation on link-up.
- **Link auth/allow-list** — config to control *which* peers may link in (don't accept an inbound link from anyone by default).
- **Resilience** — link flap, reconnect/backoff, state resync on reconnect, keepalives.

## 6. Containerised BPQ for testing (the interop oracle)

Develop against a real BPQ chat node in docker — the same way packet.net's interop stack uses `m0lte/linbpq`.

- **Base image:** `m0lte/linbpq` (already used by packet.net's interop stack — `docker/compose.interop.yml`, with `docker/linbpq/bpq32.cfg` as a read-only bind). Reuse that image; you don't need to build BPQ.
- **Configure a chat node** in `bpq32.cfg`: enable the **CHAT application** / chat-node config (the `APPLICATION`/`CHAT` directives + the chat-node link config). Derive the exact directives from the BPQ source/docs and the existing `bpq32.cfg`. Expose the chat node's link listener (the inter-node chat link port) so pdn-bpqchat can link to it, and a user-facing connect path (telnet/AXUDP) so you can drive a "user" into it for end-to-end tests.
- **Compose:** ship a `docker/` in this repo (an isolated compose, like packet.net's `docker/inp3lab`) with the LinBPQ chat node, plus a net-sim AFSK channel for the RF-link path. Link to it over whatever transport BPQ actually exposes for chat node-links (derived in W1 — AXUDP/telnet over IP for the quick dev loop, RF-via-RHP/net-sim for the on-air realism pass). Don't assume a raw TCP socket.
- **Oracle tests:** (a) pdn-bpqchat **links to** the LinBPQ chat node and a message typed on one side appears on the other (both directions); (b) a user on LinBPQ and a user on pdn-bpqchat (and a *web* user) are in one room and all see each other's traffic; (c) topic/channel + presence propagate; (d) a 3-node cycle (pdn ↔ BPQ ↔ pdn, or pdn ↔ pdn ↔ pdn) does **not** loop-storm. These are the acceptance gates.

## 7. Build waves (suggested)

- **W0 — scaffold (Go).** Go module layout: `cmd/pdn-bpqchat/` (main), `internal/` (rhp client, chat core, peering, web), `go.mod`, `pdn-app.yaml`, `docker/`, `reference/`, `docs/design.md`, CI (`ci.yml` + a release workflow that `go build`s the three arches, **`runs-on: [self-hosted, Linux, X64]`** — no hosted runners). Deliver a minimal **Go RHPv2 client** + a do-nothing supervised daemon that binds the callsign over RHP and serves an empty loopback web tile.
- **W1 — vendor + spec.** Vendor the BPQ chat source under `reference/`; write `docs/design.md` (the user command protocol + the inter-node link protocol + the deficiency analysis + the loop-control design). This is the gate before real code.
- **W2 — core chat domain.** Channels/topics, users/presence, message log, SQLite. Pure, host-free, unit-tested.
- **W3 — RF user interface.** Accept inbound RF connects over RHP; a BPQ-Chat-compatible command parser (hostile-input-safe); a user can join, talk, switch topic, see who's on.
- **W4 — web chat.** The full browser UI at `/apps/bpqchat/`: live stream (SSE/WS), channels, presence, history, send. App-gateway identity. RF + web share rooms.
- **W5 — peering (TCP).** `IPeerLink` + the BPQ link protocol over TCP; **link to the containerised BPQ chat node**; message/presence/topic propagation; **loop/dup suppression + the cycle-no-storm test**.
- **W6 — peering (RF) + multi-peer.** RHP-`open` peer links; multiple simultaneous peers; flap/resync/keepalives; the net-sim RF interop pass.
- **W7 — packaging + ship.** Self-contained `.deb` + release pipeline; `pdn-app.yaml` finalised; then raise the packet.net side to **bundle it default-off** (stage from the published release like DAPPS; mirror packet.net #403). Hardening + docs polish.

## 8. Settled decisions + open questions for Tom

Settled (confirm in `docs/design.md`):
- **Default-off** (app-package model — discovered, owner enables).
- **Interop with the BPQ Chat network is required** (so the peer wire protocol matches BPQ); internals are clean-room.
- **Web chat is in scope** and first-class (not a stretch goal).
- **Peering is in scope** and the headline feature.

Open (flag early, don't block W0–W2):
- **Default channel/topic** users land in on connect (BPQ has a notion of a default; pick a number/name).
- **Which real peer(s)** to link to for the live network, and the **default peer transport** (RF-via-RHP, vs BPQ's internet node-link mechanism — AXUDP/telnet, as derived in W1). Like pdn-convers, this may be **blocked on an external prerequisite** (a parent/peer chat node to link to) — develop against the docker BPQ node meanwhile; leave the live peer unset until Tom arranges one.
- **Multi-peer mesh in v1 vs one-link-first** (lean multi-peer since peering is the point, but stage it W5→W6).
- **Link auth posture** — cleartext-for-BPQ-interop but allow-list inbound; confirm.

## 9. References / siblings

- **`m0lte/pdn-bbs`** — the template pdn app (structure, RHP client, packaging, oracle testing). Copy its bones.
- **`m0lte/pdn-convers`** — the sibling chat app (a *different* network); reuse its `IUpstreamLink`/app shape, its `pdn-app.yaml`, its HANDOVER/design discipline. Read its `design.md`.
- **packet.net docs:** `docs/app-packages.md` (the manifest/lifecycle contract), `docs/app-gateway.md` (web-tile identity), `docs/rhp2-server.md` (RHPv2). The interop LinBPQ container: `docker/compose.interop.yml` + `docker/linbpq/bpq32.cfg`.
- **BPQ:** the LinBPQ/BPQ32 source (G8BPQ) — the chat-node component (vendor under `reference/`). The `m0lte/linbpq` docker image is the test peer.
- **packet.net #403** — the precedent for bundling a pdn app default-off in the node `.deb`.

## 10. Suggested first message for a fresh agent

> Build `m0lte/pdn-bpqchat` per `HANDOVER.md` — **in Go** (single static binary; see the language decision). Start at **W0** (Go scaffold + a minimal RHPv2 client, mirroring pdn-bbs's *discipline*) then **W1**: clone + vendor the LinBPQ chat-node source under `reference/`, derive the user command protocol and the inter-node link protocol, write `docs/design.md` including a deficiency analysis (cleartext/auth, callsign trust, loop control, buffer/parse safety, persistence) and your loop-suppression design. Stand up the containerised `m0lte/linbpq` chat node as the interop oracle. Don't start protocol code before `docs/design.md` is reviewed. CI on self-hosted runners only.
