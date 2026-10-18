#!/usr/bin/env python3
"""Uplink lab analysis (docs/73 §4): joins a sender log and a receiver log on
frameId and reports, per run, what the relay would have had and when.

Usage: analyze.py <run-dir> [--skip-s 5] [--json out.json] [--viewer-trace out.json]
                  [--viewer-trace-noparity out.json]

The no-parity trace is what a viewer on reliable delivery (Smoother, Most
stable) is sent: the relay serves those viewers no parity (hub.NegotiateParity),
so a delta reaches them complete only if every data chunk reached the relay.

Sender log lines (gawk-broadcast-desktop/tools/uplinklab): F frame, K keyframe
stream outcome, C carrier retired, O carrier opened, W a write that blocked
over 1 ms, D a record shed, G a carrier never opened, M the chunk budget
shrunk, E a frame dropped at send, T/S path stats. Receiver lines
(gawk-server/cmd/gawk-uplinklab recv): see recv.go.

A delta frame is *complete* at the relay when its data chunks are all there
or, with parity, once any n of its n+k symbols have arrived (P/Q is MDS for
k <= 2). A keyframe is complete when its stream has been read to the end.
*Decodable* adds the GOP dependency: a frame can be shown only once its GOP's
keyframe and every earlier delta in the GOP are complete, so its
decode-ready time is the max over that prefix.
"""
import json
import sys
from collections import defaultdict


def pct(xs, p):
    if not xs:
        return None
    xs = sorted(xs)
    k = (len(xs) - 1) * p / 100.0
    lo = int(k)
    hi = min(lo + 1, len(xs) - 1)
    return xs[lo] + (xs[hi] - xs[lo]) * (k - lo)


def load(run):
    frames = {}
    kf_out = {}
    carriers = []
    stats = None
    tstats = []
    writes = []
    shed = 0
    for line in open(f"{run}/send.csv"):
        p = line.rstrip("\n").split(",")
        if p[0] == "F":
            fid = int(p[1])
            frames[fid] = dict(id=fid, key=p[2] == "1", size=int(p[3]),
                               n=int(p[4]), k=int(p[5]), t_send=int(p[6]))
        elif p[0] == "K":
            kf_out[int(p[1])] = (int(p[2]), int(p[3]), p[4])
        elif p[0] == "C":
            carriers.append((int(p[1]), int(p[2]), int(p[3]), p[4]))
        elif p[0] == "S":
            stats = dict(rtt_us=int(p[1]), cwnd=int(p[2]), sent_pkts=int(p[3]),
                         lost_pkts=int(p[4]), lost_bytes=int(p[5]),
                         cong_events=int(p[6]), dgram_frames_tx=int(p[7]),
                         dgrams_offered=int(p[8]), udp_tx_bytes=int(p[9]))
        elif p[0] == "T":
            tstats.append(dict(t=int(p[1]), rtt_us=int(p[2]), cwnd=int(p[3]),
                               sent=int(p[4]), lost=int(p[5])))
        elif p[0] == "W":
            writes.append((int(p[1]), int(p[2]), int(p[3])))
        elif p[0] == "D":
            shed += 1
    data = defaultdict(dict)    # fid -> idx -> t
    parity = defaultdict(dict)  # fid -> pidx -> t
    kdone = {}
    for line in open(f"{run}/recv.csv"):
        p = line.rstrip("\n").split(",")
        t, kind, fid, idx = int(p[0]), p[1], int(p[2]), int(p[3])
        if kind in ("D", "R"):
            data[fid].setdefault(idx, t)
        elif kind in ("P", "Q"):
            parity[fid].setdefault(idx, t)
        elif kind == "K":
            kdone[fid] = t
    frames["_shed"] = shed
    return frames, kf_out, carriers, stats, tstats, writes, data, parity, kdone


def complete_time(f, data, parity, use_parity=True):
    if f["key"]:
        return None
    d = data.get(f["id"], {})
    n = f["n"]
    if use_parity:
        arr = sorted(list(d.values()) + list(parity.get(f["id"], {}).values()))
        if len(d) >= 1 and len(arr) >= n:
            # Any n symbols reconstruct — but all n must be data if no parity.
            return arr[n - 1]
        return None
    if len(d) == n:
        return max(d.values())
    return None


