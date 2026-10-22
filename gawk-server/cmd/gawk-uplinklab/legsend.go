package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// runLegSend is the R74 PoP's own sender (docs/76 item 18, PP8): a hub with a
// synthetic publisher, and the receiver dialled as an uplink leg through
// hub.SubscribeLeg — the leg writer that ships, carrier or datagram — so the
// lab measures exactly what a PoP puts on a venue's uplink. The QUIC timers
// are the leg's (poplink.LinkIdleTimeout / LinkKeepAlive).
//
// It writes the sender log analyze.py reads: one F line per frame
// (F,frame,key,size,n,k,t_ns,0) at the moment the frame entered the hub.
//
//	gawk-uplinklab legsend --url https://127.0.0.1:4600/lab --out send.csv --mode carrier --mbps 8
func runLegSend(args []string) error {
	fs := flag.NewFlagSet("legsend", flag.ExitOnError)
	target := fs.String("url", "https://127.0.0.1:4600/lab", "receiver URL (through the proxy)")
	out := fs.String("out", "send.csv", "sender log path")
	mode := fs.String("mode", "carrier", "carrier | datagram (the PoP's -pop-leg-delivery)")
	mbps := fs.Float64("mbps", 8, "stream bitrate, Mbit/s")
	fps := fs.Int("fps", 60, "frames per second")
	gop := fs.Duration("gop", 2*time.Second, "keyframe interval")
	kfRatio := fs.Float64("keyframe-ratio", 8, "a keyframe's size as a multiple of a delta's")
	duration := fs.Duration("duration", 30*time.Second, "run length")
	chunk := fs.Int("chunk", 1100, "delta chunk payload bytes")
	parity := fs.Int("parity", 2, "parity symbols per delta (0-2), as a broadcaster at the fleet's -parity-default sends them: the leg forwards them, it never opts down")
	tracePath := fs.String("trace", "", "frame-size trace (size,flags per frame, as traces.sh writes; K marks a keyframe); replaces -mbps, -gop and -keyframe-ratio")
	seconds := fs.Float64("seconds", 0, "run length in seconds (fair/run.sh's flag; overrides -duration)")
	cc := fs.String("cc", "cubic", "congestion controller: quic-go ships Cubic only (docs/76 item 18)")
	_ = fs.Parse(args)
	if *cc != "cubic" {
		return fmt.Errorf("legsend: -cc %q: the PoP's quic-go has Cubic only", *cc)
	}
	if *seconds > 0 {
		*duration = time.Duration(*seconds * float64(time.Second))
	}
	var trace []traceFrame
	if *tracePath != "" {
		var err error
		if trace, err = loadTrace(*tracePath); err != nil {
			return err
		}
	}
	if *parity < 0 || *parity > 2 {
		return fmt.Errorf("legsend: -parity %d: want 0, 1 or 2", *parity)
	}
	if *mode != "carrier" && *mode != "datagram" {
		return fmt.Errorf("legsend: -mode %q: want carrier or datagram", *mode)
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	logw := bufio.NewWriterSize(f, 1<<20)
	defer logw.Flush()

	d := &webtransport.Transport{
		// The lab receiver's certificate is self-signed and per run.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // lab only
		QUICConfig: &quic.Config{
			EnableDatagrams:                  true,
			EnableStreamResetPartialDelivery: true,
			MaxIdleTimeout:                   poplink.LinkIdleTimeout,
			KeepAlivePeriod:                  poplink.LinkKeepAlive,
		},
	}
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), *duration+10*time.Second)
	defer cancel()
	_, sess, err := d.Dial(ctx, *target, nil)
	if err != nil {
		return fmt.Errorf("legsend: dial: %w", err)
	}
	defer sess.CloseWithError(0, "done")

	reg := hub.NewRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)), hub.Options{MaxSubscribers: 4, BroadcastGrace: time.Minute})
	id, pub, err := reg.StartPublish("")
	if err != nil {
		return err
	}
	defer pub.Close()
	sub, err := reg.SubscribeLeg(id, &labLegConn{sess: sess}, hub.LegOptions{Carrier: *mode == "carrier"})
	if err != nil {
		return err
	}
	defer sub.Close()

	perFrame := *mbps * 1e6 / 8 / float64(*fps)
	gopFrames := max(1, int(gop.Seconds()*float64(*fps)))
	delta := perFrame * float64(gopFrames) / (*kfRatio + float64(gopFrames-1))
	kfSize := int(delta * *kfRatio)
	tick := time.NewTicker(time.Second / time.Duration(*fps))
	defer tick.Stop()
	end := time.Now().Add(*duration)
	var mu sync.Mutex
	for frame := uint32(1); time.Now().Before(end); frame++ {
		<-tick.C
		ts := uint64(frame) * uint64(time.Second/time.Microsecond) / uint64(*fps)
		key := (int(frame)-1)%gopFrames == 0
		kfLen, deltaLen := kfSize, int(delta)
		if len(trace) > 0 {
			tf := trace[(int(frame)-1)%len(trace)]
			key, kfLen, deltaLen = tf.key, tf.size, tf.size
		}
		now := time.Now().UnixNano()
		if key {
			payload := bytes.Repeat([]byte{byte(frame)}, kfLen)
			hdr, err := wire.AppendStreamFrameHeader(nil, wire.StreamFrameHeader{
				Keyframe: true, FrameID: frame, TimestampUs: ts, PayloadLen: uint32(len(payload)),
			})
			if err != nil {
				return err
			}
			if err := pub.IngestKeyframeStream(bytes.NewReader(append(hdr, payload...))); err != nil {
				return err
			}
			mu.Lock()
			fmt.Fprintf(logw, "F,%d,1,%d,0,0,%d,0\n", frame, len(hdr)+kfLen, now)
			mu.Unlock()
			continue
		}
		size := deltaLen
		dgrams, par, err := deltaDatagrams(frame, ts, size, *chunk, *parity)
		if err != nil {
			return err
		}
		for _, dg := range dgrams {
			pub.HandleDatagram(dg)
		}
		for _, dg := range par {
			pub.HandleDatagram(dg)
		}
		mu.Lock()
		fmt.Fprintf(logw, "F,%d,0,%d,%d,%d,%d,0\n", frame, size, len(dgrams), len(par), now)
		mu.Unlock()
	}
	// Let the last GOP's carrier finish.
	time.Sleep(500 * time.Millisecond)
	return nil
}

