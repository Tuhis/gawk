// gawk-server is the WebTransport relay for the gawk game stream:
// one publisher fans out encoded video datagrams to a small set of
// subscribers.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/moderationsrc"
	"github.com/Tuhis/gawk/gawk-server/internal/ops"
	"github.com/Tuhis/gawk/gawk-server/internal/roomcluster"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrc"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
	"github.com/Tuhis/gawk/gawk-server/internal/transport"
	"github.com/Tuhis/gawk/gawk-server/moderation"
	"github.com/Tuhis/gawk/gawk-server/rooms"
)

// Stamped at build time via -ldflags "-X main.version=..." (see
// deploy/Dockerfile); "dev" for plain go build/run.
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gawk-server:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.ParseFlags(os.Args[1:], os.Getenv)
	if err != nil {
		return err
	}
	// R37: the build version rides config so the transport's RelayIdentity
	// (wire 0x11) can carry it without importing main.
	cfg.ReleaseVersion = version

	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	log := slog.New(handler)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logStartup(log, cfg, version)

	getCert, err := certSource(cfg, log)
	if err != nil {
		return err
	}

	// Cluster mode (R17 W3, docs/22): the hub's lifecycle hooks and the
	// coordinator reference each other, so the hooks close over this pointer,
	// which is assigned right after the registry exists. Both hook paths are
	// nil-safe until then (no publisher can connect before Run anyway).
	var coord *cluster.Coordinator
	// R42 rooms (docs/44): the registry is assigned right after the hub
	// exists and closed over by the hub hooks below, exactly like coord.
	var roomReg *roomsrv.Registry
	// RM3: the cluster room store (Room CRs + home leases), assigned by the
	// wiring step in cluster mode and nil otherwise. The registry's cluster
	// seams and the hub's mirror check close over it, nil-safe like coord.
	var roomStore *roomcluster.Store
	hubOpts := registryOptions(cfg)
	if cfg.ClusterMode {
		hubOpts.OnPublisherClosed = func(id string) {
			if coord == nil {
				return
			}
			opCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := coord.EnterGrace(opCtx, id); err != nil {
				log.Warn("lease grace stamp failed", "broadcast_id", id, "err", err)
			}
		}
		hubOpts.OnBroadcastExpired = func(id string) {
			if coord == nil {
				return
			}
			opCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := coord.Delete(opCtx, id); err != nil {
				log.Warn("lease delete failed", "broadcast_id", id, "err", err)
			}
		}
	}
	if cfg.Rooms {
		// Rooms need both lifecycle hooks in single-pod mode too (an
		// attachment flips to "away" and is removed on expiry — docs/44
		// §4.4), so they chain onto whatever cluster mode installed.
		hubOpts.OnPublisherClosed = chainHook(hubOpts.OnPublisherClosed, func(id string) {
			if roomReg != nil {
				roomReg.PublisherClosed(id)
			}
		})
		hubOpts.OnBroadcastExpired = chainHook(hubOpts.OnBroadcastExpired, func(id string) {
			if roomReg != nil {
				roomReg.BroadcastExpired(id)
			}
		})
		// The mirror check consults the local registry AND, in cluster mode,
		// the Room CR cache: a room homed on another pod still reserves its
		// code fleet-wide (docs/44 §4.2).
		hubOpts.IDReserved = func(id string) bool {
			return (roomReg != nil && roomReg.Has(id)) || (roomStore != nil && roomStore.Known(id))
		}
	}

	r := hub.NewRegistry(log, hubOpts)
	if cfg.Rooms {
		ro := roomOptions(cfg)
		ro.Broadcasts = hubBroadcasts{r}
		ro.Obfuscate = r.ObfuscateID
		ro.PodName = os.Getenv("POD_NAME")
		ro.Log = log
		if cfg.ClusterMode {
			// The store is built later (buildRooms below) and read through
			// the closure, as the hub's lease hooks read coord.
			wireRoomClusterSeams(&ro, func() *roomcluster.Store { return roomStore })
		}
		roomReg = roomsrv.NewRegistry(ro)
	}

	// Prometheus wiring (R9, docs/13): runtime collectors + build info, the
	// hub registry collector, and the transport connection counters — all
	// served by the TCP ops endpoint alongside /healthz and /statusz.
	promReg := metrics.NewBaseRegistry(version)
	promReg.MustRegister(metrics.NewRegistryCollector(r))
	sm := metrics.NewServerMetrics(promReg)

	// R39 moderation (docs/42 §4.3). The set is always constructed and always
	// scraped — with -moderation-source=off nothing feeds it, every publish
	// check is a cheap miss, and gawk_moderation_bans_active reads zero,
	// which is how an operator tells "no bans" from "no moderation".
	bans := moderation.NewSet()
	promReg.MustRegister(metrics.NewModerationCollector(bans))

	// The WebTransport (UDP) server and the ops (TCP) listener run together;
	// either one failing tears the other down.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	srv := transport.New(cfg, r, getCert, log, sm)
	// Publishing `coord` — the variable the hub's lease hooks above close
	// over — is part of the cluster wiring, so it happens here rather than at
	// the call site: wireSubsystems only guarantees the ORDER, and both
	// halves have to be inside the ordered step for that to mean anything.
	buildCoord := func() (transport.ClusterCoordinator, string, error) {
		c, podName, cerr := buildCoordinator(cfg, srv.HandleLeaseDeleted, srv.HandleLeaseLost, log)
		if cerr != nil {
			return nil, "", cerr
		}
		coord = c
		go coord.Run(runCtx)
		return c, podName, nil
	}
	// RM3: the room store is built from the same in-cluster client shape
	// as the coordinator and published here for the same reason coord is.
	// Its informer is the LAST thing wireSubsystems starts (its first
	// events reach the registry and the transport within milliseconds).
	var buildRooms func() (transport.RoomCluster, string, func(), error)
	if cfg.ClusterMode && cfg.Rooms {
		buildRooms = func() (transport.RoomCluster, string, func(), error) {
			st, podName, rerr := buildRoomStore(cfg, roomReg, r.ObfuscateID, srv.HandleRoomLeaseLost, log)
			if rerr != nil {
				return nil, "", nil, rerr
			}
			roomStore = st
			return st, podName, func() { go st.Run(runCtx) }, nil
		}
	}
	if err := wireSubsystems(runCtx, cfg, srv, bans, log, buildCoord, buildRooms); err != nil {
		return err
	}
	// Installed after the cluster wiring (SetRooms hands the registry the
	// transport's token key), before Run — the same rule as SetModeration.
	// The stats source is read AFTER the install (installRooms pins the
	// order): it is nil until the registry is on the transport, and reading
	// it first silently dropped the /statusz section and every room series
	// from a rooms-enabled relay while the unit suite stayed green.
	roomStats := installRooms(srv, roomReg)
	if roomStats != nil {
		promReg.MustRegister(metrics.NewRoomCollector(roomStats))
	}
	if roomReg != nil {
		// The static-room file source starts last, like the ban source,
		// for the same reason.
		go roomReg.RunRefresh(runCtx)
		if cfg.RoomsFile != "" {
			if err := roomsrc.StartFile(runCtx, cfg.RoomsFile, roomsrc.Options{Registry: roomReg, Log: log}); err != nil {
				return err
			}
		}
	}

	// The R18 viewer-count pump (docs/23 Decision 4): one registry-wide
	// goroutine, started explicitly here — never inside NewRegistry — so
	// tests drive PumpViewerCounts ticks directly.
	go r.RunViewerCountPump(runCtx)

	// R39 AP3 (docs/42 §4.5): the credential-gated admin API on the ops
	// listener. NewAdminAuth never blocks and never fails on an unreachable
	// IdP — discovery is retried in the background — so the relay starts
	// whether or not the identity provider is up. With neither credential
	// configured, Handler registers no admin route at all (404).
	adminAuth := ops.NewAdminAuth(runCtx, ops.AdminAuthOptions{
		Token:      cfg.AdminAPIToken,
		Issuer:     cfg.AdminOIDCIssuer,
		Audience:   cfg.AdminOIDCAudience,
		RolesClaim: cfg.AdminOIDCRolesClaim,
		Role:       cfg.AdminOIDCRole,
		Log:        log,
	})
	adminOpts := &ops.AdminOptions{
		Registry:        r,
		Config:          cfg,
		Pod:             os.Getenv("POD_NAME"),
		Version:         version,
		PublisherRemote: srv.PublisherRemote,
		Auth:            adminAuth,
		Log:             log,
	}

	errCh := make(chan error, 2)
	go func() { errCh <- srv.Run(runCtx) }()
	go func() {
		errCh <- ops.Run(runCtx, cfg.MetricsAddr, ops.Handler(r, roomStats, promReg, log, srv.Ready, adminOpts), log)
	}()

	var firstErr error
	for range 2 {
		if err := <-errCh; err != nil && firstErr == nil {
			firstErr = err
			cancel()
		}
	}
	if firstErr != nil {
		return firstErr
	}

	log.Info("shutting down")
	return nil
}

