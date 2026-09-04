# Self-hosting gawk

Everything you need to run your own gawk. Two Helm charts, two container
images, one Kubernetes namespace. Budget an hour for a first install, most of
it waiting for DNS and certificates.

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
9. [`gawk-admin` — the moderation portal](#9-gawk-admin--the-moderation-portal-r39)
10. [Rooms (R42)](#10-rooms-r42)
11. [Troubleshooting](#11-troubleshooting)

---

## 1. What you are deploying

| Component | What it does | Exposed as |
|---|---|---|
| **`gawk-server`** (the relay) | Terminates WebTransport, fans each broadcast out to its viewers | **UDP LoadBalancer**, its own DNS name and certificate |
| **`gawk-app`** (the web app) | Static SPA: landing/join, broadcaster, viewer | Ordinary HTTPS Ingress |
| `gawk-telemetry` *(optional)* | Per-session diagnostics. **Off by default; skip it on a first install.** | Internal only, plus one same-origin ingest path |
| `gawk-admin` *(optional)* | The moderation portal: fleet-wide kill, durable ID/IP bans, signed webhooks. **Off by default; skip it on a first install.** | ClusterIP by default; an OIDC-gated Ingress is a deliberate opt-in ([§9](#9-gawk-admin--the-moderation-portal-r39)) |

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

The images and charts are public. Nothing here needs a registry login.

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

**Rehearse this part before you touch Kubernetes.** The local development
stack's ACME level does the same issuance on your own domain, by hand:
`./dev/certs.sh` → *Let's Encrypt on a domain you control* obtains a
DNS-01-validated wildcard, checks CAA before spending an attempt, waits for
the TXT record to reach your authoritative nameservers, and explains the rate
limits when you hit them. What you learn there — which zone the challenge
lands in, how long propagation takes, what a CAA record does to you — is
exactly what the ClusterIssuer will need. It costs nothing and touches no
cluster: the A records point at `127.0.0.1`, because DNS-01 proves control of
the *name*, not reachability of the host. See
[`docs/41`](41-local-dev-stack.md) §4.4 and the
[README quickstart](../README.md#quickstart-local-dev).

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

## 9. `gawk-admin` — the moderation portal (R39)

Optional, off by default, and the only part of gawk that is designed to be
**exposed to the internet behind an identity provider**. Read
[`docs/42`](42-admin-moderation-portal.md) for why; this section is what an
operator has to do.

What it gives you: every live broadcast on the fleet in one list, a **kill**
that ends one broadcast everywhere at once (viewers see "ended by a
moderator", the broadcaster's auto-resume is refused), durable **bans** on a
broadcast ID or a publisher's IP, an audit trail, and **webhooks** so a flagged
stream reaches you rather than waiting for you to look.

What it does not change: with `moderation.enabled` on, the relay enforces bans
from `Ban` custom resources **whether or not `gawk-admin` is running**. A
`kubectl apply` of one enforces on its own. That is the break-glass path, and
it is the reason the enforcement object is a CR and not an API call.

Without `gawk-admin` you manage bans by hand: `kubectl apply`/`delete` of
`Ban` objects ([§9.6](#96-break-glass-banning-with-kubectl)), or — for a relay
running outside Kubernetes, or if you would rather mount a file than install
the CRD — the relay's **file source** (`moderation.source:
"file:/path/to/bans.json"` in the chart, `-moderation-source=file:<path>` on
the binary): a JSON ban list, reloaded on change and on SIGHUP. What you give
up either way is the portal — the fleet list, the one-click kill, the audit
trail and the webhooks.

### 9.1 Prerequisites, beyond §2

- **A Postgres — and it has to exist before you install the chart.**
  `gawk-admin` keeps its system of record in Postgres (the audit trail, the ban
  rows, events, webhook deliveries), and every replica reads and writes the
  same database. **The chart does not create one.** It takes a *connection* to
  a database you run: the stateful half stays decoupled from the stateless
  release, and `helm uninstall` can never take the audit trail with it — that
  trail is the one artifact here that cannot be rebuilt from anything else.

  Give it the connection one of two ways — a literal DSN, or a Secret holding
  one (the Ref wins if you set both):

  ```yaml
  postgres:
    dsn: "postgres://gawk_admin:pw@postgres.db.svc:5432/gawk_admin?sslmode=require"
  ```

  ```yaml
  postgres:
    dsnSecretRef: {name: gawk-admin-pg, key: dsn}   # key defaults to "dsn"
  ```

  ```sh
  kubectl -n gawk create secret generic gawk-admin-pg \
    --from-literal=dsn='postgres://gawk_admin:pw@postgres.db.svc:5432/gawk_admin?sslmode=require'
  ```

  With neither set the chart **refuses to render**, naming both forms. Already
  running a Postgres — managed, on a VM, in another namespace? That is the
  whole integration: create the database and role, hand over the DSN.

  **Order matters.** The migration Job is a `pre-install` hook: it runs, and
  must succeed, *before* the Deployment rolls. If the database is not there
  yet, that Job fails, the release does not roll, and the portal is simply not
  installed — the correct, visible failure rather than pods CrashLooping on a
  connection that was never going to work. `kubectl -n gawk logs
  job/gawk-admin-migrate` is where it says so.

- **CloudNativePG, if you want the cluster run for you.** Nothing about
  `gawk-admin` requires CNPG, but it is the operator-grade way to run Postgres
  in a cluster and it needs no special support from this chart — its generated
  Secret *is* the Ref form above. The operator itself is a prerequisite;
  operators are cluster-scoped and usually already there:

  ```sh
  kubectl apply --server-side -f \
    https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-1.28/releases/cnpg-1.28.0.yaml
  ```

  Then apply a `Cluster` of your own, once, outside the release. Two instances
  — a primary and one hot standby — because HA of the moderation plane is a
  stated reason it has a real database at all, and a single-instance cluster
  makes routine node maintenance an outage of the thing you reach for during
  an incident:

  ```yaml
  apiVersion: postgresql.cnpg.io/v1
  kind: Cluster
  metadata:
    name: gawk-admin-db
    namespace: gawk
  spec:
    instances: 2
    # Pinned rather than floating: a major-version jump is a migration, not an
    # upgrade.
    imageName: ghcr.io/cloudnative-pg/postgresql:18.2
    bootstrap:
      initdb:
        database: gawk_admin
        owner: gawk_admin
    storage:
      size: 2Gi
    resources:
      requests:
        cpu: 50m
        memory: 256Mi
      limits:
        memory: 1Gi
  ```

  CNPG publishes `<cluster>-app` for the application user it creates, and that
  Secret's `uri` key is a complete connection URI — exactly the DSN the chart
  wants. So the wiring is one value:

  ```yaml
  postgres:
    dsnSecretRef: {name: gawk-admin-db-app, key: uri}
  ```

  Wait for `kubectl -n gawk get cluster gawk-admin-db` to report a healthy
  primary before installing the chart, for the ordering reason above.

- **An OIDC provider** — Keycloak, Authelia, authentik, Google, anything
  spec-compliant. [§9.3](#93-the-identity-provider) is the recipe.

- **A third DNS name**, if you want the portal reachable from a phone. If you
  do not, skip the Ingress entirely and use
  `kubectl -n gawk port-forward svc/gawk-admin 8090:8090`.

```
oci://ghcr.io/tuhis/charts/gawk-admin       ghcr.io/tuhis/gawk-admin
```

### 9.2 Install

Two halves. **Turn the relay's side on first** — it is what installs the `Ban`
CRD, and `gawk-admin` writes into it. The ordering is an **upgrade** rule too,
not only an install one: upgrade the `gawk-server` chart before `gawk-admin`,
the same schema-before-writer discipline the migration hook enforces for
Postgres — a newer portal writing against an older installed CRD has its new
spec fields silently pruned (docs/42 §4.2 carries the CRD's additive-only
compatibility rule).

```sh
helm upgrade --install gawk-server oci://ghcr.io/tuhis/charts/gawk-server \
  -n gawk -f relay-values.yaml \
  --set moderation.enabled=true \
  --set moderation.adminApi.tokenRef.name=gawk-admin-relay-token \
  --set moderation.adminApi.tokenRef.key=token
```

with the shared machine credential created once:

```sh
kubectl -n gawk create secret generic gawk-admin-relay-token \
  --from-literal=token=$(openssl rand -hex 32)
```

That token gates the relay's `/internal/admin/*` routes, which are
ClusterIP-only and return **404** until a credential is configured — the
surface stays dark, not merely locked. It is deliberately **not** the internal
PSK: a different trust domain, rotatable on its own.

Then the portal:

```sh
helm upgrade --install gawk-admin oci://ghcr.io/tuhis/charts/gawk-admin \
  -n gawk \
  --set postgres.dsnSecretRef.name=gawk-admin-db-app \
  --set postgres.dsnSecretRef.key=uri \
  --set externalUrl=https://admin.gawk.example.com \
  --set oidc.issuer=https://id.example.com/realms/gawk \
  --set oidc.clientId=gawk-admin \
  --set oidc.audience=gawk-admin \
  --set relay.adminTokenRef.name=gawk-admin-relay-token \
  --set relay.adminTokenRef.key=token \
  --set links.appBaseUrl=https://gawk.example.com \
  --set ingress.enabled=true \
  --set ingress.host=admin.gawk.example.com \
  --set ingress.className=nginx \
  --set ingress.tls.enabled=true --set ingress.tls.secretName=gawk-admin-tls
```

The chart **refuses to render an Ingress** unless `oidc.issuer`,
`oidc.clientId` and `oidc.audience` are all set. Authentication is enforced in
the process regardless; the guard exists so a misconfiguration cannot publish a
public hostname in front of a service that cannot boot.

`relay.scanTarget` defaults to `gawk-server-metrics-headless`, the relay
chart's headless metrics Service. If you renamed your relay release, set it to
match — that Service's A records are how the portal finds pods.

**Migrations run themselves.** The chart ships a `pre-install`/`pre-upgrade`
hook Job that runs `gawk-admin migrate` from the same image the Deployment is
about to roll, before it rolls — on a first install as much as on an upgrade,
which is possible precisely because the database is a prerequisite of the
release rather than something the release creates. If it fails, the release
halts: on an upgrade the old pods keep serving against the old schema (which
they are guaranteed to be able to do), and on a first install nothing comes up
at all. The manual form, for when you need it:

```sh
kubectl -n gawk run gawk-admin-migrate --rm -it --restart=Never \
  --image=ghcr.io/tuhis/gawk-admin:<the version you are installing> \
  --env=GAWK_ADMIN_PG_DSN="$(kubectl -n gawk get secret gawk-admin-db-app \
        -o jsonpath='{.data.uri}' | base64 -d)" \
  -- migrate
```

Use **the version you are installing**, not `latest`: the schema a release
needs travels inside that release's image, which is the whole point of the
subcommand living in the same binary.

### 9.3 The identity provider

The portal SPA is a **public client** — authorization code + PKCE, no client
secret, tokens held in browser memory only. The API authenticates every request
with the provider-issued JWT and authorizes it with a **role carried in the
token**. There is no allowlist in any config file: granting or revoking an
operator happens in the IdP, where identities are already managed and audited.

Keycloak, concretely:

1. Create a realm (or use an existing one).
2. Create client `gawk-admin`: **public** client, *Standard flow* on, *Direct
   access grants* off. Valid redirect URI: `https://admin.gawk.example.com/*`.
   Web origins: `https://admin.gawk.example.com`.
3. On that client, create a **client role** named `operator`, and assign it to
   the users who may act. (`flagger` is reserved for R40 and unused today.)
4. Client scopes → make sure the client's own audience lands in `aud`. With
   Keycloak's defaults, setting `oidc.audience` to the client ID is right.
5. **Access token lifespan: 5–15 minutes.** Realm settings → Tokens.
6. **Refresh token rotation on**: *Revoke Refresh Token* on, with a refresh
   token lifespan (SSO Session Idle) sized to how long you want an unattended
   tab to stay useful — 30 minutes is a reasonable start.

Steps 5 and 6 are not hardening theatre, they are the revocation model. **A JWT
cannot be revoked server-side before it expires**, so the access-token lifetime
*is* the revocation horizon: removing the `operator` role, or revoking the
refresh token or the IdP session, takes effect at the next refresh and not
before. Short access tokens make that horizon minutes instead of hours, and
refresh-token rotation makes it free for the person using the portal — the SPA
renews silently and nobody types a password more often.

Other providers: override `oidc.rolesClaim` with the dot-path to wherever that
provider puts roles (`groups` for a top-level claim, for example). The default
is Keycloak's client-roles shape with `{audience}` substituted into each
segment.

**When the IdP is down.** `gawk-admin` does not die and does not hang. It
starts, retries discovery in the background, and answers authenticated routes
with `401 idp_unavailable` — never a 500, and without spending the caller's
rate-limit budget — until discovery resolves; `/readyz` reports the unresolved
state, so the pods show it plainly. Once JWKS is cached, validation is offline,
so an IdP outage that starts *after* people are logged in costs nothing until
their tokens expire. **Enforcement is untouched either way**: relay pods are
reading CRs, and `kubectl apply` of a `Ban` needs no identity provider at all.

### 9.4 IP bans need `externalTrafficPolicy: Local`

**This one can take your whole deployment out, so decide it before you need
it.** An IP ban bans the address the relay saw the publisher connect from.
Under `service.externalTrafficPolicy: Cluster`, kube-proxy SNATs cross-node
traffic, so the relay sees a **node IP** for many publishers — and banning that
is a fleet-wide outage switch that also blocks every innocent broadcaster
behind it. The relay cannot detect the misconfiguration; it has no way to know
the address it was handed is not the client's.

So: set `service.externalTrafficPolicy: Local` on the relay chart (which also
wants `podAntiAffinity` so every LB-eligible node has a pod), and check what
your relay actually logs as a publisher's remote address before you rely on ID
+ IP as distinct handles. The portal warns when the IP it resolved is shared by
more than half the live broadcasts, but that heuristic only fires when you
already have enough traffic for it to be obvious.

Also worth knowing: **an IP ban is the only handle that survives a re-mint**.
Banning a broadcast ID bans its resume token too (the token is
`HMAC(key, broadcastID)`), but nothing stops the same person minting a fresh
ID. With no accounts, ID and IP is all the identity that exists — the portal
says so in the UI rather than implying otherwise.

### 9.5 Webhooks

Chart-defined webhooks stay GitOps-reviewable and are immutable in the portal;
operator-created ones live in Postgres and can be added from a phone. Both are
signed with HMAC-SHA256, each with its own key.

```yaml
notifications:
  webhooks:
    - name: ops-pager
      url: https://ntfy.example.com/gawk-moderation
      secretRef: { name: gawk-admin-webhooks, key: ops-pager }
```

The signing key never appears in your values file: the chart renders the
webhook's *name*, its URL and the *name of an environment variable*, and wires
that variable from your Secret.

**What your receiver must do**, and none of it is optional:

- **Verify the signature** before parsing anything. An unverified webhook
  endpoint is an endpoint anyone who learns its URL can page you from — and
  with a URL that appears in a values file, in Helm history and in your CI
  logs, assume they can.
- **Reject stale timestamps.** The signed material includes a timestamp;
  refuse a delivery where `|now − timestamp| > 300 s`. Without that check a
  captured request replays forever, signature and all.
- **Treat the payload as sensitive.** Payloads deliberately carry **no raw
  broadcast ID and no IP** — only the HMAC'd broadcast key and a link back to
  the portal, because webhooks transit third-party push infrastructure and a
  raw ID is a join capability. But they **do carry ban reasons**, which are
  free operator text and routinely hold context you would not publish. Send
  them to a channel you would be comfortable having read.

### 9.6 Break-glass: banning with `kubectl`

`gawk-admin` down, Postgres down, or you simply have kubectl and no time:

```sh
kubectl -n gawk apply -f - <<'EOF'
apiVersion: gawk.ioio.fi/v1alpha1
kind: Ban
metadata:
  name: ban-id-abc123          # ban-id-<lowercased broadcast ID>
spec:
  target:
    type: broadcastId          # or: ip
    value: "ABC123"            # uppercase ID, or a CIDR like 203.0.113.7/32
  expiresAt: "2026-08-21T18:00:00Z"   # omit entirely for permanent
  reason: "operator: abuse report"
  createdBy: "kubectl"
EOF

kubectl -n gawk get bans
```

Relay pods pick it up through their informer and act immediately: any live
broadcast matching it is terminated with close code 4006 and every reclaim or
fresh publish attempt is refused with HTTP **451**. The name matters only for
idempotence — a re-ban of the same target should update the same object rather
than create a second one — and there is deliberately **no label selector on the
informer**, so a hand-written Ban that forgot a label still enforces.

When `gawk-admin` comes back, its reconciler **adopts** custom resources it has
no row for (recording them as `createdBy: kubectl`) rather than deleting them.
It also never garbage-collects while it cannot reach Postgres.

Unban by deleting the object — or, once the portal is back, from the portal, so
the action lands in the audit trail:

```sh
kubectl -n gawk delete ban ban-id-abc123
```

Expiry needs no janitor: relays evaluate `expiresAt` against their own clock at
check time, so an expired ban stops enforcing on schedule even if the object
outlives it.

### 9.7 What a portal compromise yields

Decide this is acceptable **before** you enable the Ingress, because it is the
honest inventory:

**It does yield** the ability to kill and ban broadcasts, and the **raw
broadcast IDs of every live broadcast** — which are joinable, so an attacker
holding the portal can watch anything currently streaming.

**It does not yield** relay configuration secrets (the effective-config view
redacts every secret-bearing field at the source, so the portal never holds
one), any media, or cluster credentials beyond `Ban` CRUD and a leader Lease in
one namespace — that is the entire Role the chart creates.

Two more things worth naming rather than discovering:

- **A database reader can forge webhook signatures** for portal-created
  webhooks, because their secrets are rows. Chart-defined webhooks keep their
  keys in Kubernetes Secrets and never enter the database; use those for the
  channel that matters.
- **Rotating `config.resumeTokenKey` revokes every resume token fleet-wide**,
  instantly, for every broadcaster. It is the largest hammer in the box and it
  is not a moderation tool — but on a day when it is the right one, it is
  there.

## 10. Rooms (R42)

Optional, **off by default**, and off is byte-identical to a relay without
the feature ([`docs/44`](44-rooms.md) D17). A room is a collection of
broadcasts people join as participants: multi-POV watching with a roster.
The media path is untouched; a room is control-plane state.

Turn it on in the relay chart:

```yaml
rooms:
  enabled: true
  emptyGrace: "60s"        # keep longer than a client reconnect interval
  maxRooms: 10
  maxBroadcasts: 4
  maxParticipants: 50
  # createSecretRef: {name: gawk-room-invite, key: secret}   # optional gate on minting
```

That renders the `Room` CRD (kept on uninstall, like `Ban`), the room
knobs, and — in cluster mode — a Role that lets the pods write `Room` CRs
and read the Secrets static rooms reference. Dynamic rooms need nothing
else: a broadcaster mints one from the broadcaster page (or
`gawk-broadcast -room-new`), viewers type the six-character code into the
same join box a broadcast code goes in. `-room-create-secret` (chart
`rooms.createSecret`/`createSecretRef`) makes minting invite-only; static
rooms are unaffected by it.

**A static room** is a `Room` CR with a stable slug. The recipe by hand:

```bash
kubectl -n gawk create secret generic room-tuhisroom --from-literal=attachSecret=$(openssl rand -base64 18)
kubectl -n gawk apply -f - <<'EOF'
apiVersion: gawk.ioio.fi/v1alpha1
kind: Room
metadata: {name: tuhisroom}
spec:
  kind: static
  displayCode: TuhisRoom
  displayName: "Tuhis' room"
  attachSecretRef: {name: room-tuhisroom, key: attachSecret}
EOF
```

It is reachable at `https://<app>/#/room/TuhisRoom` on both pods the
moment it exists; the first participant's pod becomes its home. Anyone
with the link may watch; attaching a broadcast needs the attach secret
(`gawk-broadcast -room TuhisRoom -room-attach-secret …`, or the key field
in the broadcaster page's Room panel). Deleting the CR ends the room
everywhere with close code 4007. With `gawk-admin` installed and its
`rooms.enabled=true`, the portal's Rooms page does all of this — create,
rotate the attach secret (shown once), end a dynamic room — but read
[§9.7](#97-what-a-portal-compromise-yields)'s Secrets caveat in the admin
chart's `rbac.yaml` first.

**Without Kubernetes** (`docker compose`, a bare binary) static rooms come
from `-rooms-file` / `GAWK_ROOMS_FILE`: a JSON array of
`{"code": "TuhisRoom", "displayName": "…", "attachSecret": "…",
"maxBroadcasts": 4}`, reloaded on change and on SIGHUP exactly like
`-moderation-source=file`. Dynamic rooms are in-memory there and do not
survive a restart.

Observability: `/statusz` gains a `rooms` section keyed by the same HMAC'd
handle broadcasts use (never the code), `gawk_rooms_live{kind}`,
`gawk_room_participants{room}`, `gawk_room_attachments{room}` and
`gawk_room_proxied_sessions`; telemetry sessions carry the room key so the
R31 UI can list a room's sessions. Room codes are joinable secrets: they
appear in no log at Info, no webhook, and no `/statusz`.

## 11. Troubleshooting

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
| Portal answers 401 `idp_unavailable` on every request | The identity provider is unreachable and discovery has not resolved yet — see [§9.3](#93-the-identity-provider). `/readyz` says so too |
| Portal pods never go Ready | Postgres unreachable, or the schema is older than the binary's minimum — check the migration hook Job's logs |
| `gawk-admin` renders no Ingress and `helm` refuses the upgrade | `ingress.enabled` without all three of `oidc.issuer` / `oidc.clientId` / `oidc.audience` |
| `helm` refuses to render `gawk-admin` at all, naming `postgres.dsn` | Neither connection form is set. The chart does not create a database — [§9.1](#91-prerequisites-beyond-2) |
| `helm install gawk-admin` fails on the `-migrate` Job and nothing rolls | The database is not reachable yet (or does not exist). That is the pre-install hook doing its job; `kubectl -n gawk logs job/gawk-admin-migrate` |
| An IP ban blocked everybody | `service.externalTrafficPolicy: Cluster` — every publisher shared a node IP. [§9.4](#94-ip-bans-need-externaltrafficpolicy-local) |
| `kubectl get bans` says the resource type is unknown | The relay chart's `moderation.enabled` is off, so the CRD was never installed |
| A killed broadcaster came straight back | The kill cooldown elapsed, or the ban was on the ID and they minted a new one — [§9.4](#94-ip-bans-need-externaltrafficpolicy-local) |

Two starting points when something is stranger than the table:
[`docs/gotchas.md`](gotchas.md) is the consolidated list of things that have
already cost someone a debugging session, and [docs/13](13-observability.md)
maps metric signatures to causes.

Still stuck, or something here is wrong or missing? Open an issue — a
self-hosting report is a genuinely useful bug, since the reference deployment
cannot exercise every cluster shape.
