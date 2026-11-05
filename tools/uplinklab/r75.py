#!/usr/bin/env python3
"""R75 (docs/85): the upload-bandwidth warning against a full uplink, from the
engine sender's runs (bin/sender-engine; its T, B, K, F, N and Y lines).

  r75.py seconds <run-dir> [--from S]     one row a second, for reading a run
  r75.py fair [<fair-dir>] [--replay]     docs/85 tables U1 and U3 (fair/run.sh's runs)
  r75.py matrix [<results-dir>] [--replay] docs/85 table U4 (matrix.py's engine runs)

--replay replays the R75 monitor over each run's own counters (keyframe
drain from K and F lines against the wire rate from T lines, OR-ed with the
RU4 signals from Y lines, carrier expiries gated on the T lines' minimum RTT,
through the monitor's hysteresis): how §2 judged runs made before the engine
had the rule. R75_SLOW_SHARE sweeps the drain threshold (default 0.75). Without it the warning columns are
the run's own N lines — what the engine in the run actually showed.
Undecodable is analyze.py's, over the run from 25 s (fair) or whole (matrix).
"""
import glob
import json
import os
import re
import subprocess
import sys
from collections import defaultdict

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.environ.get("UPLINK_OUT", os.path.join(HERE, "out"))
SLOW_SHARE = float(os.environ.get("R75_SLOW_SHARE", "0.75"))  # drain::SLOW_DRAIN_SHARE
WARN_AFTER, CLEAR_AFTER = 5, 15                                 # uplink.rs
EXPIRY_MAX_MIN_RTT_S = 0.05                                     # CARRIER_EXPIRY_MAX_MIN_RTT_MS
FROM_S, TO_S = 25, 80                                           # table O3's window


def q(xs, f):
    xs = sorted(x for x in xs if x == x)
    return xs[min(len(xs) - 1, int(f * len(xs)))] if xs else float("nan")


def load(d):
    """The send log's lines by kind."""
    r = defaultdict(list)
    for line in open(f"{d}/send.csv"):
        p = line.rstrip("\n").split(",")
        if p[0] in "TBKFNY":
            r[p[0]].append(p)
    return r


def first_frame_ns(lg):
    fs = [int(p[6]) for p in lg["F"]]
    return min(fs) if fs else int(lg["T"][0][1])


def per_second(lg):
    """One dict a tick: wire Mbps, the B line's medians, queueing delay and
    loss — what quinn and its controller said."""
    t0 = first_frame_ns(lg)
    out = []
    ts, bs = lg["T"], lg["B"]
    for i in range(1, len(ts)):
        a, b = ts[i - 1], ts[i]
        if len(b) < 12:
            continue
        dt = (int(b[1]) - int(a[1])) / 1e9
        sent, lost = int(b[4]) - int(a[4]), int(b[5]) - int(a[5])
        bl = bs[i] if i < len(bs) else None
        out.append(dict(
            t=(int(b[1]) - t0) / 1e9,
            tx=(int(b[10]) - int(a[10])) * 8 / dt / 1e6 if dt > 0 else 0.0,
            pace=int(bl[4]) / 1e6 if bl else float("nan"),
            cwnd=int(bl[7]) / 1e6 if bl else float("nan"),
            qd=(int(b[2]) - int(b[11])) / 1000,
            loss=lost / sent if sent else 0.0,
            min_rtt=int(b[11]) / 1e6,
        ))
    return out


def keyframes(lg):
    """(t_acked_ns, drain Mbps or 0 for a stream that never finished)."""
    size = {int(p[1]): int(p[3]) for p in lg["F"]}
    min_rtt = min((int(p[11]) for p in lg["T"] if len(p) >= 12), default=0) / 1e6
    out = []
    for p in lg["K"]:
        fid, a, b, outcome = int(p[1]), int(p[2]), int(p[3]), p[4]
        if outcome != "sent":
            out.append((b, 0.0))
            continue
        dur = max((b - a) / 1e9 - min_rtt, 1e-3)
        out.append((b, size.get(fid, 0) * 8 / dur / 1e6))
    return out


def hysteresis(flags):
    bad = good = 0
    shown, out = False, []
    for f in flags:
        if f:
            bad, good = bad + 1, 0
            shown = shown or bad >= WARN_AFTER
        else:
            good, bad = good + 1, 0
            shown = shown and good < CLEAR_AFTER
        out.append(shown)
    return out