// logStartup emits the one line an operator reads to confirm what this pod is
// actually running. Extracted from run so it can be asserted in a test:
// docs/42 §9 AP2 requires the moderation source to be stated here, and the
// R2 lesson is that a knob nobody can see is a knob nobody notices is inert.
// wiredServer is the slice of *transport.Server that startup wiring touches.
// It is an interface for one reason: so the ORDER below can be asserted in a
// test. The window it guards opens and closes during process startup, which
// nothing else in the suite can observe.
type wiredServer interface {
	SetModeration(*moderation.Set)
	SetCluster(transport.ClusterCoordinator, string)
	SetRoomCluster(transport.RoomCluster, string)
	HandleBanAdded(moderation.Record)
}

// wireSubsystems installs the optional subsystems on srv in the one order
// that is safe, and returns once the ban source is running.
//
// moderationsrc.Start goes LAST, and that is the entire point of this
// function existing (PR #280 review). It launches the Ban informer, and a
// pod cold-starting in a namespace that already holds Ban CRs gets its first
// Add events within milliseconds. Those reach srv.HandleBanAdded ->
// terminate(), which reads this pod's edge manager and — through the hub's
// OnBroadcastExpired hook — the cluster coordinator. Starting the source
// before SetCluster made that a data race on plain field writes, and left a
// real hole behind it: a kill actuated in the window tears the broadcast down
// locally but never deletes its origin Lease, so every other pod in the fleet
// keeps routing viewers to an origin that is already dead.
//
// buildCoord is the seam: it builds AND publishes the coordinator (see run),
// so "cluster wiring is complete" is one step rather than two.
//
// buildRooms (R42 RM3) is the same seam for the cluster room store: nil
// unless -cluster-mode AND -rooms. It returns the store, the pod name, and
// a start function for its informer, which runs AFTER the ban source for
// the reason the ban source runs after SetCluster: the informer's first
// events call into the transport (adoption, lease loss) and the registry,
// and SetRoomCluster chains the drain hook onto SetCluster's, so both must
// be installed before anything can fire.
func wireSubsystems(
	ctx context.Context,
	cfg config.Config,
	srv wiredServer,
	bans *moderation.Set,
	log *slog.Logger,
	buildCoord func() (transport.ClusterCoordinator, string, error),
	buildRooms func() (transport.RoomCluster, string, func(), error),
) error {
	srv.SetModeration(bans)

	if cfg.ClusterMode {
		coord, podName, err := buildCoord()
		if err != nil {
			return err
		}
		srv.SetCluster(coord, podName)
	}

	var startRooms func()
	if buildRooms != nil {
		store, podName, start, err := buildRooms()
		if err != nil {
			return err
		}
		srv.SetRoomCluster(store, podName)
		startRooms = start
	}

	if err := moderationsrc.Start(ctx, moderationsrc.Options{
		Source: cfg.ModerationSource,
		Set:    bans,
		Log:    log,
		// R39 AP3 (docs/42 §4.3): the actuation half. Wired for EVERY source
		// and independently of -cluster-mode — a single-pod relay kills just
		// as well as a fleet, and each pod acts on its own event.
		OnBanAdded: srv.HandleBanAdded,
	}); err != nil {
		return err
	}
	if startRooms != nil {
		startRooms()
	}
	return nil
}

