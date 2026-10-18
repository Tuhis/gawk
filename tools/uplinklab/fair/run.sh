#!/bin/sh
# Inside the container (docs/73 RU4) — or any network namespace of its own,
# e.g. `unshare -rn` (fair/chrome.sh) — a home uplink with a household on it.
#
#   run.sh <out> <rate> <mode|none> <cc> <trace> <tcp: 1|0> [seconds]
#
# One bottleneck on lo, in the upstream direction only: 5 ms of delay each way
# (netem), then everything bound for the relay (UDP 4500), the TCP sink
# (5201) and ping's echo requests through one tbf at <rate> with a 50 ms
# drop-tail queue. Acknowledgements and echo replies come back unshaped, as
# they would down a separate downlink. The broadcast (lab sender -> lab
# receiver) runs throughout; a bulk TCP flow (iperf3, the kernel's Cubic)
# from 20 s to the end when <tcp> is 1; ping the whole time.
# LAB (default /lab) holds bin/; SENDER overrides the sender command (the
# browser sender, fair/chrome.sh); IPERF3 the iperf3 binary.
set -u
out=$1 rate=$2 mode=$3 cc=$4 trace=$5 tcp=$6 secs=${7:-80}
LAB=${LAB:-/lab} IPERF3=${IPERF3:-iperf3}
mkdir -p "$out"
ip link set lo up
ip link set lo mtu 1500
tc qdisc add dev lo root handle 1: netem delay 5ms limit 100000
tc qdisc add dev lo parent 1:1 handle 2: prio bands 2 priomap 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0
tc qdisc add dev lo parent 2:1 handle 21: pfifo limit 100000
tc qdisc add dev lo parent 2:2 handle 22: tbf rate "$rate" burst 32kb latency 50ms
tc filter add dev lo parent 2: protocol ip prio 1 u32 match ip protocol 17 0xff match ip dport 4500 0xffff flowid 2:2
tc filter add dev lo parent 2: protocol ip prio 1 u32 match ip protocol 6 0xff match ip dport 5201 0xffff flowid 2:2
tc filter add dev lo parent 2: protocol ip prio 1 u32 match ip protocol 1 0xff match u8 8 0xff at 20 flowid 2:2
ping -i 0.2 -w $((secs + 2)) 127.0.0.1 > "$out/ping.txt" 2>&1 &
"$IPERF3" -s -1 -D
sp= rp=
if [ "$mode" != none ]; then
  "$LAB/bin/gawk-uplinklab-bigq" recv -addr 127.0.0.1:4500 -out "$out/recv.csv" -cert-hash-file "$out/cert.hash" \
    2> "$out/recv.log" &
  rp=$!
  sleep 0.3
  ${SENDER:-$LAB/bin/sender} --url https://127.0.0.1:4500/lab --trace "$trace" --seconds "$secs" --mode "$mode" --cc "$cc" \
    --out "$out/send.csv" 2> "$out/send.log" &
  sp=$!
fi
sleep 20
if [ "$tcp" = 1 ]; then "$IPERF3" -c 127.0.0.1 -t $((secs - 20)) -i 1 -J > "$out/iperf.json"; else sleep $((secs - 20)); fi
if [ -n "$sp" ]; then wait $sp; sleep 0.5; kill -INT $rp; wait $rp; fi
wait
tc -s qdisc show dev lo > "$out/qdisc.txt"
tc -s class show dev lo >> "$out/qdisc.txt"
chown -R "${OWNER:-0:0}" "$out"
