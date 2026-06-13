# pdn-bpqchat

A **BPQ-Chat-compatible chat node** for the pdn (packet.net) platform — a
first-class multi-user chat for RF users and the node owner (via a **web chat
tile**) that **peers with the BPQ Chat network**. Shipped as a **default-off
pdn app package**; a single static Go binary.

> Status: **W0–W2 complete.** The Go scaffold, a working RHPv2 client, and the
> do-nothing supervised daemon are in place (W0); the BPQ chat protocol is
> derived and specified in [`docs/design.md`](docs/design.md) with the BPQ
> source vendored under [`reference/`](reference/linbpq-chat/) (W1); and the
> pure, host-free chat domain (`internal/chat`) plus the SQLite store
> (`internal/store/sqlite`) are built and unit-tested (W2). Next is **W3** — the
> RF user session (the BPQ `/command` parser wired to the hub).

## What it is

Four pillars (all in scope — `HANDOVER.md` §4):

1. **RF chat node** — local users connect over AX.25 through the pdn node (via
   RHPv2) to the app's bound callsign, with a BPQ-Chat-compatible `/` command
   interface.
2. **Full web chat** — a complete browser UI at `/apps/bpqchat/` (live stream,
   topics, presence, history, send), authenticated via the pdn app-gateway.
3. **Peering** — node-to-node links to BPQ chat nodes and other pdn-bpqchat
   nodes, with robust loop/duplicate suppression (the headline feature).
4. **BPQ interop, oracle-first** — developed and tested against a containerised
   LinBPQ chat node as ground truth.

RF users and web users share the same topics.

## Layout

```
cmd/pdn-bpqchat      the supervised daemon
internal/rhp         the RHPv2 client (framing, codec, client) — W0 ✅, tested
internal/config      supervisor-env → config; derives the on-air callsign
internal/web         the loopback web tile (W0 placeholder; full chat in W4)
internal/chat        the pure chat domain: hub, events, topics, presence, dedup — W2 ✅, tested
internal/store/sqlite  durable message log + config KV (pure-Go SQLite) — W2 ✅, tested
docs/design.md       the BPQ wire spec, deficiency analysis, loop-control design (W1)
reference/           vendored LinBPQ chat source (pinned; provenance recorded)
docker/              the LinBPQ chat interop oracle (compose + bpq32.cfg)
pdn-app.yaml         the pdn app-package manifest (default-off)
.github/workflows/   CI + release (self-hosted runners only)
```

## Build & test

```sh
go build ./...
go test ./...
go vet ./...
```

Cross-compiling (what the release workflow does):

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o pdn-bpqchat ./cmd/pdn-bpqchat
```

## Running locally

The daemon expects the pdn supervisor environment; for a local smoke test point
it at a running RHPv2 server:

```sh
PDN_NODE_CALLSIGN=M0LTE PDN_RHP_HOST=127.0.0.1 PDN_RHP_PORT=9000 \
PDN_APP_STATE=/tmp/bpqchat go run ./cmd/pdn-bpqchat
```

It derives the chat callsign (`M0LTE-4` by default), binds and listens over
RHP, and serves the web tile on `127.0.0.1:18093`. In W0 it greets and closes
inbound connections — the chat service itself is not built yet.

## References

- [`HANDOVER.md`](HANDOVER.md) — the build plan and waves.
- [`docs/design.md`](docs/design.md) — the derived BPQ wire spec + design.
- `m0lte/pdn-bbs`, `m0lte/pdn-convers` — sibling pdn apps whose discipline this
  mirrors.
- packet.net `docs/rhp2-server.md` — the RHPv2 wire the client implements.
