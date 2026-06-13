# Interop lab — pdn-bpqchat ↔ a real LinBPQ chat node

This is the runbook for the BPQ-interop oracle test (HANDOVER.md §6). It has two
tiers; tier 1 is validated and runnable today, tier 2 is the full end-to-end
path that needs a running pdn node.

## Why a pdn node is required for tier 2

BPQ chat node **links ride AX.25**, not a raw TCP socket (design.md §5): a peer
is authenticated by its AX.25 callsign and must be a configured chat node.
pdn-bpqchat reaches AX.25 **only through a pdn node over RHPv2**. So the faithful
topology is:

```
pdn-bpqchat ──RHPv2──> pdn node ──AX.25 over AXUDP──> LinBPQ chat node
   (the app)            (packet engine)                 (the oracle)
```

The telnet/IP peer transport (`CALL@host:port`) is for pdn↔pdn or a peer that
exposes a chat link directly; vanilla BPQ does not, so use the RF path
(`rf:CALL`) for real BPQ interop.

## Tier 1 — the LinBPQ chat oracle (validated 2026-06-13)

Bring up a real LinBPQ chat node:

```sh
docker compose -f docker/compose.oracle.yml up -d --wait
```

Drive a user into chat to confirm it works:

```sh
# telnet 127.0.0.1 8010 ; login admin / admin ; then:
CHAT
```

You should see `Connected to CHAT`, the `[BPQChatServer-<ver>]` banner, the
welcome, and land in topic `[General]`. `/h` lists commands, `/U` shows users,
`/p` shows links. This proves the oracle config in `docker/linbpq/bpq32.cfg`.

> **Interop finding from this oracle:** real LinBPQ sends the banner mixed-case
> (`[BPQChatServer-6.0.25.28]`) but checks the uppercase form on receive (after
> `_strupr`). pdn-bpqchat therefore detects the banner **case-insensitively**
> (`internal/peer.isBanner`) — caught and fixed by running this lab.

## Tier 2 — full end-to-end (validated 2026-06-13)

The full path `pdn-bpqchat ──RHPv2──> pdn node ──AXUDP──> LinBPQ` runs and was
driven both ways. The lab harness lives in `docker/lab-tier2/`; the node is the
real `m0lte/packet.net` image (config schema sourced from that repo's
`src/Packet.Node.Core/Configuration` and `docs/rhp2-server.md`).

### Topology that makes `rf:` reach LinBPQ

An RHP `open` with no port label dials a **direct AX.25 SABM out the node's
first port** (`SupervisorRhpGateway.OpenAx25StreamAsync`). So the pdn node's
**first and only port is the AXUDP link to LinBPQ** — then pdn-bpqchat's
`rf:GB7PDN-2` becomes a SABM to `GB7PDN-2` straight down the AXUDP tunnel.

- **pdn node** (`docker/lab-tier2/packetnet.yaml`): callsign `M0LTE`, one
  `kind: axudp` port to `linbpq:8093`, and `rhp.{enabled,bind:0.0.0.0,port:9000}`.
- **LinBPQ** (`docker/lab-tier2/linbpq/bpq32.cfg`): the tier-1 chat config plus a
  `DRIVER=BPQAXIP` port on `UDP 8093` (`AUTOADDQUIET` + a static `MAP M0LTE-4`).
  FCS is mandatory on BPQAXIP; pdn's AXUDP always carries the matching CRC-16/X.25.
- **LinBPQ chat** (`docker/lab-tier2/linbpq/chatconfig.cfg`): `M0LTE-4` is in
  `OtherChatNodes` — **required**, or LinBPQ treats the inbound connection as a
  *user* (or closes it) instead of accepting the `*RTL` node link
  (`reference/linbpq-chat/ChatUtils.c` link-list check).

### Run it

```sh
docker compose -f docker/lab-tier2/compose.tier2.yml up -d --wait   # node shows
# "unhealthy" only because the packet.net image lacks bash for the /dev/tcp probe;
# `docker logs pdnbpqchat-t2-node` confirms RHP :9000 + the bpqlink port are up.

# pdn-bpqchat runs on the host against the published RHP port:
go build -o /tmp/pdn-bpqchat ./cmd/...
PDN_NODE_CALLSIGN=M0LTE PDN_RHP_HOST=127.0.0.1 PDN_RHP_PORT=9000 \
PDN_APP_STATE=/tmp/pdn-state PDN_WEB_PORT=18093 \
PDN_BPQCHAT_PEERS=rf:GB7PDN-2 /tmp/pdn-bpqchat
```

### What was observed (ground truth)

Link establishment — a complete BPQ chat node-link handshake in LinBPQ's
`/data/logs/log_*_CHAT.txt`:

```
>M0LTE-4  [BPQChatServer-6.0.25.28]      LinBPQ banner
<M0LTE-4  *RTL                           pdn-bpqchat promotes to a node link
>M0LTE-4  OK                             LinBPQ accepts
>M0LTE-4  KGB7PDN-2 M0LTE-4 6.0.25.28    K (version) records both ways
<M0LTE-4  KM0LTE-4 GB7PDN-2 pdn
>M0LTE-4  RGB7PDN-2 M0LTE-4              R (topology) records both ways
<M0LTE-4  RM0LTE-4 GB7PDN-2
```

