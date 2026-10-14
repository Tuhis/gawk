package ops

// R40 CF1 (docs/71 D1, §9.2): the keyframe route's credential, answers and
// headers, for every keyframe shape a broadcaster sends.

import (
	"bytes"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/screening"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

const screeningToken = "sampler-only-token"

// screeningHandler builds the ops mux with the keyframe route and, beside it,
// the admin API under adminAuth (nil: no admin API), so a test can try each
// credential on the other's route.
func screeningHandler(t *testing.T, token string, adminAuth *AdminAuth) (http.Handler, *hub.Registry) {
	t.Helper()
	r := hub.NewRegistry(discardLog, hub.Options{BroadcastGrace: 5 * time.Minute})
	var admin *AdminOptions
	if adminAuth != nil {
		admin = &AdminOptions{Registry: r, Config: config.Config{AdminAPIToken: adminToken}, Auth: adminAuth, Log: discardLog}
	}
	h := Handler(r, nil, metrics.NewBaseRegistry("v"), discardLog, nil, admin,
		&ScreeningOptions{Registry: r, Token: token, Log: discardLog})
	return h, r
}

func keyframePath(id string) string { return screening.KeyframePath(id) }

func getWith(h http.Handler, path, bearer, ifNoneMatch string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	h.ServeHTTP(w, req)
	return w
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// streamFrame builds a StreamFrame message the way a broadcaster writes it:
// header, embedded DecoderConfig, payload.
func streamFrame(t *testing.T, frameID uint32, codec string, extradata, payload []byte) []byte {
	t.Helper()
	cfg, err := wire.AppendDecoderConfig(nil, wire.DecoderConfig{Codec: codec, Extradata: extradata})
	if err != nil {
		t.Fatalf("AppendDecoderConfig: %v", err)
	}
	msg, err := wire.AppendStreamFrameHeader(nil, wire.StreamFrameHeader{
		Keyframe: true, FrameID: frameID, TimestampUs: uint64(frameID) * 33_333,
		ConfigLen: uint32(len(cfg)), PayloadLen: uint32(len(payload)),
	})
	if err != nil {
		t.Fatalf("AppendStreamFrameHeader: %v", err)
	}
	return append(append(msg, cfg...), payload...)
}

// Keyframe headers from ffmpeg 7.0.2 encodes of lavfi testsrc — the same
// fixtures internal/mediaprobe's tests use — wrapped the way each broadcaster
// ships them.
const (
	baselineSPS    = "6742c01fd9005005bb0110000003001000000303c0f1832480"
	baselinePPS    = "68cb83cb20"
	highAnnexB     = "0000000167640028acd940780227e5c044000003000400000300f03c60c65800000001"
	vp8Keyframe    = "3077019d012a0005d0020007088585888584880202224c71"
	vp9Keyframe    = "a2498342e09ff059f60638241c184a1c02c65f7fdffbdf9b"
	idrSliceHeader = "65888404"
)

func keyframeFixtures(t *testing.T) map[string]struct {
	msg   []byte
	codec string
} {
	sps, pps := unhex(t, baselineSPS), unhex(t, baselinePPS)
	// WebCodecs' avc format: an AVCDecoderConfigurationRecord in the config,
	// length-prefixed NAL units in the payload (no in-band SPS).
	avcC := []byte{1, sps[1], sps[2], sps[3], 0xff, 0xe1, 0, byte(len(sps))}
	avcC = append(avcC, sps...)
	avcC = append(avcC, 1, 0, byte(len(pps)))
	avcC = append(avcC, pps...)
	idr := unhex(t, idrSliceHeader)
	avccPayload := append([]byte{0, 0, 0, byte(len(idr))}, idr...)
	// The native broadcasters: Annex B with SPS/PPS in-band, empty extradata.
	annexB := append(unhex(t, highAnnexB), idr...)

	return map[string]struct {
		msg   []byte
		codec string
	}{
		"web AVCC H.264":        {streamFrame(t, 30, "avc1.42C01F", avcC, avccPayload), "h264"},
		"desktop Annex-B H.264": {streamFrame(t, 30, "avc1.640028", nil, annexB), "h264"},
		"VP8":                   {streamFrame(t, 30, "vp8", nil, unhex(t, vp8Keyframe)), "vp8"},
		"VP9":                   {streamFrame(t, 30, "vp09.00.10.08", nil, unhex(t, vp9Keyframe)), "vp9"},
	}
}

// With no screening token the route does not exist — 404 even to a caller
// presenting some bearer — exactly like a relay that predates R40.
func TestScreeningRouteIsDarkWithoutAToken(t *testing.T) {
	for name, opts := range map[string]*ScreeningOptions{
		"nil options": nil,
		"empty token": {Token: ""},
	} {
		t.Run(name, func(t *testing.T) {
			r := hub.NewRegistry(discardLog, hub.Options{})
			if opts != nil {
				opts.Registry = r
			}
			h := Handler(r, nil, metrics.NewBaseRegistry("v"), discardLog, nil, nil, opts)
			id, pub, err := r.StartPublish("")
			if err != nil {
				t.Fatalf("StartPublish: %v", err)
			}
			defer pub.Close()
			if w := getWith(h, keyframePath(id), "anything", ""); w.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (unregistered)", w.Code)
			}
		})
	}
}

// Every wrong credential is 401 — including the admin token and a valid admin
// JWT, which are good credentials for a DIFFERENT surface — and the screening
// token is 401 on /internal/admin/*. Least privilege (D1): neither surface's
// holder can use the other.
func TestScreeningRouteAuthIsItsOwn(t *testing.T) {
	idp := newFakeIDP(t)
	adminAuth := oidcAuth(t, idp, func(o *AdminAuthOptions) { o.Token = adminToken })
	h, r := screeningHandler(t, screeningToken, adminAuth)
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	defer pub.Close()
	if err := pub.IngestKeyframeStream(bytes.NewReader(keyframeFixtures(t)["VP8"].msg)); err != nil {
		t.Fatalf("IngestKeyframeStream: %v", err)
	}

	if w := getWith(h, keyframePath(id), screeningToken, ""); w.Code != http.StatusOK {
		t.Fatalf("the screening token = %d, want 200", w.Code)
	}
	// The admin credentials really are good — on their own routes.
	for name, cred := range map[string]string{"admin token": adminToken, "admin JWT": idp.goodToken()} {
		if w := getWith(h, "/internal/admin/broadcasts", cred, ""); w.Code != http.StatusOK {
			t.Fatalf("%s on /internal/admin/broadcasts = %d, want 200; the fixture is wrong", name, w.Code)
		}
	}

	for _, tc := range []struct{ name, bearer string }{
		{"no header", ""},
		{"wrong token", "not-the-token"},
		{"a prefix of the token", screeningToken[:len(screeningToken)-1]},
		{"the token with a trailing byte", screeningToken + "x"},
		{"empty credential", " "},
		{"the admin token", adminToken},
		{"a valid admin JWT", idp.goodToken()},
	} {
		w := getWith(h, keyframePath(id), tc.bearer, "")
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", tc.name, w.Code)
		}
		if w.Body.Len() > 0 && bytes.Contains(w.Body.Bytes(), []byte{wire.Version, wire.TypeStreamFrame}) {
			t.Errorf("%s: a refused request got frame bytes", tc.name)
		}
	}
	// A Basic scheme carrying the right token is still not a bearer.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, keyframePath(id), nil)
	req.Header.Set("Authorization", "Basic "+screeningToken)
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("Basic scheme = %d, want 401", w.Code)
	}

	for _, path := range []string{"/internal/admin/broadcasts", "/internal/admin/rooms", "/internal/admin/config"} {
		if w := getWith(h, path, screeningToken, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("the screening token on %s = %d, want 401", path, w.Code)
		}
	}
}

