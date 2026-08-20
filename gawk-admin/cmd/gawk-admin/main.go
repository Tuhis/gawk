// Command gawk-admin is R39's moderation portal and enforcement control plane
// (docs/42).
//
// It is the fourth top-level module and the first internet-exposable admin
// surface in the stack. Two properties of its shape are load-bearing and are
// the reason this file looks the way it does:
//
//   - **The data plane never calls the admin plane at runtime** (D2). This
//     process writes `Ban` custom resources; relay pods watch them. So a
//     gawk-admin outage cannot lift an existing ban, and a relay cold-starting
//     while this process is down still gets its ban set from the API server.
//     Nothing here is on any broadcast's hot path.
//   - **The API is stateless, so replicas are cheap** (D16/D17). Every request
//     carries its own IdP-issued JWT; there is no session table and no cookie.
//     What must NOT run on every replica is the singleton background work —
//     the reconciler/janitor and the webhook dispatcher — so both are started
//     only from the leader-election callback.
//
// The `migrate` subcommand shares this binary and this image on purpose
// (§4.15): the schema a release needs travels with the release, and the
// serving path below never runs DDL — it only refuses to serve a schema older
// than it understands.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/api"
	"github.com/Tuhis/gawk/gawk-admin/internal/auth"
	"github.com/Tuhis/gawk/gawk-admin/internal/config"
	"github.com/Tuhis/gawk/gawk-admin/internal/kube"
	"github.com/Tuhis/gawk/gawk-admin/internal/portal"
	"github.com/Tuhis/gawk/gawk-admin/internal/relayscan"
	"github.com/Tuhis/gawk/gawk-admin/internal/store"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// version is stamped at build time; release-please keeps the chart and image
// tag in step with it.
var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, getenv func(string) string) error {
	cfg, err := config.ParseFlags(args, getenv)
	if err != nil {
		return err
	}
	log := newLogger(cfg)

	// SIGINT/SIGTERM cancels everything below: the HTTP server drains, the
	// leader releases its Lease immediately rather than making the next leader
	// wait out the TTL, and the auth worker's background refresh stops.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.Mode == config.ModeMigrate {
		log.Info("gawk-admin migrating", cfg.LogAttrs()...)
		if err := store.Migrate(ctx, cfg.PGDSN); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		v, dirty, ok, err := store.MigrateVersion(cfg.PGDSN)
		if err != nil {
			return fmt.Errorf("migrate: reading back the version: %w", err)
		}
		log.Info("migrations applied", "version", v, "dirty", dirty, "hasVersion", ok)
		return nil
	}

	log.Info("gawk-admin starting", append([]any{"version", version}, cfg.LogAttrs()...)...)

	st, err := store.Open(ctx, cfg.PGDSN)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer st.Close()

	// Authentication resolves the issuer in the BACKGROUND (§4.8): an
	// unreachable IdP must not crashloop both replicas, so New only refuses a
	// configuration that could never be safe. Until it resolves, authenticated
	// routes answer 401 and /readyz says why.
	authn, err := auth.New(ctx, cfg, auth.Options{Logger: log})
	if err != nil {
		return fmt.Errorf("oidc: %w", err)
	}
	defer authn.Close()

	restCfg, err := restConfig()
	if err != nil {
		return fmt.Errorf("kubernetes: %w", err)
	}
	bans, err := kube.NewCRClient(restCfg, cfg.Namespace)
	if err != nil {
		return err
	}
	clientset, err := kube.NewClientset(restCfg)
	if err != nil {
		return err
	}

	reconciler, err := kube.NewReconciler(kube.ReconcilerOptions{
		Records: st,
		Bans:    bans,
		Log:     log,
	})
	if err != nil {
		return err
	}

	scanner, err := relayscan.New(relayscan.Options{
		Resolve: relayscan.DNSResolver(cfg.RelayScanTarget, cfg.RelayOpsPort),
		Token:   cfg.RelayAdminToken,
		Log:     log,
	})
	if err != nil {
		return err
	}

	a, err := api.New(api.Options{
		Store:       st,
		Projector:   reconciler,
		Reconciler:  reconciler,
		Fleet:       scanner,
		Config:      cfg,
		Authn:       authn.Middleware,
		RequireRole: authn.RequireRole,
		// Readiness is the AND of "the schema is one we can serve" and "we can
		// authenticate somebody". A portal that is up but cannot validate a
		// token serves nothing an operator can use, so it should not take
		// traffic while a sibling replica might already be resolved.
		ReadyChecks: []api.ReadyCheck{{
			Name: "oidc",
			Check: func(context.Context) error {
				if authn.Ready() {
					return nil
				}
				if err := authn.ResolveError(); err != nil {
					return err
				}
				return errors.New("issuer not resolved yet")
			},
		}},
		Log: log,
	})
	if err != nil {
		return err
	}

	pages, err := portal.Handler()
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("/api/v1/", a.Routes())
	mux.HandleFunc("GET /healthz", a.Healthz)
	mux.HandleFunc("GET /readyz", a.Readyz)
	// Unauthenticated on purpose: it is how the SPA learns which IdP to bounce
	// the browser to, and it carries nothing but that (§4.8). No client secret
	// exists anywhere in this system.
	mux.Handle("GET /auth/config", authn.ConfigHandler())
	mux.Handle("/", pages)

	// The security headers wrap EVERYTHING, including 404s and the SPA itself:
	// the CSP is what stands between an XSS and the in-memory access token,
	// and a response that skips it is the one an attacker looks for.
	root := auth.SecurityHeaders(cfg.OIDCIssuer)(mux)

	election, err := kube.NewElection(kube.LeaderOptions{
		Client:    clientset,
		Namespace: cfg.Namespace,
		Identity:  podIdentity(getenv),
		Log:       log,
		// The one place singleton work may start. Its context is cancelled the
		// moment leadership is lost, so a demoted replica stops reconciling
		// before the new leader begins.
		OnLeading: func(ctx context.Context) {
			reconciler.Run(ctx)
		},
		// A clean shutdown hands the Lease over rather than making the next
		// leader wait out the TTL — a rollout should not pause enforcement
		// bookkeeping for 15 seconds per pod.
		ReleaseOnCancel: true,
	})
	if err != nil {
		return err
	}
	go election.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           root,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("portal listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return <-errCh
	}
}

// restConfig resolves Kubernetes credentials: in-cluster first, then the
// ordinary kubeconfig loading rules (KUBECONFIG, then ~/.kube/config).
//
// The fallback is what makes the documented local lane real — gawk-admin
// requires Kubernetes by design (docs/42 §4.14), so "run it against kind"
// has to work without pretending to be a pod. It is deliberately NOT a
// configuration knob: the environment variable is the conventional mechanism,
// a flag would have to be plumbed through the chart to satisfy the
// carry-all-limits rule, and in a pod it would never be set.
//
// Failure here IS fatal, unlike an unreachable IdP: without the API server
// there is nowhere to write a Ban CR, and a portal whose kill button records
// a row nothing enforces is worse than one that refuses to start.
func restConfig() (*rest.Config, error) {
	if cfg, err := kube.InClusterConfig(); err == nil {
		return cfg, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("no in-cluster config and no usable kubeconfig: %w", err)
	}
	return cfg, nil
}

// podIdentity distinguishes leader-election candidates. POD_NAME is what the
// chart supplies through the downward API; the fallback keeps a laptop run
// from electing itself under an empty identity, which the election library
// rejects outright.
func podIdentity(getenv func(string) string) string {
	if name := getenv("POD_NAME"); name != "" {
		return name
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return "gawk-admin"
}

func newLogger(cfg config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}
