#!/usr/bin/env python3
"""docs/75 §11 table O from matrix.sh's runs in $UPLINK_OUT/fair: what a
household's TCP upload and the broadcast each get from one shared uplink, per
delivery mode and controller. Prints markdown to stdout."""
import json
import os
import re
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
LAB = os.path.dirname(HERE)
REPO = os.path.dirname(os.path.dirname(LAB))
OUT = os.environ.get("UPLINK_OUT", os.path.join(LAB, "out"))
RES = os.path.join(OUT, "fair")
TCP_FROM_S = 25   # TCP runs from 20 s; skip its slow start
VIEW_FROM_MS = 40000


def pct(xs, p):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int((len(xs) - 1) * p / 100))] if xs else None


def one(d):
    r = {}
    ip = os.path.join(d, "iperf.json")
    if os.path.exists(ip):
        j = json.load(open(ip))
        iv = [i["sum"]["bits_per_second"] / 1e6 for i in j["intervals"]][TCP_FROM_S - 20:]
        r["tcp"] = sum(iv) / len(iv)
        r["retx"] = j["end"]["sum_sent"].get("retransmits")
    pings = [float(m.group(2)) for m in (re.search(r"icmp_seq=(\d+).*time=([\d.]+)", l)
                                          for l in open(os.path.join(d, "ping.txt")))
             if m and int(m.group(1)) >= TCP_FROM_S * 5]
    r["ping50"], r["ping95"] = pct(pings, 50), pct(pings, 95)
    if os.path.exists(os.path.join(d, "send.csv")):
        out = subprocess.run(["python3", f"{LAB}/analyze.py", d, "--skip-s", str(TCP_FROM_S),
                              "--viewer-trace", f"{d}/vt.json"], capture_output=True, text=True, check=True)
        t = json.loads(out.stdout)
        r.update(undec=t["undecodable_pct"], ready99=t["ready_lat_ms"]["p99"], bmbps=t["uplink_mbps"])
        if "engine" in t:
            # The engine's upload-bandwidth warning (RU6): when it first
            # raised, s from the broadcast's start, and its share of the
            # analysis window (the upload's 25-80 s).
            r["warn_first"] = t["warn_first_s"]
            r["warn_pct"] = 100 * t["warn_raised_s"] / t["window_s"] if t["window_s"] else 0
        # quinn's own count; the browser sender's S line has none (Chrome
        # exposes no loss counter, browser/sender.mjs).
        if not any(l.startswith("I,cc,") for l in open(os.path.join(d, "send.csv"))):
            r["pktloss"] = t["pkt_loss_pct"]
    st = os.path.join(d, "steal.txt")
    if os.path.exists(st):
        r["steal"] = float(open(st).read())
    return r


runs = {}
for name in sorted(os.listdir(RES)) if os.path.isdir(RES) else []:
    d = os.path.join(RES, name)
    if os.path.exists(os.path.join(d, "done")):
        runs[name] = one(d)

# The viewer replay, Balanced and Lowest latency, over the second half.
with tempfile.NamedTemporaryFile("w", suffix=".txt", delete=False) as b:
    for name in runs:
        d = os.path.join(RES, name)
        if os.path.exists(f"{d}/vt.json"):
            b.write(f"{d}/vt.json adaptive {d}/view-adaptive.json live\n")
            b.write(f"{d}/vt.json off {d}/view-off.json live\n")
    batch = b.name
rep = subprocess.run(["npx", "vitest", "run", "lab/uplink-replay.lab.test.ts", "--testTimeout", "3600000"], cwd=f"{REPO}/gawk-app",
                     env=dict(os.environ, UPLINK_BATCH=batch, UPLINK_SKIP_MS=str(VIEW_FROM_MS)),
                     capture_output=True, text=True)
if rep.returncode != 0:
    sys.exit(f"replay failed (batch {batch}):\n{rep.stdout[-4000:]}")
