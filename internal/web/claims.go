package web

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/m0lte/pdn-bpqchat/internal/store/sqlite"
)

// ClaimStore is the persistence seam for the pdn-user → amateur-callsign mapping
// (the multi-user web plane, S2). It lives in the durable SQLite store so a claim
// survives a reinstall — a returning web user keeps the identity they post, DM,
// and federate under. The web layer depends only on this narrow interface, not on
// the concrete store, so the engine stays host-free.
//
// Claim must return sqlite.ErrCallsignClaimed (via errors.Is) when callsign is
// already held by a DIFFERENT pdn user — the cross-account-collision case the web
// layer renders as HTTP 409.
type ClaimStore interface {
	// ClaimedCall returns the callsign a pdn user has claimed, or ("", false).
	ClaimedCall(ctx context.Context, pdnUser string) (string, bool, error)
	// Claim upserts the pdn user's claim. A user re-claiming their own current
	// callsign is an idempotent success; claiming another user's callsign returns
	// sqlite.ErrCallsignClaimed.
	Claim(ctx context.Context, pdnUser, callsign string, claimedAt time.Time) error
}

// ErrCallsignClaimed re-exports the store's cross-account-collision sentinel so
// callers in this package can match it without naming the sqlite package twice.
var ErrCallsignClaimed = sqlite.ErrCallsignClaimed

// isCallsignClaimed reports whether err is the cross-account-collision sentinel
// (→ HTTP 409), matched through errors.Is so a wrapped error still counts.
func isCallsignClaimed(err error) bool { return errors.Is(err, ErrCallsignClaimed) }

