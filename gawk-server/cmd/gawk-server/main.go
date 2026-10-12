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
	"github.com/prometheus/client_golang/prometheus"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/eventbus"
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

// version is stamped at build time via -ldflags "-X main.version=..." (see
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

	// Late-bound subsystems. The hub's hooks are built before these exist and
	// close over the variables; every hook path is nil-safe until assignment
	// (no publisher can connect before Run anyway).
	//
	// coord: the cluster coordinator and the hub reference each other.
	var coord *cluster.Coordinator
	// roomReg: assigned right after the hub exists.
	var roomReg *roomsrv.Registry
	// roomStore: the cluster room store (Room CRs + home leases), assigned by
	// the wiring step in cluster mode and nil otherwise.
	var roomStore *roomcluster.Store
	// bus: built after the metrics registry; a nil publisher's Publish is a
	// no-op.
	var bus *eventbus.Publisher
	busHook := func(ev eventbus.Event) { bus.Publish(ev) }
	hubOpts := registryOptions(cfg)
	hubOpts.OnEvent = busHook
	// sm: an origin broadcast's lifetime lands in the usage histograms; a nil
	// *ServerMetrics records nothing.
	var sm *metrics.ServerMetrics
	hubOpts.OnOriginEnded = func(e hub.BroadcastEnd) { sm.BroadcastEnded(e) }
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
		// The origin stamps its Lease at stall onset and clears it on
		// recovery, so a room homed on another pod reads the tile away from
		// Lookup.
		hubOpts.OnPublisherStalled = func(id string, stalled bool) {
			if coord == nil {
				return
			}
			opCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := coord.SetStalled(opCtx, id, stalled); err != nil {
				log.Warn("lease stall stamp failed", "broadcast_id", id, "stalled", stalled, "err", err)
			}
		}
	}
	if cfg.Rooms {
		// Rooms need both lifecycle hooks in single-pod mode too (an
		// attachment flips to "away" and is removed on expiry), so they chain
		// onto whatever cluster mode installed.
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
		// code fleet-wide.
		hubOpts.IDReserved = func(id string) bool {
			return (roomReg != nil && roomReg.Has(id)) || (roomStore != nil && roomStore.Known(id))
		}
	}

	r := hub.NewRegistry(log, hubOpts)

	// Prometheus wiring (docs/13), served by the TCP ops endpoint.
	promReg := metrics.NewBaseRegistry(version)
	promReg.MustRegister(metrics.NewRegistryCollector(r))
	sm = metrics.NewServerMetrics(promReg)
	promReg.MustRegister(metrics.NewLimitsCollector(limits(cfg)))

	// The ban set is always constructed and always scraped — with
	// -moderation-source=off nothing feeds it and gawk_moderation_bans_active
	// reads zero, which is how an operator tells "no bans" from "no
	// moderation".
	bans := moderation.NewSet()
	promReg.MustRegister(metrics.NewModerationCollector(bans))

	// Event bus (docs/51): constructed before anything can produce an event
	// and closed last; with -eventbus-url unset New returns a nil publisher
	// and every hook is a no-op.
	busMetrics := eventBusMetrics(cfg, promReg)
	bus, err = eventbus.New(eventbus.Options{
		URL:            cfg.EventBusURL,
		CredsFile:      cfg.EventBusCredsFile,
		TLSCertFile:    cfg.EventBusTLSCert,
		TLSKeyFile:     cfg.EventBusTLSKey,
		CAFile:         cfg.EventBusCAFile,
		SubjectPrefix:  cfg.EventBusSubjectPrefix,
		Pod:            podIdentity(),
		ViewerInterval: cfg.EventBusViewerInterval,
		Insecure:       cfg.EventBusInsecure,
		Logger:         log,
		Metrics:        busMetrics,
	})
	if err != nil {
		return fmt.Errorf("event bus: %w", err)
	}
	defer bus.Close()

	// The WebTransport (UDP) server and the ops (TCP) listener run together;
	// either one failing tears the other down.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	srv := transport.New(cfg, r, getCert, log, sm)
	if cfg.Rooms {
		ro := roomOptions(cfg)
		// The registry's view of a broadcast is the transport's: the local
		// hub first and, in cluster mode, the origin lease through the
		// coordinator SetCluster installs below — read late-bound, so the
		// registry can exist before it.
		ro.Broadcasts = srv.RoomBroadcasts()
		ro.Obfuscate = r.ObfuscateID
		ro.OnEvent = busHook
		ro.PodName = os.Getenv("POD_NAME")
		ro.Log = log
		if cfg.ClusterMode {
			wireRoomClusterSeams(&ro, func() *roomcluster.Store { return roomStore })
		}
		roomReg = roomsrv.NewRegistry(ro)
	}
	// Publishing `coord` happens inside the ordered wiring step, not at the
	// call site: wireSubsystems only guarantees the ORDER of what it calls.
	buildCoord := func() (transport.ClusterCoordinator, string, error) {
		c, podName, cerr := buildCoordinator(cfg, srv.HandleLeaseDeleted, srv.HandleLeaseLost, log)
		if cerr != nil {
			return nil, "", cerr
		}
		coord = c
		go coord.Run(runCtx)
		return c, podName, nil
	}
	// The room store is published here for the same reason coord is.
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
	// transport's token key), before Run. installRooms pins the stats-source
	// read after the install (docs/gotchas.md).
	roomStats := installRooms(srv, roomReg)
	if roomStats != nil {
		promReg.MustRegister(metrics.NewRoomCollector(roomStats))
	}
	if roomReg != nil {
		// The refresh poll turns "no lease" into an expiry in cluster mode
		// (UnknownIsExpired), so it must not run against a lease cache that
		// has not synced yet — an empty cache is not "no leases", and would
		// remove an adopted room's attachments.
		go func() {
			if coord != nil && !coord.WaitLeaseSync(runCtx) {
				return
			}
			roomReg.RunRefresh(runCtx)
		}()
		// The static-room file source starts last, like the ban source.
		if cfg.RoomsFile != "" {
			if err := roomsrc.StartFile(runCtx, cfg.RoomsFile, roomsrc.Options{Registry: roomReg, Log: log}); err != nil {
				return err
			}
		}
	}

	// The viewer-count pump: one registry-wide goroutine, started explicitly
	// here — never inside NewRegistry — so tests drive PumpViewerCounts ticks
	// directly.
	go r.RunViewerCountPump(runCtx)

	// The credential-gated admin API on the ops listener (docs/42 §4.5).
	// NewAdminAuth never blocks or fails on an unreachable IdP — discovery is
	// retried in the background — so the relay starts whether or not the IdP
	// is up. With neither credential configured, no admin route exists (404).
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
	if roomReg != nil {
		adminOpts.Rooms = roomReg.AdminRooms
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

// wiredServer is the slice of *transport.Server that startup wiring touches.
// It is an interface so the wiring ORDER can be asserted in a test; nothing
// else in the suite can observe the startup window it guards.
type wiredServer interface {
	SetModeration(*moderation.Set)
	SetCluster(transport.ClusterCoordinator, string)
	SetRoomCluster(transport.RoomCluster, string)
	HandleBanAdded(moderation.Record)
}

// wireSubsystems installs the optional subsystems on srv in the one order
// that is safe, and returns once the ban source is running.
//
// moderationsrc.Start goes LAST. A pod cold-starting in a namespace that
// already holds Ban CRs gets Add events within milliseconds; they reach
// srv.HandleBanAdded -> terminate(), which reads the edge manager and, via
// OnBroadcastExpired, the coordinator. Started before SetCluster, that is a
// data race, and a kill in the window tears the broadcast down locally but
// never deletes its origin Lease — the rest of the fleet keeps routing
// viewers to a dead origin.
//
// buildCoord builds AND publishes the coordinator (see run), so "cluster
// wiring is complete" is one step. buildRooms is the same seam for the
// cluster room store (nil unless -cluster-mode AND -rooms); its informer
// starts after the ban source for the same reason: its first events call
// into the transport and registry, and SetRoomCluster chains the drain hook
// onto SetCluster's, so both must be installed before anything can fire.
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
		// Wired for EVERY source and independently of -cluster-mode: each
		// pod acts on its own event.
		OnBanAdded: srv.HandleBanAdded,
	}); err != nil {
		return err
	}
	if startRooms != nil {
		startRooms()
	}
	return nil
}

