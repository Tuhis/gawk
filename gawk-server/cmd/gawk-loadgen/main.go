// gawk-loadgen is the R17 W6 scale-proof tool: N synthetic viewers against
// one broadcast, counting what a real viewer would care about — complete
// frames, keyframe streams, frameID gaps (upstream loss/reorder visible at
// the subscriber), and bytes — aggregated across all sessions and reported
// periodically. It speaks the ordinary /subscribe route, so pointing it at
// the LoadBalancer exercises exactly the fan-out path viewers use (edge
// pulls included when the fleet spreads it across pods).
//
// Example:
//
//	gawk-loadgen -url https://gawk-relay.example.com:4433 -id K7XQ2M -viewers 200 -duration 60s
//
// With -expect-close-code it is additionally an OBSERVER of why its sessions
// ended (R39, docs/42 §11.2): the E2E tiers could previously prove that a
// broadcast disappeared, never that the relay told its viewers *why*. Without
// the flag nothing about the run changes — same dials, same stdout, same exit
// code.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// closeExpectOff is the -expect-close-code value that disables the check.
// -1 rather than 0 because 0 is a legal WebTransport application close code —
// it is what a clean client-side close sends — so 0 cannot mean "off".
const closeExpectOff = -1

// closeSettle bounds the wait for a close capsule to land on the session
// after a read noticed it going away.
//
// The read that errored is not the authority: ReceiveDatagram's stream-end
// error races the close-capsule parse (the Go twin of the JS wt.closed settle
// race — internal/transport/drain_test.go reads codes off AcceptUniStream for
// exactly this reason). The session's context is the authority, so give it a
// bounded moment before concluding anything.
const closeSettle = 2 * time.Second

type totals struct {
	sessionsUp   atomic.Int64
	dialErrors   atomic.Uint64
	sessionDrops atomic.Uint64
	datagrams    atomic.Uint64
	frames       atomic.Uint64 // delta frames (chunk 0 seen)
	keyframes    atomic.Uint64 // keyframe streams fully read
	// frameGaps counts datagram frameID jumps > 1. NOTE: keyframes travel on
	// reliable streams (R8), so the datagram sequence structurally skips one
	// ID per GOP — expect a baseline of (keyframes/s × viewers) gaps/s on a
	// healthy stream; only growth beyond that is loss or reorder (docs/25
	// finding 8).
	frameGaps      atomic.Uint64
	bytesDatagrams atomic.Uint64
	bytesKeyframes atomic.Uint64

	// How each session ended. Populated only under -expect-close-code, and
	// read only by the exit assertion. A map behind a mutex rather than
	// atomics because the useful failure message is "closed with 4000, want
	// 4006" — naming the code we got instead is the whole point of the flag.
	closeMu      sync.Mutex
	closeCodes   map[uint32]int // application close codes the peer sent
	closeNoCode  int            // ended, but with no application close code
	closeStillUp int            // still open when the run ended
}

func (t *totals) recordCode(code uint32) {
	t.closeMu.Lock()
	defer t.closeMu.Unlock()
	if t.closeCodes == nil {
		t.closeCodes = make(map[uint32]int)
	}
	t.closeCodes[code]++
}

func (t *totals) recordNoCode() {
	t.closeMu.Lock()
	defer t.closeMu.Unlock()
	t.closeNoCode++
}

func (t *totals) recordStillOpen() {
	t.closeMu.Lock()
	defer t.closeMu.Unlock()
	t.closeStillUp++
}

func main() {
	url := flag.String("url", "https://127.0.0.1:4433", "relay base URL")
	id := flag.String("id", "", "broadcast ID to subscribe to (required)")
	viewers := flag.Int("viewers", 10, "synthetic viewer sessions")
	duration := flag.Duration("duration", 30*time.Second, "how long to hold the load")
	rampMs := flag.Int("ramp-ms", 25, "delay between session dials (avoid tripping the rate limiter from one IP)")
	insecure := flag.Bool("insecure", false, "skip TLS verification (dev certs)")
	delivery := flag.String("delivery", "", "delivery mode: \"\" (datagrams), \"reliable\" (R19 carriers), or \"deep\" (R21 DVR ring)")
	bufferMs := flag.Int("buffer-ms", 3000, "playout buffer to declare with -delivery=deep (R21)")
	report := flag.Duration("report", 5*time.Second, "aggregate report interval")
	expectClose := flag.Int("expect-close-code", closeExpectOff,
		"assert every viewer session was closed by the relay with this WebTransport application close code (e.g. 4006 for an operator kill) and exit 1 with a diagnosis otherwise; -1 disables the check")
	flag.Parse()
	if *id == "" {
		fmt.Fprintln(os.Stderr, "gawk-loadgen: -id is required")
		os.Exit(2)
	}
	if *expectClose < closeExpectOff || int64(*expectClose) > math.MaxUint32 {
		fmt.Fprintf(os.Stderr, "gawk-loadgen: -expect-close-code %d is out of range (-1 to disable, or 0..%d)\n",
			*expectClose, uint32(math.MaxUint32))
		os.Exit(2)
	}
	observe := *expectClose != closeExpectOff

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *duration)
	defer cancel()

	// R19/R21: the delivery mode rides on the subscribe query, exactly as the
	// browser negotiates it — reliable carriers for "reliable", and the DVR
	// ring for "deep". Built once so every session dials the same URL.
	subscribeURL := *url + "/subscribe/" + *id
	switch *delivery {
	case "":
	case "reliable":
		subscribeURL += "?delivery=reliable"
	case "deep":
		subscribeURL += fmt.Sprintf("?delivery=reliable&buffer=%d", *bufferMs)
	default:
		fmt.Fprintf(os.Stderr, "gawk-loadgen: unknown -delivery %q (want \"\", \"reliable\" or \"deep\")\n", *delivery)
		os.Exit(2)
	}

	var t totals
	var wg sync.WaitGroup
	for i := 0; i < *viewers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runViewer(ctx, subscribeURL, *insecure, observe, &t)
		}()
		select {
		case <-ctx.Done():
		case <-time.After(time.Duration(*rampMs) * time.Millisecond):
		}
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	start := time.Now()
	var lastFrames, lastKf, lastBytes uint64
	tick := time.NewTicker(*report)
	defer tick.Stop()