func logStartup(log *slog.Logger, cfg config.Config, version string) {
	log.Info("starting",
		"version", version,
		"addr", cfg.Addr,
		"dev_cert", cfg.DevCert,
		"max_subscribers", cfg.MaxSubscribers,
		"max_broadcasts", cfg.MaxBroadcasts,
		"max_total_subscribers", cfg.MaxTotalSubscribers,
		"publish_secret_set", cfg.PublishSecret != "",
		"conn_rate_limit", cfg.ConnRateLimit,
		"conn_burst_limit", cfg.ConnBurstLimit,
		"max_bandwidth_bytes", cfg.MaxBandwidthBytes,
		"max_keyframe_bytes", cfg.MaxKeyframeBytes,
		"keyframe_write_timeout", cfg.KeyframeWriteTimeout,
		"dvr_window", cfg.DVRWindow,
		"dvr_max_bytes", cfg.DVRMaxBytes,
		"dvr_max_catchup", cfg.DVRMaxCatchup,
		"dvr_audio", cfg.DVRAudio,
		"live_edge_audio_on_reliable_stream", cfg.LiveEdgeAudioOnReliableStream,
		"parity_default", cfg.ParityDefault,
		"striped_delivery", cfg.StripedDelivery,
		"max_idle_timeout", cfg.MaxIdleTimeout,
		"keepalive_period", cfg.KeepAlivePeriod,
		"broadcast_grace", cfg.BroadcastGrace,
		"metrics_addr", cfg.MetricsAddr,
		"stateless_reset_key_set", len(cfg.StatelessResetKey) > 0,
		"resume_token_key_mode", resumeTokenKeyMode(cfg),
		// R28: the key's presence is the feature switch, so logging whether it
		// is set is how an operator confirms a fleet is collecting at all —
		// the key itself is never logged.
		"telemetry_enabled", len(cfg.TelemetryKey) > 0,
		"telemetry_report_interval", cfg.TelemetryReportInterval,
		"telemetry_advertise_url", cfg.TelemetryAdvertiseURL,
		"server_name", cfg.ServerName,
		"cluster_mode", cfg.ClusterMode,
		// R39 (docs/42 §4.3): the operator's confirmation surface for which
		// ban source this pod is actually enforcing from, and which
		// credentials open the admin API. The token itself is never logged —
		// only whether one is set, which is what decides 404 vs. 401.
		"moderation_source", cfg.ModerationSource,
		"admin_api_token_set", cfg.AdminAPIToken != "",
		"admin_oidc_issuer", cfg.AdminOIDCIssuer,
		"admin_oidc_audience", cfg.AdminOIDCAudience,
		"admin_oidc_roles_claim", cfg.AdminOIDCRolesClaim,
		"admin_oidc_role", cfg.AdminOIDCRole,
		"admin_api_enabled", cfg.AdminAPIToken != "" || cfg.AdminOIDCIssuer != "",
		// R42 (docs/44 §4.10): the same confirmation surface for rooms. The
		// create secret itself is never logged, only whether one gates
		// minting.
		"rooms", cfg.Rooms,
		"room_empty_grace", cfg.RoomEmptyGrace,
		"max_rooms", cfg.MaxRooms,
		"max_room_broadcasts", cfg.MaxRoomBroadcasts,
		"max_room_participants", cfg.MaxRoomParticipants,
		"room_create_secret_set", cfg.RoomCreateSecret != "",
		"rooms_file", cfg.RoomsFile,
	)
}

