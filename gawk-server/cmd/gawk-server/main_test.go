package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/roomcluster"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
	"github.com/Tuhis/gawk/gawk-server/internal/transport"
	"github.com/Tuhis/gawk/gawk-server/moderation"
	"github.com/Tuhis/gawk/gawk-server/rooms"

	"github.com/prometheus/client_golang/prometheus"
)

// R2 review finding F1: the hardening limits parsed by config.ParseFlags must
// actually reach hub.Options in production. The original R2 change wired them
// only into the transport test helper, so -max-bandwidth was a no-op and
// -max-broadcasts / -max-total-subscribers overrides were silently ignored.
func TestRegistryOptionsCarryAllLimits(t *testing.T) {
	// Every value here is deliberately non-zero. A bool left at its zero value
	// proves nothing about plumbing — it matches whether the assignment exists
	// or not — which is the same blind spot F1 was, one type down.
	cfg := config.Config{
		MaxSubscribers:                7,
		BroadcastGrace:                42 * time.Second,
		PublisherStallTimeout:         11 * time.Second,
		PublisherStallEnds:            true,
		MaxBroadcasts:                 9,
		MaxTotalSubscribers:           33,
		MaxBandwidthBytes:             1250000,
		DVRAudio:                      true,
		LiveEdgeAudioOnReliableStream: true,
	}
	want := hub.Options{
		MaxSubscribers:                7,
		BroadcastGrace:                42 * time.Second,
		PublisherStallTimeout:         11 * time.Second,
		PublisherStallEnds:            true,
		MaxBroadcasts:                 9,
		MaxTotalSubscribers:           33,
		MaxBandwidthBytes:             1250000,
		DVRAudio:                      true,
		LiveEdgeAudioOnReliableStream: true,
	}
	// DeepEqual, not ==: Options grew func-typed cluster hooks in R17 W3
	// (nil here on both sides — registryOptions never sets them; main wires
	// them separately when -cluster-mode is on).
	if got := registryOptions(cfg); !reflect.DeepEqual(got, want) {
		t.Errorf("registryOptions(cfg) = %+v, want %+v", got, want)
	}
}

// R42 (docs/44 RM2): every -room-* knob must reach roomsrv.Options in
// production — the R2 rule, one registry over. Every value is deliberately
// non-zero, and IDReserved on the hub side is asserted separately by
// TestRoomsWiringReservesLiveRoomCodes.
func TestRoomOptionsCarryAllKnobs(t *testing.T) {
	cfg := config.Config{
		Rooms:               true,
		RoomEmptyGrace:      77 * time.Second,
		MaxRooms:            3,
		MaxRoomBroadcasts:   5,
		MaxRoomParticipants: 11,
		RoomCreateSecret:    "invite",
	}
	want := roomsrv.Options{
		EmptyGrace:      77 * time.Second,
		MaxRooms:        3,
		MaxBroadcasts:   5,
		MaxParticipants: 11,
		CreateSecret:    "invite",
	}
	if got := roomOptions(cfg); !reflect.DeepEqual(got, want) {
		t.Errorf("roomOptions(cfg) = %+v, want %+v", got, want)
	}
}

