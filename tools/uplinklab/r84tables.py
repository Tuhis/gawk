#!/usr/bin/env python3
"""The tables docs/88 §11 prints (R84, the browser Reliable uplink), generated
from matrix.py's r84-a, r84-b, r84-c158 and r84-br4 runs so no number is
transcribed by hand. Prints markdown to stdout.

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
    # BR4: the shipping code, both delivery modes.
    "c151-reliable": "Reliable (shipping)",
    "c157-reliable": "Reliable (shipping)",
    "c158-reliable": "Reliable (shipping)",
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

# --- BR4: the shipping code (docs/88 §8) -------------------------------------


def kf_outcomes(name, mode):
    """Keyframes produced (F lines with the key flag) and how each ended (K
    lines' outcome), from the sender's log."""
    produced = sum(1 for x in send_lines(name, mode, "F") if x[2] == "1")
    out = {}
    for x in send_lines(name, mode, "K"):
        out[x[4]] = out.get(x[4], 0) + 1
    return produced, out


def med_shown(name, mode, k):
    return statistics.median(min(100, v["shownPct"]) for v in phases(name, mode, k))


BR4_MODES = [m for m in ("c151-dgram", "c151-reliable", "c157-dgram", "c157-reliable", "c158-dgram", "c158-reliable")
             if any(m in R[n] for n in R)]
if any(m.endswith("-reliable") for m in BR4_MODES):
    order = [n for n in PATH_LABEL if n in R and any(m in R[n] for m in BR4_MODES) and n not in runs_a]
    print("\n#### Table S5 — BR4: the shipping code, Lowest latency against Reliable\n")
    print("`matrix.py r84-br4`, one 100 s run a cell, then `phase.py`. *Lowest latency* is RU9's datagram path")
    print("as it ships; *Reliable* the shipping `KeyframeSender` GOP streams and `CarrierSender` (BR2) on the")
    print("lab page. Viewer columns: shown · stalled, the median over four presentation phases (F1); the")
    print("Balanced viewer's on-screen latency p50 at phase 0. *Keyframes*: acknowledged of produced, and")
    print("how many the 2 s rule aborted.\n")
    print("| RTT | Path | Chrome | Sender | Balanced | Lowest latency | Smoother | Most stable | "
          "Decode-ready p50 / p99 ms · undecodable | Keyframes acked / produced · aborted | Deltas shed | Steal |")
    print("|---|---|---|---|---|---|---|---|---|---|---|---|")
    for n in order:
        rtt, path = PATH_LABEL[n]
        for mode in BR4_MODES:
            s = R[n].get(mode)
            if not s:
                continue
            i = sent_info(n, mode)
            produced, out = kf_outcomes(n, mode)
            label = "datagrams (Lowest latency, RU9)" if mode.endswith("-dgram") else MODE_LABEL[mode]
            print(f"| {rtt} | {path} | {mode[1:4]} | {label} | {med(n, mode, 'adaptive')} · {lat(s, 'adaptive')} | "
                  f"{med(n, mode, 'off')} | {med(n, mode, 'resilient-parity')} | {med(n, mode, 'deep-parity')} | "
                  f"{ready(s)} | {out.get('sent', 0)} / {produced} · {out.get('aborted', 0)} | {i.get('shed', 0)} | "
                  f"{steal(n, mode)} |")

    print("\n#### Table S6 — BR4's goals on the shipping code\n")
    print("G1–G5 (§8) judged on table S5's runs. G1: Reliable against Lowest latency, the Balanced viewer's")
    print("on-screen latency p50 (phase 0) within one vsync (17 ms) and shown (four-phase median) within 1")
    print("point. G5's notice half is the unit table test (`uplink-monitor.test.ts`).\n")
    print("| Goal | Chrome | Cell | Lowest latency | Reliable | Bound | Met |")
    print("|---|---|---|---|---|---|---|")

    def row(goal, major, n, dg, rel, bound, ok):
        print(f"| {goal} | {major} | {' '.join(PATH_LABEL[n])} | {dg} | {rel} | {bound} | {'yes' if ok else '**no**'} |")

    for major in ("151", "157", "158"):
        dgm, rel = f"c{major}-dgram", f"c{major}-reliable"
        for n in ("rtt10-clean", "rtt40-clean", "rtt100-clean"):
            a, b = R.get(n, {}).get(dgm), R.get(n, {}).get(rel)
            if not a or not b:
                continue
            la, lb = a["view"]["adaptive"]["latencyMs"]["p50"], b["view"]["adaptive"]["latencyMs"]["p50"]
            sa, sb = med_shown(n, dgm, "adaptive"), med_shown(n, rel, "adaptive")
            row("G1", major, n, f"{f(la)} ms · {sa:.1f} %", f"{f(lb)} ms · {sb:.1f} %",
                "Δ ≤ 17 ms · Δ ≤ 1 pt", abs(lb - la) <= 17 and abs(sb - sa) <= 1)
        for n in ("rtt10-loss0.001", "rtt10-loss0.005"):
            b = R.get(n, {}).get(rel)
            if not b:
                continue
            sb = med_shown(n, rel, "adaptive")
            sa = f"{med_shown(n, dgm, 'adaptive'):.1f} %" if R[n].get(dgm) else "–"
            row("G2", major, n, sa, f"{sb:.1f} %", "Balanced ≥ 99 %", sb >= 99)
        for n, bound in (("rtt10-ge1pct", 98), ("rtt10-out80drop", 98), ("rtt40-ge1pct", 95), ("rtt40-out80drop", 95)):
            if not R.get(n, {}).get(rel):
                continue
            vals = [med_shown(n, rel, k) for k in ("resilient-parity", "deep-parity")]
            dvals = ([f"{med_shown(n, dgm, k):.1f}" for k in ("resilient-parity", "deep-parity")]
                     if R[n].get(dgm) else ["–", "–"])
            # Two decimals: a bound met or missed by less than 0.05 point
            # must not read as met.
            row("G3", major, n, " · ".join(dvals) + " %", " · ".join(f"{v:.2f}" for v in vals) + " %",
                f"Smoother · Most stable ≥ {bound} %", min(vals) >= bound)
        for n in ("hi-rtt10-clean", "hi-rtt10-rate80M"):
            if not R.get(n, {}).get(rel):
                continue
            sb = med_shown(n, rel, "adaptive")
            sa = f"{med_shown(n, dgm, 'adaptive'):.1f} %" if R[n].get(dgm) else "–"
            row("G4", major, n, sa, f"{sb:.1f} %", "Balanced ≥ 97 %", sb >= 97)
        for n in ("rtt40-loss0.005", "rtt100-loss0.01"):
            a, b = R.get(n, {}).get(dgm), R.get(n, {}).get(rel)
            if not a or not b:
                continue
            pa, oa = kf_outcomes(n, dgm)
            pb, ob = kf_outcomes(n, rel)
            ready_b = b["transport"]["ready_lat_ms"]["p50"]
            other = sum(v for k, v in ob.items() if k not in ("sent", "aborted", "replaced", "discarded"))
            ok = ready_b < 1500 and ob.get("sent", 0) >= oa.get("sent", 0) - 0.05 * pb and other == 0
            row("G5", major, n, f"{f(a['transport']['ready_lat_ms']['p50'])} ms · {oa.get('sent', 0)} of {pa} keyframes",
                f"{f(ready_b)} ms · {ob.get('sent', 0)} of {pb} keyframes, {other} ended otherwise",
                "decode-ready p50 < 1.5 s; keyframes ≥ datagrams' − 5 %; only RU9's rules", ok)

if not R:
    print("no results under", OUT)
