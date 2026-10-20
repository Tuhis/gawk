package main

import (
	"flag"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// The proxy is one client ↔ one server UDP forwarder with a scripted path:
//
//   - one-way delay in each direction (rtt/2 each way);
//   - random loss per direction: Bernoulli (-loss) or Gilbert-Elliott
//     (-ge-* flags; the bad state loses -ge-loss of its packets);
//   - periodic outages on both directions at once — the radio is away, so
//     nothing is sent or received — either dropping what is offered
//     (-outage-mode drop) or holding it until the radio returns (hold);
//   - an uplink bottleneck (-rate) with a drop-tail queue (-queue).
//
// Impairments arm after -arm-after so the handshake is not part of the
// experiment. The RNG is seeded, so a scenario's loss process repeats
// exactly; what it lands on depends on what the sender offers.
type pathSpec struct {
	delay      time.Duration
	jitter     time.Duration // extra uniform delay per packet, order kept
	loss       float64
	geGood2Bad float64
	geBad2Good float64
	geBadLoss  float64
	rateBps    float64
	queueLimit time.Duration
	outagePer  time.Duration
	outageLen  time.Duration
	outageHold bool
	// Random loss applies only inside episodes when episodePer > 0: the
	// first episodeLen of every episodePer after arming.
	episodePer time.Duration
	episodeLen time.Duration
	armAfter   time.Duration // the zero point of the episode schedule
}

type proxyCounters struct {
	forwarded, randomLoss, outageDrop, outageHeld, queueDrop atomic.Uint64
}

func runProxy(args []string) error {
	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:4600", "client-facing UDP address")
	upstream := fs.String("upstream", "127.0.0.1:4500", "server UDP address")
	rtt := fs.Duration("rtt", 10*time.Millisecond, "base round-trip time added by the path")
	jitter := fs.Duration("jitter", 0, "extra uniform one-way delay in [0, jitter) per packet, both ways; order is kept")
	loss := fs.Float64("loss", 0, "uplink (client→server) Bernoulli loss probability")
	downLoss := fs.Float64("down-loss", 0, "downlink (server→client, the ACKs) Bernoulli loss probability")
	geG2B := fs.Float64("ge-g2b", 0, "Gilbert-Elliott good→bad transition probability per packet (uplink; overrides -loss)")
	geB2G := fs.Float64("ge-b2g", 0, "Gilbert-Elliott bad→good transition probability per packet")
	geLoss := fs.Float64("ge-loss", 1, "loss probability in the Gilbert-Elliott bad state")
	rate := fs.Float64("rate", 0, "uplink bottleneck in bits/s (0 = none)")
	queue := fs.Duration("queue", 50*time.Millisecond, "uplink bottleneck queue, as time at -rate")
	outPer := fs.Duration("outage-period", 0, "outage period (0 = none)")
	outLen := fs.Duration("outage-len", 0, "outage length")
	outMode := fs.String("outage-mode", "drop", "drop | hold")
	epPer := fs.Duration("episode-period", 0, "random loss only in episodes of this period (0 = always)")
	epLen := fs.Duration("episode-len", 0, "episode length")
	seed := fs.Uint64("seed", 1, "RNG seed")
	arm := fs.Duration("arm-after", 3*time.Second, "impairments start this long after the first packet")
	stats := fs.String("stats", "", "write counters here on exit")
	_ = fs.Parse(args)

	up := pathSpec{
		delay: *rtt / 2, jitter: *jitter, loss: *loss,
		geGood2Bad: *geG2B, geBad2Good: *geB2G, geBadLoss: *geLoss,
		rateBps: *rate, queueLimit: *queue,
		outagePer: *outPer, outageLen: *outLen, outageHold: *outMode == "hold",
		episodePer: *epPer, episodeLen: *epLen, armAfter: *arm,
	}
	down := pathSpec{
		delay: *rtt / 2, jitter: *jitter, loss: *downLoss,
		outagePer: *outPer, outageLen: *outLen, outageHold: *outMode == "hold",
	}

	laddr, err := net.ResolveUDPAddr("udp", *listen)
	if err != nil {
		return err
	}
	uaddr, err := net.ResolveUDPAddr("udp", *upstream)
	if err != nil {
		return err
	}
	front, err := net.ListenUDP("udp", laddr)
	if err != nil {
		return err
	}
	back, err := net.DialUDP("udp", nil, uaddr)
	if err != nil {
		return err
	}
	for _, c := range []*net.UDPConn{front, back} {
		_ = c.SetReadBuffer(8 << 20)
		_ = c.SetWriteBuffer(8 << 20)
	}

	var (
		upC, downC proxyCounters
		client     atomic.Pointer[net.UDPAddr]
		epoch      atomic.Int64 // first packet, ns
	)
	armed := func(now time.Time) bool {
		e := epoch.Load()
		return e != 0 && now.UnixNano()-e >= arm.Nanoseconds()
	}

	upQ := newShaper(up, rand.New(rand.NewPCG(*seed, 1)), &upC, armed, &epoch, func(b []byte) {
		_, _ = back.Write(b)
	})
	downQ := newShaper(down, rand.New(rand.NewPCG(*seed, 2)), &downC, armed, &epoch, func(b []byte) {
		if a := client.Load(); a != nil {
			_, _ = front.WriteToUDP(b, a)
		}
	})

	go func() {
		buf := make([]byte, 2048)
		for {
			n, a, err := front.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if client.Load() == nil {
				client.Store(a)
				epoch.CompareAndSwap(0, time.Now().UnixNano())
			}
			upQ.offer(buf[:n])
		}
	}()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, err := back.Read(buf)
			if err != nil {
				return
			}
			downQ.offer(buf[:n])
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	report := fmt.Sprintf(
		"up forwarded=%d random_loss=%d outage_drop=%d outage_held=%d queue_drop=%d\n"+
			"down forwarded=%d random_loss=%d outage_drop=%d outage_held=%d queue_drop=%d\n",
		upC.forwarded.Load(), upC.randomLoss.Load(), upC.outageDrop.Load(), upC.outageHeld.Load(), upC.queueDrop.Load(),
		downC.forwarded.Load(), downC.randomLoss.Load(), downC.outageDrop.Load(), downC.outageHeld.Load(), downC.queueDrop.Load())
	fmt.Fprint(os.Stderr, report)
	if *stats != "" {
		_ = os.WriteFile(*stats, []byte(report), 0o644)
	}
	return nil
}

type packet struct {
	at   time.Time
	data []byte
}

// shaper decides each packet's fate and departure time on arrival, then a
// single writer goroutine releases packets in order at their departure time.
// FIFO order holds because every packet's departure is non-decreasing.
type shaper struct {
	spec    pathSpec
	rng     *rand.Rand
	c       *proxyCounters
	armed   func(time.Time) bool
	epoch   *atomic.Int64
	out     chan packet
	mu      sync.Mutex
	bad     bool      // Gilbert-Elliott state
	busy    time.Time // bottleneck busy until
	lastDep time.Time
}

func newShaper(spec pathSpec, rng *rand.Rand, c *proxyCounters, armed func(time.Time) bool,
	epoch *atomic.Int64, write func([]byte)) *shaper {
	s := &shaper{spec: spec, rng: rng, c: c, armed: armed, epoch: epoch, out: make(chan packet, 1<<16)}
	go func() {
		for p := range s.out {
			if d := time.Until(p.at); d > 0 {
				sleepPrecise(d)
			}
			write(p.data)
		}
	}()
	return s
}

// sleepPrecise sleeps most of d and spins the last stretch: the Go timer
// overshoots by tens of microseconds to a millisecond under load, which would
// otherwise show up as path jitter in a latency measurement.
func sleepPrecise(d time.Duration) {
	end := time.Now().Add(d)
	if d > 2*time.Millisecond {
		time.Sleep(d - time.Millisecond)
	}
	for time.Now().Before(end) {
	}
}

// outageEnd reports whether t is inside an outage and when it ends.
func (s *shaper) outageEnd(t time.Time) (bool, time.Time) {
	if s.spec.outagePer <= 0 || s.spec.outageLen <= 0 {
		return false, time.Time{}
	}
	since := time.Duration(t.UnixNano() - s.epoch.Load())
	pos := since % s.spec.outagePer
	if pos < s.spec.outageLen {
		return true, t.Add(s.spec.outageLen - pos)
	}
	return false, time.Time{}
}

func (s *shaper) offer(b []byte) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	dep := now
	if s.armed(now) {
		// Radio loss first: a packet lost on the air never reaches a queue.
		if s.lost(now) {
			s.c.randomLoss.Add(1)
			return
		}
		if in, end := s.outageEnd(now); in {
			if !s.spec.outageHold {
				s.c.outageDrop.Add(1)
				return
			}
			s.c.outageHeld.Add(1)
			dep = end
		}
		if s.spec.rateBps > 0 {
			start := dep
			if s.busy.After(start) {
				start = s.busy
			}
			if start.Sub(dep) > s.spec.queueLimit {
				s.c.queueDrop.Add(1)
				return
			}
			s.busy = start.Add(time.Duration(float64(len(b)*8) / s.spec.rateBps * float64(time.Second)))
			dep = s.busy
		}
	}
	dep = dep.Add(s.spec.delay)
	if s.spec.jitter > 0 {
		dep = dep.Add(time.Duration(s.rng.Int64N(int64(s.spec.jitter))))
	}
	// FIFO: a path reorders nothing here, so jitter delays the packets
	// queued behind a slow one rather than overtaking it.
	if dep.Before(s.lastDep) {
		dep = s.lastDep
	}
	s.lastDep = dep
	s.c.forwarded.Add(1)
	cp := make([]byte, len(b))
	copy(cp, b)
	s.out <- packet{at: dep, data: cp}
}

func (s *shaper) lost(now time.Time) bool {
	if s.spec.episodePer > 0 {
		since := time.Duration(now.UnixNano()-s.epoch.Load()) - s.spec.armAfter
		if since < 0 || since%s.spec.episodePer >= s.spec.episodeLen {
			return false
		}
	}
	if s.spec.geGood2Bad > 0 {
		if s.bad {
			if s.rng.Float64() < s.spec.geBad2Good {
				s.bad = false
			}
		} else if s.rng.Float64() < s.spec.geGood2Bad {
			s.bad = true
		}
		return s.bad && s.rng.Float64() < s.spec.geBadLoss
	}
	return s.spec.loss > 0 && s.rng.Float64() < s.spec.loss
}
