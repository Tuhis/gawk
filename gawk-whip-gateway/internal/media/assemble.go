package media

import (
	"bytes"
	"errors"

	"github.com/Tuhis/gawk/gawk-server/publisher"
)

// Violation is a stream the gateway refuses to republish: the session ends
// with Reason (a status.Reason* string, set by the caller's mapping) and D7's
// backoff starts.
type Violation struct {
	Kind ViolationKind
	Err  error
}

func (v *Violation) Error() string { return v.Err.Error() }

// ViolationKind says why.
type ViolationKind int

const (
	// ViolationBFrames: a B slice (slice_type 1 or 6).
	ViolationBFrames ViolationKind = iota + 1
	// ViolationInvalid: not a decodable H.264 stream for gawk's viewers.
	ViolationInvalid
)

// AU is one rebuilt access unit.
type AU struct {
	Timestamp uint32 // RTP, 90 kHz
	Data      []byte // Annex-B
	Keyframe  bool
	// ParamsChanged marks an IDR whose SPS differs from the one before it
	// (the engine's EncoderRestarted: re-derive the decoder config).
	ParamsChanged bool
}

// AssembleResult is what one frame produced.
type AssembleResult struct {
	AU *AU
	// Skipped is set when a whole frame was discarded here: a delta while
	// waiting for an IDR, or an IDR with no parameter sets to go with it.
	Skipped bool
	// SPS is set when this frame carried (or was given) a parsed SPS.
	SPS *SPS
	// Violation ends the session.
	Violation *Violation
}

// Assembler turns complete frames into Annex-B access units and enforces
// gawk's encoder invariants on what OBS sends (docs/19, docs/83 §3.3: OBS
// forces bf=0 and repeat_headers for WHIP, but older OBS and other WHIP
// senders may not):
//
//   - Every keyframe is an IDR carrying its SPS and PPS in-band, because the
//     relay primes late joiners from the cached keyframe alone and the
//     Annex-B path's DecoderConfig has no extradata. An IDR without them gets
//     the last ones seen (in-band earlier, out-of-band in a parameter-set-only
//     frame, or from the SDP's sprop-parameter-sets) prepended. An IDR with
//     none known is skipped; MaxIDRWithoutParams of those in a row is
//     ViolationInvalid.
//   - No B-frames. Detected from slice_type in every slice header
//     (publisher.HasBSlices, the engine's own assertion): definitive per
//     slice, unlike the SPS's POC type or VUI max_num_reorder_frames, which
//     are hints many encoders omit or set conservatively.
//   - The SPS parses and names a profile and level viewers decode (D5).
//
// After a loss (Dropped) it discards deltas until the next IDR, exactly as a
// viewer does.
type Assembler struct {
	sps, pps []byte
	spsInfo  *SPS
	needIDR  bool

	idrWithoutParams int
	// MaxIDRWithoutParams is how many consecutive IDRs may arrive with no
	// parameter sets known before the stream is invalid.
	MaxIDRWithoutParams int
}

// NewAssembler builds an assembler waiting for its first IDR.
func NewAssembler() *Assembler {
	return &Assembler{needIDR: true, MaxIDRWithoutParams: 3}
}

// SetParameterSets seeds out-of-band SPS/PPS (sprop-parameter-sets). Invalid
// ones are ignored: the in-band ones are what count.
func (a *Assembler) SetParameterSets(sps, pps []byte) {
	if info, err := ParseSPS(sps); err == nil && len(pps) > 0 && pps[0]&0x1f == nalPPS {
		a.sps, a.pps, a.spsInfo = sps, pps, &info
	}
}

// Dropped records that frames were lost: deltas wait for the next IDR.
func (a *Assembler) Dropped() { a.needIDR = true }

// WaitingForIDR reports whether deltas are being discarded.
func (a *Assembler) WaitingForIDR() bool { return a.needIDR }

// Frame assembles one complete frame.
func (a *Assembler) Frame(f Frame) AssembleResult {
	nals, err := depacketize(f.Payloads)
	if err != nil {
		return AssembleResult{Violation: &Violation{Kind: ViolationInvalid, Err: err}}
	}
	var res AssembleResult
	var sps, pps []byte
	hasIDR, hasSlice := false, false
	for _, n := range nals {
		switch n[0] & 0x1f {
		case nalSPS:
			sps = n
		case nalPPS:
			pps = n
		case nalIDR:
			hasIDR, hasSlice = true, true
		case nalSlice:
			hasSlice = true
		}
	}
	paramsChanged := false
	if sps != nil {
		info, err := ParseSPS(sps)
		if err != nil {
			return AssembleResult{Violation: &Violation{Kind: ViolationInvalid, Err: err}}
		}
		if !info.Supported() {
			return AssembleResult{Violation: &Violation{Kind: ViolationInvalid,
				Err: errors.New("media: H.264 profile or level viewers can't decode")}}
		}
		if a.sps != nil && !bytes.Equal(a.sps, sps) {
			paramsChanged = true
		}
		a.sps = append([]byte(nil), sps...)
		a.spsInfo = &info
		res.SPS = &info
	}
	if pps != nil {
		a.pps = append([]byte(nil), pps...)
	}
	if !hasSlice {
		// A parameter-set-only (or SEI-only) frame: nothing to show. Its
		// parameter sets are cached for the next IDR.
		return res
	}
	data := annexB(nals)
	if publisher.HasBSlices(data) {
		return AssembleResult{Violation: &Violation{Kind: ViolationBFrames, Err: errors.New("media: B slices in the stream")}}
	}
	if !hasIDR {
		if a.needIDR {
			res.Skipped = true
			return res
		}
		res.AU = &AU{Timestamp: f.Timestamp, Data: data}
		return res
	}
	if sps == nil || pps == nil {
		if a.sps == nil || a.pps == nil {
			a.idrWithoutParams++
			if a.idrWithoutParams >= a.MaxIDRWithoutParams {
				return AssembleResult{Violation: &Violation{Kind: ViolationInvalid,
					Err: errors.New("media: keyframes arrive without SPS/PPS")}}
			}
			res.Skipped = true
			return res
		}
		// Insert the cached sets the IDR is missing before its first slice
		// (after any AUD/SEI, where they belong).
		with := make([][]byte, 0, len(nals)+2)
		inserted := false
		for _, n := range nals {
			if t := n[0] & 0x1f; !inserted && (t == nalSlice || t == nalIDR) {
				if sps == nil {
					with = append(with, a.sps)
				}
				if pps == nil {
					with = append(with, a.pps)
				}
				inserted = true
			}
			with = append(with, n)
		}
		data = annexB(with)
		if res.SPS == nil {
			res.SPS = a.spsInfo
		}
	}
	a.idrWithoutParams = 0
	a.needIDR = false
	res.AU = &AU{Timestamp: f.Timestamp, Data: data, Keyframe: true, ParamsChanged: paramsChanged}
	return res
}