// roomOptions maps the parsed config onto roomsrv.Options — the R42 twin of
// registryOptions, under the same rule: every -room-* knob crosses here, and
// TestRoomOptionsCarryAllKnobs asserts it. Wiring-only fields (Broadcasts,
// Obfuscate, Log, cluster seams) are set by run.
func roomOptions(cfg config.Config) roomsrv.Options {
	return roomsrv.Options{
		EmptyGrace:      cfg.RoomEmptyGrace,
		MaxRooms:        cfg.MaxRooms,
		MaxBroadcasts:   cfg.MaxRoomBroadcasts,
		MaxParticipants: cfg.MaxRoomParticipants,
		CreateSecret:    cfg.RoomCreateSecret,
	}
}

// wireRoomClusterSeams installs the registry's cluster seams (docs/44 §4.3,
// §4.5) on top of a store that may not exist yet: the CR create is the code
// reservation (and Unreserve gives it back on the local re-check race),
// the attach secret is read from the room's Secret per join, and the
// status writes follow the room's life. Before the store exists the gates
// fail closed and the notifications are no-ops. One function so a test can
// prove every seam is wired (CODE-REVIEW.md: the R2 F1 blind spot).
func wireRoomClusterSeams(ro *roomsrv.Options, store func() *roomcluster.Store) {
	ro.Reserve = func(ctx context.Context, room *rooms.Room) error {
		st := store()
		if st == nil {
			return roomsrv.ErrUnavailable
		}
		return st.Reserve(ctx, room)
	}
	ro.Unreserve = func(ctx context.Context, code string) {
		if st := store(); st != nil {
			st.Unreserve(ctx, code)
		}
	}
	ro.AttachSecret = func(code string) (string, bool, error) {
		st := store()
		if st == nil {
			return "", false, fmt.Errorf("%w: room store not installed", roomsrv.ErrUnavailable)
		}
		return st.AttachSecret(code)
	}
	ro.OnRoomEnded = func(code string, reason uint8) {
		if st := store(); st != nil {
			st.RoomEnded(code, reason)
		}
	}
	ro.OnRoomEmpty = func(code string, empty bool) {
		if st := store(); st != nil {
			st.RoomEmpty(code, empty)
		}
	}
	ro.OnAttachmentsChanged = func(code string, list []rooms.Attachment) {
		if st := store(); st != nil {
			st.AttachmentsChanged(code, list)
		}
	}
}

