package main

import (
	"bytes"
	"context"
	"crypto/tls"
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
		MaxBroadcasts:                 9,
		MaxTotalSubscribers:           33,
		MaxBandwidthBytes:             1250000,
		DVRAudio:                      true,
		LiveEdgeAudioOnReliableStream: true,
	}
	want := hub.Options{
		MaxSubscribers:                7,
		BroadcastGrace:                42 * time.Second,
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

// docs/44 §4.10: the startup line states the room knobs, and never the
// create secret itself.
func TestStartupLogStatesTheRoomKnobs(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	logStartup(log, config.Config{Rooms: true, MaxRooms: 12, RoomCreateSecret: "hunter2", RoomsFile: "/r.json"}, "v")
	out := buf.String()
	for _, want := range []string{"rooms=true", "max_rooms=12", "room_create_secret_set=true", "rooms_file=/r.json"} {
		if !strings.Contains(out, want) {
			t.Errorf("startup log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "hunter2") {
		t.Errorf("startup log leaks the room create secret:\n%s", out)
	}
}

// R39 AP2 (docs/42 §9): the startup log must state which ban source this pod
// is enforcing from — the operator's only confirmation surface for a knob
// that otherwise shows up nowhere.
func TestStartupLogStatesTheModerationSource(t *testing.T) {
	for _, source := range []string{"off", "k8s", "file:/etc/gawk/bans.json"} {
		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
		logStartup(log, config.Config{ModerationSource: source}, "test")
		if got := buf.String(); !strings.Contains(got, "moderation_source="+source) {
			t.Errorf("startup log for source %q does not state it:\n%s", source, got)
		}
	}
}

// R39 AP3 (docs/42 §4.3 table, §9): none of the admin-API knobs is a
// hub.Options field, so registryOptions never sees them — which makes the
// startup log the ONLY place an operator can confirm what this pod will do
// with /internal/admin/*. The R2 lesson, one knob-shape over: a knob nobody
// can see is a knob nobody notices is inert.
func TestStartupLogStatesTheAdminAPIConfiguration(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logStartup(log, config.Config{
		AdminAPIToken:       "s3cr3t-token",
		AdminOIDCIssuer:     "https://idp.example/realms/gawk",
		AdminOIDCAudience:   "gawk-admin",
		AdminOIDCRolesClaim: config.DefaultAdminOIDCRolesClaim,
		AdminOIDCRole:       config.DefaultAdminOIDCRole,
	}, "test")
	out := buf.String()

	for _, want := range []string{
		"admin_api_token_set=true",
		"admin_oidc_issuer=https://idp.example/realms/gawk",
		"admin_oidc_audience=gawk-admin",
		"admin_oidc_role=operator",
		"admin_api_enabled=true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("startup log is missing %q:\n%s", want, out)
		}
	}
	// THE TOKEN ITSELF IS NEVER LOGGED — only whether one is set, which is
	// what decides 404 vs. 401.
	if strings.Contains(out, "s3cr3t-token") {
		t.Fatalf("the admin API token leaked into the startup log:\n%s", out)
	}

	// With no credential the log says so, so "why is /internal/admin 404ing?"
	// is answerable from the pod's own first line.
	buf.Reset()
	logStartup(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})),
		config.Config{}, "test")
	if got := buf.String(); !strings.Contains(got, "admin_api_enabled=false") {
		t.Errorf("startup log does not state that the admin API is disabled:\n%s", got)
	}
}

// The redacted config view and the startup log must agree about the
// resume-token key mode — docs/42 §4.5 says the sanitized value "echoes the
// startup log", and one definition is what makes that true rather than
// aspirational.
func TestResumeTokenKeyModeMatchesTheSanitizedConfig(t *testing.T) {
	for _, cfg := range []config.Config{
		{ResumeTokenKey: make([]byte, 32)},
		{PublishSecret: "hunter2"},
		{},
	} {
		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
		logStartup(log, cfg, "test")
		mode := resumeTokenKeyMode(cfg)
		if !strings.Contains(buf.String(), "resume_token_key_mode="+mode) {
			t.Errorf("startup log does not carry mode %q:\n%s", mode, buf.String())
		}
		if got := cfg.Sanitized().ResumeTokenKey; !strings.Contains(got, mode) {
			t.Errorf("sanitized resumeTokenKey %q does not carry the logged mode %q", got, mode)
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