loop:
	for {
		select {
		case <-tick.C:
			frames, kf := t.frames.Load(), t.keyframes.Load()
			bytes := t.bytesDatagrams.Load() + t.bytesKeyframes.Load()
			secs := report.Seconds()
			fmt.Printf("[%6.1fs] up=%d/%d dialErr=%d drops=%d | frames/s=%.0f kf/s=%.1f gaps=%d | Mbps(total)=%.1f\n",
				time.Since(start).Seconds(), t.sessionsUp.Load(), *viewers,
				t.dialErrors.Load(), t.sessionDrops.Load(),
				float64(frames-lastFrames)/secs, float64(kf-lastKf)/secs,
				t.frameGaps.Load(), float64(bytes-lastBytes)*8/1e6/secs)
			lastFrames, lastKf, lastBytes = frames, kf, bytes
		case <-done:
			break loop
		case <-ctx.Done():
			<-done
			break loop
		}
	}
	elapsed := time.Since(start)
	printSummary(&t, *viewers, elapsed)
	if observe && !assertCloseCode(&t, uint32(*expectClose), *viewers, elapsed) {
		os.Exit(1)
	}
}

// assertCloseCode reports the -expect-close-code verdict and returns whether
// it passed.
//
// The verdict goes to STDERR, never stdout: stdout is loadgen's data output
// and stays byte-for-byte what it is without the flag, so a harness can add
// the assertion to an existing invocation without disturbing anything that
// parses the report.
//
// The four ways to fail read differently on purpose, because they mean
// different things: a wrong code is a relay behaviour bug; no code at all is a
// transport death (idle timeout, pod crash, a UDP path that went away) that
// never carried an operator's intent; a session still open at the end means
// whatever should have closed it never reached this viewer; and a dial that
// never connected is a viewer that was never there to be closed — which would
// otherwise pass vacuously.
func assertCloseCode(t *totals, want uint32, viewers int, elapsed time.Duration) bool {
	t.closeMu.Lock()
	defer t.closeMu.Unlock()

	got := t.closeCodes[want]
	var problems []string
	for code, n := range t.closeCodes {
		if code != want {
			problems = append(problems,
				fmt.Sprintf("%d session(s) closed with code %d, want %d", n, code, want))
		}
	}
	// Map iteration order is random; a diagnostic that reorders itself
	// between runs is a diagnostic nobody trusts.
	sort.Strings(problems)
	if t.closeNoCode > 0 {
		problems = append(problems, fmt.Sprintf(
			"%d session(s) ended with NO application close code — a transport death (idle timeout, crash, path loss), not a close the relay chose",
			t.closeNoCode))
	}
	if t.closeStillUp > 0 {
		problems = append(problems, fmt.Sprintf(
			"%d session(s) were STILL OPEN when the run ended after %.1fs — nothing closed them",
			t.closeStillUp, elapsed.Seconds()))
	}
	if n := t.dialErrors.Load(); n > 0 {
		problems = append(problems, fmt.Sprintf(
			"%d session(s) never connected (dial failed), so they were never closed by anything", n))
	}

	if viewers > 0 && got == viewers && len(problems) == 0 {
		fmt.Fprintf(os.Stderr, "gawk-loadgen: PASS -expect-close-code %d: all %d session(s) were closed by the relay with %d\n",
			want, viewers, want)
		return true
	}
	fmt.Fprintf(os.Stderr, "gawk-loadgen: FAIL -expect-close-code %d: %d of %d session(s) closed with %d\n",
		want, got, viewers, want)
	for _, p := range problems {
		fmt.Fprintln(os.Stderr, "  -", p)
	}
	return false
}

