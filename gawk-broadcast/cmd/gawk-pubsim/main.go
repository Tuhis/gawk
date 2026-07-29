// Command gawk-pubsim publishes the committed H.264 test fixture to a gawk
// relay in a loop — a deterministic synthetic broadcaster (R20, docs/25
// Decision 3). It drives the real engine (announce, resume token, reliable
// keyframe streams, delta datagrams, TimeSync, ClockMapping) with canned
// frames instead of a GPU, so the CI E2E gates and the manual scale drill
// (pubsim + gawk-loadgen) need no gaming PC.
//
// It imports the engine and wire only — no Gio, no cgo, no GStreamer — so it
// builds anywhere the relay does.
//
// The minted broadcast code is printed to stdout as a single machine-readable
// line for harnesses:
//
//	GAWK_PUBSIM_ID=AB2CD3
//
// Everything human-facing goes to stderr.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/engine"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/fixture"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/pubsim"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gawk-pubsim:", err)
		os.Exit(1)
	}
}

func run() error {
	url := flag.String("url", "https://127.0.0.1:4433", "relay base URL")
	secret := flag.String("secret", "", "publish secret, if the relay requires one (env GAWK_SECRET)")
	insecure := flag.Bool("insecure", false, "skip TLS verification (dev certs)")
	duration := flag.Duration("duration", 0, "how long to publish; 0 = until interrupted")
	fps := flag.Int("fps", 30, "playback rate; the fixture's keyframe cadence is every 15 frames (500 ms at 30)")
	statsEvery := flag.Duration("stats", 5*time.Second, "how often to print a stats line to stderr (0 disables)")
	verbose := flag.Bool("v", false, "verbose engine logging")
	// Default off, deliberately (docs/28 Decision 12): docs/25's tier-1 asserts
	// the no-audio path stays intact, and that assertion must keep running. The
	// audio pass is a *second* run with this flag seeded, following the
	// docs/25 finding 16 precedent.
	audio := flag.Bool("audio", false, "also publish the committed Opus fixture (R25 audio lane)")
	// R30 (docs/35 §12): the default clip's ~2–4-chunk deltas sit under one
	// stripe share, so a striped viewer against it correctly engages nothing.
	// The large clip's deltas are all past the ~8-chunk burst threshold,
	// which is what the striped e2e pass needs — and only that pass: every
	// other tier-1 step keeps the small clip so its cost and flake profile
	// stay untouched.
	fixtureName := flag.String("fixture", "default",
		"committed clip to publish: default (320x240) or large (720p noise, >8-chunk deltas — the R30 striping fixture)")
	flag.Parse()

	if *secret == "" {
		*secret = os.Getenv("GAWK_SECRET")
	}

	ts := fixture.TS
	switch *fixtureName {
	case "default":
	case "large":
		ts = fixture.TSLarge
	default:
		return fmt.Errorf("unknown -fixture %q (want default or large)", *fixtureName)
	}

	aus, err := pubsim.Demux(ts)
	if err != nil {
		return err
	}
	var packets [][]byte
	if *audio {
		if packets, err = fixture.SplitAudio(fixture.Audio); err != nil {
			return err
		}
	}

	level := slog.LevelWarn
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	media := engine.DefaultMediaConfig()
	// The fixture's real properties, not the shipped 1080p60 rung: stats and
	// the announced dimensions should describe what is actually sent.
	media.Width, media.Height = 320, 240
	media.Fps = *fps
	media.BitrateBps = 0

	ended := make(chan struct{})
	endOnce := make(chan struct{}, 1)
	sess := engine.New(
		engine.Config{
			RelayURL:      *url,
			PublishSecret: *secret,
			Insecure:      *insecure,
			Media:         media,
		},
		engine.Callbacks{
			OnBroadcastID: func(id string) {
				// The one machine-readable line; harnesses scrape it.
				fmt.Printf("GAWK_PUBSIM_ID=%s\n", id)
			},
			OnError: func(err error) {
				fmt.Fprintln(os.Stderr, "gawk-pubsim:", err)
			},
			OnEnded: func() {
				select {
				case endOnce <- struct{}{}:
					close(ended)
				default:
				}
			},
		},
		engine.Options{
			Log:          log,
			MediaFactory: pubsim.Factory(aus, *fps, packets),
		},
	)

	if err := sess.Start(ctx); err != nil {
		return err
	}
	defer sess.Stop()
	fmt.Fprintf(os.Stderr, "publishing the %d-frame fixture at %d fps to %s\n", len(aus), *fps, *url)
	if *audio {
		fmt.Fprintf(os.Stderr, "…with %d Opus packets on the audio lane (%d ms, looping)\n",
			len(packets), len(packets)*engine.AudioFrameMs)
	}

	if *statsEvery > 0 {
		go statsLoop(ctx, sess, *statsEvery)
	}

	select {
	case <-ctx.Done():
		// Signal or -duration elapsed: both are a clean end.
		return sess.Stop()
	case <-ended:
		// The relay closed us or the loop broke — either way this synthetic
		// publisher should have run until told to stop.
		return errors.New("session ended unexpectedly")
	}
}

func statsLoop(ctx context.Context, sess *engine.Session, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s := sess.Stats()
			rtt := "n/a"
			if s.TimeSyncAvailable {
				rtt = fmt.Sprintf("%.1fms", s.TimeSyncRttMs)
			}
			fmt.Fprintf(os.Stderr,
				"sent %.1f fps · %s · %d keyframe streams (%d failed, %d superseded) · %d dropped at send · %.1f MB · rtt %s\n",
				s.SentFps, s.Codec, s.KeyframeStreamsSent, s.KeyframeStreamsFailed,
				s.KeyframeStreamsSuperseded, s.FramesDroppedAtSend, float64(s.BytesSent)/1e6, rtt)
		}
	}
}

// audioLine summarises the audio lane, or says it is off — never an absent
// field a reader could mistake for zero (docs/19 Decision 20).
func audioLine(s engine.Stats) string {
	if s.AudioState != engine.AudioActive {
		return "audio " + string(s.AudioState)
	}
	return fmt.Sprintf("audio %d pkt (%d cfg, %d dropped)",
		s.AudioPacketsSent, s.AudioConfigsSent, s.AudioPacketsDropped)
}
