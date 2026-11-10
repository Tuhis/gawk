#!/usr/bin/env python3
"""The tables docs/88 §11 prints (R84, the browser Reliable uplink), generated
from matrix.py's r84-a, r84-b and r84-c158 runs so no number is transcribed by
hand. Prints markdown to stdout.

    python3 tools/uplinklab/r84tables.py

Viewer columns are the replay's presets (matrix.py PRESETS): Balanced is
"adaptive", Lowest latency "off", and Smoother / Most stable the relay as it
ships since docs/75 RU8 ("resilient-parity" / "deep-parity"); "Smoother, no
parity" is docs/75 table P8's column (the relay before RU8), kept because
F18's dip was measured there.
"""
import glob
import json
import os
import statistics

OUT = os.environ.get("UPLINK_OUT", os.path.join(os.path.dirname(os.path.abspath(__file__)), "out"))
R = {}
for p in glob.glob(f"{OUT}/results/*/*/summary.json"):
    mode = os.path.basename(os.path.dirname(p))
    name = os.path.basename(os.path.dirname(os.path.dirname(p)))
    R.setdefault(name, {})[mode] = json.load(open(p))

MODE_LABEL = {
    "c151-dgram": "datagrams (today, RU9)",
    "c151-carrier": "carrier, no gate",
    "c151-kfa": "carrier + keyframe gate",
    "c151-inline": "carrier, keyframe inline (D3)",
    "c151-inline-f12": "carrier, keyframe inline",
    "c158-dgram": "datagrams (today, RU9)",
    "c158-inline-f12": "carrier, keyframe inline",
}
PATH_LABEL = {
    "rtt10-clean": ("10 ms", "clean"), "rtt40-clean": ("40 ms", "clean"), "rtt100-clean": ("100 ms", "clean"),
    "rtt10-loss0.001": ("10 ms", "0.1 % loss"), "rtt10-loss0.005": ("10 ms", "0.5 % loss"),
    "rtt10-ge1pct": ("10 ms", "bursts (GE)"), "rtt10-out80drop": ("10 ms", "radio away 80 ms"),
    "rtt40-loss0.001": ("40 ms", "0.1 % loss"), "rtt40-ge1pct": ("40 ms", "bursts (GE)"),
    "rtt40-out80drop": ("40 ms", "radio away 80 ms"), "rtt100-loss0.001": ("100 ms", "0.1 % loss"),
    "hi-rtt10-clean": ("10 ms", "1440p clean"), "hi-rtt10-loss0.001": ("10 ms", "1440p 0.1 % loss"),
    "hi-rtt10-ge1pct": ("10 ms", "1440p bursts"), "hi-rtt10-rate80M": ("10 ms", "1440p 80 Mbps"),
    "hi-rtt10-rate80M-out80drop": ("10 ms", "1440p 80 Mbps + outages"),
    "rtt40-loss0.005": ("40 ms", "0.5 % loss"), "rtt100-loss0.01": ("100 ms", "1 % loss"),
}


def f(x, d=0):
    return "–" if x is None else f"{x:.{d}f}"


def sv(s, k):
    x = s["view"][k]
    return f"{f(min(100, x['shownPct']), 1)} % · {f(x['stallPct'], 1)} %"


SHIFTS = ("4.17", "8.33", "12.5")  # phase.py's


def phases(name, mode, k):
    """The run's replay of preset k at phase.py's four phases (the matrix's
    own, then the shifted ones that exist)."""
    d = f"{OUT}/results/{name}/{mode}"
    vs = [json.load(open(f"{d}/view-{k}.json"))]
    for sh in SHIFTS:
        p = f"{d}/view-{k}-lb{sh}.json"
        if os.path.exists(p):
            vs.append(json.load(open(p)))
    return vs


def med(name, mode, k):
    """shown · stalled, the median over the phases (docs/88 F1); a dagger
    when one phase showed 5 points fewer than the median."""
    vs = phases(name, mode, k)
    sh = statistics.median(min(100, v["shownPct"]) for v in vs)
    st = statistics.median(v["stallPct"] for v in vs)
    low = min(min(100, v["shownPct"]) for v in vs)
    mark = f" (one phase {low:.1f})" if sh - low >= 5 else ""
    return f"{sh:.1f} % · {st:.1f} %{mark}"