// chainHook runs both hooks (either may be nil).
func chainHook(a, b func(string)) func(string) {
	if a == nil {
		return b
	}
	return func(id string) {
		a(id)
		b(id)
	}
}

// hubBroadcasts adapts the hub registry to roomsrv.BroadcastSource.
type hubBroadcasts struct{ r *hub.Registry }

func (h hubBroadcasts) BroadcastState(id string) (roomsrv.BroadcastState, bool) {
	live, viewers, known := h.r.BroadcastState(id)
	return roomsrv.BroadcastState{Live: live, Viewers: viewers}, known
}

// installRooms puts the room registry on the transport and returns the
// rooms stats source for /statusz and the metrics collector — the
// transport's "proxy" rows merged over the registry's "home" rows (docs/44
// §4.10). Nil with -rooms off, so neither the /statusz section nor the room
// series exist. One function rather than two calls in run because the
// ORDER is the contract: the source does not exist before the install.
func installRooms(srv roomsServer, reg *roomsrv.Registry) metrics.RoomStatsSource {
	if reg == nil {
		return nil
	}
	srv.SetRooms(reg)
	return srv.RoomStatsSource()
}

// roomsServer is the transport slice installRooms needs.
type roomsServer interface {
	SetRooms(*roomsrv.Registry)
	RoomStatsSource() metrics.RoomStatsSource
}