// The startup line is the operator's confirmation surface for every knob
// (the R2 lesson; docs/42 §9 AP2 for the ban source, docs/44 §4.10 for
// rooms): it carries exactly the redacted view GET /internal/admin/config
// serves, under both handlers, and never a secret. Which fields that view
// covers, and that each secret is redacted, is proven in internal/config.
func TestStartupLogCarriesTheSanitizedConfig(t *testing.T) {
	cfg := config.Config{
		ModerationSource: "file:/etc/gawk/bans.json",
		Rooms:            true,
		MaxRooms:         12,
		AdminOIDCIssuer:  "https://idp.example/realms/gawk",
		PublishSecret:    "sentinel-publish",
		AdminAPIToken:    "sentinel-admin",
		RoomCreateSecret: "sentinel-room",
		InternalPSK:      "sentinel-psk",
		ResumeTokenKey:   []byte("sentinel-resume"),
		TelemetryKey:     []byte("sentinel-telemetry"),
	}

	var buf bytes.Buffer
	logStartup(slog.New(slog.NewJSONHandler(&buf, nil)), cfg, "v1.2.3")
	var line struct {
		Msg     string          `json:"msg"`
		Version string          `json:"version"`
		Config  json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("startup line is not one JSON object: %v\n%s", err, buf.String())
	}
	var got, want map[string]any
	if err := json.Unmarshal(line.Config, &got); err != nil {
		t.Fatalf("config is not an object: %v\n%s", err, buf.String())
	}
	wantJSON, _ := json.Marshal(cfg.Sanitized())
	_ = json.Unmarshal(wantJSON, &want)
	if line.Msg != "starting" || line.Version != "v1.2.3" || !reflect.DeepEqual(got, want) {
		t.Errorf("startup line = %s\nwant msg=starting version=v1.2.3 config=%s", buf.String(), wantJSON)
	}

	var text bytes.Buffer
	logStartup(slog.New(slog.NewTextHandler(&text, nil)), cfg, "v1.2.3")
	for _, want := range []string{
		"config.moderationSource=file:/etc/gawk/bans.json",
		"config.maxRooms=12",
		"config.adminApiToken=<set>",
		"config.resumeTokenKey=<set:explicit-key>",
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text startup line lacks %q:\n%s", want, text.String())
		}
	}

	for _, out := range []string{buf.String(), text.String()} {
		if strings.Contains(out, "sentinel") {
			t.Errorf("startup line leaks a secret:\n%s", out)
		}
	}
}

