#!/usr/bin/env python3
"""Runs uplink-lab scenarios (docs/75 §4): each scenario × delivery mode is
one real sender → impaired path → receiver run, then the transport analysis
and the viewer replay (live edge and adaptive). Results land in
$UPLINK_OUT/results/<scenario>/<mode>/; a cell with a summary.json is skipped,
so a set resumes where it stopped.

Usage: matrix.py core|cc|hi|engine|engine-hi|engine-check|jit|win|bigq|seeds|chrome|chrome-kf|chrome-ceiling|chrome-knobs|chrome-seeds|chrome-ceiling-repeat|chrome-kftiming|chrome-kftiming2|ru8-g10|chrome-ru9-p7|chrome-ru9-p9|all [--jobs 3] [--seconds 100] [--only name-prefix]
       matrix.py rescore [substr] [--jobs N]   # analyse and replay every finished run again
"""
import concurrent.futures as cf
import json
import os
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(os.path.dirname(HERE))
OUT = os.environ.get("UPLINK_OUT", os.path.join(HERE, "out"))
APP = os.path.join(ROOT, "gawk-app")
REPLAY = "lab/uplink-replay.lab.test.ts"
TRACES = {"1080": f"{OUT}/traces/r1080p60.csv", "1440": f"{OUT}/traces/r1440p60.csv"}

MODES = {
    "dgram": ["--mode", "datagram"],
    "carrier": ["--mode", "carrier"],
    "dgram-bbr": ["--mode", "datagram", "--cc", "bbr"],
    "carrier-bbr": ["--mode", "carrier", "--cc", "bbr"],
    # ~100 ms of the default rung's peak: a queue becomes drops, not delay.
    "dgram-buf150k": ["--mode", "datagram", "--datagram-buffer", "153600"],
    "dgram-bbr-buf150k": ["--mode", "datagram", "--cc", "bbr", "--datagram-buffer", "153600"],
    # D7: the receiver with quic-go's per-session datagram queue at 4096 (the
    # relay change), datagrams kept — alone, and with BBR.
    "dgram-bigq": ["--mode", "datagram"],
    "dgram-bbr-bigq": ["--mode", "datagram", "--cc", "bbr"],
    # The receiver at the depth the relay ships since RU2 (D7's 512: the
    # tree's own build, from the vendored quic-go), datagrams and BBR.
    "dgram-bbr-q512": ["--mode", "datagram", "--cc", "bbr"],
    # OD5: the browser broadcaster's send path over Chrome's own QUIC stack
    # (browser/sender.mjs), and a browser carrier prototype on it.
    "chrome-dgram": ["--mode", "datagram"],
    "chrome-carrier": ["--mode", "carrier"],
    # The carrier prototype holding a GOP's records until its keyframe
    # stream's write resolved, or its close (acknowledged): Chrome has no
    # stream priority to put the keyframe first.
    "chrome-carrier-kfw": ["--mode", "carrier", "--kforder", "write"],
    "chrome-carrier-kfa": ["--mode", "carrier", "--kforder", "ack"],
    # The ceiling (table P6): the same sends again, the net-log filtered to
    # the lab's session, Chrome's CPU and packet spacing recorded; and with
    # every datagram of a frame handed over at once.
    "chrome-dgram-ceil": ["--mode", "datagram"],
    "chrome-dgram-nowait-ceil": ["--mode", "datagram", "--writes", "nowait"],
    "chrome-carrier-ceil": ["--mode", "carrier"],
    # Where the backlog sits, and the spec's knobs on Chrome's datagram queue
    # (table P7, the BUGS.md fix): outgoingMaxAge, and a deeper high-water mark.
    "chrome-dgram-kt": ["--mode", "datagram"],
    "chrome-dgram-maxage100": ["--mode", "datagram", "--maxage", "100"],
    "chrome-dgram-maxage100-hwm1000": ["--mode", "datagram", "--maxage", "100", "--hwm", "1000"],
    # RU9: the same sends through the fixed page (its send queue drops stale
    # frames, KeyframeSender holds the keyframes), so P7's and P9's cells
    # re-run beside the runs they measured before the fix (tables P10, P11).
    "chrome-ru9": ["--mode", "datagram"],
    "chrome-ru9-maxage100": ["--mode", "datagram", "--maxage", "100"],
    "chrome-ru9-maxage100-hwm1000": ["--mode", "datagram", "--maxage", "100", "--hwm", "1000"],
}
# Modes that change the receiver rather than the sender.
MODE_ENV = {
    "dgram-bigq": {"RECV_BIN": f"{OUT}/bin/gawk-uplinklab-bigq"},
    "dgram-bbr-bigq": {"RECV_BIN": f"{OUT}/bin/gawk-uplinklab-bigq"},
    "dgram-bbr-q512": {"RECV_BIN": f"{OUT}/bin/gawk-uplinklab"},
    "chrome-dgram": {"SENDER_BIN": f"{HERE}/browser/sender.mjs", "RECV_BIN": f"{OUT}/bin/gawk-uplinklab-bigq"},
    "chrome-carrier": {"SENDER_BIN": f"{HERE}/browser/sender.mjs", "RECV_BIN": f"{OUT}/bin/gawk-uplinklab-bigq"},
    "chrome-carrier-kfw": {"SENDER_BIN": f"{HERE}/browser/sender.mjs", "RECV_BIN": f"{OUT}/bin/gawk-uplinklab-bigq"},
    "chrome-carrier-kfa": {"SENDER_BIN": f"{HERE}/browser/sender.mjs", "RECV_BIN": f"{OUT}/bin/gawk-uplinklab-bigq"},
}
for _m in ("chrome-dgram-ceil", "chrome-dgram-nowait-ceil", "chrome-carrier-ceil", "chrome-dgram-kt", "chrome-dgram-maxage100",
           "chrome-dgram-maxage100-hwm1000", "chrome-ru9", "chrome-ru9-maxage100", "chrome-ru9-maxage100-hwm1000"):
    MODE_ENV[_m] = MODE_ENV["chrome-dgram"]
