#!/usr/bin/env python3
"""The compact tables docs/73 §11 prints, generated from the results so no
number is transcribed by hand. Prints markdown to stdout."""
import glob
import json
import os

OUT = os.environ.get("UPLINK_OUT", os.path.join(os.path.dirname(os.path.abspath(__file__)), "out"))
R = {}
for p in glob.glob(f"{OUT}/results/*/*/summary.json"):
    mode = os.path.basename(os.path.dirname(p))
    name = os.path.basename(os.path.dirname(os.path.dirname(p)))
    R.setdefault(name, {})[mode] = json.load(open(p))


def get(name, mode):
    return R.get(name, {}).get(mode)


def f(x, d=0):
    return "–" if x is None else f"{x:.{d}f}"


def pkts(name, mode):
    p = f"{OUT}/results/{name}/{mode}/send.csv"
    if not os.path.exists(p):
        return None
    S = [l for l in open(p) if l.startswith("S,")]
    F = [l.split(",") for l in open(p) if l.startswith("F,")]
    if not S or not F:
        return None
    dur = (int(F[-1][6]) - int(F[0][6])) / 1e9 + 1.5
    return int(S[-1].split(",")[3]) / dur


def ready(s):
    r = s["transport"]["ready_lat_ms"]
    return f"{f(r['p50'])} / {f(r['p95'])} / {f(r['p99'])}"


def bal(s):
    a = s["view"]["adaptive"]
    return f"{f(a['offsetMs']['mean'])} ms · {f(min(100, a['shownPct']), 1)} % · {f(a['stallPct'], 1)} %"


def le(s):
    o = s["view"]["off"]
    return f"{f(o['stallPct'], 1)} %"


print("#### Table A — the clean path (F1)\n")
print("| RTT | Controller | Delivery | Decode-ready p50 / p95 / p99 ms | Mbps on the wire | Packets/s |")
print("|---|---|---|---|---|---|")
for rtt in (10, 40, 100):
    for cc, sfx in (("Cubic", ""), ("BBR", "-bbr")):
        for d, m in (("datagram", "dgram"), ("carrier", "carrier")):
            s = get(f"rtt{rtt}-clean", m + sfx)
            if not s:
                continue
            print(f"| {rtt} ms | {cc} | {d} | {ready(s)} | {f(s['transport']['uplink_mbps'], 2)} | {f(pkts(f'rtt{rtt}-clean', m + sfx))} |")

SC = {"ge1pct": "bursts (GE, ≈1 %)", "out20drop": "radio away 20 ms/1.07 s",
      "out80drop": "radio away 80 ms/1.07 s", "out80hold": "radio away 80 ms, held"}
print("\n#### Table B — bursts and outages, BBR (F2)\n")
print("| RTT | Path | Delivery | Undecodable | GOPs broken | Decode-ready p50 / p95 / p99 ms | Balanced: offset · shown · stalled | Live edge: stalled |")
print("|---|---|---|---|---|---|---|---|")
for rtt in (10, 40, 100):
    for sc, label in SC.items():
        for d, m in (("datagram", "dgram-bbr"), ("carrier", "carrier-bbr")):
            s = get(f"rtt{rtt}-{sc}", m)
            if not s:
                continue
            t = s["transport"]
            print(f"| {rtt} ms | {label} | {d} | {f(t['undecodable_pct'], 1)} % | {f(t['broken_gops_pct'])} % | {ready(s)} | {bal(s)} | {le(s)} |")

print("\n#### Table C — random loss, BBR (F3)\n")
print("| RTT | Loss | Delivery | Undecodable | Decode-ready p50 / p95 / p99 ms | Balanced: offset · shown · stalled | Live edge: stalled |")
print("|---|---|---|---|---|---|---|")
for rtt in (10, 40, 100):
    for loss in ("0.001", "0.005", "0.01", "0.02"):
        for d, m in (("datagram", "dgram-bbr"), ("carrier", "carrier-bbr")):
            s = get(f"rtt{rtt}-loss{loss}", m)
            if not s:
                continue
            t = s["transport"]
            print(f"| {rtt} ms | {float(loss) * 100:g} % | {d} | {f(t['undecodable_pct'], 1)} % | {ready(s)} | {bal(s)} | {le(s)} |")