// logStartup emits the one line an operator reads to confirm what this pod is
// actually running; a knob nobody can see is a knob nobody notices is inert.
// Secrets are logged only as set/unset. Separate from run so a test can assert
// it.
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
		"publisher_stall_timeout", cfg.PublisherStallTimeout,
		"publisher_stall_ends", cfg.PublisherStallEnds,
		"metrics_addr", cfg.MetricsAddr,
		"stateless_reset_key_set", len(cfg.StatelessResetKey) > 0,
		"resume_token_key_mode", resumeTokenKeyMode(cfg),
		"telemetry_enabled", len(cfg.TelemetryKey) > 0,
		"telemetry_report_interval", cfg.TelemetryReportInterval,
		"telemetry_advertise_url", cfg.TelemetryAdvertiseURL,
		"server_name", cfg.ServerName,
		"cluster_mode", cfg.ClusterMode,
		// Whether an admin credential is set decides 404 vs. 401.
		"moderation_source", cfg.ModerationSource,
		"admin_api_token_set", cfg.AdminAPIToken != "",
		"admin_oidc_issuer", cfg.AdminOIDCIssuer,
		"admin_oidc_audience", cfg.AdminOIDCAudience,
		"admin_oidc_roles_claim", cfg.AdminOIDCRolesClaim,
		"admin_oidc_role", cfg.AdminOIDCRole,
		"admin_api_enabled", cfg.AdminAPIToken != "" || cfg.AdminOIDCIssuer != "",
		"rooms", cfg.Rooms,
		"room_empty_grace", cfg.RoomEmptyGrace,
		"max_rooms", cfg.MaxRooms,
		"max_room_broadcasts", cfg.MaxRoomBroadcasts,
		"max_room_participants", cfg.MaxRoomParticipants,
		"room_create_secret_set", cfg.RoomCreateSecret != "",
		"rooms_file", cfg.RoomsFile,
	)
}

