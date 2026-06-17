package web

import (
	"context"
	"net/http"
	"strings"
)

// Gateway trust + prefix threading (the bbs "claim-405" class of bug, pre-empted).
//
// The web tile is reachable ONLY through pdn's app-gateway, which mounts it under
// a public prefix (X-Forwarded-Prefix == /apps/bpqchat) and stamps every request
// with X-Pdn-Gateway: 1 after stripping any client-supplied copy
// (packet.net docs/app-gateway.md §Identity injection). Because the upstream
// binds loopback only, a request that arrives WITHOUT that stamp can only be a
// direct probe — never a real user — so we refuse it with 403 rather than trust
// forgeable identity headers on it.
//
// Two reasons this matters for the server-rendered pages S3..S5 will add:
//   - the gateway STRIPS the /apps/bpqchat prefix, so the app sees root-relative
//     paths and "is mounted at the site root from its own point of view"; any
//     absolute URL we emit (a redirect Location, a <form action>, an <a href>)
//     must be re-prefixed with X-Forwarded-Prefix or the browser will post it to
//     pdn's OWN root and get a 405/404 — exactly the bbs claim-form regression.
//   - a cached server-rendered admin page would leak one viewer's state to the
//     next, so we mark every gateway response no-store.
//
// The JS SPA (index.go) already uses relative fetch() paths ('events', 'send', …)
// which the browser resolves against /apps/bpqchat/ on its own, so it needs no
// change; this middleware exists for the pages we are about to add.

// prefixCtxKey is the context key under which the request's X-Forwarded-Prefix is
// stored. Unexported so only this package can read/write it.
type prefixCtxKey struct{}

// PrefixFromContext returns the public mount prefix (X-Forwarded-Prefix) captured
// for the in-flight request, or "" when the app is hit directly on loopback
// (absent prefix => root-relative URLs render unchanged, per the gateway spec).
func PrefixFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if p, ok := ctx.Value(prefixCtxKey{}).(string); ok {
		return p
	}
	return ""
}

// U builds a viewer-facing absolute URL by joining the gateway prefix to an
// app-root-relative path, so a server-rendered link/redirect/form-action stays
// inside the app's public mount (/apps/bpqchat/…) instead of escaping to pdn's
// root. With an empty prefix (direct loopback access) it returns path unchanged.
//
// path is treated as app-root-relative; a leading slash is normalised so callers
// may pass "claim" or "/claim" interchangeably. The result has exactly one slash
// at the join.
func U(prefix, path string) string {
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if prefix == "" {
		return path
	}
	return strings.TrimRight(prefix, "/") + path
}

// u is the per-request convenience over U: it reads the captured prefix from the
// request context so handlers need only the app-root-relative path.
func u(r *http.Request, path string) string {
	return U(PrefixFromContext(r.Context()), path)
}

// gatewayTrust wraps the app mux so that every request which is NOT the daemon's
// own liveness probe must arrive gateway-stamped:
//   - 403 unless X-Pdn-Gateway == 1 (a direct loopback probe can't be a user);
//   - X-Forwarded-Prefix is captured into the request context for U()/u();
//   - Cache-Control: no-store on the response (server-rendered pages carry
//     per-viewer state; nothing here is safely cacheable).
//
// /healthz is exempt: it is the daemon's own loopback health check, hit directly
// and never through the gateway, and must stay forgeable-identity-free anyway.
func gatewayTrust(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("X-Pdn-Gateway") != "1" {
			http.Error(w, "forbidden: requests must arrive through the pdn app-gateway", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		r = r.WithContext(context.WithValue(r.Context(), prefixCtxKey{}, r.Header.Get("X-Forwarded-Prefix")))
		next.ServeHTTP(w, r)
	})
}
