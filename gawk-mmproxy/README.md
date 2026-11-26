# gawk-mmproxy

Real client addresses for web users in Mumble (R88,
[docs/92](../docs/92-mumble-real-client-addresses.md)). Without it, murmur sees
every gawk voice user at the address of the
[`gawk-mumble-bridge`](../gawk-mumble-bridge/) pod they are on, so a Mumble ban
of one web user bans everyone on that pod and autoban counts a whole pod as
one client. With it, murmur sees each browser's own address, as it does for a
native Mumble client.

It is optional and off by default. It runs only inside your murmur's pod, as
an init container and a sidecar, so it has an image but no Helm chart.

## How it works

```
browser ──▶ bridge pod ── TCP: PROXY v2 header (the browser's address), then TLS ──▶ gawk-mmproxy :64739
                                                                                    (in murmur's pod)
gawk-mmproxy ── TCP from the browser's address (IP_TRANSPARENT) to 127.0.0.1:64738 ──▶ murmur
murmur ── replies from 127.0.0.1 to the browser's address ── policy route: deliver locally ──▶ gawk-mmproxy
```

Murmur takes a client's address from the TCP socket only; it has no PROXY
protocol support ([mumble-voip/mumble#4769](https://github.com/mumble-voip/mumble/issues/4769)).
So the bridge sends gawk-mmproxy a standard PROXY protocol v2 header with
the browser's address, and gawk-mmproxy opens its loopback connection to
murmur *from that address*. Two policy routes in the pod's network namespace
send murmur's replies on loopback back to gawk-mmproxy instead of out to the
internet:

```
from 127.0.0.0/8 iif lo lookup 64738       local 0.0.0.0/0 dev lo table 64738
from ::1/128 iif lo lookup 64738           local ::/0 dev lo table 64738
```

They only match traffic murmur sends from a loopback address, so native
clients, which reach murmur on the pod's own address, TCP and UDP, are
unaffected. Bridged users never use UDP: their audio rides the TCP tunnel.

Two subcommands, one image:

- **`gawk-mmproxy routes`**, an init container: installs those routes over
  netlink, checking first, so a rerun never duplicates them. It needs
  `NET_ADMIN`, runs as root, and **always exits 0**: a refusal is logged and
  `serve` then fails closed, so a broken proxy setup never keeps murmur down.
- **`gawk-mmproxy serve`**, the sidecar: the proxy. It runs as uid 65532 with
  only `NET_RAW`, through a `cap_net_raw+ep` file capability on the binary.

Because of that file capability, every container that runs this image needs
`NET_RAW` in its capability set, even `routes` running as root: without it the
kernel refuses to start the binary ("exec /gawk-mmproxy: operation not
permitted").

### How it fails

The bridge fails closed: it never falls back to dialling murmur from its own
address. Through gawk-mmproxy, the two ways a connection can end before TLS
stay distinct:

| What happened | gawk-mmproxy sends | The bridge reports |
|---|---|---|
| murmur refused the address (a ban, an autoban) | murmur's reset, as a reset | 4016 `autoban` |
| murmur unreachable, no routes, a bad header, over `-max-conns` | a FIN (SHUT_WR, drain, close) | 4016 `murmur_unreachable` |

`serve` never exits on its own, and the snippets give it **no readiness or
liveness probe**: it shares murmur's pod, and a pod is Ready only while every
container is. A probe on the proxy could take murmur out of its Service and
cut native users. `/readyz` is for you and your alerts.

## Deploying it

Kubernetes, with your existing murmur Deployment:

1. Add the init container and the sidecar from
   [`deploy/k8s/mmproxy-patch.yaml`](deploy/k8s/mmproxy-patch.yaml), as a
   kustomize patch or by hand. The init container's `NET_ADMIN` needs the
   **`privileged` Pod Security level** on murmur's namespace
   (`pod-security.kubernetes.io/enforce: privileged`). Under `baseline` or
   `restricted` the pod is rejected, and a murmur on `strategy: Recreate`
   would be left with none, so run `kubectl apply --dry-run=server` first.
2. Add the ClusterIP Service in [`deploy/k8s/service.yaml`](deploy/k8s/service.yaml).
   **Never** put port 64739 on a LoadBalancer, a NodePort, or the Service that
   carries murmur's public port.
3. Add the NetworkPolicy in [`deploy/k8s/networkpolicy.yaml`](deploy/k8s/networkpolicy.yaml).
   It is the trust boundary: gawk-mmproxy believes any header that reaches
   it, and a header decides the address murmur sees. Set its namespace
   selector to the bridge's namespace: pods choose their own labels, so the
   bridge's label alone would admit any pod in any namespace. You need a CNI that
   enforces NetworkPolicy. The policy selects murmur's pod, so list every other
   port that reaches it (Ice, scrapes), or they are cut.
4. On the bridge, set `mumble.proxy.addr` to the Service,
   e.g. `mumble-mmproxy.mumble:64739`.

[`deploy/k8s/kustomization.yaml`](deploy/k8s/kustomization.yaml) puts all of it
together around a stock murmur
([`example/murmur.yaml`](deploy/k8s/example/murmur.yaml)); CI applies exactly
those files in kind.

Docker Compose: [`deploy/compose/murmur.yml`](deploy/compose/murmur.yml). There is
no NetworkPolicy outside Kubernetes, so the proxy's port must be reachable
only from the bridge: an internal network that only murmur and the bridge
share, no published port, and `-listen` bound to murmur's address on that
network (the default listens on every interface murmur has, its public
network included). On a bare host, bind `-listen` to an address
only the bridge reaches, or firewall it. The loopback routes apply to the
whole namespace: on a bare host that is every loopback service on the host,
so don't run another proxy that reaches murmur over loopback there.

Prerequisites:

- **murmur listens on loopback.** It does with `host=` blank, the default, which
  binds a dual-stack `::`. `host=::` binds an IPv6-only `::`, which refuses
  gawk-mmproxy's IPv4 dial; `/readyz` says so.
- **IPv6 sources** need `::1` on the pod's loopback (Kubernetes' default);
  otherwise they are refused.
- Murmur's admins see web users' real addresses from now on, in its log, in
  user information, in bans and through Ice, as they do for native users.

## Flags

`gawk-mmproxy routes`:

| Flag | Default | Meaning |
|---|---|---|
| `-route-table` | `64738` | the policy-routing table |

`gawk-mmproxy serve`:

| Flag | Default | Meaning |
|---|---|---|
| `-listen` | `:64739` | where bridges send PROXY v2 headers; reachable from the bridge only |
| `-ops-listen` | `:2114` | `/healthz`, `/readyz`, `/metrics`; never routed publicly |
| `-murmur-port` | `64738` | murmur's port on loopback |
| `-route-table` | `64738` | the table whose routes `serve` checks |
| `-require-routes` | `true` | fail closed without the routes; `false` only where every source is a loopback address (the voice e2e) |
| `-header-timeout` | `2s` | the deadline for a complete header, 100ms to 30s |
| `-max-conns` | `1024` | slots for proxied and draining connections; past them a separate pool of 64 drains, and past that a connection is closed at once |

Both take `-log-level` and `-log-format`, and every flag has an environment
twin, `GAWK_MMPROXY_<FLAG>` (`-max-conns` is `GAWK_MMPROXY_MAX_CONNS`). Flags win.

## Is it working?

- `/readyz` on 2114 answers 200 `ready`, or 503 with the reason: routes
  missing, murmur not listening where the loopback dial reaches, or the
  kernel refusing the socket listing.
- `/metrics`: `gawk_mmproxy_connections_total{result}` (each connection once:
  `proxied`, `bad_header`, `header_timeout`, `refused_source`,
  `murmur_unreachable`, `no_routes`, `over_cap`, `undrained`),
  `gawk_mmproxy_resets_total`, `gawk_mmproxy_active`,
  `gawk_mmproxy_draining{pool}` and `gawk_mmproxy_routes_ok`. No label ever
  carries an address.
- Murmur's log shows each web user's own address on its `New connection`
  line, not the bridge pod's.

gawk-mmproxy never logs a client address.

## Tests

```sh
cd gawk-mmproxy && go test -race ./...
```

The routes, the transparent dial and the socket listing are tested against
the real kernel, in a throwaway user and network namespace the test binary
creates for itself; where the kernel or a sandbox refuses unprivileged user
namespaces those tests skip (`GAWK_MMPROXY_REQUIRE_NETNS=1` makes that a
failure, as CI does).

Against real murmur, in a pod-shaped Docker topology:

```sh
eval "$(gawk-mmproxy/testdata/topology.sh up 1.6)"     # or 1.5; GAWK_MMPROXY_MURMUR_HOST=:: for host=::
(cd gawk-mmproxy && go test ./internal/itest/)
gawk-mmproxy/testdata/topology.sh down
```

The deploy files in kind: `gawk-mmproxy/deploy/k8s/kind-test.sh`.

The bridge's side, with real murmur behind the real proxy, is in
`gawk-mumble-bridge` (its `testdata/murmur.sh` starts a `proxied` server).

## Image

`ghcr.io/tuhis/gawk-mmproxy:<version>`, distroless, nonroot, built from the
repository root:

```sh
docker build -f gawk-mmproxy/deploy/Dockerfile -t gawk-mmproxy:dev .
```
