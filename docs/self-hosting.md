# Self-hosting gawk

Everything you need to run your own gawk. Two required Helm charts and
container images, plus an optional third for telemetry, one Kubernetes
namespace. Budget an hour for a first install, most of it waiting for DNS
and certificates.

If you only want to try gawk locally, you do not need any of this — the
[Quickstart](../README.md#quickstart-local-dev) runs the relay and the app on
your laptop in about five minutes with a self-signed certificate.

**Contents**

1. [What you are deploying](#1-what-you-are-deploying)
2. [Prerequisites](#2-prerequisites)
3. [TLS and DNS — read this before installing](#3-tls-and-dns--read-this-before-installing)
4. [Install](#4-install)
5. [Verify](#5-verify)
6. [Single-node vs cluster mode](#6-single-node-vs-cluster-mode)
7. [Broadcasting](#7-broadcasting)
8. [Operating it](#8-operating-it)
9. [Troubleshooting](#9-troubleshooting)

---

## 1. What you are deploying

| Component | What it does | Exposed as |
|---|---|---|
| **`gawk-server`** (the relay) | Terminates WebTransport, fans each broadcast out to its viewers | **UDP LoadBalancer**, its own DNS name and certificate |
| **`gawk-app`** (the web app) | Static SPA: landing/join, broadcaster, viewer | Ordinary HTTPS Ingress |
| `gawk-telemetry` *(optional)* | Per-session diagnostics. **Off by default; skip it on a first install.** | Internal only, plus one same-origin ingest path |

The native broadcasters (`gawk-broadcast` for Linux,
`gawk-broadcast-windows`) are **not** deployed — they are binaries a
broadcaster downloads and runs against your relay. Nothing here installs
them.

Charts and images are published to GHCR on every release, with chart
`version` == `appVersion` == image tag always in lockstep:

```
oci://ghcr.io/tuhis/charts/gawk-server      ghcr.io/tuhis/gawk-server
oci://ghcr.io/tuhis/charts/gawk-app         ghcr.io/tuhis/gawk-app
```

## 2. Prerequisites

- **A Kubernetes cluster** you can create LoadBalancer Services in.
- **A load balancer that can expose UDP** — MetalLB, or a cloud LB with UDP
  support. This is not optional and it is the requirement most likely to be
  missing: the relay speaks QUIC, so a UDP path from the public internet to
  the pod is the whole game.
- **An Ingress controller** for the web app (the chart defaults to class
  `nginx-int`; change `ingress.className` to whatever yours is).
- **cert-manager**, with a ClusterIssuer that can issue publicly-trusted
  certificates — the chart defaults to `letsencrypt-production` and
  `letsencrypt-staging` is the right first target. See
  [§3](#3-tls-and-dns--read-this-before-installing) for why a self-signed
  certificate will not do.
- **Two DNS names** you control, both resolving to this cluster.
- **`helm` ≥ 3.8** (OCI registry support).
- **Bandwidth.** Fan-out is linear and unencoded by the relay: 15 viewers of
  one 12 Mbps broadcast is ~180 Mbps of egress. The reference deployment runs
  on a 1 Gbps symmetric uplink. The relay has caps for exactly this
  (`config.maxSubscribers`, `config.maxBandwidth`) — set them to match your
  link rather than discovering the limit during a stream.

If `helm pull` or the image pull returns 401/403, the packages are not public
to you and you need a pull secret. A classic PAT with `read:packages`
(fine-grained PATs do not cover GHCR):

```sh
kubectl -n gawk create secret docker-registry ghcr-pull \
  --docker-server=ghcr.io --docker-username=<user> --docker-password=<PAT>
```

then set `imagePullSecrets: [{name: ghcr-pull}]` in both charts' values.

## 3. TLS and DNS — read this before installing

This is where self-hosted installs go wrong, because the relay is not a
normal HTTPS service.

**The relay cannot sit behind your Ingress controller.** nginx (and most
reverse proxies) cannot proxy WebTransport: it is HTTP/3 over QUIC over UDP,
and the relay terminates TLS itself. So the relay gets a **LoadBalancer
Service on UDP**, its own DNS name, and its own certificate mounted into the
pod. Only the web app goes through your Ingress.

**The certificate must be publicly trusted.** Browsers will not open a
WebTransport session to an untrusted certificate. There is exactly one
exception, `serverCertificateHashes`, and it is a development mechanism: the
certificate must be ECDSA, valid for **at most 14 days**, and every client
has to be handed its SHA-256 hash out of band. That is what `-dev-cert` and
the app's "Dev cert hash" settings field are for. Do not try to run a
deployment on it — you would be re-issuing and re-distributing a hash every
fortnight.

So: **cert-manager issues a real certificate** for the relay's name, the
chart mounts it at `/tls`, and the relay's reloader picks up renewals per
handshake — no pod restart, no downtime.

**Two names, both pointing at this cluster:**

| Name | Points at | Example |
|---|---|---|
| The app | your Ingress controller's address | `gawk.example.com` |
| The relay | the **UDP LoadBalancer's** external IP | `relay.example.com` |

They must be different names: the relay's IP is its own LoadBalancer, not the
Ingress. Pin the LoadBalancer address (`service.loadBalancerIP`, or your
LB's annotation) so the record stays valid across reinstalls.

**A note on the port.** The relay listens on UDP **4433** by default. Some
networks — corporate guest wifi especially — block outbound UDP to anything
but 443. If your viewers hit that, set the chart's `service.port: 443`; the
container keeps listening on 4433 and the Service maps it, so only the URL
your clients dial changes. Whatever you choose has to appear in the app's
`config.relayUrl`.

## 4. Install

### 4.1 Namespace

```sh
kubectl create namespace gawk
```

### 4.2 Relay values

`relay-values.yaml`:

```yaml
certificate:
  enabled: true
  issuerRef:
    name: letsencrypt-staging      # switch to production once it works
    kind: ClusterIssuer
  dnsNames:
    - relay.example.com            # CHANGEME

service:
  type: LoadBalancer
  port: 4433
  loadBalancerIP: ""               # pin it if your DNS record is fixed

config:
  # Who may connect — see the note below. Empty allows anything, which is
  # fine for a first smoke test and wrong to leave in place.
  allowedOrigins:
    - https://gawk.example.com     # CHANGEME: the web app
    - gawk-broadcast://native      # the Linux native broadcaster
    - gawk-broadcast://windows     # the Windows native broadcaster

  # Capacity. Defaults are conservative; raise them against your uplink.
  maxSubscribers: 15               # per broadcast
  maxBroadcasts: 5                 # concurrent broadcasts per pod
  maxTotalSubscribers: 50          # per pod

  # Who may broadcast. Without it, anyone who can reach the relay can start
  # a stream. Strongly recommended on anything public.
  publishSecret: "change-me"       # or publishSecretKeyRef, see below
```

**About `allowedOrigins`.** The relay checks the `Origin` header on every
CONNECT, and the list is exact strings — no wildcards, and the scheme counts.
Three kinds of client, and only the first two go in the list:

| Client | Origin it sends | Action |
|---|---|---|
| The web app | `https://` + the app's Ingress host | **Add it.** Must match `ingress.host` exactly |
| `gawk-broadcast` (Linux) | `gawk-broadcast://native` | **Add it** if anyone will broadcast from the Linux app |
| `gawk-broadcast-windows` | `gawk-broadcast://windows` | **Add it** if anyone will broadcast from Windows |
| Relay pods, in cluster mode | `gawk-server://native-internal-edge` | **Do not add it.** Built in |

The native broadcasters do not send an `https://` origin — they are not
browsers — so leaving them out is a working web app plus native apps that
are refused, with `origin rejected` in the relay log and nothing obviously
wrong on the client. Add them now even if you only expect browser
broadcasters; the strings are fixed and cost nothing.

The pod-to-pod edge origin is the opposite case: the relay accepts it
**only** on the PSK-gated `/internal/*` routes and never on a client-facing
path, so it is honoured without being listed and adding it buys nothing. It
is deliberately unlistable rather than merely unnecessary — that exemption
exists because setting `-allowed-origins` at all once made cluster-mode edge
dials fail silently ([docs/22](22-relay-scale-out.md) finding 12).

Prefer a Secret over a literal:

```sh
kubectl -n gawk create secret generic gawk-publish \
  --from-literal=secret="$(openssl rand -hex 24)"
```

```yaml
config:
  publishSecretKeyRef: { name: gawk-publish, key: secret }
```

Install it:

```sh
helm upgrade --install gawk-server oci://ghcr.io/tuhis/charts/gawk-server \
  --version <X.Y.Z> -n gawk -f relay-values.yaml
```

### 4.3 DNS

Get the LoadBalancer address and point the relay's name at it:

```sh
kubectl -n gawk get svc gawk-server -o wide
```

Wait for the certificate before moving on — `helm install` returns long
before Let's Encrypt does:

```sh
kubectl -n gawk get certificate
kubectl -n gawk describe certificate gawk-server-tls   # if it stays not-ready
```

### 4.4 App values

`app-values.yaml`:

```yaml
ingress:
  className: nginx                 # CHANGEME to your ingress class
  host: gawk.example.com           # CHANGEME
  tls:
    enabled: true
    secretName: gawk-app-tls
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-production

config:
  # The one value a self-hosted install cannot omit. Must match the relay's
  # certificate name and the Service port from §4.2. Without it the app
  # falls back to https://localhost:4433 and every viewer would have to
  # paste the URL into settings before a join link worked.
  relayUrl: "https://relay.example.com:4433"     # CHANGEME

  # Set true when the relay runs with a publish secret, so the broadcaster
  # is prompted for it. The secret itself is never shipped to the client.
  requirePublishSecret: true

  # Shown in the bundled terms text as the operating party.
  operatorName: "Example Org"
  operatorContact: "gawk@example.com"
```

```sh
helm upgrade --install gawk-app oci://ghcr.io/tuhis/charts/gawk-app \
  --version <X.Y.Z> -n gawk -f app-values.yaml
```

**On the terms text**: the app ships a default Terms of Use, and it is a
**template**. `operatorName` and `operatorContact` are substituted into it so
it names you rather than a placeholder, and `config.termsUrl` +
`config.termsHtml` replace the document wholesale with your own
([docs/29](29-terms-and-conditions.md)). Whoever runs an instance is
responsible for the terms it presents being meaningful for that deployment —
the bundled default is a starting point, not a decision anyone has made on
your behalf. Bump `config.termsVersion` after a meaningful edit and every
broadcaster is re-prompted to acknowledge them.

## 5. Verify

**The relay answers, with a real certificate:**

```sh
go run ./cmd/gawk-echo \
  -url https://relay.example.com:4433/echo \
  -origin https://gawk.example.com
```

A datagram round-trip means DNS, the UDP path, the certificate and the origin
allowlist are all correct at once. No `-cert-hash` is needed against a
publicly-trusted certificate; `-origin` has to be one of your
`allowedOrigins` values.

**The pod is healthy from the inside** (the ops endpoint is ClusterIP-only —
never expose it):

```sh
kubectl -n gawk port-forward svc/gawk-server-metrics 2112:2112
curl -s localhost:2112/healthz
curl -s localhost:2112/statusz | jq
curl -s localhost:2112/metrics | head
```

`/statusz` keys broadcasts by an HMAC rather than their real IDs — the IDs
are joinable, so they are never exposed.

**End to end**: open `https://gawk.example.com`, start a stream, and join the
code from a second browser (ideally on another network — that is what
actually proves the UDP path from outside).

## 6. Single-node vs cluster mode

**Start with one pod.** It is the default, it never talks to the Kubernetes
API, and it serves a real audience.

| | Single pod (default) | Cluster mode |
|---|---|---|
| `replicas` | 1 | 2+ |
| `config.clusterMode` | `false` | **`true`** (the chart refuses `replicas > 1` without it) |
| Kubernetes API | never touched | per-broadcast Leases (chart creates SA + Role + RoleBinding) |
| Shared Secrets | just the publish secret | reset key, internal PSK, resume-token key, stats key |
| Rollout behaviour | one pod drains, ~1 s freeze | pods drain one at a time, viewers reconnect at 0 ms |
| Capacity | one pod's CPU and NIC | one broadcast's audience spans pods |

You need cluster mode when a single broadcast's audience outgrows one pod, or
when you want rollouts not to interrupt streams. The mechanism —  the
publisher's pod claims a per-broadcast Lease as *origin*, other pods
*edge-pull* from it and re-fan-out — is in
[docs/22](22-relay-scale-out.md).

Create the fleet Secrets once, shared by every pod:

```sh
kubectl -n gawk create secret generic gawk-fleet \
  --from-literal=statelessResetKey=$(openssl rand -hex 32) \
  --from-literal=statsKey=$(openssl rand -hex 32) \
  --from-literal=internalPsk=$(openssl rand -hex 32) \
  --from-literal=resumeTokenKey=$(openssl rand -hex 32)
```

```yaml
replicas: 2
config:
  clusterMode: true
  statelessResetKeyRef: { name: gawk-fleet, key: statelessResetKey }
  statsKeyRef:          { name: gawk-fleet, key: statsKey }
  internalPskRef:       { name: gawk-fleet, key: internalPsk }
  resumeTokenKeyRef:    { name: gawk-fleet, key: resumeTokenKey }
  # Cross-node hops are SNAT'd to node IPs under externalTrafficPolicy:
  # Cluster, so a rollout's reconnect herd would trip the per-IP rate
  # limiter. List your node/pod CIDRs.
  trustedCidrs: ["10.42.0.0/16", "10.12.0.0/24"]   # CHANGEME
```

**`allowedOrigins` needs nothing added for cluster mode.** Pods announce
`gawk-server://native-internal-edge` to each other, which the relay accepts
only on the PSK-gated internal route — see [§4.2](#42-relay-values).

**Set `resumeTokenKeyRef` explicitly.** Left empty, the resume-token key is
derived from the publish secret — which every broadcaster holds, so any
broadcaster could compute another's token and take over their broadcast ID.
The independent key is what prevents that. Confirm
`resume_token_key_mode=explicit-key` in the startup log.

**MetalLB in L2 mode funnels all traffic through one announcing node**, so
replicas buy fan-out CPU and redundancy but not NIC bandwidth. With BGP mode
and ECMP on the router, add `service.externalTrafficPolicy: Local` — real
client IPs, exact rate limiting, and only nodes with a ready pod advertise
the route.

## 7. Broadcasting

**From a browser** — open the app, **Start a stream**, share a screen. Chrome
or another Chromium browser is the happy path. On Windows and macOS this gets
hardware encoding.

**From Linux, use the native broadcaster.** Browser hardware encode does not
exist on Linux — WebCodecs ships HW encode on Windows/macOS/Android only —
so a Linux broadcaster in a browser is software-encoding and will feel it.
[`gawk-broadcast`](../gawk-broadcast/README.md) exists for this: portal
capture plus a hardware GStreamer pipeline.

Both native broadcasters default to the reference deployment, so point them
at yours:

```sh
gawk-broadcast -url https://relay.example.com:4433 -secret <publish-secret>
```

or set the relay URL in the GUI's settings. Set the telemetry URL to `off`
unless you are running your own telemetry service — the pairing rule already
means a non-default relay reports nowhere, but being explicit costs nothing.

Both send their own `Origin` header rather than an `https://` one, so they
have to be in the relay's `allowedOrigins` — see the table in
[§4.2](#42-relay-values).

## 8. Operating it

**Upgrades** are `helm upgrade` with a newer chart version. The Deployment
uses RollingUpdate with `maxSurge: 1, maxUnavailable: 0`, so a replacement pod
is Ready before the old one exits; draining pods send close code 4002 and
clients reconnect immediately. `helm rollback` is the same motion in reverse.

**Metrics.** The relay serves Prometheus metrics on the ClusterIP ops
endpoint (`metrics.port`, default 2112). Set
`metrics.serviceMonitor.enabled: true` if you run prometheus-operator, with
labels matching your Prometheus's `serviceMonitorSelector`. The symptom →
signature playbook is [docs/13](13-observability.md).

**Capacity knobs**, all under `config`:

| Value | Default | What it protects |
|---|---|---|
| `maxSubscribers` | 15 | viewers per broadcast |
| `maxBroadcasts` | 5 | concurrent broadcasts per pod |
| `maxTotalSubscribers` | 50 | viewers per pod |
| `maxBandwidth` | 0 (unlimited) | total egress |
| `connRateLimit` / `connBurstLimit` | 3/s, 10 | connection floods per IP |

**Keepalive, not idle timeout.** If viewers drop while a broadcaster is away,
the knob is `config.keepalivePeriod` (default 10 s), not
`maxIdleTimeout` — the effective idle timeout is the minimum of both
endpoints' and browsers advertise ~30 s, so raising the server's alone does
nothing.

**Backups**: there is nothing to back up. The relay is stateless — broadcasts
live in memory and IDs are minted per session.

**Using your relay from other gawk UIs (R37,
[docs/40](40-relay-server-picker.md)).** Since R37 any gawk frontend that
permits alternative relays (its operator's `config.allowCustomRelays`,
default on) can talk to your relay — viewers arrive through share links
carrying `?relay=`, or by picking your server in the UI. Three things are
yours to configure:

- **Allow the UI origins.** The relay's `config.allowedOrigins` is the
  gate: list every frontend origin you want to serve (e.g.
  `["https://gawk.example.com", "https://gawk.ioio.fi"]`), or leave it
  empty to accept any origin. A UI you haven't allowed fails at CONNECT,
  which a browser can only report as "unreachable".
- **Name your relay.** `config.serverName` (e.g. `"Juho's homelab"`) is
  what server pickers show next to your relay's host after probing it over
  `/echo` — unset leaves it identified by version only.
- **Advertise your telemetry ingest** (only with `telemetry.enabled`).
  `telemetry.advertiseUrl` names YOUR frontend's public ingest URL (e.g.
  `https://gawk.example.com/api/telemetry/v1/ingest`); sessions arriving
  from foreign UIs then report diagnostics to *your* collector instead of
  dying at their home deployment's token check. An invalid URL fails relay
  startup on purpose. Verify the pairing end-to-end: join your relay from a
  foreign UI and check the session appears in your dashboard.

You can also **host a server directory** for your own UI's picker: a static
JSON (schema v1 — `{"version": 1, "servers": [{"label": "...", "url":
"https://relay...:4433"}]}`) referenced by the gawk-app chart's
`config.serverDirectoryUrl`. Serve it same-origin next to `/config.js` (the
`terms.html` ConfigMap pattern) or from any host that answers CORS for your
frontend's origin. Directory entries never carry credentials.

## 9. Troubleshooting

| Symptom | Likely cause |
|---|---|
| Viewer shows "connecting" forever; `gawk-echo` also fails | UDP is not reaching the pod. Check the LoadBalancer has an external IP, DNS points at it, and no firewall blocks UDP on your port |
| Works on your LAN, not from outside | Outbound UDP to non-443 blocked upstream — try `service.port: 443` |
| Browser console shows a certificate error on the WebTransport session | The relay certificate is not publicly trusted, or its name does not match `relayUrl`. `kubectl -n gawk get certificate` |
| Relay logs `origin rejected` | `config.allowedOrigins` does not contain the app's origin, scheme included |
| Every viewer has to paste a relay URL | `config.relayUrl` is unset on the app chart |
| Broadcaster is asked for a secret it does not have, or is refused | `requirePublishSecret` and the relay's `publishSecret` disagree |
| `helm upgrade` refuses with a replicas error | `replicas > 1` without `config.clusterMode: true` |
| Reconnect storms fail with rate-limit errors after a rollout | `config.trustedCidrs` missing in cluster mode |
| Relay logs `resume_token_key_mode=derived` | `resumeTokenKeyRef` not set — see [§6](#6-single-node-vs-cluster-mode) |

Two starting points when something is stranger than the table:
[`docs/gotchas.md`](gotchas.md) is the consolidated list of things that have
already cost someone a debugging session, and [docs/13](13-observability.md)
maps metric signatures to causes.

Still stuck, or something here is wrong or missing? Open an issue — a
self-hosting report is a genuinely useful bug, since the reference deployment
cannot exercise every cluster shape.
