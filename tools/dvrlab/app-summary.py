#!/usr/bin/env python3
"""One line per app-run.sh run (out/app/* or the directory given): engagement,
widths over time, resyncs (relay = gawk_relay_dvr_resyncs_total, client = the
viewer's gap resyncs) in total and from engagement + 10 s, duplicates, late
frames and the steady-state median fps."""
import csv, glob, json, os, sys

root = sys.argv[1] if len(sys.argv) > 1 else os.path.join(os.path.dirname(os.path.abspath(__file__)), "out", "app")
print(f"{'run':<14} {'engage s':>8} {'widths (t_s:×w phase)':<52} {'relay rs':>8} {'after':>5} "
      f"{'gap rs':>6} {'after':>5} {'dup':>4} {'late':>4} {'fps rx/dec':>10}")
for d in sorted(glob.glob(os.path.join(root, "*"))):
    p = os.path.join(d, "summary.json")
    if not os.path.exists(p):
        continue
    s = json.load(open(p))
    tr = " ".join(f"{t['t_s']:.0f}:×{t['width']}{t['phase'][:3]}" for t in s["widthTransitions"])
    after = s["afterEngagePlus10s"] or s["after30s"] or {}
    fps = s["medianFpsSteady"]
    eng = "-" if s["timeToEngageS"] is None else f"{s['timeToEngageS']:.1f}"
    print(f"{os.path.basename(d):<14} {eng:>8} {tr:<52} {s['relayResyncsTotal']:>8.0f} {after.get('relayResyncs', 0):>5} "
          f"{s['gapResyncsTotal']:>6} {after.get('gapResyncs', 0):>5} {s['duplicates']:>4} {s['lateDropped']:>4} "
          f"{fps['received']:>4}/{fps['decoder']:<5}")
