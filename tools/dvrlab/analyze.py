#!/usr/bin/env python3
"""Per-session goodput (steady window: 20 s to the end), the total, the relay's
DVR resyncs during the run, and the relay's own per-connection RTT and loss
from /statusz, for every run under out/runs (or the directory given)."""
import csv, glob, json, os, re, sys

root = sys.argv[1] if len(sys.argv) > 1 else os.path.join(os.path.dirname(os.path.abspath(__file__)), "out", "runs")

def resyncs(path):
    if not os.path.exists(path):
        return 0.0
    t = open(path).read()
    return sum(float(x) for x in re.findall(r"^gawk_relay_dvr_resyncs_total(?:\{[^}]*\})? (\S+)", t, re.M))

def paths(o, acc):
    if isinstance(o, dict):
        if o.get("dvr") and "pathPacketsSent" in o:
            acc.append(o)
        for v in o.values():
            paths(v, acc)
    elif isinstance(o, list):
        for v in o:
            paths(v, acc)
    return acc

print(f"{'run':<22} {'per-session Mbit/s':<30} {'total':>6} {'resyncs':>8}  relay srtt ms / loss %")
for d in sorted(glob.glob(os.path.join(root, "*"))):
    try:
        series = {}
        for r in csv.DictReader(open(os.path.join(d, "sessions.csv"))):
            series.setdefault(int(r["session"]), []).append((float(r["t_s"]), int(r["bytes"])))
    except FileNotFoundError:
        continue
    per = []
    for _, pts in sorted(series.items()):
        w = [p for p in pts if p[0] >= 20]
        if len(w) >= 2:
            per.append((w[-1][1] - w[0][1]) * 8 / 1e6 / (w[-1][0] - w[0][0]))
    res = resyncs(os.path.join(d, "metrics.txt")) - resyncs(os.path.join(d, "metrics-before.txt"))
    st = os.path.join(d, "statusz.json")
    rel = ""
    if os.path.exists(st):
        rel = " ".join(f"{p.get('pathSrttMs', 0):.0f}/{100 * p.get('pathPacketsLost', 0) / max(p['pathPacketsSent'], 1):.2f}"
                       for p in paths(json.load(open(st)), []))
    print(f"{os.path.basename(d):<22} {' '.join(f'{x:5.2f}' for x in per):<30} {sum(per):6.2f} {res:8.0f}  {rel}")
