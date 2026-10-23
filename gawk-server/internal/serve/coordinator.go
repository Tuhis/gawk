package serve

import (
	"fmt"
	"log/slog"
	"net"
	"os"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/roomcluster"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
)

// buildCoordinator constructs the cluster coordinator from the in-cluster
// Kubernetes config and the downward-API pod identity. Only called when
// -cluster-mode is on: single-pod deployments never touch the k8s API.
func buildCoordinator(cfg config.Config, hooks coordinatorHooks, log *slog.Logger) (*cluster.Coordinator, string, error) {
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
	coord, err := cluster.New(clusterOptions(cfg, hooks, client, namespace, podName, net.JoinHostPort(podIP, port), log))
	if err != nil {
		return nil, "", err
	}
	return coord, podName, nil
}

// coordinatorHooks are the coordinator's informer dispatches into the
// transport, all late-bound through the server.
type coordinatorHooks struct {
	// OnLeaseDeleted is the cluster-wide "broadcast ended" dispatch (edge
	// stop + local hub expiry).
	OnLeaseDeleted func(string)
	// OnLeaseLost is the demote path (stale publisher close, 4003 to edges,
	// self-demote to edge).
	OnLeaseLost func(string, cluster.Origin)
	// OnOriginClaimed wakes a pull waiting on an R74 PoP reservation when the
	// PoP's uplink leg claims it on some pod (docs/76 item 17).
	OnOriginClaimed func(string, cluster.Origin)
}

// clusterOptions maps the config and hooks onto cluster.Options — the
// registryOptions rule for the coordinator (TestClusterOptionsCarryEveryHook).
func clusterOptions(cfg config.Config, hooks coordinatorHooks, client kubernetes.Interface, namespace, podName, advertise string, log *slog.Logger) cluster.Options {
	return cluster.Options{
		Client:          client,
		Namespace:       namespace,
		PodName:         podName,
		AdvertiseAddr:   advertise,
		BroadcastGrace:  cfg.BroadcastGrace,
		MaxBroadcasts:   cfg.MaxBroadcasts,
		Log:             log,
		OnLeaseDeleted:  hooks.OnLeaseDeleted,
		OnLeaseLost:     hooks.OnLeaseLost,
		OnOriginClaimed: hooks.OnOriginClaimed,
	}
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