// 200 for every keyframe shape a broadcaster sends — the bytes verbatim, the
// probed codec, the session's epoch and an ETag of the two — and 304 for a
// matching If-None-Match.
func TestScreeningRouteServesEveryKeyframeShape(t *testing.T) {
	for name, fx := range keyframeFixtures(t) {
		t.Run(name, func(t *testing.T) {
			h, r := screeningHandler(t, screeningToken, nil)
			id, pub, err := r.StartPublish("")
			if err != nil {
				t.Fatalf("StartPublish: %v", err)
			}
			defer pub.Close()
			if err := pub.IngestKeyframeStream(bytes.NewReader(fx.msg)); err != nil {
				t.Fatalf("IngestKeyframeStream: %v", err)
			}

			w := getWith(h, keyframePath(id), screeningToken, "")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			if !bytes.Equal(w.Body.Bytes(), fx.msg) {
				t.Fatalf("body is not the StreamFrame verbatim:\n got %x\nwant %x", w.Body.Bytes(), fx.msg)
			}
			hdr := w.Header()
			epoch := hdr.Get(screening.HeaderPublisherEpoch)
			if got := hdr.Get(screening.HeaderCodec); got != fx.codec {
				t.Errorf("%s = %q, want %q", screening.HeaderCodec, got, fx.codec)
			}
			if len(epoch) != 16 {
				t.Errorf("%s = %q, want a 16-character epoch", screening.HeaderPublisherEpoch, epoch)
			}
			etag := hdr.Get("ETag")
			if etag != screening.KeyframeETag(epoch, 30) {
				t.Errorf("ETag = %q, want %q", etag, screening.KeyframeETag(epoch, 30))
			}
			if got := hdr.Get("Content-Type"); got != screening.KeyframeContentType {
				t.Errorf("Content-Type = %q", got)
			}
			if got := hdr.Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}

			for _, inm := range []string{etag, "W/" + etag, `"other", ` + etag, "*"} {
				nm := getWith(h, keyframePath(id), screeningToken, inm)
				if nm.Code != http.StatusNotModified || nm.Body.Len() != 0 {
					t.Errorf("If-None-Match %s: status %d body %d bytes, want 304 and none", inm, nm.Code, nm.Body.Len())
				}
				if nm.Header().Get("ETag") != etag || nm.Header().Get(screening.HeaderPublisherEpoch) != epoch {
					t.Errorf("If-None-Match %s: the 304 lost the ETag or epoch headers", inm)
				}
			}
			if stale := getWith(h, keyframePath(id), screeningToken, `"`+epoch+`-29"`); stale.Code != http.StatusOK {
				t.Errorf("a stale tag = %d, want 200", stale.Code)
			}
		})
	}
}

