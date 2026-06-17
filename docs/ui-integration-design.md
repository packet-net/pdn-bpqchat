# pdn-bpqchat — web UI integration design

**Status:** ACCEPTED 2026-06-17

This document is the approved plan for the **bpqchat UI arc**: bringing the web tile (`internal/web`) up to a first-class, multi-user, federation-aware, admin-manageable surface inside the pdn app-gateway, without breaking the layering that makes pdn-bpqchat a clean app package. It supersedes nothing in [`design.md`](design.md) — the W0–W7 design stands — it fills the gaps that emerged once the tile met a real multi-user node behind the gateway.

The arc is delivered as slices **S0..S6**, one commit each, each gated on `go build ./... && go vet ./... && go test ./...` green.

---

## Decoupling is sacrosanct (the non-negotiable frame)

Everything below obeys the pdn app-package contract from [`design.md` §2](design.md):

- **Zero pdn-side bpqchat code.** pdn never learns what a topic, a claim, or a peer is.
- The **only** contracts with pdn are:
  - **RHPv2** (the packet plane — `internal/rhp`), and
  - the **app-gateway** (the manifest `ui:` block + the injected `X-Pdn-*` identity headers + `X-Forwarded-Prefix`). See packet.net `docs/app-gateway.md`.
- The chat engine (`chat.Hub`) **stays the pure, host-free core.** It already serves RF, web, and mesh users uniformly. We never fork it. All new web behaviour lives in `internal/web`, with a thin, justified touch of `internal/config` + `internal/store/sqlite` + `internal/node` + `internal/peer` only for the allow-list config source (S4) — and even that is config/store plumbing, not engine logic.

If a slice would require pdn to know anything bpqchat-specific, the slice is wrong.

---

## Locked design decisions (Tom approved — do not deviate)

1. **Live transport = SSE.** The tile already streams over SSE and the pdn app-gateway proxies SSE/chunked fine. **WebSockets are NOT proxied by the gateway — never introduce a WS dependency.** Keep the existing **≤20s SSE keepalive** (`handlers.go` `handleEvents` ticker) — it must stay under the gateway's **100s activity timeout** so the proxied stream is never reaped mid-idle.

