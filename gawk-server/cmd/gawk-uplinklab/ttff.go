package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// runTTFF is one viewer's time to first frame (R74 PP8, docs/76 §11.3): it
// dials /subscribe/{id} on a relay and reports, in ms from just before the
// dial, when the session was up, when the decoder config and the first
// complete keyframe arrived (by datagram or inline in the keyframe stream;
// a keyframe stream counts only once its FIN follows its declared length,
// as the hub and the app take it), and when the first delta chunk of a later
// frame arrived. ttff_ms is the first decodable frame: the later of the
// keyframe and its config. It prints one JSON line and stays subscribed for
// -hold after the first frame (so a second viewer can join a live leg).
//
//	gawk-uplinklab ttff -url https://127.0.0.1:4433 -id ABC123 -hold 3s
func runTTFF(args []string) error {
	fs := flag.NewFlagSet("ttff", flag.ExitOnError)
	base := fs.String("url", "https://127.0.0.1:4433", "relay base URL")
	id := fs.String("id", "", "broadcast ID")
	idFile := fs.String("id-file", "", "read the broadcast ID from this file instead")
	timeout := fs.Duration("timeout", 20*time.Second, "give up when no frame has arrived by then")
	hold := fs.Duration("hold", 0, "stay subscribed this long after the first frame")
	label := fs.String("label", "", "copied into the JSON line")
	_ = fs.Parse(args)
	if *idFile != "" {
		b, err := os.ReadFile(*idFile)
		if err != nil {
			return err
		}
		*id = string(trimSpace(b))
	}
	if *id == "" {
		return errors.New("ttff: -id or -id-file is required")
	}

	var mu sync.Mutex
	res := map[string]any{"label": *label}
	var t0 time.Time
	ms := func() float64 { return float64(time.Since(t0).Microseconds()) / 1000 }
	var keyID uint32
	var haveKey, haveCfg bool // guarded by mu, as res is
	done := make(chan struct{})
	var doneOnce sync.Once
	mark := func(k string) {
		if _, ok := res[k]; !ok {
			res[k] = ms()
		}
		kf, okK := res["keyframe_ms"].(float64)
		cf, okC := res["config_ms"].(float64)
		if _, ok := res["ttff_ms"]; !ok && okK && okC {
			res["ttff_ms"] = max(kf, cf)
		}
		if _, ok := res["ttff_ms"]; ok {
			if _, ok := res["first_delta_ms"]; ok {
				doneOnce.Do(func() { close(done) })
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout+*hold)
	defer cancel()
	d := webtransport.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h3"}}, //nolint:gosec // lab only
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	defer d.Close()
	t0 = time.Now()
	res["t0_unix_ns"] = t0.UnixNano()
	_, sess, err := d.Dial(ctx, *base+"/subscribe/"+*id, nil)
	if err != nil {
		return fmt.Errorf("ttff: subscribe: %w", err)
	}
	mu.Lock()
	res["session_ms"] = ms()
	mu.Unlock()
	go func() {
		for {
			st, err := sess.AcceptUniStream(ctx)
			if err != nil {
				return
			}
			go func() {
				br := bufio.NewReaderSize(st, 64<<10)
				pro, err := br.Peek(2)
				if err != nil {
					return
				}
				typ := pro[1]
				if typ == wire.TypeKeyframeCarrier {
					if _, err := br.Discard(2); err != nil {
						return
					}
				} else if typ != wire.TypeStreamFrame {
					_, _ = io.Copy(io.Discard, br)
					return
				}
				hdr := make([]byte, wire.StreamFrameHeaderSize)
				if _, err := io.ReadFull(br, hdr); err != nil {
					return
				}
				h, err := wire.ParseStreamFrameHeader(hdr)
				if err != nil {
					return
				}
				if _, err := io.CopyN(io.Discard, br, int64(h.ConfigLen)+int64(h.PayloadLen)); err != nil {
					return
				}
				if typ == wire.TypeStreamFrame {
					var extra [1]byte
					if n, _ := br.Read(extra[:]); n > 0 {
						return
					}
				}
				mu.Lock()
				defer mu.Unlock()
				if haveKey {
					return
				}
				haveKey, keyID = true, h.FrameID
				res["keyframe_bytes"] = int(h.ConfigLen) + int(h.PayloadLen)
				if h.ConfigLen > 0 {
					haveCfg = true
					res["config_inline"] = true
					mark("config_ms")
				}
				mark("keyframe_ms")
				_, _ = io.Copy(io.Discard, br) // a 0x1F carrier's records
			}()
		}
	}()
	go func() {
		for {
			dg, err := sess.ReceiveDatagram(ctx)
			if err != nil {
				return
			}
			_, typ, err := wire.PeekType(dg)
			if err != nil {
				continue
			}
			mu.Lock()
			switch typ {
			case wire.TypeDecoderConfig:
				if !haveCfg {
					haveCfg = true
					mark("config_ms")
				}
			case wire.TypeVideoChunk:
				if h, _, err := wire.ParseVideoChunk(dg); err == nil && haveKey && h.FrameID > keyID {
					mark("first_delta_ms")
				}
			}
			mu.Unlock()
		}
	}()
	select {
	case <-done:
	case <-time.After(*timeout):
	}
	mu.Lock()
	line, _ := json.Marshal(res)
	_, ok := res["ttff_ms"]
	mu.Unlock()
	fmt.Println(string(line))
	if ok && *hold > 0 {
		time.Sleep(*hold)
	}
	_ = sess.CloseWithError(0, "done")
	// Let the close reach the relay before the process (and its socket)
	// goes: without it home sometimes holds the session to its idle timeout,
	// which holds the leg's linger, and the next cold join waits for it.
	time.Sleep(300 * time.Millisecond)
	if !ok {
		return errors.New("ttff: no decodable frame before the timeout")
	}
	return nil
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == ' ' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
