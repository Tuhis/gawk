// Package serve runs a relay: the hub, the WebTransport server, the rooms,
// the cluster and moderation wiring and the ops listener. It is the body
// gawk-server and gawk-relay share (R74 PP2, docs/76 §6.1 item 1): the two
// binaries differ in how they pick a certificate and in their defaults,
// never in the mechanisms below. Certificate selection stays in each
// binary, so gawk-server's "no certificate configured" refusal (docs/41
// §4.2.1) cannot be weakened from here.
package serve

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/eventbus"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/ops"
	"github.com/Tuhis/gawk/gawk-server/internal/roomcluster"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrc"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
	"github.com/Tuhis/gawk/gawk-server/internal/transport"
	"github.com/Tuhis/gawk/gawk-server/moderation"
)

// CertFunc is the per-handshake certificate callback.
type CertFunc = func(*tls.ClientHelloInfo) (*tls.Certificate, error)

// Wired is what Options.Extra and Options.PreRun see: every subsystem Run
// built, after the ordered wiring step (so Coord is set in cluster mode).
type Wired struct {
	Registry *hub.Registry
	Server   *transport.Server
	Rooms    *roomsrv.Registry    // nil with -rooms off
	Coord    *cluster.Coordinator // nil without -cluster-mode
	Bus      *eventbus.Publisher  // nil without -eventbus-url (a nil publisher is a no-op)
	Prom     *prometheus.Registry
	Bans     *moderation.Set
	Cfg      config.Config
}

// Options is what differs between the binaries.
type Options struct {
	// Version is the release version (main.version).
	Version string
	// Identity names this process when POD_NAME and the hostname are both
	// unavailable ("gawk-server", "gawk-relay").
	Identity string
	// Extra goroutines run beside the relay and the ops listener under the
	// same context; the first error cancels everything. Return nil on
	// cancellation.
	Extra []func(ctx context.Context, w Wired) error
	// PreRun hooks run once everything is wired and before the relay
	// accepts a session (e.g. installing a local-relay info provider).
	PreRun []func(w Wired) error
}

// NewLogger builds the process logger from -log-level and -log-format.
func NewLogger(cfg config.Config, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// LogStartup emits the one line an operator reads to confirm what this pod
// is actually running; a knob nobody can see is a knob nobody notices is
// inert. It logs the same redacted view GET /internal/admin/config serves
// (docs/42 §4.5) rather than a second, hand-kept list: Sanitized is the one
// place that enumerates every field, and its completeness and sentinel tests
// keep a new knob visible here and every secret out of it.
func LogStartup(log *slog.Logger, cfg config.Config, version string, extra ...any) {
	log.Info("starting", append([]any{"version", version, "config", cfg.Sanitized()}, extra...)...)
}

// Run builds and runs the relay until ctx ends or a component fails.
func Run(ctx context.Context, cfg config.Config, getCert CertFunc, log *slog.Logger, o Options) error {
	if o.Identity == "" {
		o.Identity = "gawk-server"
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
	promReg := metrics.NewBaseRegistry(o.Version)
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
	var err error
	bus, err = eventbus.New(eventbus.Options{
		URL:            cfg.EventBusURL,
		CredsFile:      cfg.EventBusCredsFile,
		TLSCertFile:    cfg.EventBusTLSCert,
		TLSKeyFile:     cfg.EventBusTLSKey,
		CAFile:         cfg.EventBusCAFile,
		SubjectPrefix:  cfg.EventBusSubjectPrefix,
		Pod:            podIdentity(o.Identity),
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
	w := Wired{
		Registry: r,
		Server:   srv,
		Rooms:    roomReg,
		Coord:    coord,
		Bus:      bus,
		Prom:     promReg,
		Bans:     bans,
		Cfg:      cfg,
	}
	// R74 (docs/76): the home side of PoP relays, after the cluster and ban
	// wiring it builds on.
	popMgr, err := wirePops(runCtx, w, log)
	if err != nil {
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
		Version:         o.Version,
		PublisherRemote: srv.PublisherRemote,
		Auth:            adminAuth,
		Log:             log,
	}
	if roomReg != nil {
		adminOpts.Rooms = roomReg.AdminRooms
	}
	if popMgr != nil {
		adminOpts.Pops = popMgr.AdminPops
	}
	// R40's keyframe route (docs/71 D1), under its own token and nothing
	// else; an empty -screening-token leaves it unregistered (404).
	screeningOpts := screeningOptions(cfg, r, log)

	for _, pre := range o.PreRun {
		if err := pre(w); err != nil {
			return err
		}
	}

	n := 2 + len(o.Extra)
	errCh := make(chan error, n)
	go func() { errCh <- srv.Run(runCtx) }()
	go func() {
		errCh <- ops.Run(runCtx, cfg.MetricsAddr, ops.Handler(r, roomStats, promReg, log, srv.Ready, adminOpts, screeningOpts), log)
	}()
	for _, extra := range o.Extra {
		go func() { errCh <- extra(runCtx, w) }()
	}

	var firstErr error
	for range n {
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

// certExpiryWarning is how close to NotAfter a certificate has to be before
// the relay says so at startup. 72 h is comfortably longer than a working day
// and comfortably shorter than the 14-day dev-cert life, so it fires while
// there is still time to act rather than on the morning it breaks.
const certExpiryWarning = 72 * time.Hour

// LogCertIdentity logs what the browser side of a local stack needs (the hex
// DER hash) plus the two values that make an expiry failure legible, and
// warns when the certificate is nearly out of time. remedy names the command
// that replaces it, which differs per lane (docs/41 §4.2.1).
func LogCertIdentity(log *slog.Logger, leaf *x509.Certificate, msg, remedy string, extra ...any) {
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
// events are still attributable and its ids still unique per producer;
// fallback when neither is available.
func podIdentity(fallback string) string {
	if name := os.Getenv("POD_NAME"); name != "" {
		return name
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return fallback
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