// roomOptions maps the parsed config onto roomsrv.Options — registryOptions'
// twin, under the same rule: every -room-* knob crosses here
// (TestRoomOptionsCarryAllKnobs). Wiring-only fields are set by run.
func roomOptions(cfg config.Config) roomsrv.Options {
	return roomsrv.Options{
		EmptyGrace:      cfg.RoomEmptyGrace,
		MaxRooms:        cfg.MaxRooms,
		MaxBroadcasts:   cfg.MaxRoomBroadcasts,
		MaxParticipants: cfg.MaxRoomParticipants,
		CreateSecret:    cfg.RoomCreateSecret,
	}
}

// wireRoomClusterSeams installs the registry's cluster seams (docs/44 §4.5)
// on top of a store that may not exist yet: the CR create is the code
// reservation (Unreserve gives it back on the local re-check race), the
// attach secret is read from the room's Secret per join, and the status
// writes follow the room's life. Before the store exists the gates fail
// closed and the notifications are no-ops. One function so a test can prove
// every seam is wired.
//
// It also sets UnknownIsExpired: with the broadcast source fleet-wide,
// "unknown" means no hub here and no origin lease anywhere, and the poll must
// remove the attachment because the hub's expiry hook fires on the origin
// pod, not on the room's home.
func wireRoomClusterSeams(ro *roomsrv.Options, store func() *roomcluster.Store) {
	ro.UnknownIsExpired = true
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

// limits is the configured caps as gawk_limit series (docs/61). The names
// follow the flags; 0 = unlimited, as there.
func limits(cfg config.Config) metrics.Limits {
	return metrics.Limits{
		"max_broadcasts":        float64(cfg.MaxBroadcasts),
		"max_subscribers":       float64(cfg.MaxSubscribers),
		"max_total_subscribers": float64(cfg.MaxTotalSubscribers),
		"max_bandwidth_bytes":   float64(cfg.MaxBandwidthBytes),
		"dvr_max_bytes":         float64(cfg.DVRMaxBytes),
		"max_rooms":             float64(cfg.MaxRooms),
		"max_room_broadcasts":   float64(cfg.MaxRoomBroadcasts),
		"max_room_participants": float64(cfg.MaxRoomParticipants),
		"conn_rate_limit":       cfg.ConnRateLimit,
		"conn_burst_limit":      float64(cfg.ConnBurstLimit),
	}
}

// installRooms puts the room registry on the transport and returns the rooms
// stats source for /statusz and the metrics collector (the transport's
// "proxy" rows merged over the registry's "home" rows). Nil with -rooms off.
// One function because the ORDER is the contract: the source is nil until
// the registry is installed.
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

// registryOptions maps the parsed config onto hub.Options. Every knob must be
// plumbed through here (flag + GAWK_* env + Helm value) — a knob that parses
// but isn't mapped is silently inert in production while wired-by-hand tests
// stay green.
func registryOptions(cfg config.Config) hub.Options {
	return hub.Options{
		MaxSubscribers:                cfg.MaxSubscribers,
		BroadcastGrace:                cfg.BroadcastGrace,
		PublisherStallTimeout:         cfg.PublisherStallTimeout,
		PublisherStallEnds:            cfg.PublisherStallEnds,
		MaxBroadcasts:                 cfg.MaxBroadcasts,
		MaxTotalSubscribers:           cfg.MaxTotalSubscribers,
		MaxBandwidthBytes:             cfg.MaxBandwidthBytes,
		MaxKeyframeBytes:              cfg.MaxKeyframeBytes,
		KeyframeWriteTimeout:          cfg.KeyframeWriteTimeout,
		DVR:                           hub.DVROptions{Window: cfg.DVRWindow, MaxBytes: cfg.DVRMaxBytes},
		DVRMaxCatchup:                 cfg.DVRMaxCatchup,
		DVRAudio:                      cfg.DVRAudio,
		LiveEdgeAudioOnReliableStream: cfg.LiveEdgeAudioOnReliableStream,
		// Also the ceiling on any subscriber's parity level.
		ParityDefault: cfg.ParityDefault,
		// The transport reads cfg directly for the dial gate and capability
		// bit; this mirror keeps the hub's options honest and the
		// carry-all-limits test complete.
		StripedDelivery: cfg.StripedDelivery,
		StatsKey:        cfg.StatsKey,
	}
}

// buildCoordinator constructs the cluster coordinator from the in-cluster
// Kubernetes config and the downward-API pod identity. Only called when
// -cluster-mode is on: single-pod deployments never touch the k8s API.
// onLeaseDeleted is the cluster-wide "broadcast ended" dispatch (edge stop +
// local hub expiry); onLeaseLost is the demote path (stale publisher close,
// 4003 to edges, self-demote to edge).
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

// buildRoomStore constructs the cluster room store (docs/44 §4.5) the way
// buildCoordinator builds the coordinator. Only called with both
// -cluster-mode and -rooms on: a single-pod relay with rooms never touches
// the k8s API. The Room CRD and the `rooms`/`rooms/status`/`secrets` RBAC ride
// the chart's rooms.enabled.
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

// resumeTokenKeyMode delegates to config.Config.ResumeTokenKeyMode, the one
// definition shared with GET /internal/admin/config.
func resumeTokenKeyMode(cfg config.Config) string { return cfg.ResumeTokenKeyMode() }

// certSource returns the per-handshake certificate callback: an ephemeral
// in-memory dev cert (hashes logged for the browser side), a *persisted* dev
// cert generated once into -cert-file/-key-file, or a reloading file-backed
// pair for production. The full truth table is docs/41 §4.2.1.
func certSource(cfg config.Config, log *slog.Logger) (func(*tls.ClientHelloInfo) (*tls.Certificate, error), error) {
	switch {
	// -dev-cert AND -cert-file means "generate into these paths if absent,
	// otherwise load them". Persisting the pair stops every restart
	// invalidating the hash a browser was given, and puts local dev on the
	// file-backed path production uses.
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
		// Logged so a developer on a persisted dev cert can obtain its hash.
		if cert, err := r.GetCertificate(nil); err == nil && cert != nil && cert.Leaf != nil {
			// This arm is both the dev stack's ACME lane and every real
			// deployment, so the remedy has to name both: ./dev/certs.sh does
			// not exist inside the image and means nothing for a mounted
			// Secret.
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

	// Already dead is not "expires soon": a negative `remaining` in a WARN
	// reads like a rounding artefact, not the reason browsers refuse.
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

// podIdentity names this process in every event's source and id (docs/51).
// POD_NAME in a cluster; the hostname otherwise, so a single-node deployment's
// events are still attributable and its ids still unique per producer.
func podIdentity() string {
	if name := os.Getenv("POD_NAME"); name != "" {
		return name
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return "gawk-server"
}

// eventBusMetrics registers the bus counters, and only when there is a bus: a
// deployment without one exports no gawk_eventbus_* series, since an
// always-zero counter would claim a subsystem that is not there.
func eventBusMetrics(cfg config.Config, reg prometheus.Registerer) *metrics.EventBusMetrics {
	if cfg.EventBusURL == "" {
		return nil
	}
	return metrics.NewEventBusMetrics(reg)
}
