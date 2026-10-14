package ops

// R40's keyframe route (docs/71 D1, §9.2): GET
// /internal/screening/broadcasts/{id}/keyframe on the ops listener, the one
// read gawk-sampler makes of the relay. It serves the hub's cached keyframe —
// the StreamFrame bytes verbatim — so one GET is a fresh, self-contained
// frame, and an unchanged frame costs a 304.
//
// ITS OWN CREDENTIAL, NOTHING ELSE. The route is gated by -screening-token
// alone: the admin token and admin JWTs are refused here, and the screening
// token is refused on /internal/admin/* (it is unknown to AdminAuth). The
// admin surface reads publisher IPs; the sampler never needs them, so it
// never holds a credential that could (least privilege, D1). Unregistered —
// 404, not 401 — when the token is empty, like the admin routes.
//
// Frames pass through and are never logged (D8): the handler writes the
// cached slice to the response and keeps nothing.

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/screening"
)

// ScreeningOptions wires the keyframe route. A nil *ScreeningOptions, an
// empty Token or a nil Registry leaves it UNREGISTERED.
type ScreeningOptions struct {
	Registry *hub.Registry
	// Token is -screening-token, already trimmed by config.ParseFlags.
	Token string
	Log   *slog.Logger
}

// registerScreening adds the keyframe route to mux when a token is
// configured. It reports whether it registered anything.
func registerScreening(mux *http.ServeMux, opts *ScreeningOptions) bool {
	if opts == nil || opts.Token == "" || opts.Registry == nil {
		return false
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	token := []byte(opts.Token)
	mux.Handle(screening.KeyframeRoutePattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reason := authorizeScreening(r, token); reason != "" {
			log.Debug("screening request rejected", "path", r.URL.Path, "remote", r.RemoteAddr, "reason", reason)
			w.Header().Set("WWW-Authenticate", `Bearer realm="gawk-screening"`)
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		serveKeyframe(w, r, opts.Registry)
	}))
	return true
}

// authorizeScreening returns "" for the configured bearer token, or the
// reason it refused (Debug log only — never in the body). Constant-time, so
// the route is not a byte-at-a-time oracle for the token.
func authorizeScreening(r *http.Request, token []byte) string {
	raw := r.Header.Get("Authorization")
	if len(raw) <= adminBearerPrefixLen || !strings.EqualFold(raw[:adminBearerPrefixLen], "Bearer ") {
		return "missing or malformed Authorization header"
	}
	cred := strings.TrimSpace(raw[adminBearerPrefixLen:])
	if cred == "" {
		return "empty bearer credential"
	}
	if subtle.ConstantTimeCompare([]byte(cred), token) != 1 {
		return "bearer token did not match the screening token"
	}
	return ""
}

func serveKeyframe(w http.ResponseWriter, r *http.Request, reg *hub.Registry) {
	f, ok := reg.ScreeningKeyframe(r.PathValue("id"))
	if !ok {
		// One answer for "no such broadcast", "no keyframe yet" and "the
		// broadcaster is away" (§9.2): to the sampler all three mean "nothing
		// to score now", and none of them is an error.
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	etag := screening.KeyframeETag(f.PublisherEpoch, f.FrameID)
	h := w.Header()
	// The frame is a live screen capture; no intermediary may keep it.
	h.Set("Cache-Control", "no-store")
	h.Set("ETag", etag)
	h.Set(screening.HeaderPublisherEpoch, f.PublisherEpoch)
	if f.Codec != "" {
		h.Set(screening.HeaderCodec, f.Codec)
	}
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		// Unchanged frame (a static screen, a paused game): it costs
		// nothing, and the sampler repeats its previous score (D20).
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", screening.KeyframeContentType)
	h.Set("Content-Length", strconv.Itoa(len(f.Msg)))
	_, _ = w.Write(f.Msg)
}

// etagMatches applies If-None-Match's weak comparison (RFC 9110 §13.1.2):
// any listed tag equal to etag once a W/ prefix is stripped, or "*".
func etagMatches(header, etag string) bool {
	if header == "" {
		return false
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}
