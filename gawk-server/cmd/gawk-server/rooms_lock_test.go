package main

import (
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
	"github.com/Tuhis/gawk/gawk-server/internal/transport"
	"github.com/Tuhis/gawk/gawk-server/rooms"
)

// PR #302 review, blocking finding 1: the hub and the room registry call
// into each other from under their own locks — hub.StartPublish holds the
// hub lock while asking IDReserved → Registry.Has, and Registry.Refresh /
// AdoptDynamic asked the hub for BroadcastState under the registry lock —
// an AB/BA deadlock that wedged a relay with one attached broadcast the
// moment /publish got busy. This drives both directions concurrently,
// wired exactly as run() wires them, and fails by timeout if either side
// ever holds its lock across the other's.
func TestHubAndRoomRegistryNeverDeadlock(t *testing.T) {
	discardLog := slog.New(slog.NewTextHandler(io.Discard, nil))
	var roomReg *roomsrv.Registry
	r := hub.NewRegistry(discardLog, hub.Options{
		BroadcastGrace: time.Minute,
		IDReserved:     func(id string) bool { return roomReg != nil && roomReg.Has(id) },
	})
	// The broadcast source is the transport's, as in run: single-pod shape
	// here (no SetCluster), so it is the local hub behind one more call.
	cfg := config.Config{Addr: "127.0.0.1:0", MaxIdleTimeout: 30 * time.Second, KeepAlivePeriod: 10 * time.Second, Rooms: true}
	srv := transport.New(cfg, r, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, errors.New("no cert") },
		discardLog, metrics.NewServerMetrics(prometheus.NewRegistry()))
	roomReg = roomsrv.NewRegistry(roomsrv.Options{
		Broadcasts: srv.RoomBroadcasts(),
		Obfuscate:  r.ObfuscateID,
		Log:        discardLog,
		EmptyGrace: time.Hour,
	})

	// One live broadcast attached to one adopted room, so Refresh has
	// something to ask the hub about.
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	cr := &rooms.Room{Spec: rooms.RoomSpec{Kind: rooms.KindDynamic}}
	cr.Name = "5up4xw"
	cr.Status.Attachments = []rooms.Attachment{{BroadcastID: id, Label: "pc"}}
	if !roomReg.AdoptDynamic(cr) {
		t.Fatal("adopt refused")
	}

	const workers, rounds = 8, 200
	var wg sync.WaitGroup
	for range workers {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range rounds {
				_, p, err := r.StartPublish("")
				if err == nil {
					p.Close()
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range rounds {
				roomReg.Refresh()
				// AdoptDynamic of an already-live code is the other hub
				// caller; it refuses, but only after its prefetch.
				roomReg.AdoptDynamic(cr)
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("hub ↔ room registry deadlocked (StartPublish/IDReserved vs Refresh/AdoptDynamic)")
	}
}
