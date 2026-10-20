package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"
)

// runSub subscribes to a broadcast on a real relay and logs every arrival in
// recv's format, so analyze.py measures sender → relay → viewer per frame.
// Build it against the longer quic-go queue (gawk-uplinklab-bigq) when the
// relay is the thing under test: a viewer dropping datagrams in its own
// 32-deep queue would be counted against the relay.
func runSub(args []string) error {
	fs := flag.NewFlagSet("sub", flag.ExitOnError)
	base := fs.String("url", "https://127.0.0.1:4433", "relay base URL")
	idFile := fs.String("id-file", "", "file the sender writes the broadcast ID to")
	out := fs.String("out", "recv.csv", "event log path")
	_ = fs.Parse(args)

	var id string
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if b, err := os.ReadFile(*idFile); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			id = strings.TrimSpace(string(b))
			break
		}
	}
	if id == "" {
		return fmt.Errorf("no broadcast ID in %s", *idFile)
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	log := &eventLog{w: bufio.NewWriterSize(f, 1<<20)}
	defer log.flush()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := webtransport.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h3"}},
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	defer d.Close()
	_, sess, err := d.Dial(ctx, *base+"/subscribe/"+id, nil)
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	go func() {
		for {
			st, err := sess.AcceptUniStream(ctx)
			if err != nil {
				return
			}
			go readUni(st, log)
		}
	}()
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		cancel()
	}()
	for {
		dg, err := sess.ReceiveDatagram(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("session ended: %w", err)
		}
		logDatagram(log, dg, 'D', 'P')
	}
}