def nph(name, mode):
    return len(phases(name, mode, "adaptive"))


def lat(s, k):
    return f"{f(s['view'][k]['latencyMs']['p50'])} ms"


def ready(s):
    t = s["transport"]
    return f"{f(t['ready_lat_ms']['p50'])} / {f(t['ready_lat_ms']['p99'])} · {f(t['undecodable_pct'], 1)} %"


def keys(s):
    k = s["transport"]["key_lat_ms"]
    return f"{f(k['p50'])} / {f(k['p95'])}"


def steal(name, mode):
    p = f"{OUT}/results/{name}/{mode}/steal.txt"
    return open(p).read().strip() + " %" if os.path.exists(p) else "–"


def send_lines(name, mode, kind):
    p = f"{OUT}/results/{name}/{mode}/send.csv"
    if not os.path.exists(p):
        return []
    return [l.rstrip("\n").split(",") for l in open(p) if l.startswith(kind + ",")]


def sent_info(name, mode):
    r = {}
    for x in send_lines(name, mode, "I"):
        if x[1] in ("cc", "gap_us", "cpu", "thread"):
            r[x[1]] = x[2:]
    S = send_lines(name, mode, "S")
    F = send_lines(name, mode, "F")
    if S:
        r["S"] = [int(v) for v in S[-1][1:]]
    if F:
        r["dur"] = (int(F[-1][6]) - int(F[0][6])) / 1e9 + 1.5
    r["shed"] = len(send_lines(name, mode, "D"))
    r["kwait"] = len(send_lines(name, mode, "KQ"))
    return r


# --- (a) the Smoother dip behind the gate, 1440p, 10 ms, clean -------------


def phase0(s, k):
    x = s["view"][k]
    return f"{f(min(100, x['shownPct']), 1)} %"


runs_a = sorted((n for n in R if n.startswith("hi-rtt10-clean-r")), key=lambda n: int(n.rsplit("r", 1)[1]))
if runs_a:
    print("\n#### Table S1 — docs/75 F18's Smoother dip: 1440p, 10 ms RTT, clean, run by run\n")
    print("`matrix.py r84-a`, Chrome for Testing 151, then `phase.py`. Each viewer cell: shown at the matrix's own")
    print("replay (phase 0) → shown · stalled, the median over four presentation phases (F1), with the worst")
    print("phase when it is 5 points or more below the median. *Superseded*: frames the Balanced and the")
    print("Smoother (no parity) sinks replaced before showing them, at phase 0.\n")
    print("| Run | Sender | Balanced | Smoother, no parity | Smoother | Most stable | Superseded: Balanced · Smoother | "
          "Decode-ready p50 / p99 ms · undecodable | Keyframes p50 / p95 ms | Carriers expired | Steal |")
    print("|---|---|---|---|---|---|---|---|---|---|---|")
    for mode in ("c151-kfa", "c151-carrier", "c151-inline"):
        for n in runs_a:
            s = R[n].get(mode)
            if not s:
                continue
            v = s["view"]
            print(f"| {n.rsplit('-', 1)[1]} | {MODE_LABEL[mode]} | {phase0(s, 'adaptive')} → {med(n, mode, 'adaptive')} | "
                  f"{phase0(s, 'resilient')} → {med(n, mode, 'resilient')} | {med(n, mode, 'resilient-parity')} | "
                  f"{med(n, mode, 'deep-parity')} | {v['adaptive']['sinkDropped']} · {v['resilient']['sinkDropped']} | "
                  f"{ready(s)} | {keys(s)} | "
                  f"{s['transport']['carriers_expired']} of {s['transport']['carriers_total']} | {steal(n, mode)} |")

# --- (b) the gate's round trip, and the inline keyframe ---------------------