# RU6: the shipped engine (bin/sender-engine: gawk-engine's transport, Sender
# and upload monitor) in each delivery mode and controller, against the
# receiver at the depth the relay ships (D7's 512).
for _d, _w in (("dgram", "datagram"), ("carrier", "carrier")):
    for _c, _s in (("cubic", ""), ("bbr", "-bbr")):
        MODES[f"eng-{_d}{_s}"] = ["--mode", _w, "--cc", _c]
        MODE_ENV[f"eng-{_d}{_s}"] = {"SENDER_BIN": f"{OUT}/bin/sender-engine",
                                    "RECV_BIN": f"{OUT}/bin/gawk-uplinklab"}
ENGINE_MODES = ("eng-dgram", "eng-carrier", "eng-dgram-bbr", "eng-carrier-bbr")
# A cell whose CPU steal (% of the host's CPU time, sampled over the run)
# exceeds this is moved aside and run again, at most STEAL_RETRIES times.
STEAL_MAX = float(os.environ.get("UPLINK_STEAL_MAX", "5"))
STEAL_RETRIES = 2


def scen(name, proxy, trace="1080", modes=("dgram", "carrier"), extra=(), recv="", seed=7):
    return dict(name=name, proxy=proxy, trace=trace, modes=modes, extra=list(extra), recv=recv, seed=seed)