- **pdn → LinBPQ:** `curl -X POST 127.0.0.1:18093/send -d '{"text":"…"}'` arrives
  as a `D` (data) record in LinBPQ's chat log, with a `J` (join) placing the web
  user in topic `[General]`.
- **LinBPQ → pdn:** a telnet user (`:8010`, admin/admin, `CHAT`) typing a line
  shows up in pdn-bpqchat's `GET /history` as `{"from":"GB7PDN",…}`.
  (NB: LinBPQ's first post-`CHAT` line answers its *"enter your Name"* prompt for
  a new user — send a name first, then chat.)
- **Presence both ways:** pdn's user shows in LinBPQ's `/U`
  (`SYSOP at PDNCHT [General]`); the LinBPQ user shows in pdn's `GET /users`
  (`{"call":"GB7PDN","node":"GB7PDN-2"}`).

### The 3-node cycle — no loop-storm on the wire (validated 2026-06-13)

A real triangle of chat nodes, so a message that can circulate must not storm:

```
pdnA(M0LTE-4) ──IP──> pdnB(G0XYZ-4)
       \                  /
        rf            rf            all three edges are real chat links
          \            /
        LinBPQ (GB7PDN-2)
```

pdnA↔BPQ and pdnB↔BPQ are RF (each pdn has its own node → AXUDP → the shared
LinBPQ AXIP port); the pdnA↔pdnB edge is the **pdn↔pdn IP transport**: pdnB
listens (`PDN_BPQCHAT_PEER_LISTEN`) and pdnA dials it (`…@host:port`). The packet
plane (BPQ + two pdn nodes) is `docker/lab-tier2/cycle/`; the two apps run on the
host.

```sh
docker compose -f docker/lab-tier2/cycle/compose.cycle.yml up -d --wait
go build -o /tmp/pdn-bpqchat ./cmd/...

# pdnB — links BPQ over RF and ACCEPTS pdnA's IP peer link:
PDN_NODE_CALLSIGN=G0XYZ PDN_RHP_PORT=9001 PDN_APP_STATE=/tmp/stateB PDN_WEB_PORT=18095 \
PDN_BPQCHAT_PEERS=rf:GB7PDN-2 PDN_BPQCHAT_PEER_LISTEN=127.0.0.1:18094 /tmp/pdn-bpqchat &

# pdnA — links BPQ over RF and DIALS pdnB over IP (closing the triangle):
PDN_NODE_CALLSIGN=M0LTE PDN_RHP_PORT=9000 PDN_APP_STATE=/tmp/stateA PDN_WEB_PORT=18093 \
PDN_BPQCHAT_PEERS="rf:GB7PDN-2,G0XYZ-4@127.0.0.1:18094" /tmp/pdn-bpqchat &

# inject ONE message and count copies everywhere:
curl -sX POST 127.0.0.1:18093/send -H 'Content-Type: application/json' -d '{"text":"CYCLE-PROBE-42"}'
```

Observed: pdnA `/history` = 1 copy, pdnB `/history` = 1 copy (pdnB received it via
*both* BPQ and the direct IP link, deduped to one). At BPQ — the cycle vertex —
the chat log shows it arriving by two paths and BPQ's own dedup catching it:

```
<G0XYZ-4  DM0LTE-4 M0LTE CYCLE-PROBE-42        (relayed via pdnB)
<M0LTE-4  DM0LTE-4 M0LTE CYCLE-PROBE-42        (direct from pdnA)
?         Duplicate Message From M0LTE CYCLE-PROBE-42 suppressed
```

Exactly one delivery per node, bounded traffic — the wire confirmation of the
`TestCycleNoStorm` guarantee (design.md §5), now across a heterogeneous cycle
(two pdn nodes + a real LinBPQ).

Teardown: `docker compose -f docker/lab-tier2/cycle/compose.cycle.yml down -v`.

### Reaching a peer across the network — connect scripts

The cycle above keeps every peer one hop away because a direct `rf:CALL` opens an
AX.25 SABM out the node's first port only. To peer with a chat node that is
several hops across a packet network, use a **connect script**
(`PDN_BPQCHAT_PEERS=via:…`) — open to a node we can reach and walk node-prompt
`C` commands to it, the last connecting to the peer's chat app:

```sh
# two-node shortcut: open to G0BBB's node prompt, then "C G0BBB-4"
PDN_BPQCHAT_PEERS=via:G0BBB-4
# explicit multi-hop: peer GB7RDG-1, open GB7STH, then walk on
PDN_BPQCHAT_PEERS="via:GB7RDG-1|GB7STH|C RDGCHT"
```

The node-prompt→local-app connect (the final `C G0BBB-4`) needs **PDN ≥0.9.0**.
This is the mechanism for scaling chat peering over an AXUDP/NET-ROM backbone
(`design.md` §W6); the unit test `TestConnectScriptDial` proves the walk.

### The .deb alternative

Instead of running the host binary, install the package into the node
(`sudo apt install ./pdn-bpqchat_<version>_<arch>.deb`, enable it from the
control panel, default-off) and set `PDN_BPQCHAT_PEERS=rf:GB7PDN-2` in the app
config — same peer, supervised by pdn.

## Teardown

```sh
docker compose -f docker/lab-tier2/compose.tier2.yml down -v   # tier 2
docker compose -f docker/compose.oracle.yml down -v            # tier 1
```