2. **Identity = multi-user on the web plane.** Each pdn user (`X-Pdn-User`) claims their **own** amateur callsign. There is **one app callsign on air** (the node-owned bind, `PDN_APP_CALLSIGN`) but **distinct chat identities per web viewer**. With anonymous access / management-auth-off, the web plane degrades to the **node-owner base callsign** (today's `viewerCall` → `ownerCall` fallback).

3. **Claim mapping persists.** A new **`claims` table** in the existing SQLite store (`bpqchat.db` under `PDN_APP_STATE`) maps a pdn account to its claimed callsign. It **survives reinstall**. A **uniqueness constraint** prevents two pdn accounts claiming the same callsign — a cross-account collision returns **409**.

4. **The §4.1 inbound-peer allow-list editor is a full admin-scope web editor.** Gated on `X-Pdn-Scope == admin`, **audited**, and built **on top of the EXISTING #3 allow-list** (do not reimplement enforcement — see the NOTE on Gap 4). A `chat.yaml` / `PDN_BPQCHAT_PEER_ALLOW` seed remains for headless operation.

5. **Federation view = split.**
   - **For everyone:** origin-node badges on off-node users and messages (the wire shape already carries `node`; `offNode()` already computes it).
   - **Admin scope only:** the full topology + per-link state / last-seen / RTT panel.

6. **Gateway trust is enforced.** Requests reach the tile **only** through the gateway; the loopback bind means an unstamped request is a direct probe, never a viewer. We **403** unless `X-Pdn-Gateway == 1`, thread `X-Forwarded-Prefix` through every server-rendered absolute URL, and mark responses `no-store`. (Delivered in S0.)

---

## The five gaps

These are the deltas between "W4 tile that proved the contract" and "a tile a real multi-user federated node owner can live in".

### Gap 1 — Gateway trust + prefix threading (server-rendered-page safety)

The tile served only the SPA, whose `fetch()`/`EventSource` paths are relative, so prefix-rebasing was invisible. The moment we add **server-rendered pages** (the claim form, the admin editor), any absolute URL we emit — a redirect `Location`, a `<form action>`, an `<a href>` — must be re-prefixed with `X-Forwarded-Prefix` or the browser posts it to **pdn's own root** and gets a 405/404. This is exactly the **pdn-bbs "claim-405" regression**, pre-empted here. There was also no defence against a direct unstamped probe reaching the loopback upstream.

**Addressed by S0.**

### Gap 2 — Multi-user identity is not yet first-class

`viewerCall` already reads `X-Pdn-User` and falls back to the node owner, but there is **no way for a pdn user to choose which amateur callsign they speak as**, and no persistence of that choice. Two pdn users today would each appear under whatever `X-Pdn-User` string the gateway injects, with no claim model, no validation, and no collision handling.

**Addressed by S2 (store + claim model) and S3 (claim UI).**

### Gap 3 — No federation visibility

Off-node users/messages carry an origin node on the wire (`offNode()`), but the UI does not **badge** them, and there is **no topology/link-state view** at all. An operator cannot see who is linked, which links are healthy, or where a remote user actually lives.

**Addressed by S6 (badges for everyone; admin topology/link-state panel).**

### Gap 4 — Inbound peer allow-list: no persisted+editable config source, no admin editor

> **NOTE — stale design / already shipped.** The §4.1 allow-list **ENFORCEMENT already shipped in #3** (`internal/peer/allow.go` `AllowList`, default-deny, SSID-exact, telemetry-counted). It is **enforced at BOTH ingress points** — `node.(*Link).serveInbound` and `peer.ServeInboundIP` — and is seeded from `PDN_BPQCHAT_PEER_ALLOW` with outbound peers folded in (`config.(*Config).EffectiveAllow`). **Do not reimplement enforcement.**
>
> The remaining gap is narrower: there is **no persisted, editable config source** beyond the env var (an owner can't change the list without restarting the daemon with a new env), and **no admin web editor**. That is all S4/S5 build — on top of the existing `AllowList`.

**Addressed by S4 (persisted+editable config source feeding the existing `AllowList`) and S5 (the admin-scope web editor, audited).**

### Gap 5 — No admin scope surface at all

`X-Pdn-Scope` is read into `Identity` but nothing in the tile is **gated** on it. The split-federation view and the allow-list editor both need a real admin boundary, and admin mutations need an **audit trail**.

**Addressed by S5 (admin editor + audit) and S6 (admin topology panel).** S0 lays the prefix/no-store/403 groundwork every admin page depends on.

---

## Slice plan (S0..S6)

Each slice is one commit, tagged `(S#, bpqchat UI arc)`, build/vet/test green before commit, tests asserting **rendered** HTTP/SSE behaviour (not just persisted values).

| Slice | Scope | Touches |
|------|-------|---------|
| **S0** | **Gateway-trust middleware + `X-Forwarded-Prefix` threading.** Wrap the mux: 403 unless `X-Pdn-Gateway==1` (exempting the daemon's own `/healthz` probe); capture `X-Forwarded-Prefix` into request context; `Cache-Control: no-store`; a `U(prefix, path)` / `u(r, path)` helper so all future absolute URLs/redirects/form-actions stay inside `/apps/bpqchat/`. The SPA is unchanged (relative paths). +prefix httptest alongside the no-prefix tests (absent prefix ⇒ byte-identical SPA). **This doc.** | `internal/web` (+ this doc) |
| **S1** | **Identity plumbing groundwork** in `internal/web`: factor `viewerCall` so a claimed callsign (when present, from S2's store) overrides the raw `X-Pdn-User`, keeping the owner fallback; expose viewer scope to handlers. Pure web-layer; no store yet. | `internal/web` |
| **S2** | **`claims` table + claim model** in the existing SQLite store: `(pdn_user PK, callsign UNIQUE, claimed_at)`. Store API to get/set/clear a claim, returning a typed collision error → **409**. Survives reinstall. Wire `viewerCall` to consult it. | `internal/store/sqlite`, `internal/web` |
| **S3** | **Claim UI** — a server-rendered claim form + POST handler (the first page that exercises S0's `U()`/no-store). Validates callsign shape, persists via S2, 409 on cross-account collision, redirects via `u(r, …)` back into the mount. | `internal/web` |
| **S4** | **Persisted + editable allow-list config source** feeding the **existing** `#3 AllowList`: a `chat.yaml`/`PDN_BPQCHAT_PEER_ALLOW`-seeded, owner-editable source the daemon can reload, so the effective allow-list can change without an env-var restart. No enforcement changes — enforcement already exists. | `internal/config`, `internal/peer`, `internal/node` (thin) |
| **S5** | **Admin allow-list editor** — `X-Pdn-Scope==admin`-gated, **audited** server-rendered editor (add/remove peers) that mutates S4's source and the live `AllowList`. Non-admins get 403. | `internal/web` |
| **S6** | **Federation view (split).** Origin-node **badges** on off-node users/messages for everyone (extend the existing wire shape/SPA render). **Admin-only** topology + per-link state / last-seen / RTT panel. | `internal/web` (+ read-only peer state surface) |

Slices are independently shippable; S3 depends on S2, S5 depends on S4, S6 is self-contained on the existing peer state.

---

## What S0 actually changed

- `internal/web/gateway.go` — `gatewayTrust` middleware (403/prefix-capture/no-store, `/healthz` exempt), `U`/`u`/`PrefixFromContext`.
- `internal/web/web.go` — front the mux with `gatewayTrust(mux)`.
- `internal/web/gateway_test.go` — 403-on-unstamped, `/healthz` exemption, no-store, **+prefix vs no-prefix byte-identical SPA**, `U()` table, nil-context default.
- `internal/web/web_test.go` — existing tests now send `X-Pdn-Gateway: 1` (they model real gateway requests); `gwGet`/`gwPost` helpers.
- This document.

No engine, RHPv2, manifest, or pdn-side change. The locked decisions and decoupling rules above are unviolated.
