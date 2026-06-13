# Vendored LinBPQ chat-node source — provenance

These files are a **pinned, read-only copy** of the BPQ Chat ("Round Table")
node source from John Wiseman G8BPQ's LinBPQ/BPQ32 codebase. They are the
**ground truth** from which `docs/design.md` derives the BPQ chat wire
protocol (the user command interface and the inter-node link protocol) and
its deficiency analysis (HANDOVER.md §3). They are **not compiled** into
pdn-bpqchat — pdn-bpqchat is a clean-room Go implementation; this is reference
material only.

## Source

- **Upstream:** https://github.com/g8bpq/LinBPQ
- **Commit:** `45dc77a4e18c41ce91f844f1ce6ccd0a5fc44fb8`
- **Vendored:** 2026-06-13
- **License:** LinBPQ is distributed under John Wiseman's BPQ32 licence
  (free for amateur radio use). These files remain under their original
  licence and copyright (© John Wiseman G8BPQ); they are included here under
  that licence solely as a protocol reference for an interoperating
  implementation.

## Files and why each is here

| File | Role |
|------|------|
| `HanksRT.c` | **The chat-node engine.** "Hank's Round Table" — the heart of the BPQ chat node: `chkctl()` (the inter-node link-protocol parser), the `*_xmit`/`*_tell` builders, `rt_cmd()`/`ProcessChatLine()` (the user `/` command parser), `CheckforDups()` (loop/dup suppression), the link-establishment handshake (`rtloginl`, `ProcessChatConnectScript`), keepalive/poll timers. **The primary source for the wire spec.** |
| `bpqchat.h` | The protocol constants (`FORMAT`, the `id_*` record types, offsets, flags) and the core structs (`ChatCIRCUIT`, `USER`, `CHATNODE`, `TOPIC`, `LINK`). |
| `bpqchat.c` | The Windows GUI front-end (config, monitor windows). Limited protocol value; kept for the config-field names and the `chatconfig.cfg` mapping. |
| `ChatUtils.c` | Connection plumbing (`Connected`/`Disconnected`/`DoReceivedData`), config load/save (`GetChatConfig`), the `OtherChatNodes` connect-script format. |
| `ChatUtilities.c` | Misc helpers. |
| `ChatMonitor.c` | The monitor/report datagram format. |
| `ChatHTMLConfig.c` | BPQ's own web-config of the chat node (reference for field semantics). |
| `chatconfig.cfg` | A real chat-node config: `OtherChatNodes` connect scripts, `ChatWelcomeMsg`, `ApplNum`, `MaxStreams`. |

## Updating the pin

To refresh against a newer upstream, re-clone the commit above (or a newer
one), re-copy these files, update the **Commit** and **Vendored** lines, and
re-review `docs/design.md` against any protocol changes. Never edit the
vendored files in place — they must remain a faithful copy.
