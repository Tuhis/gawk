package relay

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/screening"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

func TestFetch(t *testing.T) {
	frame := []byte{1, 2, 3}
	var gotAuth, gotINM string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotINM = r.Header.Get("Authorization"), r.Header.Get("If-None-Match")
		switch r.URL.Path {
		case screening.KeyframePath("ABC234"):
			w.Header().Set(screening.HeaderPublisherEpoch, "e1")
			w.Header().Set(screening.HeaderCodec, "h264")
			w.Header().Set("ETag", screening.KeyframeETag("e1", 7))
			if r.Header.Get("If-None-Match") == screening.KeyframeETag("e1", 7) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Write(frame)
		case screening.KeyframePath("REDIR2"):
			http.Redirect(w, r, "http://elsewhere.invalid/", http.StatusFound)
		case screening.KeyframePath("BROKEN"):
			w.WriteHeader(http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	f := New("tok", time.Second)
	ctx := context.Background()

	k, err := f.Fetch(ctx, srv.URL+"/", "ABC234", "")
	if err != nil {
		t.Fatal(err)
	}
	if k.NotModified || string(k.Frame) != string(frame) || k.Epoch != "e1" || k.Codec != "h264" || k.ETag != `"e1-7"` {
		t.Fatalf("200: %+v", k)
	}
	if gotAuth != "Bearer tok" || gotINM != "" {
		t.Fatalf("auth %q, If-None-Match %q", gotAuth, gotINM)
	}

	k, err = f.Fetch(ctx, srv.URL, "ABC234", `"e1-7"`)
	if err != nil || !k.NotModified || k.Frame != nil || k.Epoch != "e1" {
		t.Fatalf("304: %+v %v", k, err)
	}

	if _, err := f.Fetch(ctx, srv.URL, "GONE22", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("404: %v", err)
	}
	if _, err := f.Fetch(ctx, srv.URL, "BROKEN", ""); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("500: %v", err)
	}
	// The bearer never follows a redirect off the relay.
	if _, err := f.Fetch(ctx, srv.URL, "REDIR2", ""); err == nil {
		t.Fatal("a redirect was followed or accepted")
	}
}

func TestFetchRefusesOversizeAndEpochless(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == screening.KeyframePath("NOEPOC") {
			w.Write([]byte{1})
			return
		}
		w.Header().Set(screening.HeaderPublisherEpoch, "e1")
		w.Write(make([]byte, wire.MaxKeyframeBytes+1))
	}))
	defer srv.Close()
	f := New("tok", 5*time.Second)
	if _, err := f.Fetch(context.Background(), srv.URL, "ABC234", ""); err == nil {
		t.Fatal("an oversize keyframe was accepted")
	}
	if _, err := f.Fetch(context.Background(), srv.URL, "NOEPOC", ""); err == nil {
		t.Fatal("a keyframe without an epoch was accepted")
	}
}