os.unlink(batch)
for name, r in runs.items():
    d = os.path.join(RES, name)
    for k in ("adaptive", "off"):
        if os.path.exists(f"{d}/view-{k}.json"):
            v = json.load(open(f"{d}/view-{k}.json"))
            r[k] = {"shown": min(100, v["shownPct"]), "stall": v["stallPct"], "lat": v["latencyMs"]["p50"]}

cells = {}
for name, r in runs.items():
    cells.setdefault(re.sub(r"-s\d+$", "", name), []).append(r)


def vals(rs, path):
    out = []
    for r in rs:
        x = r
        for k in path:
            x = x.get(k) if isinstance(x, dict) else None
        if x is not None:
            out.append(x)
    return out


def agg(rs, *path, d=0, unit=""):
    xs = vals(rs, path)
    if not xs:
        return "–"
    m = sum(xs) / len(xs)
    if len(xs) == 1 or round(max(xs), d) == round(min(xs), d):
        return f"{m:.{d}f}{unit}"
    return f"{m:.{d}f}{unit} ({min(xs):.{d}f}–{max(xs):.{d}f})"


def mean(rs, *path):
    xs = vals(rs, path)
    return sum(xs) / len(xs) if xs else None


RATES = [("30", "30 Mbps, 1080p (9.5 Mbps mean, 12 ceiling)"), ("15", "15 Mbps, 1080p"),
         ("80", "80 Mbps, 1440p (38.9 Mbps mean, 50 ceiling)")]
MODES = [("datagram", "cubic", "datagram · Cubic (today)"), ("datagram", "bbr", "datagram · BBR"),
         ("carrier", "cubic", "carrier · Cubic"), ("carrier", "bbr", "carrier · BBR"),
         # OD5: the browser sender (fair/chrome.sh); Chrome's QUIC runs Cubic.
         ("datagram", "chrome", "Chrome datagrams (today's browser)"), ("carrier", "chrome", "Chrome carrier (prototype)"),
         # RU6: the shipped engine (FAIR_CCS="engine-cubic engine-bbr").
         ("datagram", "engine-cubic", "engine: datagram · Cubic"), ("datagram", "engine-bbr", "engine: datagram · BBR"),
         ("carrier", "engine-cubic", "engine: carrier · Cubic"), ("carrier", "engine-bbr", "engine: carrier · BBR"),
         # R74 PP8: the PoP's leg writer (FAIR_CCS=pop); its quic-go runs Cubic.
         ("datagram", "pop", "PoP leg: datagram · Cubic"), ("carrier", "pop", "PoP leg: carrier · Cubic")]

print("#### Table O — a shared uplink: the broadcast beside a household's TCP upload (F17, D5)\n")
print("`tools/uplinklab/fair` (§4): one uplink at 30, 15 or 80 Mbps with a 50 ms\n"
      "drop-tail queue, the downlink unshaped; the broadcast for 80 s, a bulk TCP\n"
      "upload (kernel Cubic) from 20 s. Three runs a cell at 1080p, two at 1440p\n"
      "(alone on the host); the Chrome rows (`fair/chrome.sh`) two beside the\n"
      "upload and one alone at every rate. Mean, range in brackets.\n")
print("**O1 — what the household's upload gets.** *Of its fair share* is against its\n"
      "max-min fair share: what it gets alone, less the broadcast's own rate when\n"
      "that is under half the link, else half the link. Ping is the household's,\n"
      "while the upload runs.\n")
