package peer

import (
	"context"
	"sort"
	"strings"
)

// ConfigKVAllowKey is the config-table key (chat.Store GetConfig/SetConfig) the
// inbound-peer allow-list persists under. Storing the editable set here — rather
// than only in RAM seeded from the environment — is what makes the list MUTABLE
// across restarts: the S5 web editor reads/writes this key, and a fresh process
// reloads it. The value is a newline-separated list of canonical callsigns.
const ConfigKVAllowKey = "peer_allow"

// AllowConfigStore is the persistence seam for the inbound-peer allow-list: the
// small string KV already on chat.Store (GetConfig/SetConfig). The allow-list
// keeps no store reference of its own; the loader/persister below take one, so the
// peer package stays decoupled from any concrete store (tests pass chat.MemStore).
type AllowConfigStore interface {
	GetConfig(ctx context.Context, key string) (string, bool, error)
	SetConfig(ctx context.Context, key, value string) error
}

// LoadAllowList resolves the EDITABLE allow-list at startup with this precedence:
//
//   - If the config table already holds a persisted set (ConfigKVAllowKey), that
//     persisted set is authoritative — it reflects every live web edit since the
//     last seed, so a restart never silently reverts an operator's changes.
//   - Otherwise the PDN_BPQCHAT_PEER_ALLOW value (passed as envSeed, already parsed
//     and canonicalised by config.parseAllow) seeds the list, and that seed is
//     PERSISTED so the next start (and the S5 editor) sees it in the config table.
//
// Either way the result is a hot-reloadable *AllowList. Outbound-dialed peers are
// pinned separately by the caller (Pin) and are not part of this editable set, so
// they are neither persisted nor strippable by an editor. A nil store yields an
// env-seeded, non-persisted list (the standalone / no-state-dir run).
func LoadAllowList(ctx context.Context, store AllowConfigStore, envSeed []string) (*AllowList, error) {
	a := NewAllowList()
	if store == nil {
		a.Replace(envSeed)
		return a, nil
	}
	raw, ok, err := store.GetConfig(ctx, ConfigKVAllowKey)
	if err != nil {
		return nil, err
	}
	if ok {
		// A persisted set exists (even an explicit empty one): it wins over the env
		// seed, so live edits survive restarts.
		a.Replace(decodeAllow(raw))
		return a, nil
	}
	// First run with no persisted set: seed from the environment and persist it so
	// the config table becomes the source of truth from here on.
	a.Replace(envSeed)
	if err := PersistAllowList(ctx, store, a); err != nil {
		return nil, err
	}
	return a, nil
}

// PersistAllowList writes the allow-list's current EDITABLE set to the config
// table. Pinned (outbound-dialed) entries are deliberately NOT persisted: they are
// derived from the dial config, not operator-owned. Call this after a live edit so
// the change survives a restart. A nil store is a no-op (standalone run).
func PersistAllowList(ctx context.Context, store AllowConfigStore, a *AllowList) error {
	if store == nil {
		return nil
	}
	return store.SetConfig(ctx, ConfigKVAllowKey, encodeAllow(a.Entries()))
}

// ReloadAllowList re-reads the persisted editable set and applies it to the live
// allow-list WITHOUT a restart (the hot-reload path). Both ingresses share the
// *AllowList pointer, so the new set takes effect at both immediately. Pinned
// entries are untouched. A nil store or an absent key leaves the list unchanged.
func ReloadAllowList(ctx context.Context, store AllowConfigStore, a *AllowList) error {
	if store == nil || a == nil {
		return nil
	}
	raw, ok, err := store.GetConfig(ctx, ConfigKVAllowKey)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	a.Replace(decodeAllow(raw))
	return nil
}

// encodeAllow serialises a callsign set for the config table: canonical callsigns,
// sorted, newline-separated. Sorting keeps the stored value stable (no spurious
// rewrites when set membership is unchanged) and human-legible.
func encodeAllow(callsigns []string) string {
	out := make([]string, 0, len(callsigns))
	for _, c := range callsigns {
		if cc := normCallsign(c); cc != "" {
			out = append(out, cc)
		}
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// decodeAllow parses a persisted allow-list value (newline-, comma-, or
// whitespace-separated) back into canonical callsigns. It is tolerant of either
// separator so a hand-edited config value still loads.
func decodeAllow(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ' ' || r == '\t'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if cc := normCallsign(f); cc != "" {
			out = append(out, cc)
		}
	}
	return out
}