def sets():
    s = {}
    core = []
    for rtt in (10, 40, 100):
        r = f"{rtt}ms"
        core.append(scen(f"rtt{rtt}-clean", ["-rtt", r]))
        for loss in ("0.001", "0.005", "0.01", "0.02", "0.05"):
            core.append(scen(f"rtt{rtt}-loss{loss}", ["-rtt", r, "-loss", loss]))
        # Bursty: ~1 % mean loss in bursts of ~4 packets.
        core.append(scen(f"rtt{rtt}-ge1pct", ["-rtt", r, "-ge-g2b", "0.0025", "-ge-b2g", "0.25"]))
        # AWDL-like: the radio away 20 or 80 ms of every second, both ways.
        core.append(scen(f"rtt{rtt}-out20drop", ["-rtt", r, "-outage-period", "1070ms", "-outage-len", "20ms"]))
        core.append(scen(f"rtt{rtt}-out80drop", ["-rtt", r, "-outage-period", "1070ms", "-outage-len", "80ms"]))
        core.append(scen(f"rtt{rtt}-out80hold", ["-rtt", r, "-outage-period", "1070ms", "-outage-len", "80ms",
                                                 "-outage-mode", "hold"]))
        # Episodic: 2 % loss for 10 s of every 40 s.
        core.append(scen(f"rtt{rtt}-episodic", ["-rtt", r, "-loss", "0.02", "-episode-period", "40s",
                                                "-episode-len", "10s"]))
    s["core"] = core
    # The congestion-controller and send-buffer variants, on the cells that
    # separate them.
    cc = []
    for rtt in (10, 40, 100):
        r = f"{rtt}ms"
        variants = ("dgram-bbr", "carrier-bbr", "dgram-buf150k", "dgram-bbr-buf150k")
        cc.append(scen(f"rtt{rtt}-clean", ["-rtt", r], modes=variants))
        for loss in ("0.001", "0.005", "0.01", "0.02"):
            cc.append(scen(f"rtt{rtt}-loss{loss}", ["-rtt", r, "-loss", loss], modes=variants))
        cc.append(scen(f"rtt{rtt}-ge1pct", ["-rtt", r, "-ge-g2b", "0.0025", "-ge-b2g", "0.25"], modes=variants))
        cc.append(scen(f"rtt{rtt}-out80drop", ["-rtt", r, "-outage-period", "1070ms", "-outage-len", "80ms"],
                       modes=variants))
        cc.append(scen(f"rtt{rtt}-out20drop", ["-rtt", r, "-outage-period", "1070ms", "-outage-len", "20ms"],
                       modes=variants))
    s["cc"] = cc
    # The high rung (1440p60, 38.9 Mbps mean, 50 Mbps ceiling) and docs/57
    # D9's headroom table: a bottleneck at 300 or 80 Mbps behind the radio.
    hi = []
    both = ("dgram", "carrier", "dgram-bbr", "carrier-bbr")
    hi.append(scen("hi-rtt10-clean", ["-rtt", "10ms"], trace="1440", modes=both))
    hi.append(scen("hi-rtt10-loss0.001", ["-rtt", "10ms", "-loss", "0.001"], trace="1440", modes=both))
    hi.append(scen("hi-rtt10-ge1pct", ["-rtt", "10ms", "-ge-g2b", "0.0025", "-ge-b2g", "0.25"], trace="1440",
                   modes=both))
    for rate in ("300e6", "80e6"):
        tag = rate.replace("e6", "M")
        hi.append(scen(f"hi-rtt10-rate{tag}", ["-rtt", "10ms", "-rate", rate, "-queue", "50ms"], trace="1440",
                       modes=both))
        hi.append(scen(f"hi-rtt10-rate{tag}-out80drop", ["-rtt", "10ms", "-rate", rate, "-queue", "50ms",
                                                         "-outage-period", "1070ms", "-outage-len", "80ms"],
                       trace="1440", modes=both))
    s["hi"] = hi
    # Robustness: the same comparisons on a path whose delay jitters (3 ms,
    # order kept), so neither controller is flattered by a constant RTT.
    jit = []
    four = ("dgram", "carrier", "dgram-bbr", "carrier-bbr")
    for rtt in (10, 40):
        r = f"{rtt}ms"
        base = ["-rtt", r, "-jitter", "3ms"]
        jit.append(scen(f"jit-rtt{rtt}-clean", base, modes=four))
        jit.append(scen(f"jit-rtt{rtt}-loss0.001", base + ["-loss", "0.001"], modes=four))
        jit.append(scen(f"jit-rtt{rtt}-loss0.005", base + ["-loss", "0.005"], modes=four))
        jit.append(scen(f"jit-rtt{rtt}-ge1pct", base + ["-ge-g2b", "0.0025", "-ge-b2g", "0.25"], modes=four))
        jit.append(scen(f"jit-rtt{rtt}-out80drop", base + ["-outage-period", "1070ms", "-outage-len", "80ms"],
                        modes=four))
    s["jit"] = jit
    # D10: the relay's receive windows at the 50 Mbps ceiling (docs/57 D9's
    # rule), against quic-go's 512 KiB / 768 KiB defaults in "hi".
    win = [scen(f"win-hi-rtt10-{n}", p, trace="1440", modes=("carrier-bbr",),
                recv="-stream-window 2097152 -conn-window 3145728")
           for n, p in (("clean", ["-rtt", "10ms"]),
                        ("rate80M-out80drop", ["-rtt", "10ms", "-rate", "80e6", "-queue", "50ms",
                                               "-outage-period", "1070ms", "-outage-len", "80ms"]))]
    s["win"] = win
    # "Only BBR and a longer relay queue": datagram delivery kept, every
    # scenario that separates it from the carrier.
    bigq = []
    qm = ("dgram-bigq", "dgram-bbr-bigq")
    for rtt in (10, 40, 100):
        r = f"{rtt}ms"
        bigq.append(scen(f"rtt{rtt}-clean", ["-rtt", r], modes=qm))
        for loss in ("0.001", "0.005", "0.01", "0.02"):
            bigq.append(scen(f"rtt{rtt}-loss{loss}", ["-rtt", r, "-loss", loss], modes=qm))
        bigq.append(scen(f"rtt{rtt}-ge1pct", ["-rtt", r, "-ge-g2b", "0.0025", "-ge-b2g", "0.25"], modes=qm))
        for ln in ("20ms", "80ms"):
            bigq.append(scen(f"rtt{rtt}-out{ln[:-2]}drop", ["-rtt", r, "-outage-period", "1070ms",
                                                           "-outage-len", ln], modes=qm))
    for sc in hi:
        bigq.append(dict(sc, modes=qm))
    s["bigq"] = bigq
    # Run-to-run spread: the same cells with two more loss-process seeds.
    seeds = []
    sm = ("dgram-bbr", "dgram-bbr-bigq", "carrier-bbr")
    for name, proxy in (("rtt40-loss0.005", ["-rtt", "40ms", "-loss", "0.005"]),
                        ("rtt40-loss0.01", ["-rtt", "40ms", "-loss", "0.01"]),
                        ("rtt10-ge1pct", ["-rtt", "10ms", "-ge-g2b", "0.0025", "-ge-b2g", "0.25"]),
                        ("rtt10-out80drop", ["-rtt", "10ms", "-outage-period", "1070ms", "-outage-len", "80ms"])):
        for seed in (8, 9):
            seeds.append(scen(f"{name}-s{seed}", proxy, modes=sm, seed=seed))
    s["seeds"] = seeds
    # OD5: Chrome (whose QUIC stack runs Cubic whatever the page asks), its
    # datagrams and a carrier prototype, on the cells that separate them.
    # Run --jobs 1: Chrome's datagram path is CPU-bound on the lab host.
    chrome = []
    cm = ("chrome-dgram", "chrome-carrier")
    for rtt in (10, 40, 100):
        r = f"{rtt}ms"
        chrome.append(scen(f"rtt{rtt}-clean", ["-rtt", r], modes=cm))
        for loss in ("0.001", "0.005", "0.01"):
            chrome.append(scen(f"rtt{rtt}-loss{loss}", ["-rtt", r, "-loss", loss], modes=cm))
        chrome.append(scen(f"rtt{rtt}-ge1pct", ["-rtt", r, "-ge-g2b", "0.0025", "-ge-b2g", "0.25"], modes=cm))
        for ln in ("20ms", "80ms"):
            chrome.append(scen(f"rtt{rtt}-out{ln[:-2]}drop", ["-rtt", r, "-outage-period", "1070ms",
                                                             "-outage-len", ln], modes=cm))
    for sc in hi:
        if sc["name"] in ("hi-rtt10-clean", "hi-rtt10-loss0.001", "hi-rtt10-ge1pct", "hi-rtt10-rate80M",
                          "hi-rtt10-rate80M-out80drop"):
            chrome.append(dict(sc, modes=cm))
    s["chrome"] = chrome
    # The keyframe gate: on acknowledgement on every Chrome cell, on the
    # write on five.
    five = ("rtt10-clean", "rtt10-loss0.005", "rtt10-out80drop", "rtt40-loss0.001", "hi-rtt10-clean")
    s["chrome-kf"] = [dict(sc, modes=("chrome-carrier-kfa",) + (("chrome-carrier-kfw",) if sc["name"] in five else ()))
                      for sc in chrome]
    by = {sc["name"]: sc for sc in chrome}
    ceil = [scen("rtt0-clean", ["-rtt", "0ms"], modes=("chrome-dgram-ceil", "chrome-dgram-nowait-ceil"))]
    for n in ("rtt10-clean", "rtt100-clean"):
        ceil.append(dict(by[n], modes=("chrome-dgram-ceil", "chrome-dgram-nowait-ceil")))
    ceil.append(dict(by["hi-rtt10-clean"], modes=("chrome-dgram-ceil", "chrome-dgram-nowait-ceil", "chrome-carrier-ceil")))
    s["chrome-ceiling"] = ceil
    # Run-to-run spread at 1440p, where one run once swung 25 points: two
    # more loss-process seeds per cell (table P8).
    s["chrome-seeds"] = [scen(f"{sc['name']}-s{seed}", sc["proxy"], trace="1440",
                              modes=("chrome-dgram", "chrome-carrier", "chrome-carrier-kfa"), seed=seed)
                         for sc in chrome if sc["name"].startswith("hi-") for seed in (8, 9)]
    # The clean 10 ms ceiling cell twice more: one run fell behind mid-run
    # with nothing on the path to explain it.
    s["chrome-ceiling-repeat"] = [scen(f"rtt10-clean-r{i}", ["-rtt", "10ms"], modes=("chrome-dgram-ceil",))
                                  for i in (2, 3)]
    # Keyframe timing on clean paths by RTT, long ones included (table P9):
    # production → handed over (KW) and → acknowledged (K), the clocks RU9's
    # "late keyframe" rule runs on.
    s["chrome-kftiming"] = [scen(f"rtt{r}-clean", ["-rtt", f"{r}ms"], modes=("chrome-dgram-kt",))
                            for r in (0, 10, 40, 100, 200, 300)]
    # More of P9: a second run at 40 and 100 ms, light loss, and the 1440p
    # keyframe (495 KB) at 10 / 40 / 100 ms.
    s["chrome-kftiming2"] = [scen("rtt40-clean-r2", ["-rtt", "40ms"], modes=("chrome-dgram-kt",), seed=8),
                             scen("rtt100-clean-r2", ["-rtt", "100ms"], modes=("chrome-dgram-kt",), seed=8),
                             scen("rtt40-loss0.001", ["-rtt", "40ms", "-loss", "0.001"], modes=("chrome-dgram-kt",))]
    s["chrome-kftiming2"] += [scen(f"hi-rtt{r}-clean", ["-rtt", f"{r}ms"], trace="1440", modes=("chrome-dgram-kt",))
                              for r in (10, 40, 100)]
    s["chrome-knobs"] = [dict(by["rtt40-loss0.005"], modes=("chrome-dgram-ceil", "chrome-dgram-maxage100",
                                                            "chrome-dgram-maxage100-hwm1000")),
                         dict(by["hi-rtt10-clean"], modes=("chrome-dgram-maxage100",))]
    # G10's datagram half after RU8 (table K2): datagrams + BBR at 0.1-1 %
    # random loss, 10 and 40 ms, by receiver depth; the shipped 512 with
    # three loss-process seeds, the others with seed 7 (and three seeds on
    # the cell nearest the bound).
    g10 = []
    for rtt in (10, 40):
        for loss in ("0.001", "0.005", "0.01"):
            p = ["-rtt", f"{rtt}ms", "-loss", loss]
            g10.append(scen(f"rtt{rtt}-loss{loss}", p, modes=("dgram-bbr", "dgram-bbr-bigq", "dgram-bbr-q512")))
            for seed in (8, 9):
                modes = ("dgram-bbr", "dgram-bbr-bigq", "dgram-bbr-q512") if (rtt, loss) == (10, "0.01") \
                    else ("dgram-bbr-q512",)
                g10.append(scen(f"rtt{rtt}-loss{loss}-s{seed}", p, modes=modes, seed=seed))
    s["ru8-g10"] = g10
    # RU9's lab criterion: table P7's cells and table P9's, through the fixed page.
    s["chrome-ru9-p7"] = [dict(by["rtt40-loss0.005"], modes=("chrome-ru9", "chrome-ru9-maxage100",
                                                             "chrome-ru9-maxage100-hwm1000")),
                          dict(by["hi-rtt10-clean"], modes=("chrome-ru9", "chrome-ru9-maxage100"))]
    s["chrome-ru9-p9"] = [dict(sc, modes=("chrome-ru9",)) for sc in s["chrome-kftiming"] + s["chrome-kftiming2"]]
    s["all"] = core + cc + hi + jit + win + bigq + seeds
    # RU6: the engine as shipped (#541) on the cells G1, G2, G3 and G5 are
    # judged on — "cc"'s at 10/40/100 ms (clean, random loss, bursts,
    # outages), then the 1440p ones alone on the host (--jobs 1).
    s["engine"] = [dict(sc, modes=ENGINE_MODES) for sc in cc]
    s["engine-hi"] = [dict(sc, modes=ENGINE_MODES) for sc in hi]
    # One representative cell (G1 at 10 ms, both deliveries, both
    # controllers): the tooling check for a host too noisy for the rest.
    s["engine-check"] = [scen("rtt10-clean", ["-rtt", "10ms"], modes=ENGINE_MODES)]
    return s


