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

## Tier 2 — full end-to-end (needs a pdn node)

1. **Run a pdn node** with RHPv2 enabled (`rhp.enabled: true`, loopback
   `:9000`) and an **AXUDP port** to the LinBPQ container.
2. **Add to `docker/linbpq/bpq32.cfg`** (see the commented section there): a
   `DRIVER=BPQAXIP` PORT carrying AX.25 to the pdn node over UDP, and the
   pdn-bpqchat chat callsign in LinBPQ's chat node-link config so it will accept
   a `*RTL` link. Uncomment the `8093/udp` port in `compose.oracle.yml`.
3. **Install pdn-bpqchat into the node:**
   ```sh
   sudo apt install ./pdn-bpqchat_<version>_<arch>.deb   # lands in /usr/share/packetnet/apps/bpqchat
   ```
   Enable it from the node control panel (it is default-off).
4. **Point it at the oracle** via the supervisor env / app config:
   ```sh
   PDN_BPQCHAT_PEERS=rf:GB7PDN-2     # dial LinBPQ's chat callsign over AX.25
   ```
5. **Drive it** and watch both ways:
   - A message typed by a LinBPQ chat user appears in pdn-bpqchat's web chat
     (`/apps/bpqchat/`) and to any RF user on the pdn side.
   - A message sent from pdn-bpqchat (web or RF user) appears in the LinBPQ chat.
   - Presence (`/U`) and topic changes propagate both ways.
   - Form a 3-node cycle (pdn ↔ BPQ ↔ pdn) and confirm no loop-storm (the
     in-code guarantee, design.md §5; the unit test `TestCycleNoStorm` proves the
     algorithm — this confirms it on the wire).

## Teardown

```sh
docker compose -f docker/compose.oracle.yml down -v
```