// claimFormTmpl is the server-rendered page an authenticated-but-unclaimed pdn
// user sees in place of the chat. It is dependency-free (inline CSS), embed-aware
// via ?pdn_embed=1, and posts to .Action — built through u() so the form stays
// inside the app's gateway mount (the bbs claim-405 class of bug, pre-empted).
// .User and .Err are auto-escaped by html/template (the page is server-rendered,
// so untrusted X-Pdn-User text must never reach the DOM raw).
var claimFormTmpl = template.Must(template.New("claim").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Claim your callsign — BPQ Chat</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
  :root { color-scheme: light dark; }
  body { font: 15px/1.5 system-ui, -apple-system, Segoe UI, Roboto, sans-serif;
    margin: 0; min-height: 100vh; display: flex; align-items: center; justify-content: center; }
  .card { max-width: 26rem; width: 100%; padding: 1.6rem; margin: 1rem; }
  h1 { font-size: 1.25rem; margin: 0 0 .4rem; }
  p { color: #6b7280; margin: .2rem 0 1rem; }
  form { display: flex; gap: .5rem; }
  input { flex: 1; padding: .55rem .7rem; font: inherit; border: 1px solid #d7dae0; border-radius: 8px; }
  button { padding: .55rem 1.1rem; font: inherit; font-weight: 600; cursor: pointer;
    border: 0; border-radius: 8px; background: #2563eb; color: #fff; }
  .err { color: #c0392b; margin: 0 0 1rem; font-weight: 600; }
  .hint { font-size: .85rem; }
</style>
</head>
<body>
<div class="card">
  <h1>Claim your callsign</h1>
  <p>Signed in as <b>{{.User}}</b>. Choose the amateur callsign you'll chat under.
     It becomes your identity for messages, DMs, and across linked nodes.</p>
  {{if .Err}}<p class="err">{{.Err}}</p>{{end}}
  <form method="post" action="{{.Action}}">
    <input name="callsign" autocomplete="off" autocapitalize="characters" autofocus
      placeholder="e.g. M0LTE" value="{{.Call}}">
    <button>Claim</button>
  </form>
  <p class="hint">A callsign is letters and digits including at least one number; no SSID suffix.</p>
</div>
</body>
</html>
`))

// renderClaimForm writes the claim page (HTTP 200) for the in-flight viewer — the
// first-visit form an authenticated-but-unclaimed user sees in place of the chat.
// The form action is built through u() so it posts back inside the gateway mount
// regardless of the public prefix.
func (s *Server) renderClaimForm(w http.ResponseWriter, r *http.Request, errMsg string) {
	s.renderClaimFormErr(w, r, http.StatusOK, "", errMsg)
}

// renderClaimFormErr re-shows the claim form with a status code and a reason —
// used to redisplay it on a rejected POST (400 bad callsign, 409 collision) while
// preserving what the user typed (prefill) so they need not retype it.
func (s *Server) renderClaimFormErr(w http.ResponseWriter, r *http.Request, status int, prefill, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	data := struct{ User, Action, Err, Call string }{
		User:   strings.TrimSpace(IdentityFromRequest(r).User),
		Action: u(r, "/claim"),
		Err:    errMsg,
		Call:   strings.TrimSpace(prefill),
	}
	if err := claimFormTmpl.Execute(w, data); err != nil {
		s.log.Warn("claim form render failed", "err", err)
	}
}

// looksLikeCallsign is the lightweight "is this plausibly an amateur callsign"
// check the claim form applies before persisting (design §S2): base callsign
// rules only — at least 3 characters, alphanumeric, and containing at least one
// digit (every amateur callsign carries a number in its prefix). It is the same
// permissive base check the on-air allow-list uses, minus the optional SSID
// (a claim is the SSID-less base identity; the on-air bind keeps the node SSID).
// It deliberately does NOT enforce a full regional callsign grammar — that would
// reject legitimate calls from regions we did not enumerate.
func looksLikeCallsign(cs string) bool {
	cs = strings.TrimSpace(cs)
	if len(cs) < 3 {
		return false
	}
	hasDigit := false
	for _, r := range cs {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		default:
			return false // spaces, hyphens (SSID), punctuation are all rejected
		}
	}
	return hasDigit
}

// normClaim canonicalises a claimed callsign: trimmed and upper-cased, with any
// SSID suffix stripped, so the stored identity is the bare base callsign (the
// on-air bind re-applies the node SSID — design.md §6). Returns the base.
func normClaim(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if i := strings.IndexByte(s, '-'); i >= 0 {
		s = s[:i]
	}
	return s
}

// handleClaim accepts the claim form POST: validate the callsign, upsert the
// mapping, then redirect the viewer back to the app root (303) so a refresh shows
// the chat rather than re-POSTing. The redirect Location is built through u() so
// it stays inside the app's /apps/bpqchat/ mount behind the gateway prefix.
//
// Anonymous viewers (auth off — empty X-Pdn-User) never reach a claim: they take
// the node-owner fallback and the form is never shown, so a claim POST from an
// anonymous session is a 400 (there is no pdn identity to map).
func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	pdnUser := strings.TrimSpace(IdentityFromRequest(r).User)
	if pdnUser == "" {
		http.Error(w, "no pdn identity to claim a callsign for (management auth is off)", http.StatusBadRequest)
		return
	}
	if s.claims == nil {
		http.Error(w, "claims are not available", http.StatusServiceUnavailable)
		return
	}
	raw := readField(r, "callsign")
	call := normClaim(raw)
	if !looksLikeCallsign(call) {
		// Re-show the form with the reason (and the rejected status the API asserts).
		s.renderClaimFormErr(w, r, http.StatusBadRequest, raw,
			"That does not look like a callsign (need at least 3 letters/digits including a digit, no SSID).")
		return
	}
	if err := s.claims.Claim(r.Context(), pdnUser, call, time.Now()); err != nil {
		if isCallsignClaimed(err) {
			s.renderClaimFormErr(w, r, http.StatusConflict, raw,
				"That callsign is already claimed by another user.")
			return
		}
		s.log.Warn("claim failed", "pdnUser", pdnUser, "call", call, "err", err)
		http.Error(w, "could not record the claim", http.StatusInternalServerError)
		return
	}
	s.log.Info("callsign claimed", "pdnUser", pdnUser, "call", call)
	http.Redirect(w, r, u(r, "/"), http.StatusSeeOther)
}

// requireViewer resolves the viewer for an action/live endpoint, writing a 403
// and returning ok=false when the viewer is authenticated but has not yet claimed
// a callsign (they must POST /claim first). The SPA never reaches these endpoints
// while unclaimed — the index serves the claim form instead — so this is the
// defensive guard for a direct/raced request, not the primary UX path.
func (s *Server) requireViewer(w http.ResponseWriter, r *http.Request) (string, bool) {
	call, mapped := s.resolveViewer(r)
	if !mapped {
		http.Error(w, "claim a callsign before chatting", http.StatusForbidden)
		return "", false
	}
	return call, true
}

// resolveViewer maps a request to its chat callsign and whether a claim is needed.
//
//   - empty X-Pdn-User (management auth off / anonymous): the node-owner fallback
//     — the single-user degenerate path — so it resolves to ownerCall, mapped.
//   - X-Pdn-User set with a stored claim: the claimed base callsign, mapped.
//   - X-Pdn-User set with NO claim: ("", false) — the viewer must claim first, so
//     the index renders the claim form and the live/action endpoints refuse.
func (s *Server) resolveViewer(r *http.Request) (call string, mapped bool) {
	user := strings.TrimSpace(IdentityFromRequest(r).User)
	if user == "" {
		return s.ownerCall, true // anonymous degenerate path: node owner
	}
	if s.claims == nil {
		return "", false
	}
	cs, ok, err := s.claims.ClaimedCall(r.Context(), user)
	if err != nil {
		s.log.Warn("claim lookup failed", "pdnUser", user, "err", err)
		return "", false
	}
	if !ok {
		return "", false
	}
	return cs, true
}