# The four viewer presets (gawk-app playbackPresets.ts): name → the trace it
# is sent, its playout mode, its delivery mode. Live-edge delivery gets parity;
# reliable delivery (resilient, deep) was served none before RU8, so these
# replay the trace of frames complete without it.
PRESETS = {
    "off": ("vt.json", "off", "live"),            # Lowest latency
    "adaptive": ("vt.json", "adaptive", "live"),  # Balanced (default)
    "resilient": ("vt-noparity.json", "adaptive", "resilient"),  # Smoother
    "deep": ("vt-noparity.json", "adaptive", "deep"),            # Most stable
    # The relay serving parity to reliable delivery too: docs/75 F14's
    # what-if, and since RU8 (D11) the relay as shipped — the plain
    # "resilient" / "deep" above stay the relay before RU8, so tables J-L
    # still reproduce. Leg B is reliable and clean here, so such a viewer
    # receives exactly what a live-edge viewer can complete.
    "resilient-parity": ("vt.json", "adaptive", "resilient"),
    "deep-parity": ("vt.json", "adaptive", "deep"),
}


def replay(runs, skip_ms):
    """The viewer replay for each run dir, every preset, in one vitest
    process; then each run's summary.json is (re)written."""
    batch = f"{OUT}/replay-batch.{os.getpid()}.{id(runs)}.txt"
    with open(batch, "w") as b:
        for r in runs:
            for name, (trace, po, delivery) in PRESETS.items():
                b.write(f"{r}/{trace} {po} {r}/view-{name}.json {delivery}\n")
    env = dict(os.environ, UPLINK_BATCH=batch, UPLINK_SKIP_MS=str(int(skip_ms)))
    subprocess.run(["npx", "vitest", "run", REPLAY, "--testTimeout", "3600000"], cwd=APP,
                   env=env, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    os.remove(batch)
    for r in runs:
        view = {name: json.load(open(f"{r}/view-{name}.json")) for name in PRESETS}
        t = json.load(open(f"{r}/transport.json"))
        json.dump(dict(transport=t, view=view, proxy=open(f"{r}/proxy.txt").read()),
                  open(f"{r}/summary.json", "w"), indent=1)


def cpu():
    """/proc/stat's cpu line: user nice system idle iowait irq softirq steal guest guest_nice."""
    with open("/proc/stat") as fh:
        return [int(x) for x in fh.readline().split()[1:11]]


def steal_pct(a, b):
    d = [y - x for x, y in zip(a, b)]
    return 100 * d[7] / max(1, sum(d))


def run_one(sc, mode, seconds):
    """One cell; re-run while the host's steal over it exceeds STEAL_MAX
    (the over-steal attempts are kept beside it as <mode>.steal<n>)."""
    out = f"{OUT}/results/{sc['name']}/{mode}"
    if os.path.exists(f"{out}/summary.json"):  # finished: a resume skips it, steal and all
        return out
    for attempt in range(STEAL_RETRIES + 1):
        run_cell(sc, mode, seconds, out)
        if not os.path.exists(f"{out}/steal.txt"):  # finished before steal was recorded
            return out
        st = float(open(f"{out}/steal.txt").read())
        if st <= STEAL_MAX or attempt == STEAL_RETRIES:
            return out
        print(f"steal {st:.1f} % on {sc['name']} {mode}: re-running", flush=True)
        os.rename(out, f"{out}.steal{attempt + 1}")
    return out


def run_cell(sc, mode, seconds, out):
    os.makedirs(out, exist_ok=True)
    if os.path.exists(f"{out}/summary.json"):
        return out
    # The steal just before the cell (1 s), beside the steal over it.
    p0 = cpu()
    time.sleep(1)
    with open(f"{out}/steal-pre.txt", "w") as fh:
        fh.write(f"{steal_pct(p0, cpu()):.1f}\n")
    c0 = cpu()
    args = [f"{HERE}/run.sh", out, "--trace", TRACES[sc["trace"]], "--seconds", str(seconds)]
    args += MODES[mode] + sc["extra"] + ["--"] + sc["proxy"] + ["-seed", str(sc.get("seed", 7))]
    env = dict(os.environ, RECV_ARGS=sc["recv"], **MODE_ENV.get(mode, {}))
    subprocess.run(args, check=True, timeout=seconds + 120, env=env)
    subprocess.run(["python3", f"{HERE}/analyze.py", out, "--json", f"{out}/transport.json",
                    "--viewer-trace", f"{out}/vt.json", "--viewer-trace-noparity", f"{out}/vt-noparity.json"],
                   check=True, stdout=subprocess.DEVNULL)
    # The second half of the run: the adaptive controller has settled.
    replay([out], seconds * 1000 / 2)
    # The host's CPU steal over the cell: a hypervisor taking the cores makes
    # every latency and loss figure meaningless (docs/75 §11, table N1).
    with open(f"{out}/steal.txt", "w") as fh:
        fh.write(f"{steal_pct(c0, cpu()):.1f}\n")
    return out


def rescore(flt, jobs):
    """Re-analyses every finished run and replays it again, split over `jobs`
    vitest processes."""
    import glob
    runs = sorted(os.path.dirname(p) for p in glob.glob(f"{OUT}/results/*/*/transport.json"))
    runs = [r for r in runs if flt in r]

    def analyse(r):
        subprocess.run(["python3", f"{HERE}/analyze.py", r, "--json", f"{r}/transport.json",
                        "--viewer-trace", f"{r}/vt.json", "--viewer-trace-noparity", f"{r}/vt-noparity.json"],
                       check=True, stdout=subprocess.DEVNULL)
    with cf.ThreadPoolExecutor(jobs) as ex:
        list(ex.map(analyse, runs))
    parts = [runs[i::jobs] for i in range(jobs)]
    with cf.ThreadPoolExecutor(jobs) as ex:
        list(ex.map(lambda part: replay(part, 50000), [p for p in parts if p]))
    print(len(runs), "rescored")


def main():
    which = sys.argv[1]
    if which == "rescore":
        a = sys.argv[2:]
        jobs = 1
        if "--jobs" in a:
            i = a.index("--jobs")
            jobs = int(a[i + 1])
            a = a[:i] + a[i + 2:]
        rescore(a[0] if a else "", jobs)
        return
    jobs, seconds, only = 3, 100, None
    a = sys.argv[2:]
    while a:
        if a[0] == "--jobs":
            jobs = int(a[1]); a = a[2:]
        elif a[0] == "--seconds":
            seconds = float(a[1]); a = a[2:]
        elif a[0] == "--only":
            only = a[1]; a = a[2:]
        else:
            raise SystemExit("bad arg")
    work = [(sc, m) for sc in sets()[which] for m in sc["modes"] if not only or sc["name"].startswith(only)]
    print(f"{len(work)} runs, {jobs} at a time, {seconds}s each", flush=True)
    with cf.ThreadPoolExecutor(jobs) as ex:
        futs = {ex.submit(run_one, sc, m, seconds): (sc["name"], m) for sc, m in work}
        for f in cf.as_completed(futs):
            n, m = futs[f]
            try:
                f.result()
                print("done", n, m, flush=True)
            except Exception as e:  # noqa: BLE001
                print("FAILED", n, m, e, flush=True)


if __name__ == "__main__":
    main()