// R39 (PR #280 review): moderationsrc.Start launches the ban informer, whose
// very first events can reach srv.HandleBanAdded -> terminate() within
// milliseconds — and terminate() reads the edge manager and, through the hub
// hooks, the cluster coordinator. Both must already be wired.
//
// The stake is not only the data race. A kill actuated before the coordinator
// exists tears the broadcast down locally but never deletes its origin Lease,
// so every other pod in the fleet keeps routing viewers to a dead origin
// until something else cleans up. A pod that cold-started with Ban CRs
// already present is exactly the case that hits it.
//
// The file source's startup load is synchronous, so a ban in the file
// actuates inside wireSubsystems — which is what makes the ordering
// observable at all.
func TestWiringInstallsTheClusterBeforeTheBanSourceCanActuate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bans.json")
	if err := os.WriteFile(path, []byte(
		`[{"target":{"type":"broadcastId","value":"ABC23Z"},"reason":"kill"}]`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	srv := &recordingWiredServer{}
	cfg := config.Config{ClusterMode: true, ModerationSource: "file:" + path}
	built := false
	buildCoord := func() (transport.ClusterCoordinator, string, error) {
		built = true
		return nil, "pod-0", nil
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := wireSubsystems(context.Background(), cfg, srv, moderation.NewSet(), log, buildCoord, nil); err != nil {
		t.Fatalf("wireSubsystems: %v", err)
	}
	if !built {
		t.Fatal("the coordinator was never built in cluster mode")
	}

	want := []string{"set-moderation", "set-cluster", "ban-added"}
	if got := srv.calls(); !reflect.DeepEqual(got, want) {
		t.Errorf("wiring order = %v, want %v", got, want)
	}
}

// Without -cluster-mode there is no coordinator to build, and the ban source
// still starts and still actuates: enforcement is not a federation feature
// (docs/42 §4.3).
func TestWiringWithoutClusterModeStillStartsTheBanSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bans.json")
	if err := os.WriteFile(path, []byte(
		`[{"target":{"type":"broadcastId","value":"ABC23Z"},"reason":"kill"}]`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	srv := &recordingWiredServer{}
	cfg := config.Config{ModerationSource: "file:" + path}
	buildCoord := func() (transport.ClusterCoordinator, string, error) {
		t.Error("the coordinator was built with -cluster-mode off")
		return nil, "", nil
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := wireSubsystems(context.Background(), cfg, srv, moderation.NewSet(), log, buildCoord, nil); err != nil {
		t.Fatalf("wireSubsystems: %v", err)
	}
	want := []string{"set-moderation", "ban-added"}
	if got := srv.calls(); !reflect.DeepEqual(got, want) {
		t.Errorf("wiring order = %v, want %v", got, want)
	}
}

// A coordinator that cannot be built fails the process, and the ban source is
// never started against a half-wired server.
func TestWiringFailsBeforeStartingTheBanSourceWhenTheCoordinatorFails(t *testing.T) {
	srv := &recordingWiredServer{}
	cfg := config.Config{ClusterMode: true, ModerationSource: "off"}
	buildCoord := func() (transport.ClusterCoordinator, string, error) {
		return nil, "", errors.New("no kubeconfig")
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := wireSubsystems(context.Background(), cfg, srv, moderation.NewSet(), log, buildCoord, nil); err == nil {
		t.Fatal("wireSubsystems returned nil, want the coordinator error")
	}
	for _, call := range srv.calls() {
		if call == "set-cluster" {
			t.Error("SetCluster ran with a coordinator that failed to build")
		}
	}
}

// recordingWiredServer records the order the wiring touches the server in.
type recordingWiredServer struct {
	mu  sync.Mutex
	seq []string
}

func (s *recordingWiredServer) record(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq = append(s.seq, name)
}

func (s *recordingWiredServer) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seq...)
}

func (s *recordingWiredServer) SetModeration(*moderation.Set) { s.record("set-moderation") }
func (s *recordingWiredServer) SetCluster(transport.ClusterCoordinator, string) {
	s.record("set-cluster")
}
func (s *recordingWiredServer) SetRoomCluster(transport.RoomCluster, string) {
	s.record("set-room-cluster")
}
func (s *recordingWiredServer) HandleBanAdded(moderation.Record) { s.record("ban-added") }

// R42 RM3: the room store is installed AFTER the cluster (its drain hook
// chains onto SetCluster's lease release, and adoption reads the cluster
// wiring) and its informer starts AFTER the ban source — the rule that
// governs the ban source itself, one informer over: its first events reach
// the transport and the registry within milliseconds of starting.
func TestWiringInstallsTheRoomStoreAfterTheClusterAndStartsItsInformerLast(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bans.json")
	if err := os.WriteFile(path, []byte(
		`[{"target":{"type":"broadcastId","value":"ABC23Z"},"reason":"kill"}]`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	srv := &recordingWiredServer{}
	cfg := config.Config{ClusterMode: true, Rooms: true, ModerationSource: "file:" + path}
	buildCoord := func() (transport.ClusterCoordinator, string, error) { return nil, "pod-0", nil }
	buildRooms := func() (transport.RoomCluster, string, func(), error) {
		srv.record("build-rooms")
		return nil, "pod-0", func() { srv.record("rooms-informer-started") }, nil
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := wireSubsystems(context.Background(), cfg, srv, moderation.NewSet(), log, buildCoord, buildRooms); err != nil {
		t.Fatalf("wireSubsystems: %v", err)
	}
	want := []string{"set-moderation", "set-cluster", "build-rooms", "set-room-cluster", "ban-added", "rooms-informer-started"}
	if got := srv.calls(); !reflect.DeepEqual(got, want) {
		t.Errorf("wiring order = %v, want %v", got, want)
	}

	// A store that cannot be built fails the process before the ban source
	// starts against a half-wired server.
	srv = &recordingWiredServer{}
	failing := func() (transport.RoomCluster, string, func(), error) {
		return nil, "", nil, errors.New("no kubeconfig")
	}
	if err := wireSubsystems(context.Background(), cfg, srv, moderation.NewSet(), log, buildCoord, failing); err == nil {
		t.Fatal("wireSubsystems returned nil, want the room store error")
	}
	for _, call := range srv.calls() {
		if call == "set-room-cluster" || call == "ban-added" {
			t.Errorf("%s ran after the room store failed to build", call)
		}
	}
}

// Without -cluster-mode there is no room store: run passes a nil builder,
// and the wiring must not touch SetRoomCluster — single-pod mode stays
// byte-identical (docs/44 §4.3).
func TestWiringWithoutARoomBuilderNeverInstallsARoomStore(t *testing.T) {
	srv := &recordingWiredServer{}
	cfg := config.Config{Rooms: true, ModerationSource: "off"}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := wireSubsystems(context.Background(), cfg, srv, moderation.NewSet(), log, nil, nil); err != nil {
		t.Fatalf("wireSubsystems: %v", err)
	}
	if got := srv.calls(); !reflect.DeepEqual(got, []string{"set-moderation"}) {
		t.Errorf("wiring calls = %v, want only set-moderation", got)
	}
}

// R42 RM3: the rooms stats source is nil until the registry is on the
// transport. run once read it BEFORE SetRooms, so a rooms-enabled relay
// exported no /statusz rooms section and no room metric series while every
// unit test stayed green — the R2 F1 blind spot again, one seam over. Real
// transport.Server, not a fake: the nil-before-install behaviour under test
// is the transport's own.
func TestInstallRoomsReadsTheStatsSourceAfterTheInstall(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := hub.NewRegistry(log, hub.Options{MaxSubscribers: 1})
	cfg := config.Config{Addr: "127.0.0.1:0", MaxIdleTimeout: 30 * time.Second, KeepAlivePeriod: 10 * time.Second, Rooms: true}
	srv := transport.New(cfg, r, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, errors.New("no cert") },
		log, metrics.NewServerMetrics(prometheus.NewRegistry()))
	if src := srv.RoomStatsSource(); src != nil {
		t.Fatalf("stats source before SetRooms = %v, want nil (the premise of the ordering rule)", src)
	}
	if got := installRooms(srv, nil); got != nil {
		t.Fatalf("installRooms with -rooms off = %v, want a nil interface", got)
	}
	reg := roomsrv.NewRegistry(roomsrv.Options{Broadcasts: srv.RoomBroadcasts(), Obfuscate: r.ObfuscateID, Log: log})
	src := installRooms(srv, reg)
	if src == nil {
		t.Fatal("installRooms returned nil with a registry: the stats source was read before the install")
	}
	if src.Stats() == nil {
		t.Fatal("the installed source reports no rooms map")
	}
}

// R42 review (PR #302): every cluster seam of roomsrv.Options must reach
// production — the R2 rule again. The registry grew Unreserve (give a
// reservation back on the local re-check race) and AttachSecret (resolve a
// static room's secret per join so a rotation needs no CR bump); a seam
// left nil is a silent no-op in cluster mode while every unit test stays
// green. Before the store exists the seams fail closed: Reserve and
// AttachSecret refuse (ErrUnavailable), the notifications are no-ops.
func TestRoomClusterSeamsAreAllWiredAndFailClosedWithoutAStore(t *testing.T) {
	var ro roomsrv.Options
	wireRoomClusterSeams(&ro, func() *roomcluster.Store { return nil })
	if ro.Reserve == nil || ro.Unreserve == nil || ro.AttachSecret == nil ||
		ro.OnRoomEnded == nil || ro.OnRoomEmpty == nil || ro.OnAttachmentsChanged == nil {
		t.Fatalf("a cluster seam is unwired: %+v", ro)
	}
	if !ro.UnknownIsExpired {
		t.Fatal("cluster mode must let the refresh poll expire a broadcast with no hub here and no lease anywhere (PR #302 review)")
	}
	if err := ro.Reserve(context.Background(), &rooms.Room{}); !errors.Is(err, roomsrv.ErrUnavailable) {
		t.Errorf("Reserve without a store = %v, want ErrUnavailable", err)
	}
	if _, found, err := ro.AttachSecret("tuhisroom"); err == nil || found {
		t.Errorf("AttachSecret without a store = found=%v err=%v, want an error (fail closed)", found, err)
	}
	// No panics on the nil store.
	ro.Unreserve(context.Background(), "k7xq2m")
	ro.OnRoomEnded("k7xq2m", 0)
	ro.OnRoomEmpty("k7xq2m", true)
	ro.OnAttachmentsChanged("k7xq2m", nil)
}

// R50's headline promise is that a deployment without a bus is byte-identical
// to one predating it, and EB1's acceptance criteria name /metrics next to
// /statusz. So the counters are registered only when a bus is configured: an
// always-zero gawk_eventbus_* series would claim a subsystem that is not
// there.
func TestEventBusMetricsAreOnlyBuiltWhenTheBusIs(t *testing.T) {
	off := prometheus.NewRegistry()
	if m := eventBusMetrics(config.Config{}, off); m != nil {
		t.Error("the bus counters were built with no -eventbus-url")
	}
	if n := testutil.CollectAndCount(off); n != 0 {
		t.Errorf("a relay with the bus off exports %d gawk_eventbus_* series, want none", n)
	}

	on := prometheus.NewRegistry()
	if m := eventBusMetrics(config.Config{EventBusURL: "nats://nats:4222"}, on); m == nil {
		t.Fatal("no counters with a bus configured")
	}
	if n := testutil.CollectAndCount(on); n == 0 {
		t.Error("a configured bus exports no counters")
	}
}