// sessionCloseCode reads the application close code the peer sent, if it sent
// one at all.
//
// Only valid once the session has ended, and that precondition is what makes
// it sound rather than a hack — it is the same idiom gawk-broadcast's engine
// uses (gawk-broadcast/internal/engine/resume.go): webtransport-go discards
// the cause when it cancels the session's context, but it keeps the close
// error, and a closed Session hands it back from OpenUniStream *before*
// touching the connection. On a dead session this therefore opens nothing.
//
// ok=false is the ordinary abrupt death — idle timeout, stateless reset, the
// path simply going away — which carries no application code by definition.
func sessionCloseCode(sess *webtransport.Session) (uint32, bool) {
	_, err := sess.OpenUniStream()
	var se *webtransport.SessionError
	if errors.As(err, &se) {
		return uint32(se.ErrorCode), true
	}
	return 0, false
}

// observeClose records why one viewer session ended. Called once per session
// that was actually established, and only under -expect-close-code — the
// settle wait below is a real (if small) change in when a viewer goroutine
// returns, and the no-flag path must stay exactly as it was.
func observeClose(runCtx context.Context, sess *webtransport.Session, t *totals) {
	// Wait for the close capsule only when the session plausibly died: if the
	// run's own deadline expired against a healthy session there is nothing
	// to wait for, and "still open" is the answer.
	if runCtx.Err() == nil || sess.Context().Err() != nil {
		wait, cancel := context.WithTimeout(context.Background(), closeSettle)
		defer cancel()
		select {
		case <-sess.Context().Done():
		case <-wait.Done():
		}
	}
	if sess.Context().Err() == nil {
		t.recordStillOpen()
		return
	}
	if code, ok := sessionCloseCode(sess); ok {
		t.recordCode(code)
		return
	}
	t.recordNoCode()
}

func printSummary(t *totals, viewers int, elapsed time.Duration) {
	fmt.Printf("\n=== gawk-loadgen summary (%.1fs, %d viewers) ===\n", elapsed.Seconds(), viewers)
	fmt.Printf("dial errors:       %d\n", t.dialErrors.Load())
	fmt.Printf("session drops:     %d\n", t.sessionDrops.Load())
	fmt.Printf("delta frames:      %d\n", t.frames.Load())
	fmt.Printf("keyframe streams:  %d\n", t.keyframes.Load())
	fmt.Printf("frameID gaps:      %d (loss or reorder seen by viewers)\n", t.frameGaps.Load())
	total := t.bytesDatagrams.Load() + t.bytesKeyframes.Load()
	fmt.Printf("bytes received:    %d (%.1f Mbps aggregate)\n", total, float64(total)*8/1e6/elapsed.Seconds())
}

func runViewer(ctx context.Context, subscribeURL string, insecure, observe bool, t *totals) {
	d := &webtransport.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure},
		QUICConfig: &quic.Config{
			EnableDatagrams:                  true,
			EnableStreamResetPartialDelivery: true,
			MaxIdleTimeout:                   30 * time.Second,
			KeepAlivePeriod:                  10 * time.Second,
		},
	}
	defer d.Close()
	_, sess, err := d.Dial(ctx, subscribeURL, nil)
	if err != nil {
		t.dialErrors.Add(1)
		return
	}
	t.sessionsUp.Add(1)
	defer t.sessionsUp.Add(-1)
	defer sess.CloseWithError(0, "loadgen done")
	// Deferred AFTER our own close, so it runs BEFORE it (LIFO): reading the
	// close code has to happen while the only close on record is the peer's.
	if observe {
		defer observeClose(ctx, sess, t)
	}

	// Keyframe streams: read each fully (that is the load — the relay's
	// store-and-forward write completes only if we consume it).
	go func() {
		for {
			stream, err := sess.AcceptUniStream(ctx)
			if err != nil {
				return
			}
			go func() {
				buf := make([]byte, 32*1024)
				var n uint64
				for {
					m, err := stream.Read(buf)
					n += uint64(m)
					if err != nil {
						break
					}
				}
				t.keyframes.Add(1)
				t.bytesKeyframes.Add(n)
			}()
		}
	}()

	var lastFrameID uint32
	var haveFrame bool
	for {
		dgram, err := sess.ReceiveDatagram(ctx)
		if err != nil {
			if ctx.Err() == nil {
				t.sessionDrops.Add(1)
			}
			return
		}
		t.datagrams.Add(1)
		t.bytesDatagrams.Add(uint64(len(dgram)))
		hdr, _, err := wire.ParseVideoChunk(dgram)
		if err != nil {
			continue // config/clock-mapping/etc.
		}
		if hdr.ChunkIndex != 0 {
			continue
		}
		t.frames.Add(1)
		if haveFrame && hdr.FrameID != lastFrameID+1 {
			t.frameGaps.Add(1)
		}
		haveFrame = true
		lastFrameID = hdr.FrameID
	}
}
