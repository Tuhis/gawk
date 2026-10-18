package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// eventLog is the receiver's trace: one CSV line per arrival.
//
//	t_ns,kind,frame,index,count,len
//
// kinds: D video chunk datagram, P parity datagram, R video chunk carrier
// record, Q parity carrier record, K keyframe stream complete, A audio
// datagram, O carrier opened, X carrier ended with an error, F carrier
// finished cleanly.
type eventLog struct {
	mu sync.Mutex
	w  *bufio.Writer
}

func (l *eventLog) add(kind byte, frame uint32, index, count, n int) {
	t := time.Now().UnixNano()
	l.mu.Lock()
	fmt.Fprintf(l.w, "%d,%c,%d,%d,%d,%d\n", t, kind, frame, index, count, n)
	l.mu.Unlock()
}

func (l *eventLog) flush() {
	l.mu.Lock()
	_ = l.w.Flush()
	l.mu.Unlock()
}

func runRecv(args []string) error {
	fs := flag.NewFlagSet("recv", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:4500", "UDP listen address")
	out := fs.String("out", "recv.csv", "event log path")
	idle := fs.Duration("max-idle-timeout", 30*time.Second, "the relay's -max-idle-timeout default")
	keepalive := fs.Duration("keepalive-period", 10*time.Second, "the relay's -keepalive-period default")
	streamWin := fs.Uint64("stream-window", 0, "InitialStreamReceiveWindow (0 = quic-go default, as the relay)")
	connWin := fs.Uint64("conn-window", 0, "InitialConnectionReceiveWindow (0 = quic-go default, as the relay)")
	_ = fs.Parse(args)

	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	log := &eventLog{w: bufio.NewWriterSize(f, 1<<20)}
	defer log.flush()

	cert, err := selfSigned()
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	// The same Server shape as internal/transport/server.go: webtransport
	// limits maximal, quic-go's own defaults for everything the relay does
	// not set.
	qcfg := &quic.Config{
		EnableDatagrams:                  true,
		MaxIdleTimeout:                   *idle,
		KeepAlivePeriod:                  *keepalive,
		EnableStreamResetPartialDelivery: true,
	}
	if *streamWin > 0 {
		qcfg.InitialStreamReceiveWindow = *streamWin
	}
	if *connWin > 0 {
		qcfg.InitialConnectionReceiveWindow = *connWin
	}
	srv := &webtransport.Server{
		Config: &webtransport.Config{
			MaxIncomingStreams:    1 << 60,
			MaxIncomingUniStreams: 1 << 60,
			MaxIncomingData:       1 << 60,
		},
		H3: &http3.Server{
			Addr:            *addr,
			TLSConfig:       http3.ConfigureTLSConfig(&tls.Config{Certificates: []tls.Certificate{cert}}),
			Handler:         mux,
			EnableDatagrams: true,
			QUICConfig:      qcfg,
		},
		CheckOrigin: func(*http.Request) bool { return true },
	}
	webtransport.ConfigureHTTP3Server(srv.H3)

	done := make(chan struct{})
	var once sync.Once
	mux.HandleFunc("/lab", func(w http.ResponseWriter, r *http.Request) {
		sess, err := srv.Upgrade(w, r)
		if err != nil {
			fmt.Fprintln(os.Stderr, "upgrade:", err)
			return
		}
		fmt.Fprintln(os.Stderr, "session up from", r.RemoteAddr)
		serveSession(sess, log)
		fmt.Fprintln(os.Stderr, "session ended:", context.Cause(sess.Context()))
		log.flush()
		once.Do(func() { close(done) })
	})

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "serve:", err)
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-done:
	case <-sig:
	}
	_ = srv.Close()
	return nil
}

func serveSession(sess *webtransport.Session, log *eventLog) {
	ctx := sess.Context()
	go func() {
		for {
			st, err := sess.AcceptUniStream(ctx)
			if err != nil {
				return
			}
			go readUni(st, log)
		}
	}()
	for {
		d, err := sess.ReceiveDatagram(ctx)
		if err != nil {
			return
		}
		logDatagram(log, d, 'D', 'P')
	}
}

// logDatagram records one datagram-shaped message, whichever way it came.
func logDatagram(log *eventLog, d []byte, video, parity byte) {
	_, typ, err := wire.PeekType(d)
	if err != nil {
		return
	}
	switch typ {
	case wire.TypeVideoChunk:
		h, _, err := wire.ParseVideoChunk(d)
		if err == nil {
			log.add(video, h.FrameID, int(h.ChunkIndex), int(h.ChunkCount), len(d))
		}
	case wire.TypeParityChunk:
		h, _, err := wire.ParseParityChunk(d)
		if err == nil {
			log.add(parity, h.FrameID, int(h.ParityIndex), int(h.ChunkCount), len(d))
		}
	case wire.TypeAudioFrame:
		log.add('A', 0, 0, 0, len(d))
	}
}

func readUni(st *webtransport.ReceiveStream, log *eventLog) {
	br := bufio.NewReaderSize(st, 64<<10)
	pro, err := br.Peek(2)
	if err != nil {
		return
	}
	switch pro[1] {
	case wire.TypeStreamFrame:
		hdr := make([]byte, wire.StreamFrameHeaderSize)
		if _, err := io.ReadFull(br, hdr); err != nil {
			return
		}
		h, err := wire.ParseStreamFrameHeader(hdr)
		if err != nil {
			return
		}
		body := int64(h.ConfigLen) + int64(h.PayloadLen)
		if _, err := io.CopyN(io.Discard, br, body); err != nil {
			log.add('X', h.FrameID, 0, 0, 0)
			return
		}
		log.add('K', h.FrameID, 0, 1, int(body))
	case wire.TypeReliableCarrier:
		if _, err := br.Discard(2); err != nil {
			return
		}
		log.add('O', 0, 0, 0, 0)
		var lenBuf [2]byte
		rec := make([]byte, 0, wire.MaxDatagramSize)
		for {
			if _, err := io.ReadFull(br, lenBuf[:]); err != nil {
				if errors.Is(err, io.EOF) {
					log.add('F', 0, 0, 0, 0)
				} else {
					log.add('X', 0, 0, 0, 0)
				}
				return
			}
			n := int(lenBuf[0])<<8 | int(lenBuf[1])
			rec = rec[:n]
			if _, err := io.ReadFull(br, rec); err != nil {
				log.add('X', 0, 0, 0, 0)
				return
			}
			logDatagram(log, rec, 'R', 'Q')
		}
	}
}

func selfSigned() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "uplinklab"},
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
