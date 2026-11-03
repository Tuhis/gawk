package main

import (
	"context"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/streamkey"
)

const testToken = "00112233445566778899aabbccddeeff"

func testKey(t *testing.T) streamkey.Key {
	t.Helper()
	k, err := streamkey.Parse(streamkey.Prefix + "K7Q2XM_" + testToken)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestParseDefaultsAndRefusals(t *testing.T) {
	o, err := parse(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.gop != time.Second || o.fps != 30 || o.origin != "gawk-broadcast://native" || !o.audio {
		t.Fatalf("defaults: %+v", o)
	}
	for _, args := range [][]string{
		{"-relay-url", "http://relay:4433"},
		{"-whip-url", "ftp://gw/whip"},
		{"-key", "not-a-key"},
		{"-fps", "0"},
		{"-gop", "10ms"},
		{"stray"},
	} {
		if _, err := parse(args, io.Discard); err == nil {
			t.Errorf("parse(%q) accepted", args)
		}
	}
}

// The sender asks FFmpeg for what docs/obs-setup.md asks OBS for: a fixed
// keyframe interval in frames (1 s at 30 fps), no B-frames, parameter sets
// before every IDR, the key as the bearer token.
func TestFFmpegArgsAskForAOneSecondGOPWithoutBFrames(t *testing.T) {
	o, err := parse([]string{"-whip-url", "http://127.0.0.1:8080/whip"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	k := testKey(t)
	args := ffmpegArgs(o, k)
	val := func(flag string) string {
		i := slices.Index(args, flag)
		if i < 0 || i+1 >= len(args) {
			t.Fatalf("%s missing from %q", flag, args)
		}
		return args[i+1]
	}
	if val("-g") != "30" || val("-keyint_min") != "30" || val("-sc_threshold") != "0" || val("-bf") != "0" {
		t.Fatalf("GOP flags: %q", args)
	}
	if val("-x264-params") != "repeat-headers=1" || val("-c:v") != "libx264" || val("-c:a") != "libopus" {
		t.Fatalf("codec flags: %q", args)
	}
	if val("-f") != "lavfi" || val("-authorization") != k.String() || args[len(args)-1] != o.whipURL || args[len(args)-3] != "-authorization" {
		t.Fatalf("WHIP output: %q", args)
	}
	o.audio, o.gop, o.fps = false, 2*time.Second, 60
	args = ffmpegArgs(o, k)
	if slices.Contains(args, "libopus") || val("-g") != "120" {
		t.Fatalf("no audio, 2 s at 60 fps: %q", args)
	}
}

func announce(t *testing.T, id string) []byte {
	t.Helper()
	b, err := wire.AppendBroadcastAnnounce(nil, id)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func token(t *testing.T) []byte {
	t.Helper()
	raw, _ := hex.DecodeString(testToken)
	b, err := wire.AppendResumeToken(nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func closing(t *testing.T) []byte {
	t.Helper()
	b, err := wire.AppendSessionClosing(nil, wire.CloseCodePublishMinted)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// webtransport-go accepts the relay's uni streams in any order, so the mint
// result is complete whichever of the three arrives first (docs/gotchas.md).
func TestMintResultDispatchesByTypeInAnyOrder(t *testing.T) {
	msgs := [][]byte{closing(t), token(t), announce(t, "K7Q2XM")}
	for _, order := range [][]int{{0, 1, 2}, {1, 0, 2}, {2, 1, 0}, {1, 2, 0}} {
		m := newMintResult()
		for _, i := range order {
			if err := m.add(msgs[i]); err != nil {
				t.Fatal(err)
			}
		}
		select {
		case <-m.done:
		default:
			t.Fatalf("order %v: not done", order)
		}
		k, err := m.key()
		if err != nil || k.ID != "K7Q2XM" || k.Token != testToken {
			t.Fatalf("order %v: %+v %v", order, k, err)
		}
		if m.closing != wire.CloseCodePublishMinted {
			t.Fatalf("order %v: closing %d", order, m.closing)
		}
	}
	m := newMintResult()
	_ = m.add(closing(t))
	_ = m.add(announce(t, "K7Q2XM"))
	select {
	case <-m.done:
		t.Fatal("done without the token")
	default:
	}
	if m.add([]byte{1}) == nil {
		t.Fatal("a truncated message was accepted")
	}
}

func TestMintURLAsksForAMintOnly(t *testing.T) {
	u, err := mintURL("https://relay:4433", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u, "https://relay:4433/publish?") || !strings.Contains(u, "mint=1") || !strings.Contains(u, "secret=s3cret") {
		t.Fatalf("mint URL %q", u)
	}
}

func TestWaitStatePollsWithTheKeyUntilLive(t *testing.T) {
	k := testKey(t)
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/whip/status" || r.Header.Get("Authorization") != "Bearer "+k.String() {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		n := polls.Add(1)
		switch {
		case n == 1:
			http.Error(w, "starting", http.StatusServiceUnavailable)
		case n < 4:
			_, _ = io.WriteString(w, `{"state":"idle","warnings":[]}`)
		default:
			_, _ = io.WriteString(w, `{"state":"live","codec":"H.264","warnings":[]}`)
		}
	}))
	defer srv.Close()
	c := statusClient{whipURL: srv.URL + "/whip", every: time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.waitState(ctx, k, true); err != nil {
		t.Fatal(err)
	}
	if polls.Load() != 4 {
		t.Fatalf("polls %d, want 4", polls.Load())
	}
	if err := c.waitState(ctx, k, true); err != nil {
		t.Fatal(err)
	}
	short, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	if err := c.waitState(short, k, false); err == nil {
		t.Fatal("waiting for not-live on a live key returned")
	}
}

func TestSplitLines(t *testing.T) {
	got := splitLines([]byte("a\r\nb\n\nc"))
	if !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("%q", got)
	}
}

func TestRunRefusesBadFlagsWithoutDialling(t *testing.T) {
	if code := run([]string{"-relay-url", "nope"}, io.Discard, io.Discard); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}
