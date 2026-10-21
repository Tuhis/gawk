#!/usr/bin/env python3
"""docs/75 §11 tables G and N, from the real-relay runs: relay.sh's runs in
$UPLINK_OUT/relay/<run> (table G, F7) and queue.sh's in $UPLINK_OUT/queue
(table N, F16). Prints markdown to stdout."""
import json
import os
import re
import subprocess

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.environ.get("UPLINK_OUT", os.path.join(HERE, "out"))


def metric(text, name):
    m = re.search(r"^" + re.escape(name) + r" ([0-9.e+]+)$", text, re.M)
    if not m:
        raise SystemExit(f"relaytables: no {name} in metrics.txt (renamed?)")
    return float(m.group(1))


def run(d):
    mt = open(f"{d}/metrics.txt").read()
    r = {"lost": metric(mt, "gawk_relay_ingress_chunks_lost_total"),
         "qfull": metric(mt, 'gawk_relay_datagrams_dropped_total{reason="queue_full"}'),
         "relayed": metric(mt, "gawk_relay_datagrams_relayed_total")}
    if os.path.exists(f"{d}/relay-mem.txt"):
        m = re.search(r"VmHWM:\s+(\d+)", open(f"{d}/relay-mem.txt").read())
        r["rss"] = int(m.group(1)) / 1024 if m else None
    if os.path.exists(f"{d}/steal.txt"):  # queue.sh since RU2's run
        r["steal"] = float(open(f"{d}/steal.txt").read().split()[0])
    if os.path.exists(f"{d}/recv.csv"):
        out = subprocess.run(["python3", f"{HERE}/analyze.py", d, "--skip-s", "5"],
                             capture_output=True, text=True, check=True)
        t = json.loads(out.stdout)
        r.update(undec=t["undecodable_pct"], p50=t["delta_lat_ms"]["p50"], p99=t["delta_lat_ms"]["p99"],
                 ready99=t["ready_lat_ms"]["p99"])
    return r


def n(x):
    return f"{x:,.0f}".replace(",", " ")


# Table G: which build each of F7's runs used.
G = [("R1", "1440p", 15, "stock"), ("R2", "1440p", 0, "stock"),
     ("R-only128", "1440p", 15, "quic-go connection queue 128 → 4096"),
     ("R-only32", "1440p", 15, "**`http3` session queue 32 → 4096**"),
     ("R7", "1440p", 15, "both queues → 4096"), ("R8", "1440p", 0, "both queues → 4096"),
     ("R4", "1080p", 15, "stock"), ("R9", "1080p", 0, "stock")]
print("#### Table G — the real relay's ingress on a clean path (F7, D7)\n")
print("| Run | Rung | Viewers | Relay build | Ingress chunks lost (60 s) | Dropped to viewers (`queue_full`) |")
print("|---|---|---|---|---|---|")
for name, rung, v, build in G:
    d = f"{OUT}/relay/{name}"
    if os.path.exists(f"{d}/metrics.txt"):
        r = run(d)
        print(f"| {name} | {rung} | {v} | {build} | {n(r['lost'])} | {n(r['qfull'])} |")

# Table N: the queue by depth, three (or two) runs a cell, mean and range.
cells = {}
qdir = f"{OUT}/queue"
for name in sorted(os.listdir(qdir)) if os.path.isdir(qdir) else []:
    if ".failed." not in name and os.path.exists(f"{qdir}/{name}/metrics.txt"):
        cells.setdefault(re.sub(r"-s\d+$", "", name), []).append(run(f"{qdir}/{name}"))


def agg(rs, k, d=0, unit=""):
    xs = [r[k] for r in rs if r.get(k) is not None]
    if not xs:
        return "–"
    fmt = (lambda x: n(x)) if d == 0 else (lambda x: f"{x:.{d}f}")
    m = sum(xs) / len(xs)
    return f"{fmt(m)}{unit}" if len(xs) == 1 or max(xs) == min(xs) else f"{fmt(m)}{unit} ({fmt(min(xs))}–{fmt(max(xs))})"


# key, label, subscribers (gawk-loadgen viewers + the lab's measuring viewer)
LOADS = [("v2", "1440p, 3 cores, 2 viewers", 3), ("v4", "1440p, 3 cores, 4 viewers", 5),
         ("v15", "1440p, 3 cores, 15 viewers (fan-out saturated)", 16),
         ("o1080", "1080p, 1 core, 15 viewers (overloaded)", 16),
         ("o1440", "1440p, 1 core, 15 viewers (saturated)", 16)]
print("\n#### Table N — the relay's datagram queue by depth (F7, F16, D7)\n")
print("`tools/uplinklab/queue.sh`: table G's setup with a lab viewer on the\n"
      "4096-deep build attached as well (its per-frame log gives latency through\n"
      "the relay); \"1 core\" pins the relay to one core instead of three. The\n"
      "viewer counts are `gawk-loadgen`'s; the lab viewer is one more. *Dropped to\n"
      "viewers* is `queue_full` as a share of the fan-out (datagrams relayed ×\n"
      "subscribers). Mean of the runs, range in brackets.\n")
# Runs that recorded the host's CPU steal (queue.sh since RU2) get a column for it.
STEAL = any(r.get("steal") is not None for rs in cells.values() for r in rs)
print("| Load | Depth | Runs | Ingress chunks lost (60 s) | Dropped to viewers | Viewer: undecodable | "
      "Viewer: chunk latency p50 / p99 ms | Viewer: decode-ready p99 ms | Relay peak RSS |"
      + (" Host CPU steal |" if STEAL else ""))
print("|---|---|---|---|---|---|---|---|---|" + ("---|" if STEAL else ""))
for key, label, subs in LOADS:
    for q in ("32", "128", "512", "4096"):
        rs = cells.get(f"{key}-q{q}")
        if not rs:
            continue
        for r in rs:
            r["fanout"] = 100 * r["qfull"] / (r["relayed"] * subs) if r["relayed"] else 0
        print(f"| {label} | {q} | {len(rs)} | {agg(rs, 'lost')} | "
              f"{agg(rs, 'fanout', 1, ' %')} | {agg(rs, 'undec', 1, ' %')} | "
              f"{agg(rs, 'p50')} / {agg(rs, 'p99')} | {agg(rs, 'ready99')} | {agg(rs, 'rss', 0, ' MB')} |"
              + (f" {agg(rs, 'steal', 1, ' %')} |" if STEAL else ""))