print("\n#### Table D — Cubic against BBR under random loss (F4, F5)\n")
print("| RTT | Loss | Delivery | Cubic: Mbps · undecodable · p99 ms | BBR: Mbps · undecodable · p99 ms |")
print("|---|---|---|---|---|")
for rtt in (10, 40, 100):
    for loss in ("0.001", "0.005", "0.01", "0.02"):
        for d, m in (("datagram", "dgram"), ("carrier", "carrier")):
            c, b = get(f"rtt{rtt}-loss{loss}", m), get(f"rtt{rtt}-loss{loss}", m + "-bbr")
            if not c or not b:
                continue

            def cell(s):
                t = s["transport"]
                return f"{f(t['uplink_mbps'], 1)} · {f(t['undecodable_pct'], 1)} % · {f(t['ready_lat_ms']['p99'])}"
            print(f"| {rtt} ms | {float(loss) * 100:g} % | {d} | {cell(c)} | {cell(b)} |")

print("\n#### Table E — 1440p60 (38.9 Mbps mean, 50 Mbps ceiling), 10 ms RTT\n")
print("| Path | Controller | Delivery | Mbps | Undecodable | Decode-ready p50 / p95 / p99 ms | Shed / expired | Balanced: offset · shown · stalled |")
print("|---|---|---|---|---|---|---|---|")
HI = [("clean", "clean"), ("loss0.001", "0.1 % loss"), ("ge1pct", "bursts (GE)"),
      ("rate300M", "300 Mbps bottleneck"), ("rate300M-out80drop", "300 Mbps + outages"),
      ("rate80M", "80 Mbps bottleneck"), ("rate80M-out80drop", "80 Mbps + outages")]
for sc, label in HI:
    for cc, sfx in (("Cubic", ""), ("BBR", "-bbr")):
        for d, m in (("datagram", "dgram"), ("carrier", "carrier")):
            s = get(f"hi-rtt10-{sc}", m + sfx)
            if not s:
                continue
            t = s["transport"]
            shed = "–" if m == "dgram" else f"{t.get('shed_records', 0)} / {t['carriers_expired']}"
            print(f"| {label} | {cc} | {d} | {f(t['uplink_mbps'], 1)} | {f(t['undecodable_pct'], 1)} % | {ready(s)} | {shed} | {bal(s)} |")

print("\n#### Table F — with 3 ms of path jitter (robustness)\n")
print("| RTT | Path | Controller | Delivery | Undecodable | Decode-ready p99 ms | Balanced: offset · shown · stalled | Live edge: stalled |")
print("|---|---|---|---|---|---|---|---|")
for rtt in (10, 40):
    for sc in ("clean", "loss0.001", "loss0.005", "ge1pct", "out80drop"):
        for cc, sfx in (("Cubic", ""), ("BBR", "-bbr")):
            for d, m in (("datagram", "dgram"), ("carrier", "carrier")):
                s = get(f"jit-rtt{rtt}-{sc}", m + sfx)
                if not s:
                    continue
                t = s["transport"]
                print(f"| {rtt} ms | {sc} | {cc} | {d} | {f(t['undecodable_pct'], 1)} % | {f(t['ready_lat_ms']['p99'])} | {bal(s)} | {le(s)} |")

print("\n#### Figure — the Balanced offset through a loss episode (10 ms RTT, 2 % loss for 10 s of every 40 s)\n")
print("| Second | " + " | ".join(str(x) for x in range(0, 100, 5)) + " |")
print("|---|" + "---|" * 20)
for cc, sfx in (("Cubic", ""), ("BBR", "-bbr")):
    for d, m in (("datagram", "dgram"), ("carrier", "carrier")):
        s = get("rtt10-episodic", m + sfx)
        if not s or "timeline" not in s["view"]["adaptive"]:
            continue
        tl = {x["s"]: x for x in s["view"]["adaptive"]["timeline"]}
        row = [f(tl[x]["offset"]) if x in tl and tl[x]["offset"] is not None else "–" for x in range(0, 100, 5)]
        print(f"| {d} · {cc} | " + " | ".join(row) + " |")

