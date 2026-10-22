package serve

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/popsrc"
	"github.com/Tuhis/gawk/gawk-server/internal/popsrv"
)

// wirePops installs the home side of R74 PoP relays (docs/76) when -pops is
// on: the Pop source (the CRs in cluster mode, else -pops-file), the
// relays' status writer, the link manager behind the /pop/* routes, and
// each Wave 3 subsystem on top of it. Run calls it after wireSubsystems, so
// the cluster and the ban source it builds on are already wired. It returns
// the manager (nil with -pops off) for the admin route.
func wirePops(ctx context.Context, w Wired, log *slog.Logger) (*popsrv.Manager, error) {
	cfg := w.Cfg
	if !cfg.Pops {
		return nil, nil
	}
	src, status, err := popSource(ctx, cfg, log)
	if err != nil {
		return nil, err
	}
	pod, podAddr := popPodIdentity(cfg)
	m := popsrv.New(popsrv.Options{
		Pod:             pod,
		PodAddr:         podAddr,
		Source:          src,
		Status:          status,
		Coord:           w.Coord,
		Registry:        w.Registry,
		HomeOrigin:      HomeOrigin(cfg),
		HomeRooms:       cfg.Rooms,
		ScreeningNotice: cfg.ScreeningNotice,
		ClusterPSK:      cfg.InternalPSK,
		Reservations:    w.Reservations,
		Log:             log,
	})
	w.Server.SetPopHome(m)
	for _, wire := range []func(context.Context, Wired, *popsrv.Manager, *slog.Logger) error{
		wirePopUplink, wirePopDownlink, wirePopModeration, wirePopWall, wirePopNamed, wirePopOffers,
	} {
		if err := wire(ctx, w, m, log); err != nil {
			return nil, err
		}
	}
	log.Info("pop relays enabled", "source", popSourceName(cfg), "pod", pod, "home_origin", HomeOrigin(cfg))
	return m, nil
}

// popSource builds the Pop source and the matching status writer. An
// explicit -pops-file wins (a single-pod home: no shared status, so the
// lease and link live in memory); otherwise cluster mode reads the CRs.
func popSource(ctx context.Context, cfg config.Config, log *slog.Logger) (popsrc.Source, popsrc.StatusWriter, error) {
	if cfg.PopsFile != "" {
		if cfg.ClusterMode {
			log.Warn("-pops-file set in cluster mode: PoP links are per pod and the lease is not shared; use the Pop CRs on a fleet")
		}
		src, err := popsrc.StartFile(ctx, cfg.PopsFile, log)
		if err != nil {
			return nil, nil, err
		}
		return src, popsrc.NewMemStatus(src), nil
	}
	if !cfg.ClusterMode {
		return nil, nil, fmt.Errorf("-pops requires -cluster-mode or -pops-file")
	}
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("pops in cluster-mode require in-cluster kubernetes config: %w", err)
	}
	dc, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, nil, err
	}
	ns := os.Getenv("POD_NAMESPACE")
	if ns == "" {
		return nil, nil, fmt.Errorf("pops in cluster-mode require POD_NAMESPACE (downward API)")
	}
	src, err := popsrc.StartK8s(ctx, dc, ns, log)
	if err != nil {
		return nil, nil, err
	}
	return src, popsrc.NewK8sStatus(dc, ns), nil
}

func popSourceName(cfg config.Config) string {
	if cfg.PopsFile != "" {
		return "file"
	}
	return "kubernetes"
}

// popPodIdentity names this pod in status.lease: the downward API's
// POD_NAME and POD_IP with the relay's port, else the hostname and -addr.
func popPodIdentity(cfg config.Config) (pod, addr string) {
	pod = os.Getenv("POD_NAME")
	if pod == "" {
		pod, _ = os.Hostname()
	}
	addr = cfg.Addr
	if ip := os.Getenv("POD_IP"); ip != "" {
		if _, port, err := net.SplitHostPort(cfg.Addr); err == nil {
			addr = net.JoinHostPort(ip, port)
		}
	}
	return pod, addr
}

// HomeOrigin is the home app origin a linked PoP is told (docs/76 item
// 11: its guests' "open at home" links): the first https:// entry of
// -allowed-origins, which on a deployment is the web app's origin. Empty
// when there is none (a dev home); the PoP then sends no origin in 0x18.
func HomeOrigin(cfg config.Config) string {
	for _, o := range cfg.AllowedOrigins {
		if strings.HasPrefix(o, "https://") {
			return o
		}
	}
	return ""
}