// registryOptions maps the parsed config onto hub.Options. Every limit knob
// in config.Config must cross here — a knob that parses but isn't mapped is
// silently inert in production while wired-by-hand tests stay green.
func registryOptions(cfg config.Config) hub.Options {
	return hub.Options{
		MaxSubscribers:                cfg.MaxSubscribers,
		BroadcastGrace:                cfg.BroadcastGrace,
		MaxBroadcasts:                 cfg.MaxBroadcasts,
		MaxTotalSubscribers:           cfg.MaxTotalSubscribers,
		MaxBandwidthBytes:             cfg.MaxBandwidthBytes,
		MaxKeyframeBytes:              cfg.MaxKeyframeBytes,
		KeyframeWriteTimeout:          cfg.KeyframeWriteTimeout,
		DVR:                           hub.DVROptions{Window: cfg.DVRWindow, MaxBytes: cfg.DVRMaxBytes},
		DVRMaxCatchup:                 cfg.DVRMaxCatchup,
		DVRAudio:                      cfg.DVRAudio,
		LiveEdgeAudioOnReliableStream: cfg.LiveEdgeAudioOnReliableStream,
		// R29: plumbed here, not only into the test helper — the R2
		// post-implementation review's finding, and docs/34's FP4 acceptance
		// criterion asserts this production path specifically.
		ParityDefault: cfg.ParityDefault,
		// R30: same rule (docs/35 ST3). The transport reads cfg directly for
		// the dial gate and capability bit; this mirror keeps the hub's
		// options honest and the carry-all-limits test complete.
		StripedDelivery: cfg.StripedDelivery,
		StatsKey:        cfg.StatsKey,
	}
}

// buildCoordinator constructs the cluster coordinator from the in-cluster
// Kubernetes config and the downward-API pod identity (R17 W3). Only called
// when -cluster-mode is on: single-pod deployments never touch the k8s API.
// onLeaseDeleted is the cluster-wide "broadcast ended" dispatch (edge stop +
// local hub expiry); onLeaseLost is the W5 demote path (stale publisher
// close, 4003 to edges, self-demote to edge).
func buildCoordinator(cfg config.Config, onLeaseDeleted func(string), onLeaseLost func(string, cluster.Origin), log *slog.Logger) (*cluster.Coordinator, string, error) {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, "", fmt.Errorf("cluster-mode requires in-cluster kubernetes config: %w", err)
	}
	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, "", err
	}
	podName := os.Getenv("POD_NAME")
	podIP := os.Getenv("POD_IP")
	namespace := os.Getenv("POD_NAMESPACE")
	if podName == "" || podIP == "" || namespace == "" {
		return nil, "", fmt.Errorf("cluster-mode requires POD_NAME, POD_IP and POD_NAMESPACE (downward API)")
	}
	_, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return nil, "", fmt.Errorf("cluster-mode: cannot derive advertise port from -addr %q: %w", cfg.Addr, err)
	}
	coord, err := cluster.New(cluster.Options{
		Client:         client,
		Namespace:      namespace,
		PodName:        podName,
		AdvertiseAddr:  net.JoinHostPort(podIP, port),
		BroadcastGrace: cfg.BroadcastGrace,
		MaxBroadcasts:  cfg.MaxBroadcasts,
		Log:            log,
		OnLeaseDeleted: onLeaseDeleted,
		OnLeaseLost:    onLeaseLost,
	})
	if err != nil {
		return nil, "", err
	}
	return coord, podName, nil
}

