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
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

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
	flag.Parse()
	if *id == "" {
		fmt.Fprintln(os.Stderr, "gawk-loadgen: -id is required")
		os.Exit(2)
	}

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
			runViewer(ctx, subscribeURL, *insecure, &t)
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
			printSummary(&t, *viewers, time.Since(start))
			return
		case <-ctx.Done():
			<-done
			printSummary(&t, *viewers, time.Since(start))
			return
		}
	}
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

func runViewer(ctx context.Context, subscribeURL string, insecure bool, t *totals) {
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