print("\n#### Table H — receive windows at 1440p, carrier · BBR (D10)\n")
print("| Path | Windows (stream / connection) | Writes blocked > 1 ms | Decode-ready p50 / p95 / p99 ms | Balanced: offset · shown · stalled |")
print("|---|---|---|---|---|")
for sc, label in (("clean", "clean"), ("rate80M-out80drop", "80 Mbps + outages")):
    for pre, win in (("hi", "512 KiB / 768 KiB (quic-go default)"), ("win-hi", "2 MB / 3 MB")):
        s = get(f"{pre}-rtt10-{sc}", "carrier-bbr")
        if not s:
            continue
        print(f"| {label} | {win} | {s['transport']['blocked_writes']} | {ready(s)} | {bal(s)} |")

print("\n#### Table I — what a live-edge viewer's hitches look like, BBR (F2, §6)\n")
print("| RTT | Path | Delivery | Hitches per minute: 50–100 · 100–250 · 250–500 · ≥ 500 ms | Longest |")
print("|---|---|---|---|---|")
for rtt in (10, 40):
    for sc, label in (("ge1pct", "bursts (GE)"), ("out20drop", "radio away 20 ms"), ("out80drop", "radio away 80 ms")):
        for d, m in (("datagram", "dgram-bbr"), ("carrier", "carrier-bbr")):
            s = get(f"rtt{rtt}-{sc}", m)
            if not s:
                continue
            b = s["view"]["off"]["hitchBandsPerMin"]
            print(f"| {rtt} ms | {label} | {d} | {f(b['50-100'])} · {f(b['100-250'])} · {f(b['250-500'])} · {f(b['500+'])} | {f(s['view']['off']['longestHoldMs'])} ms |")


def sv(s, k):
    x = s["view"][k]
    return f"{f(min(100, x['shownPct']), 1)} % · {f(x['stallPct'], 1)} %"


def freezes(s, k):
    b = s["view"][k]["hitchBandsPerMin"]
    return f(b["250-500"] + b["500+"])


print("\n#### Table J — what each viewer preset sees, by uplink delivery, BBR (F14)\n")
print("Shown · stalled. Lowest latency and Balanced get parity; Smoother and Most stable ride reliable")
print("delivery and are served none (`hub.NegotiateParity`), so they are sent only frames that reached")
print("the relay whole. Freezes: holds ≥ 250 ms per minute. For Lowest latency \"shown\" also falls when")
print("two frames land in one vsync (the newer wins); its stalled time is the loss measure.\n")
print("| RTT | Path | Uplink | Lowest latency | Balanced | Smoother | Most stable | Smoother freezes /min |")
print("|---|---|---|---|---|---|---|---|")
JP = [("clean", "clean"), ("loss0.001", "0.1 % loss"), ("loss0.01", "1 % loss"), ("ge1pct", "bursts (GE)"),
      ("out20drop", "radio away 20 ms"), ("out80drop", "radio away 80 ms")]
for rtt in (10, 40, 100):
    for sc, label in JP:
        for d, m in (("datagram", "dgram-bbr"), ("carrier", "carrier-bbr")):
            s = get(f"rtt{rtt}-{sc}", m)
            if not s or "resilient" not in s["view"]:
                continue
            print(f"| {rtt} ms | {label} | {d} | {sv(s, 'off')} | {sv(s, 'adaptive')} | {sv(s, 'resilient')} "
                  f"| {sv(s, 'deep')} | {freezes(s, 'resilient')} |")

print("\n#### Table K — what-if: the relay serves parity to reliable delivery too, datagram uplink, BBR (F14, OD6)\n")
print("| RTT | Path | Smoother today → with parity | Most stable today → with parity | Smoother on the carrier uplink |")
print("|---|---|---|---|---|")
KP = [("loss0.001", "0.1 % loss"), ("loss0.005", "0.5 % loss"), ("loss0.01", "1 % loss"), ("loss0.02", "2 % loss"),
      ("ge1pct", "bursts (GE)"), ("out20drop", "radio away 20 ms"), ("out80drop", "radio away 80 ms")]
for rtt in (10, 40, 100):
    for sc, label in KP:
        s = get(f"rtt{rtt}-{sc}", "dgram-bbr")
        c = get(f"rtt{rtt}-{sc}", "carrier-bbr")
        if not s or "resilient-parity" not in s["view"]:
            continue
        print(f"| {rtt} ms | {label} | {sv(s, 'resilient')} → {sv(s, 'resilient-parity')} "
              f"| {sv(s, 'deep')} → {sv(s, 'deep-parity')} | {sv(c, 'resilient') if c else '–'} |")