// buildRoomStore constructs the cluster room store (R42 RM3, docs/44 §4.5)
// from the in-cluster config and the downward-API pod identity, exactly as
// buildCoordinator does for the origin registry. Only called with both
// -cluster-mode and -rooms on: a single-pod relay with rooms never touches
// the k8s API (docs/44 §4.3, "non-cluster mode"). The Room CRD and the
// `rooms`/`rooms/status`/`secrets` RBAC ride the chart's rooms.enabled.
func buildRoomStore(cfg config.Config, reg *roomsrv.Registry, obfuscate func(string) string, onLeaseLost func(string), log *slog.Logger) (*roomcluster.Store, string, error) {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, "", fmt.Errorf("rooms in cluster-mode require in-cluster kubernetes config: %w", err)
	}
	client, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, "", err
	}
	podName := os.Getenv("POD_NAME")
	podIP := os.Getenv("POD_IP")
	namespace := os.Getenv("POD_NAMESPACE")
	if podName == "" || podIP == "" || namespace == "" {
		return nil, "", fmt.Errorf("rooms in cluster-mode require POD_NAME, POD_IP and POD_NAMESPACE (downward API)")
	}
	_, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return nil, "", fmt.Errorf("cluster-mode: cannot derive advertise port from -addr %q: %w", cfg.Addr, err)
	}
	store, err := roomcluster.New(roomcluster.Options{
		Client:        client,
		Namespace:     namespace,
		PodName:       podName,
		AdvertiseAddr: net.JoinHostPort(podIP, port),
		MaxRooms:      cfg.MaxRooms,
		EmptyGrace:    cfg.RoomEmptyGrace,
		Registry:      reg,
		Obfuscate:     obfuscate,
		Log:           log,
		OnLeaseLost:   onLeaseLost,
	})
	if err != nil {
		return nil, "", err
	}
	return store, podName, nil
}

// resumeTokenKeyMode names where the resume-token key comes from (R17 W2) —
// logged so a fleet misconfiguration (per-process keys on multiple pods,
// which silently breaks cross-pod resume) is visible at startup.
//
// One definition, in config, since R39: GET /internal/admin/config reports
// the same mode (docs/42 §4.5 — "the resume key also says which mode,
// echoing the startup log"), and two copies of a three-way switch is exactly
// the drift CODE-REVIEW.md's shared-constants rule exists to stop.
func resumeTokenKeyMode(cfg config.Config) string { return cfg.ResumeTokenKeyMode() }