def replayed(d, lg):
    """[(t, shown)] with the R75 rule replayed over the run's counters."""
    carrier = "carrier" in d
    t0 = first_frame_ns(lg)
    ys, ts = lg["Y"], [p for p in lg["T"] if len(p) >= 12]
    kfs = keyframes(lg)
    rates, flags, secs = [], [], []
    for i in range(1, len(ys)):
        a = [float(x) for x in ys[i - 1][1:]]
        b = [float(x) for x in ys[i][1:]]
        dl = lambda j: b[j] - a[j]  # noqa: E731  (Y: t, warned, enc, sent, sup, fail, drop, ps, pl, co, ce, rs, dd)
        min_rtt = int(ts[min(i, len(ts) - 1)][11]) / 1e6 if ts else 0
        expiry = min_rtt <= EXPIRY_MAX_MIN_RTT_S and dl(10) > 0 and dl(10) >= dl(9)
        ru4 = (dl(4) + dl(5) >= 1 or dl(6) > 0 or (b[2] > 20 and b[3] < b[2] * 0.75)
               or ((dl(11) > 0 or expiry) if carrier else dl(12) >= 10))
        if i < len(ts):
            dt = (int(ts[i][1]) - int(ts[i - 1][1])) / 1e9
            rates.append((int(ts[i][10]) - int(ts[i - 1][10])) * 8 / dt / 1e6 if dt > 0 else 0)
        rate = sum(rates[-5:]) / len(rates[-5:]) if rates else 0
        drains = [g for k, g in kfs if a[0] < k <= b[0]]
        slow = [g for g in drains if g < SLOW_SHARE * rate]
        flags.append(ru4 or (len(slow) >= 1 and len(slow) == len(drains)))
        secs.append((b[0] - t0) / 1e9)
    return list(zip(secs, hysteresis(flags)))


def recorded(lg):
    """[(t, shown)] from the run's Y lines: what its own engine showed."""
    t0 = first_frame_ns(lg)
    return [((int(p[1]) - t0) / 1e9, p[2] == "1") for p in lg["Y"]]


def verdict(series):
    first = next((t for t, s in series if s), None)
    win = [s for t, s in series if FROM_S <= t < TO_S]
    return first, 100 * sum(win) / len(win) if win else 0.0


