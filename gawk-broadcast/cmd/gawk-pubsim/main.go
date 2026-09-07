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
// Everything human-facing goes to stderr — and so does the other
// machine-readable line, emitted whenever a publish or reclaim dial is
// refused with an HTTP status:
//
//	GAWK_PUBSIM_DIAL_STATUS=451
//
// R42 (docs/44, RM6): -room-new mints a room from the broadcast once its ID
// and resume token are known and prints exactly one more stdout line —
//
//	ROOM AB2CD3 <creatorTokenHex>
//
// (the code upper-case, the creator token as 32 hex characters) — while
// -room <CODE> attaches to an existing room and prints nothing extra on
// stdout. Either way the room session is held for the process lifetime and
// re-attaches on every resume; the browser E2E drives both.
//
// That line (and exit code 3) is what makes docs/42 D15 testable end to end.
// The point of answering a banned publisher 451 rather than reusing 403 is
// that a NATIVE broadcaster can read the status where a browser cannot — so a
// harness that only observes "the publish failed" proves the rejection but
// never the property the status code exists for.
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

	"github.com/Tuhis/gawk/gawk-broadcast/internal/engine"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/fixture"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/pubsim"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// exitDialRejected is the exit code for "the relay refused the dial with an
// HTTP status" — 451 banned, 401 bad secret, 404 unknown ID, 409 taken, 429 at
// capacity. Distinct from the catch-all 1 so a harness can tell a policy
// refusal from a crash without parsing prose, and 3 rather than 2 because
// flag.Parse already owns 2 for a bad flag.
const exitDialRejected = 3

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gawk-pubsim:", err)
		if reportDialStatus(err) {
			os.Exit(exitDialRejected)
		}
		os.Exit(1)
	}
}

// reportDialStatus prints the machine-readable status line and the engine's
// own sentence for a dial the relay answered with an HTTP status, and reports
// whether there was one.
//
// The status is taken from engine.StartError rather than re-derived: the
// engine already carries it (webtransport-go's Dial returns the *http.Response
// even on rejection) and already renders each status as a sentence a human can
// act on, including R39's 451. Two places deciding what 451 means is exactly
// the drift docs/42 D13 exists to avoid.
func reportDialStatus(err error) bool {
	se, ok := engine.AsStartError(err)
	if !ok || se.Status == 0 {
		return false
	}
	fmt.Fprintf(os.Stderr, "GAWK_PUBSIM_DIAL_STATUS=%d\n", se.Status)
	fmt.Fprintln(os.Stderr, "gawk-pubsim:", se.Message())
	return true
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
	// R42 rooms.
	roomNew := flag.Bool("room-new", false, "mint a room from this broadcast and print `ROOM <CODE> <creatorTokenHex>` on stdout")
	room := flag.String("room", "", "attach this broadcast to an existing room (code or slug)")
	roomAttach := flag.String("room-attach-secret", "", "a static room's attach secret, for -room")
	roomCreate := flag.String("room-create-secret", "", "the relay's room-create secret, for -room-new (env GAWK_ROOM_CREATE_SECRET)")
	label := flag.String("label", "pubsim", "the broadcast's tile label in the room")
	nick := flag.String("nick", "pubsim", "nickname in the room's roster")
	flag.Parse()
	if *roomCreate == "" {
		*roomCreate = os.Getenv("GAWK_ROOM_CREATE_SECRET")
	}

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
			RelayURL:         *url,
			PublishSecret:    *secret,
			Insecure:         *insecure,
			Media:            media,
			Room:             *room,
			RoomNew:          *roomNew,
			RoomAttachSecret: *roomAttach,
			RoomCreateSecret: *roomCreate,
			RoomLabel:        *label,
			Nickname:         *nick,
		},
		engine.Callbacks{
			OnBroadcastID: func(id string) {
				// The one machine-readable line; harnesses scrape it.
				fmt.Printf("GAWK_PUBSIM_ID=%s\n", id)
			},
			OnRoomCreated: func(code, creatorToken string) {
				// The room's machine-readable line: the E2E reads the code
				// and the grant off it.
				fmt.Print(roomLine(code, creatorToken))
			},
			OnRoomState: func(st wire.RoomState) {
				fmt.Fprintf(os.Stderr, "in room %s: %d broadcast(s), %d participant(s)\n", st.Code, len(st.Attachments), len(st.Participants))
			},
			OnRoomEvent: func(ev wire.RoomEvent) {
				if ev.Kind == wire.RoomEventAttachmentAdded || ev.Kind == wire.RoomEventAttachmentUpdated {
					fmt.Fprintf(os.Stderr, "room attachment %s live=%v\n", ev.Attachment.Label, ev.Attachment.Live)
				}
			},
			OnRoomEnded: func(reason uint8) {
				fmt.Fprintf(os.Stderr, "room ended (reason %d); the broadcast continues\n", reason)
			},
			OnRoomError: func(err error) {
				fmt.Fprintln(os.Stderr, "gawk-pubsim: room:", err)
			},
			OnError: func(err error) {
				fmt.Fprintln(os.Stderr, "gawk-pubsim:", err)
				// A reclaim refused mid-session (the R39 kill path: the
				// broadcast is killed, its ID banned, and every auto-resume
				// answers 451) never reaches run()'s return value — it
				// arrives here, and would otherwise be prose only.
				reportDialStatus(err)
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

// roomLine is the -room-new stdout contract: `ROOM <CODE> <creatorTokenHex>`
// followed by a newline, the code upper-cased so a harness can compare it
// without normalizing (dynamic codes are case-insensitive, docs/44 §4.2).
func roomLine(code, creatorTokenHex string) string {
	return fmt.Sprintf("ROOM %s %s\n", strings.ToUpper(code), creatorTokenHex)
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
