// Command gawk-whipsim is the dev stack's WHIP sender (R33 WG9, docs/83 §6):
// what the `whip` compose profile runs so a developer sees a broadcast come
// through the gateway without installing OBS.
//
// One run:
//
//  1. mints a stream key with the relay's mint-only publish,
//     CONNECT /publish?mint=1 (D3), exactly as the web app's OBS page does;
//  2. runs FFmpeg's WHIP muxer (FFmpeg 8 or later) against the gateway with
//     that key: a moving test pattern in H.264 with a 1 s keyframe interval
//     and no B-frames, and a 440 Hz Opus tone;
//  3. once the gateway's /whip/status reports the key live, attaches the
//     broadcast to a room (-room) with the key's ID and resume token
//     (docs/44 D8), so a viewer finds it at a link that never changes;
//  4. subscribes to the broadcast as a viewer and logs
//     GAWK_WHIPSIM_ROUNDTRIP once a keyframe and -frames frames have come
//     back through gateway → relay → viewer.
//
// With -check it exits 0 after the round trip and non-zero when it does not
// happen within -timeout; otherwise it keeps FFmpeg streaming (restarting it
// with the same key if it exits) until it is stopped.
//
// The key, ID and token are dev-stack credentials and are printed on
// purpose (GAWK_WHIPSIM_KEY=…), so the OBS page's "Paste a key" can show the
// sender's status. Never point this at a relay you care about.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/streamkey"
)

// options is the parsed command line.
type options struct {
	relayURL     string
	insecure     bool
	origin       string
	secret       string
	whipURL      string
	key          string
	mintOnly     bool
	room         string
	attachSecret string
	label        string
	appURL       string
	ffmpeg       string
	size         string
	fps          int
	gop          time.Duration
	bitrateKbps  int
	audio        bool
	frames       int
	check        bool
	timeout      time.Duration
}

func parse(args []string, stderr io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("gawk-whipsim", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.relayURL, "relay-url", "https://127.0.0.1:4433", "the relay to mint the key on and to watch the broadcast from")
	fs.BoolVar(&o.insecure, "insecure", false, "skip the relay's TLS verification (dev certificates only)")
	fs.StringVar(&o.origin, "origin", "gawk-broadcast://native", "Origin sent on every relay dial; it must be in the relay's -allowed-origins")
	fs.StringVar(&o.secret, "secret", "", "the relay's publish secret, if it has one (env GAWK_SECRET)")
	fs.StringVar(&o.whipURL, "whip-url", "http://127.0.0.1:8080/whip", "the gateway's WHIP endpoint")
	fs.StringVar(&o.key, "key", "", "stream with this gawk1_… key instead of minting one")
	fs.BoolVar(&o.mintOnly, "mint-only", false, "mint a key, print it on stdout and exit")
	fs.StringVar(&o.room, "room", "", "attach the broadcast to this room (a static slug or a dynamic code) once it is live")
	fs.StringVar(&o.attachSecret, "room-attach-secret", "", "the static room's attach secret, if it has one")
	fs.StringVar(&o.label, "label", "OBS test pattern", "the room tile's label")
	fs.StringVar(&o.appURL, "app-url", "http://localhost:8080", "the web app, for the links printed at start")
	fs.StringVar(&o.ffmpeg, "ffmpeg", "ffmpeg", "the FFmpeg binary (8 or later, with the WHIP muxer)")
	fs.StringVar(&o.size, "size", "1280x720", "test pattern size")
	fs.IntVar(&o.fps, "fps", 30, "test pattern frame rate")
	fs.DurationVar(&o.gop, "gop", time.Second, "keyframe interval: 1 s is what the OBS guide asks for (docs/obs-setup.md)")
	fs.IntVar(&o.bitrateKbps, "bitrate", 2500, "video bitrate, kbit/s")
	fs.BoolVar(&o.audio, "audio", true, "also send a 440 Hz Opus tone")
	fs.IntVar(&o.frames, "frames", 30, "video frames the viewer must receive for the round trip to count")
	fs.BoolVar(&o.check, "check", false, "exit 0 once a frame has round-tripped, non-zero after -timeout")
	fs.DurationVar(&o.timeout, "timeout", 90*time.Second, "with -check: how long the round trip may take")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if o.secret == "" {
		o.secret = os.Getenv("GAWK_SECRET")
	}
	if u, err := url.Parse(o.relayURL); err != nil || u.Scheme != "https" || u.Host == "" {
		return o, fmt.Errorf("invalid -relay-url %q (want https://host:port)", o.relayURL)
	}
	if u, err := url.Parse(o.whipURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return o, fmt.Errorf("invalid -whip-url %q", o.whipURL)
	}
	if o.key != "" {
		if _, err := streamkey.Parse(o.key); err != nil {
			return o, fmt.Errorf("invalid -key: %v", err)
		}
	}
	if o.fps < 1 || o.frames < 1 || o.bitrateKbps < 1 {
		return o, errors.New("-fps, -frames and -bitrate must be positive")
	}
	if o.gop < time.Second/time.Duration(o.fps) {
		return o, fmt.Errorf("-gop %s is shorter than one frame", o.gop)
	}
	return o, nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	o, err := parse(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, "gawk-whipsim:", err)
		return 2
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	relay := relayClient{url: o.relayURL, origin: o.origin, insecure: o.insecure}
	var key streamkey.Key
	if o.key != "" {
		key, _ = streamkey.Parse(o.key)
	} else {
		key, err = mintWithRetry(ctx, relay, o.secret, log)
		if err != nil {
			log.Error("mint failed", "err", err)
			return 1
		}
	}
	if o.mintOnly {
		fmt.Fprintln(stdout, key.String())
		return 0
	}
	// Dev-stack credentials, printed on purpose (package comment).
	fmt.Fprintf(stdout, "GAWK_WHIPSIM_ID=%s\nGAWK_WHIPSIM_KEY=%s\n", key.ID, key.String())
	app := strings.TrimRight(o.appURL, "/")
	if o.room != "" {
		fmt.Fprintf(stdout, "    watch it in the room:  %s/#/room/%s\n", app, url.PathEscape(o.room))
	}
	fmt.Fprintf(stdout, "    watch it alone:        %s/#/view/%s\n", app, key.ID)
	fmt.Fprintf(stdout, "    its OBS page status:   %s/#/broadcast/obs (Paste a key)\n", app)

	if o.check {
		ctx2, cancel := context.WithTimeout(ctx, o.timeout)
		defer cancel()
		ctx = ctx2
	}
	status := statusClient{whipURL: o.whipURL}
	result := make(chan error, 1)
	go func() { result <- followBroadcast(ctx, o, relay, status, key, stdout, log) }()

	sender := ffmpegSender{bin: o.ffmpeg, args: ffmpegArgs(o, key), log: log}
	fatal := make(chan error, 1)
	go func() { fatal <- superviseFFmpeg(ctx, sender) }()
	for {
		select {
		case err := <-result:
			result = nil
			if o.check {
				if err != nil {
					log.Error("the round trip did not complete", "err", err)
					return 1
				}
				return 0
			}
			if err != nil {
				// Not -check: say so, and keep the pattern streaming for
				// whoever is looking at it.
				log.Error("the round trip did not complete", "err", err)
			}
		case err := <-fatal:
			if ctx.Err() != nil {
				if o.check {
					log.Error("the round trip did not complete in time", "timeout", o.timeout)
					return 1
				}
				return 0
			}
			log.Error("ffmpeg cannot run", "err", err)
			return 1
		}
	}
}