// 404 for an unknown ID, a broadcast with no keyframe yet, and a broadcast
// whose publisher is away (docs/71 §9.2) — and a new publisher session then
// changes the ETag, so a sampler holding the old tag gets the new frame.
func TestScreeningRouteNotFoundAndNewSession(t *testing.T) {
	h, r := screeningHandler(t, screeningToken, nil)
	if w := getWith(h, keyframePath("ZZZZZZ"), screeningToken, ""); w.Code != http.StatusNotFound {
		t.Fatalf("unknown ID = %d, want 404", w.Code)
	}
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	if w := getWith(h, keyframePath(id), screeningToken, ""); w.Code != http.StatusNotFound {
		t.Fatalf("no keyframe yet = %d, want 404", w.Code)
	}
	fx := keyframeFixtures(t)["VP9"]
	if err := pub.IngestKeyframeStream(bytes.NewReader(fx.msg)); err != nil {
		t.Fatalf("IngestKeyframeStream: %v", err)
	}
	first := getWith(h, keyframePath(id), screeningToken, "")
	oldTag := first.Header().Get("ETag")

	pub.Close()
	if w := getWith(h, keyframePath(id), screeningToken, ""); w.Code != http.StatusNotFound {
		t.Fatalf("away publisher = %d, want 404: a frozen frame must not be scored", w.Code)
	}

	_, pub2, err := r.ResumePublish(id)
	if err != nil {
		t.Fatalf("ResumePublish: %v", err)
	}
	defer pub2.Close()
	if err := pub2.IngestKeyframeStream(bytes.NewReader(fx.msg)); err != nil {
		t.Fatalf("IngestKeyframeStream: %v", err)
	}
	w := getWith(h, keyframePath(id), screeningToken, oldTag)
	if w.Code != http.StatusOK {
		t.Fatalf("the previous session's ETag = %d, want 200 with the new session's frame", w.Code)
	}
	if w.Header().Get("ETag") == oldTag {
		t.Fatalf("the new session kept ETag %s", oldTag)
	}
}