print("| Uplink | Broadcast | TCP Mbps | Of its fair share | TCP retransmits | Ping p50 / p95 ms |")
print("|---|---|---|---|---|---|")
for rate, label in RATES:
    none = cells.get(f"r{rate}-none")
    if not none:
        continue
    cap = mean(none, "tcp")
    print(f"| {label} | none | {agg(none, 'tcp', d=1)} | – | {agg(none, 'retx')} | "
          f"{agg(none, 'ping50')} / {agg(none, 'ping95')} |")
    for m, c, ml in MODES:
        rs, alone = cells.get(f"r{rate}-{m}-{c}-tcp"), cells.get(f"r{rate}-{m}-{c}-alone")
        if not rs:
            continue
        demand = mean(alone, "bmbps") if alone else None
        fair = cap - demand if demand is not None and demand < cap / 2 else cap / 2
        share = [100 * r["tcp"] / fair for r in rs if "tcp" in r]
        sh = f"{sum(share) / len(share):.0f} %" + (f" ({min(share):.0f}–{max(share):.0f})" if len(share) > 1 else "")
        print(f"| | {ml} | {agg(rs, 'tcp', d=1)} | {sh} | {agg(rs, 'retx')} | "
              f"{agg(rs, 'ping50')} / {agg(rs, 'ping95')} |")

if "r80-datagram-chrome-alone" in cells:
    alone = mean(cells["r80-datagram-chrome-alone"], "bmbps")
    print(f"\nChrome's datagrams cannot carry the 1440p stream (F18: {alone:.0f} Mbps on the wire\n"
          "alone, for a stream of 38.9 Mbps mean), so at 80 Mbps their share is computed\n"
          "against a stream that never ran at its rate; the Chrome carrier's row is the\n"
          "comparable one there.")
print("\n**O2 — what the broadcast gets, alone on the uplink → beside the upload.**")
print("Packets lost (quinn's count; Chrome exposes none), undecodable and\n"
      "decode-ready cover the upload (25–80 s); the viewer columns its last 40 s,\n"
      "after the playout controller settled. *Undecodable* is strict (a frame after\n"
      "a hole in its GOP counts); the viewers skip one lost delta per GOP (R29), so\n"
      "they can show more.\n")
print("| Uplink | Broadcast | quinn: packets lost | Undecodable | Decode-ready p99 ms | Balanced: shown · stalled | "
      "Balanced: on-screen latency p50 ms | Lowest latency: stalled |")
print("|---|---|---|---|---|---|---|---|")
for rate, label in RATES:
    first = True
    for m, c, ml in MODES:
        a, t = cells.get(f"r{rate}-{m}-{c}-alone"), cells.get(f"r{rate}-{m}-{c}-tcp")
        if not a or not t:
            continue

        def pair(*path, d=0, unit=""):
            return f"{agg(a, *path, d=d, unit=unit)} → {agg(t, *path, d=d, unit=unit)}"

        print(f"| {label if first else ''} | {ml} | {pair('pktloss', d=1, unit=' %')} | {pair('undec', d=1, unit=' %')} | {pair('ready99')} | "
              f"{agg(a, 'adaptive', 'shown', d=1)} · {agg(a, 'adaptive', 'stall', d=1)} → "
              f"{agg(t, 'adaptive', 'shown', d=1)} · {agg(t, 'adaptive', 'stall', d=1)} % | "
              f"{pair('adaptive', 'lat')} | {pair('off', 'stall', d=1, unit=' %')} |")
        first = False

# RU6: the shipped engine's rows judged against RU4's shared-uplink gate, with
# its upload-bandwidth warning and the host's steal.
ENG = [(m, c, ml) for m, c, ml in MODES if c.startswith("engine-")]


def shares(rate, m, c):
    none, rs = cells.get(f"r{rate}-none"), cells.get(f"r{rate}-{m}-{c}-tcp")
    alone = cells.get(f"r{rate}-{m}-{c}-alone")
    if not none or not rs:
        return []
    cap = mean(none, "tcp")
    demand = mean(alone, "bmbps") if alone else None
    fair = cap - demand if demand is not None and demand < cap / 2 else cap / 2
    return [100 * r["tcp"] / fair for r in rs if "tcp" in r]


def warned(rs):
    if not rs:
        return "–"
    fired = [r for r in rs if r.get("warn_first") is not None]
    if not fired:
        return f"never (0/{len(rs)})"
    return (f"{len(fired)}/{len(rs)} runs, first at {agg(fired, 'warn_first', d=1)} s · "
            f"shown {agg(rs, 'warn_pct')} % of 25–80 s")