// deltaDatagrams chunks one delta of size bytes into VideoChunk datagrams of
// at most chunk payload bytes and adds level RAID-6 parity symbols over the
// payloads (wire.ComputeParity, as gawk-pubsim and the app send them). A
// frame past wire.MaxParityDataChunks goes without parity, as theirs does.
func deltaDatagrams(frame uint32, ts uint64, size, chunk, level int) (dgrams, parity [][]byte, err error) {
	size = max(1, size)
	n := (size + chunk - 1) / chunk
	payloads := make([][]byte, 0, n)
	for i := range n {
		body := min(chunk, size-i*chunk)
		p := bytes.Repeat([]byte{byte(frame) ^ byte(i)}, body)
		d, err := wire.AppendVideoChunk(nil, wire.VideoChunkHeader{
			FrameID: frame, ChunkIndex: uint16(i), ChunkCount: uint16(n), TimestampUs: ts,
		}, p)
		if err != nil {
			return nil, nil, err
		}
		dgrams = append(dgrams, d)
		payloads = append(payloads, p)
	}
	if level <= 0 || n > wire.MaxParityDataChunks {
		return dgrams, nil, nil
	}
	symbols, err := wire.ComputeParity(payloads, level)
	if err != nil {
		return dgrams, nil, nil
	}
	for i, sym := range symbols {
		d, err := wire.AppendParityChunk(nil, wire.ParityChunkHeader{
			FrameID: frame, ParityIndex: uint8(i), ChunkCount: uint16(n), FrameBytes: uint32(size),
		}, sym)
		if err != nil {
			return nil, nil, err
		}
		parity = append(parity, d)
	}
	return dgrams, parity, nil
}

// labLegConn is the leg's hub.Conn over the lab session (the transport's
// adapter, minus the ClockMapping rewrite a lab run has no use for).
type labLegConn struct{ sess *webtransport.Session }

func (c *labLegConn) SendDatagram(b []byte) error { return c.sess.SendDatagram(b) }

func (c *labLegConn) OpenKeyframeStream() (hub.KeyframeStream, error) {
	st, err := c.sess.OpenUniStream()
	if err != nil {
		return nil, err
	}
	return labStream{st}, nil
}

func (c *labLegConn) OpenCarrierStream() (hub.KeyframeStream, error) { return c.OpenKeyframeStream() }

func (c *labLegConn) CloseWithError(code uint32, reason string) error {
	return c.sess.CloseWithError(webtransport.SessionErrorCode(code), reason)
}

type labStream struct{ *webtransport.SendStream }

func (s labStream) CancelWrite() { s.SendStream.CancelWrite(0) }

type traceFrame struct {
	size int
	key  bool
}

// loadTrace reads `size,flags` per frame, as ffprobe prints them (the Rust
// sender's format).
func loadTrace(path string) ([]traceFrame, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []traceFrame
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		sizeStr, flags, _ := strings.Cut(line, ",")
		n, err := strconv.Atoi(strings.TrimSpace(sizeStr))
		if err != nil {
			return nil, fmt.Errorf("legsend: trace %s: %q: %w", path, line, err)
		}
		out = append(out, traceFrame{size: max(1, n), key: strings.Contains(flags, "K")})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("legsend: trace %s is empty", path)
	}
	return out, nil
}