def analyze(run, skip_s=5.0, tail_s=1.0):
    frames, kf_out, carriers, stats, tstats, writes, data, parity, kdone = load(run)
    shed = frames.pop("_shed")
    ids = sorted(frames)
    t0 = frames[ids[0]]["t_send"]
    t_end = frames[ids[-1]]["t_send"]
    lo = t0 + int(skip_s * 1e9)
    hi = t_end - int(tail_s * 1e9)

    done = {}
    done_np = {}
    for fid in ids:
        f = frames[fid]
        if f["key"]:
            done[fid] = done_np[fid] = kdone.get(fid)
        else:
            done[fid] = complete_time(f, data, parity)
            done_np[fid] = complete_time(f, data, parity, use_parity=False)

    # Decode-ready: max over the GOP prefix; None once the prefix breaks.
    ready = {}
    gop_ok = False
    gop_max = None
    for fid in ids:
        f = frames[fid]
        c = done[fid]
        if f["key"]:
            gop_ok = c is not None
            gop_max = c
        else:
            if gop_ok and c is not None:
                gop_max = max(gop_max, c)
            else:
                gop_ok = False
        ready[fid] = gop_max if gop_ok else None

    win = [fid for fid in ids if lo <= frames[fid]["t_send"] <= hi]
    deltas = [fid for fid in win if not frames[fid]["key"]]
    keys = [fid for fid in win if frames[fid]["key"]]
    lat_d = [(done[f] - frames[f]["t_send"]) / 1e6 for f in deltas if done[f] is not None]
    lat_k = [(done[f] - frames[f]["t_send"]) / 1e6 for f in keys if done[f] is not None]
    lat_r = [(ready[f] - frames[f]["t_send"]) / 1e6 for f in win if ready[f] is not None]
    lost_d = sum(1 for f in deltas if done[f] is None)
    # Frames missing at least one data chunk (whether or not parity saved them).
    damaged = sum(1 for f in deltas if len(data.get(f, {})) < frames[f]["n"])
    undecodable = sum(1 for f in win if ready[f] is None)
    gops = defaultdict(list)
    g = None
    for fid in win:
        if frames[fid]["key"]:
            g = fid
        if g is not None:
            gops[g].append(fid)
    broken_gops = sum(1 for g, fs in gops.items() if any(ready[f] is None for f in fs))

    secs = (hi - lo) / 1e9
    out = dict(
        run=run,
        window_s=round(secs, 1),
        frames=len(win),
        delta_lat_ms=dict(p50=pct(lat_d, 50), p95=pct(lat_d, 95), p99=pct(lat_d, 99),
                          p999=pct(lat_d, 99.9), max=max(lat_d) if lat_d else None),
        key_lat_ms=dict(p50=pct(lat_k, 50), p95=pct(lat_k, 95), max=max(lat_k) if lat_k else None),
        ready_lat_ms=dict(p50=pct(lat_r, 50), p95=pct(lat_r, 95), p99=pct(lat_r, 99),
                          p999=pct(lat_r, 99.9), max=max(lat_r) if lat_r else None),
        deltas_damaged_pct=100.0 * damaged / max(1, len(deltas)),
        deltas_lost_pct=100.0 * lost_d / max(1, len(deltas)),
        undecodable_pct=100.0 * undecodable / max(1, len(win)),
        broken_gops_pct=100.0 * broken_gops / max(1, len(gops)),
        keyframes_cancelled=sum(1 for k, v in kf_out.items() if v[2] != "sent"),
        carriers_expired=sum(1 for c in carriers if c[3] == "expired"),
        carriers_total=len(carriers),
        blocked_writes=len(writes),
        shed_records=shed,
        stats=stats,
    )
    if stats and secs > 0:
        dur = (t_end - t0) / 1e9 + 1.5
        out["uplink_mbps"] = stats["udp_tx_bytes"] * 8 / dur / 1e6
        out["pkt_loss_pct"] = 100.0 * stats["lost_pkts"] / max(1, stats["sent_pkts"])
    return out, frames, done, done_np, t0


def viewer_trace(frames, done, t0):
    """Frames as the viewer's reorder buffer would receive them (leg B
    assumed instantaneous): arrival ms since the first send, capture time ms."""
    ev = []
    for fid in sorted(frames):
        f = frames[fid]
        c = done[fid]
        if c is None:
            continue
        ev.append(dict(id=fid, key=f["key"], ts=(f["t_send"] - t0) / 1e6,
                       sched=fid * 1000.0 / 60.0, at=(c - t0) / 1e6))
    return ev


if __name__ == "__main__":
    run = sys.argv[1]
    skip = 5.0
    jout = vout = vnp = None
    a = sys.argv[2:]
    while a:
        if a[0] == "--skip-s":
            skip = float(a[1]); a = a[2:]
        elif a[0] == "--json":
            jout = a[1]; a = a[2:]
        elif a[0] == "--viewer-trace":
            vout = a[1]; a = a[2:]
        elif a[0] == "--viewer-trace-noparity":
            vnp = a[1]; a = a[2:]
        else:
            raise SystemExit("bad arg " + a[0])
    res, frames, done, done_np, t0 = analyze(run, skip)
    if jout:
        json.dump(res, open(jout, "w"), indent=1)
    if vout:
        json.dump(viewer_trace(frames, done, t0), open(vout, "w"))
    if vnp:
        json.dump(viewer_trace(frames, done_np, t0), open(vnp, "w"))
    print(json.dumps(res, indent=1, default=lambda x: round(x, 2)))