def undecodable(d, skip):
    path = f"{d}/r75-transport.json"
    if not os.path.exists(path):
        subprocess.run(["python3", f"{HERE}/analyze.py", d, "--skip-s", str(skip), "--json", path],
                       check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    return json.load(open(path))["undecodable_pct"]


def drain_stats(lg, rows):
    rate = q([r["tx"] for r in rows if r["t"] >= FROM_S], .5)
    gs = [g for k, g in keyframes(lg) if (k - first_frame_ns(lg)) / 1e9 >= FROM_S]
    return q(gs, .5), 100 * sum(1 for g in gs if g < SLOW_SHARE * rate) / max(1, len(gs))


def mean(xs):
    xs = [x for x in xs if x == x]
    return sum(xs) / len(xs) if xs else float("nan")


def warn_cell(vs, from_s):
    """'n/m runs, first at …' over a cell's runs' (first, share) verdicts."""
    raised = [(f, s) for f, s in vs if f is not None]
    if not raised:
        return f"never (0/{len(vs)})"
    firsts = sorted(f for f, _ in raised)
    late = 0 if from_s is None else sum(1 for f in firsts if f > from_s + WARN_AFTER + 1)
    return (f"{len(raised)}/{len(vs)}, first at {firsts[0]:.0f}–{firsts[-1]:.0f} s"
            + (f" ({late} late)" if late else "") + f", shown {mean([s for _, s in vs]):.0f} %")


def fair(base, replay):
    cells = defaultdict(list)
    for d in sorted(glob.glob(f"{base}/r*-*-s*")):
        m = re.match(r"r(\d+)-(datagram|carrier)-engine-(bbr|cubic)-(tcp|alone)-s\d+$", os.path.basename(d))
        if not m or not os.path.exists(f"{d}/done"):
            continue
        cells[m.groups()].append(d)
    print("#### U1 — what quinn's controller says, and what a keyframe says (medians over 25–80 s)\n")
    print("| Uplink | Delivery · controller | Upload | Runs | Undecodable | Wire Mbps | BBR pacing rate | "
          "Window / RTT | Keyframe drain | Keyframes slow |")
    print("|---|---|---|---|---|---|---|---|---|---|")
    rows_u3 = []
    for (rate, mode, cc, tcp) in sorted(cells, key=lambda c: (int(c[0]), c[1], c[2], c[3])):
        ds = cells[(rate, mode, cc, tcp)]
        und, wire, pace, cwnd, drain, slow, vs = [], [], [], [], [], [], []
        for d in ds:
            lg = load(d)
            rows = [r for r in per_second(lg) if FROM_S <= r["t"] < TO_S]
            und.append(undecodable(d, FROM_S))
            wire.append(q([r["tx"] for r in rows], .5))
            pace.append(q([r["pace"] for r in rows], .5))
            cwnd.append(q([r["cwnd"] for r in rows], .5))
            g, s = drain_stats(lg, rows)
            drain.append(g)
            slow.append(s)
            vs.append(verdict(replayed(d, lg) if replay else recorded(lg)))
        pace_s = f"{mean(pace):.1f}" if cc == "bbr" else "–"
        print(f"| {rate} Mbps | {mode} · {'BBR' if cc == 'bbr' else 'Cubic'} | {'beside' if tcp == 'tcp' else 'alone'} "
              f"| {len(ds)} | {mean(und):.1f} % ({min(und):.1f}–{max(und):.1f}) | {mean(wire):.1f} | {pace_s} "
              f"| {mean(cwnd):.0f} | {mean(drain):.1f} | {mean(slow):.0f} % |")
        rows_u3.append(((rate, mode, cc, tcp), und, vs))
    print("\n#### U3 — the warning on the shared-uplink cells"
          + (" (R75 replayed over each run's counters)" if replay else " (as the runs' engine showed it)") + "\n")
    print("| Uplink | Delivery · controller | Upload | Undecodable | Warning | Judged |")
    print("|---|---|---|---|---|---|")
    for (rate, mode, cc, tcp), und, vs in rows_u3:
        from_s = 20 if tcp == "tcp" else None  # the upload starts at 20 s
        cell = warn_cell(vs, from_s)
        if rate == "15" and tcp == "tcp":
            ok = all(f is not None and f <= 20 + WARN_AFTER + 1 for f, _ in vs)
            judged = "pass" if ok else "**fail**"
        elif rate == "15":
            ok = all(f is not None for (f, _), u in zip(vs, und) if u >= 1.0)
            judged = "recorded (" + ("every damaged run" if ok else "missed a damaged run") + ")"
        elif tcp == "alone":
            judged = "pass" if all(f is None for f, _ in vs) else "**fail**"
        else:
            judged = "recorded"
        print(f"| {rate} Mbps | {mode} · {'BBR' if cc == 'bbr' else 'Cubic'} | "
              f"{'beside' if tcp == 'tcp' else 'alone'} | {mean(und):.1f} % | {cell} | {judged} |")


def matrix(base, replay):
    print("#### U4 — the warning on the engine matrix" + (" (R75 replayed)" if replay else "") + "\n")
    print("Must not raise (RU4, kept): lossless cells with either controller, lossy ones with BBR, "
          "the 10 ms outage cells with Cubic too. Other rows are recorded.\n")
    print("| Cell | Delivery | Cubic: undecodable · warning | BBR: undecodable · warning | Judged |")
    print("|---|---|---|---|---|")
    fails = 0
    for cell in sorted(glob.glob(f"{base}/*/"), key=lambda c: (re.sub(r"\d+", lambda m: m[0].zfill(4), c))):
        name = os.path.basename(cell.rstrip("/"))
        for mode in ("dgram", "carrier"):
            got = {}
            for cc, suf in (("cubic", ""), ("bbr", "-bbr")):
                d = f"{cell}eng-{mode}{suf}"
                if not os.path.exists(f"{d}/transport.json"):
                    continue
                lg = load(d)
                und = json.load(open(f"{d}/transport.json"))["undecodable_pct"]
                first, share = verdict(replayed(d, lg) if replay else recorded(lg))
                got[cc] = (und, first, share)
            if not got:
                continue
            lossless = "clean" in name or ("rate" in name and "out" not in name)
            outage10 = name.startswith("rtt10-out")  # 1080p, 10 ms (RU4)
            judge_cubic = lossless or outage10
            ok = True
            if "bbr" in got and got["bbr"][1] is not None:
                ok = False
            if judge_cubic and "cubic" in got and got["cubic"][1] is not None:
                ok = False
            fails += not ok
            fmt = lambda v: "–" if v is None else (  # noqa: E731
                f"{v[0]:.1f} % · " + ("no" if v[1] is None else f"at {v[1]:.0f} s, {v[2]:.0f} %"))
            print(f"| {name} | {'datagram' if mode == 'dgram' else 'carrier'} | {fmt(got.get('cubic'))} "
                  f"| {fmt(got.get('bbr'))} | {'pass' if ok else '**fail**'} |")
    print(f"\n{fails} row(s) fail.")


def seconds(d, lo):
    lg = load(d)
    print("t\twire\tpace\tcwnd/rtt\tqd ms\tloss")
    for r in per_second(lg):
        if r["t"] >= lo:
            print(f"{r['t']:.0f}\t{r['tx']:.1f}\t{r['pace']:.1f}\t{r['cwnd']:.1f}\t{r['qd']:.0f}\t{r['loss']:.3f}")


def main():
    a = sys.argv[1:]
    if not a:
        sys.exit(__doc__)
    replay = "--replay" in a
    a = [x for x in a if x != "--replay"]
    if a[0] == "seconds":
        lo = int(a[a.index("--from") + 1]) if "--from" in a else 0
        seconds(a[1], lo)
    elif a[0] == "fair":
        fair(a[1] if len(a) > 1 else f"{OUT}/fair", replay)
    elif a[0] == "matrix":
        matrix(a[1] if len(a) > 1 else f"{OUT}/results", replay)
    else:
        sys.exit(__doc__)


if __name__ == "__main__":
    main()
