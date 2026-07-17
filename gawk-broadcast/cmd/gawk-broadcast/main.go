// Command gawk-broadcast publishes this Linux machine's screen to a gawk relay
// with hardware encode (R14, docs/19).
//
// It is the headless path and the engine's harness — the direct analogue of the
// frozen #/debug/broadcast page next to the production UI. It is not
// scaffolding to be discarded once the GUI exists: it proves the engine end to
// end without a window, and it is what you reach for when something is wrong.
//
// The GUI (gawk-broadcast-gui) is what you actually use.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/config"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/engine"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/gst"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/portal"
)

func main() {
	if err := run(); err != nil {
		// One place turns errors into sentences and exit codes; the engine
		// never touches stdio or exits on its own.
		fmt.Fprintln(os.Stderr, "\n"+userMessage(err))
		os.Exit(1)
	}
}

func run() error {
	cfgPath, err := config.DefaultPath()
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("gawk-broadcast", flag.ContinueOnError)
	var (
		configPath = fs.String("config", cfgPath, "path to the config file")
		relayURL   = fs.String("url", "", "relay URL, e.g. https://gawk-relay.example:4433 (env GAWK_URL)")
		appURL     = fs.String("app-url", "", "frontend URL for join links, e.g. https://gawk.example (env GAWK_APP_URL)")
		id         = fs.String("id", "", "reclaim this broadcast code instead of minting a new one")
		secret     = fs.String("secret", "", "publish secret, if the relay requires one (env GAWK_SECRET)")
		origin     = fs.String("origin", "", "Origin header to send; must be whitelisted in the relay's -allowed-origins (default "+engine.DefaultOrigin+", env GAWK_ORIGIN)")
		insecure   = fs.Bool("insecure", false, "skip TLS verification (development certificates only)")
		resolution = fs.String("resolution", "", "capture resolution, e.g. 1920x1080 (default 1920x1080)")
		fpsFlag    = fs.Int("fps", 0, "frames per second (default 60)")
		bitrate    = fs.Float64("bitrate", 0, "peak bitrate in Mbps (default 16; VBR targets ~75% of it)")
		encoder    = fs.String("encoder", "", "force an encoder ("+strings.Join(gst.CandidateNames(), ", ")+"); default probes them in order")
		verbose    = fs.Bool("v", false, "verbose logging (the GStreamer child's stderr included)")
		statsEvery = fs.Duration("stats", 5*time.Second, "how often to print a stats line (0 disables)")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "gawk-broadcast — publish your screen to a gawk relay, with hardware encode.\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n  gawk-broadcast -url https://relay.example:4433\n\n")
		fmt.Fprintf(os.Stderr, "Settings are read from %s and overridden by these flags.\n\n", cfgPath)
		fs.PrintDefaults()
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		// Corrupt config: say so, carry on with defaults.
		fmt.Fprintln(os.Stderr, err)
	}

	// Precedence: flag > env > config file. Env is here because a secret on a
	// command line is visible in ps output.
	applyString(&cfg.RelayURL, *relayURL, os.Getenv("GAWK_URL"))
	applyString(&cfg.AppURL, *appURL, os.Getenv("GAWK_APP_URL"))
	applyString(&cfg.PublishSecret, *secret, os.Getenv("GAWK_SECRET"))
	applyString(&cfg.Origin, *origin, os.Getenv("GAWK_ORIGIN"))
	applyString(&cfg.Encoder, *encoder, os.Getenv("GAWK_ENCODER"))
	if cfg.RelayURL == "" {
		fs.Usage()
		return errors.New("no relay URL: pass -url or set GAWK_URL")
	}

	media := engine.DefaultMediaConfig()
	if cfg.Width > 0 && cfg.Height > 0 {
		media.Width, media.Height = cfg.Width, cfg.Height
	}
	if cfg.Fps > 0 {
		media.Fps = cfg.Fps
	}
	if cfg.BitrateBps > 0 {
		media.BitrateBps = cfg.BitrateBps
	}
	media.Encoder = cfg.Encoder
	if *resolution != "" {
		w, h, err := engine.ParseResolution(*resolution)
		if err != nil {
			return err
		}
		media.Width, media.Height = w, h
	}
	if *fpsFlag > 0 {
		media.Fps = *fpsFlag
	}
	if *bitrate > 0 {
		media.BitrateBps = int(*bitrate * 1_000_000)
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	// The config file is where the last-good encoder and last broadcast ID are
	// remembered, so the engine writes back through these.
	saveCfg := func() {
		if err := cfg.Save(); err != nil {
			log.Warn("could not save config", "path", cfg.Path(), "err", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The persisted resume token only fits the ID it was minted for (R17);
	// reclaiming any other ID with it would just be refused.
	resumeToken := ""
	if *id != "" && *id == cfg.LastBroadcastID {
		resumeToken = cfg.LastResumeToken
	}

	ended := make(chan struct{})
	var endOnce = make(chan struct{}, 1)
	sess := engine.New(
		engine.Config{
			RelayURL:      cfg.RelayURL,
			BroadcastID:   *id,
			PublishSecret: cfg.PublishSecret,
			ResumeToken:   resumeToken,
			Origin:        cfg.Origin,
			Insecure:      *insecure,
			Media:         media,
		},
		engine.Callbacks{
			OnBroadcastID: func(bid string) {
				cfg.LastBroadcastID = bid
				saveCfg()
				fmt.Fprintf(os.Stderr, "\nBroadcast code: %s", bid)
				if link := joinLink(cfg.AppURL, bid); link != "" {
					fmt.Fprintf(os.Stderr, "   join: %s", link)
				}
				fmt.Fprintln(os.Stderr)
			},
			OnResumeToken: func(token string) {
				cfg.LastResumeToken = token
				saveCfg()
			},
			OnEncoderChosen: func(enc string) {
				fmt.Fprintf(os.Stderr, "Encoding with %s (%s, hardware) at %dx%d@%d, %.1f Mbps\n",
					gst.EncoderAPI(enc), enc, media.Width, media.Height, media.Fps, float64(media.BitrateBps)/1e6)
			},
			OnError: func(err error) {
				fmt.Fprintln(os.Stderr, "\n"+userMessage(err))
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
			Log: log,
			MediaFactory: gst.NewFactory(gst.Options{
				LastGoodEncoder: cfg.LastGoodEncoder,
				OnEncoderChosen: func(enc string) { cfg.LastGoodEncoder = enc; saveCfg() },
			}),
		},
	)

	if err := sess.Start(ctx); err != nil {
		return err
	}
	defer sess.Stop()

	if *statsEvery > 0 {
		go statsLoop(ctx, sess, *statsEvery)
	}

	select {
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "\nStopping…")
		return sess.Stop()
	case <-ended:
		// The session ended on its own (relay closed, child died). OnError has
		// already said why, if there was a why.
		return errors.New("broadcast ended")
	}
}

func orNA(s string) string {
	if s == "" {
		return "n/a"
	}
	return s
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
			// Capture fps is deliberately absent rather than fabricated: the
			// GStreamer child owns that stage (Decision 20).
			rtt := "n/a"
			if s.TimeSyncAvailable {
				rtt = fmt.Sprintf("%.1fms", s.TimeSyncRttMs)
			}
			// The measured keyframe cadence next to its target: key-int-max is
			// a frame count, so capture running under the nominal fps
			// stretches the wall-clock GOP — this is where that shows.
			kf := "n/a"
			if s.KeyframeIntervalAvailable {
				kf = fmt.Sprintf("%.0fms", s.KeyframeIntervalMs)
			}
			fmt.Fprintf(os.Stderr,
				"encode %.1f fps · sent %.1f fps · %s · %s capture · %d keyframes every ~%s (%d failed, %d superseded) · %d dropped at send · %.1f MB · rtt %s\n",
				s.EncoderFps, s.SentFps, s.Codec, orNA(s.CapturePath), s.KeyframeStreamsSent, kf, s.KeyframeStreamsFailed,
				s.KeyframeStreamsSuperseded, s.FramesDroppedAtSend, float64(s.BytesSent)/1e6, rtt)
		}
	}
}

// userMessage turns an error into something a person can act on. This is the
// CLI's half of Decision 10's payoff: webtransport-go gives us the HTTP status
// the browser cannot see, so 401/404/409/429 become sentences.
func userMessage(err error) string {
	// Sentinels before the StartError wrapper, for the same reason as
	// app.Message: capture-phase failures arrive StartError-wrapped, and the
	// wrapper must not shadow the curated messages.
	if errors.Is(err, engine.ErrNoHardwareEncoder) {
		return gst.NoHardwareMessage + "\n\ndetail: " + err.Error()
	}
	if errors.Is(err, engine.ErrCaptureFormat) {
		return gst.CaptureFormatMessage + "\n\ndetail: " + err.Error()
	}
	if errors.Is(err, portal.ErrCancelled) {
		return "Screen sharing was cancelled."
	}
	if errors.Is(err, portal.ErrUnavailable) {
		return "No screen-sharing portal is available.\n" +
			"gawk-broadcast captures through the XDG ScreenCast portal, which your desktop provides.\n" +
			"Check that xdg-desktop-portal and your desktop's backend (xdg-desktop-portal-kde,\n" +
			"-gnome, -wlr, …) are installed and running.\n\ndetail: " + err.Error()
	}
	if errors.Is(err, gst.ErrNoLaunchBinary) {
		return err.Error()
	}
	if se, ok := engine.AsStartError(err); ok {
		return se.Message()
	}
	return err.Error()
}

// joinLink builds the viewer URL. The app URL is separate from the relay URL
// and not derivable from it — they are different hosts (Decision 19).
func joinLink(appURL, id string) string {
	if appURL == "" || id == "" {
		return ""
	}
	return strings.TrimSuffix(appURL, "/") + "/#/view/" + id
}

// applyString sets dst to the first non-empty override, leaving the config
// file's value in place when none is given.
func applyString(dst *string, overrides ...string) {
	for _, o := range overrides {
		if o != "" {
			*dst = o
			return
		}
	}
}
