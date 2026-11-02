package media

import (
	"time"

	"github.com/Tuhis/gawk/gawk-server/publisher"
)

// PipelineConfig configures a session's media adapter.
type PipelineConfig struct {
	Reorder ReorderConfig
	GOP     GOPConfig
}

// AudioPacket is one Opus RTP packet.
type AudioPacket struct {
	Seq       uint16
	Timestamp uint32
	Payload   []byte
}

// Output is what one input produced.
type Output struct {
	// AUs are ready for the engine, in order.
	AUs []publisher.AccessUnit
	// Audio packets are ready for the engine.
	Audio []publisher.AudioPacket
	// NACK lists video sequence numbers to NACK now.
	NACK []uint16
	// WindowDrops counts frames D6's window gave up on; IDRWaitDrops whole
	// frames discarded while waiting for an IDR after a loss.
	WindowDrops  int
	IDRWaitDrops int
	// Violation ends the session (bframes, invalid_h264).
	Violation *Violation
}

// Pipeline is one session's RTP → engine adapter: the reorder window (D6),
// the assembler, the GOP meter (D7) and the timeline. Not safe for
// concurrent use; the session loop owns it.
type Pipeline struct {
	reorder *Reorder
	asm     *Assembler
	gop     *GOPMeter
	tl      Timeline

	audioStarted bool
	audioSeq     uint16

	// counters for Meter
	videoBytes uint64
	frames     uint64
	sps        *SPS
}

// NewPipeline builds a pipeline.
func NewPipeline(cfg PipelineConfig) *Pipeline {
	return &Pipeline{
		reorder: NewReorder(cfg.Reorder),
		asm:     NewAssembler(),
		gop:     NewGOPMeter(cfg.GOP),
	}
}

// SetParameterSets seeds sprop-parameter-sets from the SDP.
func (p *Pipeline) SetParameterSets(sps, pps []byte) {
	p.asm.SetParameterSets(sps, pps)
	if p.asm.spsInfo != nil && p.sps == nil {
		p.sps = p.asm.spsInfo
	}
}

// GOP is the session's GOP meter.
func (p *Pipeline) GOP() *GOPMeter { return p.gop }

// Reorder is the session's reorder window.
func (p *Pipeline) Reorder() *Reorder { return p.reorder }

// SPS is the last SPS seen.
func (p *Pipeline) SPS() *SPS { return p.sps }

// Timeline is the session's A/V timeline.
func (p *Pipeline) Timeline() *Timeline { return &p.tl }

// Video feeds one video packet that arrived at now (nowUs on the engine's
// clock).
func (p *Pipeline) Video(pkt Packet, now time.Time, nowUs uint64) Output {
	p.videoBytes += uint64(len(pkt.Payload))
	// D7 records IDR starts on arrival, before D6 can drop the frame.
	p.gop.Packet(pkt.Timestamp, PacketCarriesIDR(pkt.Payload))
	return p.handle(p.reorder.Push(pkt, now), nowUs)
}

// Tick releases what a passing deadline allows.
func (p *Pipeline) Tick(now time.Time, nowUs uint64) Output {
	return p.handle(p.reorder.Tick(now), nowUs)
}

// Deadline is when Tick next has work.
func (p *Pipeline) Deadline() (time.Time, bool) { return p.reorder.Deadline() }

func (p *Pipeline) handle(r ReorderResult, nowUs uint64) Output {
	out := Output{NACK: r.NACK, WindowDrops: r.Dropped}
	p.frames += uint64(r.Dropped)
	for _, f := range r.Frames {
		p.frames++
		if f.AfterGap {
			p.asm.Dropped()
		}
		res := p.asm.Frame(f)
		if res.SPS != nil {
			p.sps = res.SPS
		}
		if res.Violation != nil {
			out.Violation = res.Violation
			return out
		}
		if res.Skipped {
			out.IDRWaitDrops++
			continue
		}
		if res.AU == nil {
			continue
		}
		out.AUs = append(out.AUs, publisher.AccessUnit{
			Data:             res.AU.Data,
			Keyframe:         res.AU.Keyframe,
			TimestampUs:      p.tl.Video(res.AU.Timestamp, nowUs),
			EncoderRestarted: res.AU.ParamsChanged,
		})
	}
	return out
}

// Audio feeds one Opus packet. Audio isn't reordered or NACKed: a late or
// duplicate packet is dropped, and a lost one is a gap the viewer's Opus
// decoder conceals.
func (p *Pipeline) Audio(pkt AudioPacket, nowUs uint64) (publisher.AudioPacket, bool) {
	if p.audioStarted && int16(pkt.Seq-p.audioSeq) <= 0 {
		return publisher.AudioPacket{}, false
	}
	p.audioStarted, p.audioSeq = true, pkt.Seq
	if len(pkt.Payload) == 0 {
		return publisher.AudioPacket{}, false
	}
	return publisher.AudioPacket{Data: pkt.Payload, TimestampUs: p.tl.Audio(pkt.Timestamp, nowUs)}, true
}

// Counters is a snapshot for Meter.
type Counters struct {
	Bytes    uint64
	Frames   uint64
	Expected uint64
	Lost     uint64
}

// Counters snapshots the pipeline's cumulative counters.
func (p *Pipeline) Counters() Counters {
	return Counters{Bytes: p.videoBytes, Frames: p.frames, Expected: p.reorder.Expected(), Lost: p.reorder.Lost()}
}

// Meter turns cumulative counters into rates over a sliding window of
// samples (fps, kbps, loss %), for D9's status.
type Meter struct {
	window  int
	samples []meterSample
}

type meterSample struct {
	at time.Time
	c  Counters
}

// NewMeter keeps the last window+1 samples.
func NewMeter(window int) *Meter {
	if window < 1 {
		window = 1
	}
	return &Meter{window: window}
}

// Rates is what the meter reports.
type Rates struct {
	FPS         float64
	BitrateKbps int
	LossPct     float64
	OK          bool
}

// Sample adds a snapshot and returns the rates over the window.
func (m *Meter) Sample(now time.Time, c Counters) Rates {
	m.samples = append(m.samples, meterSample{at: now, c: c})
	if len(m.samples) > m.window+1 {
		m.samples = m.samples[len(m.samples)-m.window-1:]
	}
	if len(m.samples) < 2 {
		return Rates{}
	}
	a, b := m.samples[0], m.samples[len(m.samples)-1]
	dt := b.at.Sub(a.at).Seconds()
	if dt <= 0 {
		return Rates{}
	}
	r := Rates{OK: true}
	r.FPS = float64(b.c.Frames-a.c.Frames) / dt
	r.BitrateKbps = int(float64(b.c.Bytes-a.c.Bytes) * 8 / 1000 / dt)
	if exp := b.c.Expected - a.c.Expected; exp > 0 {
		r.LossPct = float64(b.c.Lost-a.c.Lost) * 100 / float64(exp)
	}
	return r
}

// Lost records that frames were lost after the pipeline (the engine fell
// behind): deltas wait for the next IDR.
func (p *Pipeline) Lost() { p.asm.Dropped() }