// superviseFFmpeg keeps FFmpeg streaming until ctx ends, restarting it when
// it exits, with the same key, as OBS's auto-reconnect does, so the code
// stays. It returns nil once ctx ends, or errNoFFmpeg when FFmpeg cannot
// start at all.
func superviseFFmpeg(ctx context.Context, s ffmpegSender) error {
	for {
		err := s.run(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errNoFFmpeg) {
			return err
		}
		s.log.Warn("ffmpeg exited; restarting in 2 s", "err", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

// followBroadcast waits for the gateway to report the key live, attaches it
// to the room and proves the round trip as a viewer. Without -check it then
// keeps the room attachment: a stop (a WHIP DELETE, close 4014) removes the
// broadcast from its room (R78), so every return to live attaches again.
func followBroadcast(ctx context.Context, o options, relay relayClient, st statusClient, key streamkey.Key, stdout io.Writer, log *slog.Logger) error {
	if err := st.waitState(ctx, key, true); err != nil {
		return fmt.Errorf("the gateway never reported the key live: %w", err)
	}
	log.Info("the gateway reports the stream live", "broadcast", key.ID)
	if o.room != "" {
		if err := attachWithRetry(ctx, relay, o.room, o.attachSecret, o.label, key, log); err != nil {
			return fmt.Errorf("attach to room %q: %w", o.room, err)
		}
		fmt.Fprintf(stdout, "GAWK_WHIPSIM_ROOM=%s\n", o.room)
	}
	got, err := watchWithRetry(ctx, relay, key.ID, o.frames, log)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "GAWK_WHIPSIM_ROUNDTRIP ok broadcast=%s keyframes=%d frames=%d\n", key.ID, got.keyframes, got.frames)
	if o.check || o.room == "" {
		return nil
	}
	go func() {
		for {
			if st.waitState(ctx, key, false) != nil || st.waitState(ctx, key, true) != nil {
				return
			}
			if err := attachWithRetry(ctx, relay, o.room, o.attachSecret, o.label, key, log); err != nil && ctx.Err() == nil {
				log.Warn("re-attach after going live again failed", "err", err)
			}
		}
	}()
	return nil
}
