# Release pipeline — the pdn-bpqchat `.deb` app package

pdn-bpqchat ships as a **Packet.NET app package**: a `.deb` that drops a single
static binary + the app manifest into the node's app directory, where pdn
discovers it and the owner enables it (default off). This mirrors
pdn-convers/DAPPS exactly.

## What the `.deb` contains — code only

```
usr/share/packetnet/apps/bpqchat/pdn-bpqchat    the single static Go binary (0755)
usr/share/packetnet/apps/bpqchat/pdn-app.yaml   the app manifest (0644)
DEBIAN/control                                   package metadata
DEBIAN/postinst                                  prepares the state dir (idempotent)
```

That is the whole payload. The binary is built `CGO_ENABLED=0` — **fully static,
no libc or shared-library dependency** — and the pure-Go SQLite driver is linked
in, so there is no runtime to bundle and nothing loose to ship.

## The code-vs-state split (load-bearing)

- **Code** (binary + manifest) lives under `/usr/share/packetnet/apps/bpqchat`
  and is owned by this package — replaced on every upgrade.
- **State** (`bpqchat.db` + its WAL/SHM sidecars, any `chat.yaml`) lives under
  `/var/lib/packetnet/apps/bpqchat` and is **never shipped or clobbered**. pdn
  creates that per-app state dir at runtime (packetnet-owned, 0750); the
  `postinst` only pre-creates it when the `packetnet` user already exists, and
  never fails if it doesn't. Upgrades preserve history and config.

## Building

```sh
scripts/build-deb.sh <goarch> <version>     # goarch: amd64 | arm64 | arm
# e.g.
scripts/build-deb.sh amd64 0.0.1
scripts/build-deb.sh arm64 0.0.1
scripts/build-deb.sh arm   0.0.1            # → armhf (ARMv7 hard-float, GOARM=7)
```

Each run produces `artifacts/pdn-bpqchat_<version>_<arch>.deb`. All three arches
cross-compile on one x64 machine (Go `GOARCH`) — no arch-native runner or cross
C-toolchain.

## Releasing

`.github/workflows/release.yml` (a `v*` tag or manual dispatch, self-hosted x64
only) builds all three `.debs`, writes `SHA256SUMS`, and attaches them to a
GitHub Release with `gh release create`.

## Installing on a node

```sh
sudo apt install ./pdn-bpqchat_<version>_<arch>.deb
```

The app appears under `/usr/share/packetnet/apps/bpqchat`; enable it from the
node control panel. (Inert without the packetnet host — a soft `Recommends`, so
it installs standalone but does nothing until a node discovers it.)

## Bundling default-off in the node `.deb` (W7 follow-up)

Once a release is cut, raise the **packet.net** side to bundle pdn-bpqchat
default-off in the node `.deb`, staged from the published release exactly as
DAPPS is — mirror packet.net #403 (HANDOVER.md §2, §7).

> **Note on current state:** the daemon is built through W2 — it attaches over
> RHP, binds its callsign, and serves the web tile, but the chat service itself
> (RF sessions, web chat, peering) lands in W3–W6. The packaging is complete and
> correct now; the binary it wraps gains function wave by wave.