// certSource returns the per-handshake certificate callback: an ephemeral
// in-memory dev cert (hashes logged for the browser side), a *persisted* dev
// cert generated once into -cert-file/-key-file, or a reloading file-backed
// pair for production. The full truth table is docs/41 §4.2.1.
func certSource(cfg config.Config, log *slog.Logger) (func(*tls.ClientHelloInfo) (*tls.Certificate, error), error) {
	switch {
	// R38 (docs/41 D3): -dev-cert AND -cert-file means "generate into these
	// paths if absent, otherwise load them". Persisting the pair is what stops
	// every restart invalidating the hash a browser was given, and it moves
	// local dev onto the file-backed path production actually uses.
	case cfg.DevCert && cfg.CertFile != "":
		if cfg.KeyFile == "" {
			return nil, fmt.Errorf("-dev-cert with -cert-file also needs -key-file")
		}
		cert, generated, err := tlsutil.LoadOrGenerate(cfg.CertFile, cfg.KeyFile,
			strings.Split(cfg.DevCertHosts, ","), tlsutil.MaxDevCertValidity)
		if err != nil {
			return nil, err
		}
		// Not `docker compose down -v`: the stack bind-mounts ./certs, so the
		// pair outlives the volumes. One command covers all three lanes.
		logCertIdentity(log, cert.Leaf, "persisted dev certificate",
			"./dev/certs.sh renew (or delete the pair) to regenerate",
			"cert_file", cfg.CertFile, "generated", generated)
		if generated {
			log.Info("chrome flags for this cert",
				"flags", fmt.Sprintf("--origin-to-force-quic-on=localhost%s --ignore-certificate-errors-spki-list=%s",
					cfg.Addr, tlsutil.SPKIFingerprint(cert.Leaf)),
			)
		}
		// The reloader, not the pair just loaded: a developer who replaces the
		// files (./dev/certs.sh renew) gets the new ones without a restart,
		// exactly as production does.
		r, err := tlsutil.NewReloader(cfg.CertFile, cfg.KeyFile, log)
		if err != nil {
			return nil, err
		}
		return r.GetCertificate, nil
	case cfg.DevCert:
		cert, err := tlsutil.GenerateDevCert(strings.Split(cfg.DevCertHosts, ","), tlsutil.MaxDevCertValidity)
		if err != nil {
			return nil, err
		}
		log.Info("generated ephemeral dev certificate",
			"hosts", cfg.DevCertHosts,
			"not_after", cert.Leaf.NotAfter,
			"spki_fingerprint", tlsutil.SPKIFingerprint(cert.Leaf),
			"cert_hash_hex", tlsutil.CertHashHex(cert.Leaf),
		)
		log.Info("chrome flags for this cert",
			"flags", fmt.Sprintf("--origin-to-force-quic-on=localhost%s --ignore-certificate-errors-spki-list=%s",
				cfg.Addr, tlsutil.SPKIFingerprint(cert.Leaf)),
		)
		return func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil }, nil
	case cfg.CertFile != "":
		r, err := tlsutil.NewReloader(cfg.CertFile, cfg.KeyFile, log)
		if err != nil {
			return nil, err
		}
		// R38: the file-backed arm used to log nothing identifying, which left
		// a developer running against a persisted dev cert with no way to
		// obtain its hash — the ephemeral arm's log was the only source.
		if cert, err := r.GetCertificate(nil); err == nil && cert != nil && cert.Leaf != nil {
			// This arm is both the dev stack's ACME lane and every real
			// deployment, so the remedy has to name both. It used to name only
			// `./dev/certs.sh renew` — a command that does not exist inside the
			// image and means nothing for a mounted Secret, which is exactly
			// the operator this log line reaches when cert-manager has stopped
			// renewing.
			logCertIdentity(log, cert.Leaf, "loaded certificate",
				"locally: ./dev/certs.sh renew — in a deployment the CA/cert-manager renews the mounted Secret (docs/self-hosting.md)",
				"cert_file", cfg.CertFile)
		}
		return r.GetCertificate, nil
	default:
		return nil, fmt.Errorf("no certificate configured: pass -dev-cert or -cert-file/-key-file")
	}
}

// certExpiryWarning is how close to NotAfter a certificate has to be before
// the relay says so at startup. 72 h is comfortably longer than a working day
// and comfortably shorter than the 14-day dev-cert life, so it fires while
// there is still time to act rather than on the morning it breaks.
const certExpiryWarning = 72 * time.Hour

// logCertIdentity logs what the browser side of a local stack needs (the hex
// DER hash) plus the two values that make an expiry failure legible, and
// warns when the certificate is nearly out of time. remedy names the command
// that replaces it, which differs per lane (docs/41 §4.2.1).
func logCertIdentity(log *slog.Logger, leaf *x509.Certificate, msg, remedy string, extra ...any) {
	if leaf == nil {
		return
	}
	args := append([]any{
		"not_after", leaf.NotAfter,
		"cert_hash_hex", tlsutil.CertHashHex(leaf),
		"spki_fingerprint", tlsutil.SPKIFingerprint(leaf),
	}, extra...)
	log.Info(msg, args...)

	// Already dead is not "expires soon". A negative `remaining` in a WARN was
	// the only signal a stack whose certificate had run out ever produced, and
	// it reads like a rounding artefact rather than the reason the browser is
	// refusing to connect.
	remaining := time.Until(leaf.NotAfter)
	switch {
	case remaining <= 0:
		log.Error("certificate has EXPIRED — browsers will refuse to connect",
			"not_after", leaf.NotAfter,
			"expired_ago", (-remaining).Round(time.Minute),
			"remedy", remedy,
		)
	case remaining < certExpiryWarning:
		log.Warn("certificate expires soon",
			"not_after", leaf.NotAfter,
			"remaining", remaining.Round(time.Minute),
			"remedy", remedy,
		)
	}
}