B_MODES = ("c151-dgram", "c151-kfa", "c151-inline-f12", "c151-inline")
order_b = [n for n in PATH_LABEL if n in R and any(m in B_MODES for m in R[n]) and n not in runs_a]
if order_b:
    print("\n#### Table S2 — the keyframe gate against the keyframe inline, with today's datagrams\n")
    print("`matrix.py r84-b`, Chrome for Testing 151, one 100 s run a cell, then `phase.py`. Viewer columns:")
    print("shown · stalled, the median over four presentation phases (F1); the Balanced viewer's on-screen")
    print("latency p50 at phase 0. *Inline* is `retire=f12` (§5.3) unless marked D3.\n")
    print("| RTT | Path | Sender | Balanced | Lowest latency | Smoother | Most stable | "
          "Decode-ready p50 / p99 ms · undecodable | Keyframes p50 / p95 ms | Deltas shed | Steal |")
    print("|---|---|---|---|---|---|---|---|---|---|---|")
    for n in order_b:
        rtt, path = PATH_LABEL[n]
        for mode in B_MODES:
            s = R[n].get(mode)
            if not s:
                continue
            i = sent_info(n, mode)
            print(f"| {rtt} | {path} | {MODE_LABEL[mode]} | {med(n, mode, 'adaptive')} · {lat(s, 'adaptive')} | "
                  f"{med(n, mode, 'off')} | {med(n, mode, 'resilient-parity')} | {med(n, mode, 'deep-parity')} | "
                  f"{ready(s)} | {keys(s)} | {i.get('shed', 0)} | {steal(n, mode)} |")

    # The gate's cost on clean paths, in one line per RTT.
    print("\n#### Table S3 — what the gate costs on a clean path, and what inline costs\n")
    print("Phase 0. *Latency*: the Balanced viewer's on-screen latency (encoder output to the screen, leg B")
    print("taken as instant), p50 / p95.\n")
    print("| RTT | Sender | Balanced latency p50 / p95 ms | Balanced offset mean ms | "
          "Lowest latency: shown · stalled | Decode-ready p50 / p95 / p99 ms |")
    print("|---|---|---|---|---|---|")
    for n in ("rtt10-clean", "rtt40-clean", "rtt100-clean"):
        for mode in ("c151-dgram", "c151-kfa", "c151-inline-f12"):
            s = R.get(n, {}).get(mode)
            if not s:
                continue
            a = s["view"]["adaptive"]
            r = s["transport"]["ready_lat_ms"]
            print(f"| {PATH_LABEL[n][0]} | {MODE_LABEL[mode]} | {f(a['latencyMs']['p50'])} / {f(a['latencyMs']['p95'])} | "
                  f"{f(a['offsetMs']['mean'])} | {med(n, mode, 'off')} | {f(r['p50'])} / {f(r['p95'])} / {f(r['p99'])} |")

# --- Chrome 158 against 151 -------------------------------------------------

c158 = [(n, m) for n in R for m in R[n] if m.startswith("c158-")]
if c158:
    print("\n#### Table S4 — Chrome for Testing 158 beside 151\n")
    print("`matrix.py r84-c158` beside the same cells' 151 runs where this pass has them. *Controller*: the")
    print("net-log's `QUIC_CONGESTION_CONTROL_CONFIGURED`. Packet gap: within a burst (under 5 ms), p50.\n")
    print("| Path | Chrome | Sender | Controller | Datagram frames sent / offered | Mbps on the wire | "
          "Packet gap p50 µs | CPU: network · renderer % | Balanced shown · stalled | Decode-ready p50 / p99 ms · undecodable |")
    print("|---|---|---|---|---|---|---|---|---|---|")
    for n in PATH_LABEL:
        for mode in ("c151-dgram", "c158-dgram", "c151-inline-f12", "c158-inline-f12"):
            if not any(m.startswith("c158-") and m[5:] == mode[5:] for m in R.get(n, {})):
                continue
            s = R.get(n, {}).get(mode)
            if not s:
                continue
            i = sent_info(n, mode)
            S = i.get("S")
            sent = f"{S[6]:,} / {S[7]:,}".replace(",", " ") if S else "–"
            mbps = f(S[8] * 8 / i["dur"] / 1e6, 1) if S and i.get("dur") else "–"
            cpu = i.get("cpu", ["", "", ""])
            print(f"| {' '.join(PATH_LABEL[n])} | {mode[1:4]} | {MODE_LABEL[mode]} | {i.get('cc', ['–'])[0]} | {sent} | "
                  f"{mbps} | {i.get('gap_us', ['', '–'])[1]} | {cpu[1]} · {cpu[2]} | {med(n, mode, 'adaptive')} | {ready(s)} |")

if not R:
    print("no results under", OUT)