if any(cells.get(f"r{rate}-{m}-{c}-tcp") for rate, _ in RATES for m, c, _ in ENG):
    print("\n#### Table O3 — the shipped engine on RU4's shared-uplink gate (RU6)\n")
    print("`FAIR_CCS=\"engine-cubic engine-bbr\" FAIR_NONE=0 fair/matrix.sh`: `bin/sender-engine` in the same\n"
          "harness, beside table O's TCP-alone runs. The gate (30 and 80 Mbps, BBR's rows): the upload keeps\n"
          "≥ 50 % of its fair share, and beside it the carrier leaves ≤ 0.5 % undecodable and BBR's datagrams\n"
          "show no fewer frames than the engine's Cubic datagrams beyond 3 points. 15 Mbps is recorded, not\n"
          "gated: there the criterion is the upload-bandwidth warning, which must show in both delivery modes\n"
          "within its 5 s hysteresis — beside the upload, of its start at 20 s; alone, where the trace outgrows\n"
          "the uplink only partway in and not in every run, in every run whose broadcast was damaged (≥ 1 %\n"
          "undecodable). Since 2026-10-21 the warning is a capacity signal and never reads packet loss, so\n"
          "where BBR holds the stream's rate and only its own loss moves it can miss the link; that half is\n"
          "open and moved to R75 by owner decision. The 15 Mbps verdicts say whether it showed. *Warning*: the runs it raised\n"
          "in, when it first raised (s after the broadcast's first\n"
          "frame) and its share of 25–80 s. *Steal*: the host's, over each run (max).\n")
    print("| Uplink | Engine | Upload's fair share | Beside the upload: undecodable · Balanced shown | "
          "Warning, alone | Warning, beside the upload | RU4 | Steal |")
    print("|---|---|---|---|---|---|---|---|")
    for rate, label in RATES:
        first = True
        for m, c, ml in ENG:
            a, t = cells.get(f"r{rate}-{m}-{c}-alone"), cells.get(f"r{rate}-{m}-{c}-tcp")
            if not t:
                continue
            sh = shares(rate, m, c)
            share = (f"{sum(sh) / len(sh):.0f} %" + (f" ({min(sh):.0f}–{max(sh):.0f})" if len(sh) > 1 else "")) \
                if sh else "–"
            if rate == "15":
                # Beside the upload: raised within the hysteresis (5 bad
                # seconds, the first sample and a second of slack) of the
                # upload's start at 20 s. Alone the trace only outgrows the
                # uplink partway in, and not in every run, so: raised in
                # every run whose broadcast was damaged (≥ 1 % undecodable).
                ok = bool(a) and all(r.get("warn_first") is not None for r in a if r.get("undec", 0) >= 1) \
                    and all(r.get("warn_first") is not None and r["warn_first"] <= 27 for r in t)
                # Recorded (RU4, amended 2026-10-21): where only BBR's own
                # loss moves, the loss notice speaks, not this warning.
                verdict = "warning " + ("shown" if ok else "not shown")
            elif c != "engine-bbr":
                verdict = "baseline" if m == "datagram" else "recorded"
            else:
                ok = bool(sh) and min(sh) >= 50
                if m == "carrier":
                    ok = ok and max(vals(t, ("undec",))) <= 0.5
                elif c == "engine-bbr":
                    cub = mean(cells.get(f"r{rate}-datagram-engine-cubic-tcp", []), "adaptive", "shown")
                    ok = ok and cub is not None and mean(t, "adaptive", "shown") >= cub - 3
                verdict = "pass" if ok else "**fail**"
            st = vals((a or []) + t, ("steal",))
            print(f"| {label if first else ''} | {ml} | {share} | {agg(t, 'undec', d=1, unit=' %')} · "
                  f"{agg(t, 'adaptive', 'shown', d=1, unit=' %')} | {warned(a)} | {warned(t)} | {verdict} | "
                  f"{(f'{max(st):.1f} %') if st else '–'} |")
            first = False