LP = [("clean", "clean"), ("loss0.001", "0.1 % loss"), ("loss0.005", "0.5 % loss"), ("loss0.01", "1 % loss"),
      ("loss0.02", "2 % loss"), ("ge1pct", "bursts (GE)"), ("out20drop", "radio away 20 ms"),
      ("out80drop", "radio away 80 ms")]
HP = [("clean", "1440p clean"), ("loss0.001", "1440p 0.1 % loss"), ("ge1pct", "1440p bursts"),
      ("rate300M-out80drop", "1440p 300 Mbps + outages"), ("rate80M-out80drop", "1440p 80 Mbps + outages")]
OPTS = [("dgram", "today"), ("dgram-bbr", "BBR only"), ("dgram-bigq", "queue only"),
        ("dgram-bbr-bigq", "BBR + queue"), ("carrier-bbr", "carrier + BBR")]


def cells():
    for rtt in (10, 40, 100):
        for sc, label in LP:
            yield f"{rtt} ms", label, f"rtt{rtt}-{sc}"
    for sc, label in HP:
        yield "10 ms", label, f"hi-rtt10-{sc}"


def opt_table(title, view, extra=None):
    print(f"\n#### {title}\n")
    heads = [o[1] for o in OPTS] + ([extra[1]] if extra else [])
    print("| RTT | Path | " + " | ".join(heads) + " |")
    print("|---|---|" + "---|" * len(heads))
    for rtt, label, name in cells():
        row = []
        for m, _ in OPTS:
            s = get(name, m)
            row.append(sv(s, view) if s and view in s["view"] else "–")
        if extra:
            s = get(name, extra[0][0])
            row.append(sv(s, extra[0][1]) if s and extra[0][1] in s["view"] else "–")
        if all(r == "–" for r in row):
            continue
        print(f"| {rtt} | {label} | " + " | ".join(row) + " |")


print("\n#### Table L — what the cheap changes buy without the carrier (shown · stalled)\n")
print("\"today\": datagrams, Cubic, quic-go's 32-deep queue. \"queue\": the receiver built with D7's 4096-deep")
print("queue. \"carrier + BBR\" is OD1 + OD2 for comparison; its deltas never meet the queue.")
opt_table("Table L1 — Balanced (the default viewer)", "adaptive")
opt_table("Table L2 — Lowest latency (stalled is the loss measure)", "off")
opt_table("Table L3 — Smoother (no parity today; the last column adds OD6's parity)", "resilient",
          (("dgram-bbr-bigq", "resilient-parity"), "BBR + queue + OD6 parity"))
opt_table("Table L4 — Most stable (no parity today; the last column adds OD6's parity)", "deep",
          (("dgram-bbr-bigq", "deep-parity"), "BBR + queue + OD6 parity"))

print("\n#### Table M — run-to-run spread: three loss-process seeds per cell (shown, % of frames)\n")
print("| Path | Delivery | Balanced: seeds 7 · 8 · 9 | Smoother: seeds 7 · 8 · 9 | Lowest latency stalled: seeds 7 · 8 · 9 |")
print("|---|---|---|---|---|")
for name, label in (("rtt40-loss0.005", "40 ms, 0.5 % loss"), ("rtt40-loss0.01", "40 ms, 1 % loss"),
                    ("rtt10-ge1pct", "10 ms, bursts (GE)"), ("rtt10-out80drop", "10 ms, radio away 80 ms")):
    for m, d in (("dgram-bbr", "BBR only"), ("dgram-bbr-bigq", "BBR + queue"), ("carrier-bbr", "carrier + BBR")):
        runs = [get(n, m) for n in (name, f"{name}-s8", f"{name}-s9")]
        if not all(runs):
            continue

        def pick(k, metric):
            return " · ".join(f(min(100, r["view"][k][metric]), 1) for r in runs)
        print(f"| {label} | {d} | {pick('adaptive', 'shownPct')} | {pick('resilient', 'shownPct')} | {pick('off', 'stallPct')} |")
