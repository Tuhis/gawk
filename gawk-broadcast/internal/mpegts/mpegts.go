// Package mpegts is a deliberately minimal MPEG-TS demuxer: enough to recover
// access-unit boundaries from the GStreamer child's stdout, and nothing more.
//
// Why a container at all, when the pipe could carry raw Annex-B (docs/19
// Decision 7): the alternative is re-finding frame boundaries by scanning for
// access-unit delimiters, which is adversarial parsing over a byte stream we
// then have to trust. MPEG-TS gives the boundary structurally —
// payload_unit_start_indicator marks each PES, and one PES is one access unit
// — and throws in the PES PTS for free, which the engine uses to clock-anchor
// each access unit's timestamp (Decision 6's upgrade path, taken 2026-07-17 —
// see internal/gst/pts.go and docs/19 deviation 11).
//
// The scope here is exactly the pipeline we build in internal/gst: one
// program, one video stream (H.264), one optional audio stream (Opus, R25),
// no scrambling, no PCR use, sections that fit one packet. It is not a
// general-purpose demuxer and should not grow into one — if the pipeline ever
// needs more, the pipeline is wrong.
package mpegts

import (
	"errors"
	"fmt"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/opus"
)

const (
	// PacketSize is the fixed TS packet size.
	PacketSize = 188
	// SyncByte starts every TS packet.
	SyncByte = 0x47

	pidPAT = 0x0000
	// streamTypeH264 is the PMT stream_type for H.264/AVC video.
	streamTypeH264 = 0x1b
	// streamTypePrivate is the PMT stream_type Opus is carried under. It says
	// "private data" and nothing more, which is why the registration
	// descriptor below is what actually identifies the stream — the video
	// stream in our own fixture carries an HDMV registration descriptor, so
	// "has a registration descriptor" would match the wrong one.
	streamTypePrivate = 0x06
	// descriptorRegistration is the descriptor tag whose payload is a
	// four-character format identifier; "Opus" is what GStreamer's
	// gst_base_ts_mux_prepare_opus writes.
	descriptorRegistration = 0x05
	// pesStreamIDPrivate1 is the PES stream_id Opus is muxed under.
	pesStreamIDPrivate1 = 0xbd

	// maxAudioPES bounds one audio PES payload. A 20 ms stereo packet is
	// ~320 B and GStreamer writes one per PES; ffmpeg batches five (~1.7 KB).
	// 16 KB is generous for both and still a bound — an untrusted length must
	// never grow our heap.
	maxAudioPES = 16 << 10

	// ptsHz is the PES timestamp clock.
	ptsHz = 90_000
)

// opusFormatIdentifier is the registration descriptor's four bytes.
var opusFormatIdentifier = [4]byte{'O', 'p', 'u', 's'}

// ErrAUTooLarge is returned when an access unit exceeds the configured bound.
// It is a hard error rather than a truncation: an AU that large means either a
// broken encoder or a framing disagreement, and both are worse than stopping.
var ErrAUTooLarge = errors.New("mpegts: access unit exceeds maximum size")

// AU is one access unit — one encoded frame — as recovered from the container.
type AU struct {
	// Data is the AU's Annex-B bytes. It aliases the demuxer's buffer and is
	// only valid until the callback returns; copy to retain.
	Data []byte
	// PTS is the presentation timestamp (90 kHz), if the PES carried one.
	PTS    uint64
	HasPTS bool
}

// AudioPacket is one Opus packet as recovered from the container (R25).
type AudioPacket struct {
	// Data is the Opus packet, control header already stripped. It aliases
	// the demuxer's buffer and is only valid until the callback returns; copy
	// to retain.
	Data []byte
	// PTS is the packet's presentation timestamp (90 kHz). Only the PES
	// carries one, so for the second and later packets of a batched PES this
	// is derived by adding the duration the TOC states.
	PTS    uint64
	HasPTS bool
}

// Demuxer accumulates TS packets and emits access units. It implements
// io.Writer so it can be fed straight from the child's stdout at whatever
// sizes the pipe happens to deliver.
type Demuxer struct {
	onAU  func(AU) error
	maxAU int
	// onAudio is opt-in (OnAudioPacket). Without it the audio PID is never
	// even looked up, so a video-only stream is byte-identical to pre-R25.
	onAudio func(AudioPacket)

	// partial holds a TS packet split across writes.
	partial  [PacketSize]byte
	partialN int

	pmtPID    uint16
	videoPID  uint16
	audioPID  uint16
	havePMT   bool
	haveVideo bool
	haveAudio bool

	// au accumulates the current access unit's bytes.
	au       []byte
	auPTS    uint64
	auHasPTS bool
	inAU     bool

	// audio accumulates the current audio PES payload — control headers
	// included, since a PES may carry several packets and each brings its own.
	audio       []byte
	audioPTS    uint64
	audioHasPTS bool
	inAudio     bool
	audioDrops  uint64

	// err latches the first fatal error.
	err error
}

// NewDemuxer returns a Demuxer. maxAU bounds a single access unit (the engine
// passes wire.MaxKeyframeBytes, since an AU larger than the relay would accept
// is useless anyway). onAU is called once per access unit, in order.
func NewDemuxer(maxAU int, onAU func(AU) error) *Demuxer {
	return &Demuxer{onAU: onAU, maxAU: maxAU}
}

// OnAudioPacket opts this demuxer into the audio stream (R25). Call it before
// the first Write.
//
// The callback deliberately cannot fail. Audio is subordinate — docs/28
// Decision 6 says an audio problem must never take the video path down — and
// making that structural beats writing it in a comment and hoping: there is no
// error for a future caller to return, and every parse failure inside this
// package drops the packet and counts it (AudioParseDrops) rather than
// latching d.err.
func (d *Demuxer) OnAudioPacket(fn func(AudioPacket)) { d.onAudio = fn }

// AudioParseDrops counts audio packets lost to a malformed or truncated
// control header, or to an oversize PES. It is the honest counterpart of the
// callback that cannot fail: audio failures are dropped rather than raised,
// so something has to count them.
func (d *Demuxer) AudioParseDrops() uint64 { return d.audioDrops }

// Write feeds bytes from the child. It never returns a short write without an
// error, per io.Writer.
func (d *Demuxer) Write(p []byte) (int, error) {
	if d.err != nil {
		return 0, d.err
	}
	total := len(p)
	for len(p) > 0 {
		// Finish a packet that straddled the previous write.
		if d.partialN > 0 {
			n := copy(d.partial[d.partialN:], p)
			d.partialN += n
			p = p[n:]
			if d.partialN < PacketSize {
				return total, nil
			}
			d.partialN = 0
			if err := d.packet(d.partial[:]); err != nil {
				d.err = err
				return total - len(p), err
			}
			continue
		}

		if len(p) < PacketSize {
			d.partialN = copy(d.partial[:], p)
			return total, nil
		}

		// Resync if the grid slipped: scan for a sync byte rather than
		// trusting the stream. A pipe carrying a partial first packet (or a
		// child that died mid-packet) lands here.
		if p[0] != SyncByte {
			off := d.resync(p)
			if off < 0 {
				// No sync byte in this window; keep the tail in case the
				// pattern spans writes.
				d.partialN = copy(d.partial[:], p[max(0, len(p)-PacketSize+1):])
				return total, nil
			}
			p = p[off:]
			continue
		}

		if err := d.packet(p[:PacketSize]); err != nil {
			d.err = err
			return total - len(p), err
		}
		p = p[PacketSize:]
	}
	return total, nil
}

// resync returns the offset of the next plausible packet start, or -1.
func (d *Demuxer) resync(p []byte) int {
	for i := 0; i < len(p); i++ {
		if p[i] != SyncByte {
			continue
		}
		// A lone 0x47 is not proof; require the next packet's sync too when
		// the window is long enough to check.
		if i+PacketSize < len(p) && p[i+PacketSize] != SyncByte {
			continue
		}
		return i
	}
	return -1
}

// Close flushes the final access unit. The last AU has no following PES to
// delimit it, so without this the stream would lose its last frame.
func (d *Demuxer) Close() error {
	if d.err != nil {
		return d.err
	}
	d.flushAudio()
	return d.flushAU()
}

// packet handles one 188-byte TS packet.
func (d *Demuxer) packet(pkt []byte) error {
	if pkt[0] != SyncByte {
		return nil // resync handles the grid; a stray packet is dropped
	}
	// transport_error_indicator: the demodulator/muxer says this packet is
	// corrupt. Trusting it would inject garbage into an AU.
	if pkt[1]&0x80 != 0 {
		return nil
	}
	pusi := pkt[1]&0x40 != 0
	pid := uint16(pkt[1]&0x1f)<<8 | uint16(pkt[2])
	afc := (pkt[3] >> 4) & 0x03

	payload := pkt[4:]
	if afc&0x02 != 0 { // adaptation field present
		if len(payload) < 1 {
			return nil
		}
		afLen := int(payload[0])
		if 1+afLen > len(payload) {
			return nil // malformed; drop the packet rather than slice past it
		}
		payload = payload[1+afLen:]
	}
	if afc&0x01 == 0 || len(payload) == 0 {
		return nil // adaptation only, no payload
	}

	switch {
	case pid == pidPAT:
		d.parsePAT(pusi, payload)
	case d.havePMT && pid == d.pmtPID:
		d.parsePMT(pusi, payload)
	case d.haveVideo && pid == d.videoPID:
		return d.videoPayload(pusi, payload)
	case d.haveAudio && pid == d.audioPID:
		d.audioPayload(pusi, payload)
	}
	return nil
}

// section strips the pointer_field and returns the PSI section bytes.
func section(pusi bool, payload []byte) []byte {
	if !pusi {
		return nil // continuation of a multi-packet section: out of scope
	}
	ptr := int(payload[0])
	if 1+ptr >= len(payload) {
		return nil
	}
	return payload[1+ptr:]
}

func (d *Demuxer) parsePAT(pusi bool, payload []byte) {
	sec := section(pusi, payload)
	if len(sec) < 8 || sec[0] != 0x00 {
		return
	}
	sectionLen := int(sec[1]&0x0f)<<8 | int(sec[2])
	// Header is 3 bytes; the section body covers sectionLen bytes after it,
	// of which the last 4 are the CRC.
	end := 3 + sectionLen - 4
	if end > len(sec) || end < 8 {
		return
	}
	// Program entries start after the 8-byte section header.
	for i := 8; i+4 <= end; i += 4 {
		programNum := uint16(sec[i])<<8 | uint16(sec[i+1])
		pid := uint16(sec[i+2]&0x1f)<<8 | uint16(sec[i+3])
		if programNum == 0 {
			continue // network PID, not a program
		}
		d.pmtPID = pid
		d.havePMT = true
		return // one program is all this pipeline produces
	}
}

func (d *Demuxer) parsePMT(pusi bool, payload []byte) {
	sec := section(pusi, payload)
	if len(sec) < 12 || sec[0] != 0x02 {
		return
	}
	sectionLen := int(sec[1]&0x0f)<<8 | int(sec[2])
	end := 3 + sectionLen - 4
	if end > len(sec) || end < 12 {
		return
	}
	programInfoLen := int(sec[10]&0x0f)<<8 | int(sec[11])
	i := 12 + programInfoLen
	for i+5 <= end {
		streamType := sec[i]
		pid := uint16(sec[i+1]&0x1f)<<8 | uint16(sec[i+2])
		esInfoLen := int(sec[i+3]&0x0f)<<8 | int(sec[i+4])
		esInfo := []byte(nil)
		if i+5+esInfoLen <= end {
			esInfo = sec[i+5 : i+5+esInfoLen]
		}
		switch {
		case streamType == streamTypeH264 && !d.haveVideo:
			d.videoPID = pid
			d.haveVideo = true
		case streamType == streamTypePrivate && d.onAudio != nil && !d.haveAudio && isOpusES(esInfo):
			d.audioPID = pid
			d.haveAudio = true
		}
		i += 5 + esInfoLen
	}
}

// isOpusES reports whether an ES info block identifies an Opus stream.
//
// stream_type 0x06 alone says only "private data", so the registration
// descriptor is what actually identifies it — and it must be checked rather
// than assumed: our own fixture's *video* stream carries an HDMV registration
// descriptor, so a demuxer keying on "there is a registration descriptor here"
// would bind audio to the picture.
func isOpusES(esInfo []byte) bool {
	for i := 0; i+2 <= len(esInfo); {
		tag, length := esInfo[i], int(esInfo[i+1])
		if i+2+length > len(esInfo) {
			return false // malformed descriptor loop
		}
		if tag == descriptorRegistration && length >= 4 &&
			[4]byte(esInfo[i+2:i+6]) == opusFormatIdentifier {
			return true
		}
		i += 2 + length
	}
	return false
}

// videoPayload accumulates one access unit per PES.
//
// This is the load-bearing line of the whole package: payload_unit_start_
// indicator means "a new PES starts here", and because the pipeline muxes one
// access unit per PES, it also means "the previous access unit is complete".
// No start-code scanning, no heuristics.
func (d *Demuxer) videoPayload(pusi bool, payload []byte) error {
	if !pusi {
		if !d.inAU {
			return nil // mid-PES continuation before we ever saw a start
		}
		return d.appendAU(payload)
	}

	// A new PES: the previous AU is done.
	if err := d.flushAU(); err != nil {
		return err
	}

	streamID, hdrLen, pts, hasPTS, ok := parsePESHeader(payload)
	if !ok || streamID < 0xe0 || streamID > 0xef {
		d.inAU = false
		return nil
	}
	d.inAU = true
	d.auPTS, d.auHasPTS = pts, hasPTS
	return d.appendAU(payload[hdrLen:])
}

// audioPayload accumulates one audio PES, then hands its packets out.
//
// The shape mirrors videoPayload — PUSI delimits, no scanning — with one
// difference that comes straight from the container: a video PES is one access
// unit by construction here, while an audio PES may carry several Opus
// packets, each behind its own control header. GStreamer writes one per PES
// and ffmpeg batches five; the mapping permits both, so this loops (docs/28
// NA1 finding 2).
func (d *Demuxer) audioPayload(pusi bool, payload []byte) {
	if !pusi {
		if d.inAudio {
			d.appendAudio(payload)
		}
		return
	}

	d.flushAudio()

	streamID, hdrLen, pts, hasPTS, ok := parsePESHeader(payload)
	if !ok || streamID != pesStreamIDPrivate1 {
		d.inAudio = false
		return
	}
	d.inAudio = true
	d.audioPTS, d.audioHasPTS = pts, hasPTS
	d.appendAudio(payload[hdrLen:])
}

func (d *Demuxer) appendAudio(b []byte) {
	if len(d.audio)+len(b) > maxAudioPES {
		// A PES this large means a framing disagreement, not audio. Drop it
		// and resync at the next PUSI; video is not affected.
		d.audio = d.audio[:0]
		d.inAudio = false
		d.audioDrops++
		return
	}
	d.audio = append(d.audio, b...)
}

// flushAudio emits every Opus packet in the accumulated PES.
func (d *Demuxer) flushAudio() {
	buf := d.audio
	d.audio = d.audio[:0]
	if !d.inAudio || len(buf) == 0 {
		return
	}
	pts, hasPTS := d.audioPTS, d.audioHasPTS
	d.audioHasPTS = false

	for len(buf) > 0 {
		size, hdrLen, ok := parseOpusControlHeader(buf)
		if !ok || hdrLen+size > len(buf) {
			// A malformed or truncated header: the rest of this PES is
			// unparseable, because every packet's position depends on the
			// one before it. Drop it and wait for the next PES.
			d.audioDrops++
			return
		}
		pkt := buf[hdrLen : hdrLen+size]
		d.onAudio(AudioPacket{Data: pkt, PTS: pts, HasPTS: hasPTS})
		buf = buf[hdrLen+size:]

		if hasPTS && len(buf) > 0 {
			// Only the PES carried a timestamp, so the next packet's is this
			// one's plus what its TOC says it lasts. A packet we cannot read
			// the TOC of ends the timestamps rather than inventing them:
			// stamping a guess is worse than admitting the gap, because the
			// engine's anchor would map the guess onto the wire.
			toc, tocOK := opus.ParseTOC(pkt)
			if !tocOK {
				hasPTS = false
				continue
			}
			pts += toc.DurationUs * ptsHz / 1_000_000
		}
	}
}

// parseOpusControlHeader reads one Opus-in-MPEG-TS control header and returns
// the payload size and the header's own length.
//
// The prefix is 11 bits holding 0x3FF — which is *ten* ones, so the field is
// one zero bit followed by ten ones and the first byte is 0x7F, not 0xFF
// (docs/28 NA1 finding 1). A demuxer syncing on 0xFF finds nothing, forever.
func parseOpusControlHeader(b []byte) (payloadSize, hdrLen int, ok bool) {
	if len(b) < 2 || b[0] != 0x7f || b[1]&0xe0 != 0xe0 {
		return 0, 0, false
	}
	startTrim := b[1]&0x10 != 0
	endTrim := b[1]&0x08 != 0
	controlExtension := b[1]&0x04 != 0

	i := 2
	for {
		if i >= len(b) {
			return 0, 0, false
		}
		v := b[i]
		i++
		payloadSize += int(v)
		if v != 0xff {
			break
		}
	}
	if startTrim {
		i += 2
	}
	if endTrim {
		i += 2
	}
	if controlExtension {
		if i >= len(b) {
			return 0, 0, false
		}
		i += 1 + int(b[i])
	}
	if i > len(b) || payloadSize == 0 {
		return 0, 0, false
	}
	return payloadSize, i, true
}

func (d *Demuxer) appendAU(b []byte) error {
	if len(d.au)+len(b) > d.maxAU {
		return fmt.Errorf("%w: %d bytes, max %d", ErrAUTooLarge, len(d.au)+len(b), d.maxAU)
	}
	d.au = append(d.au, b...)
	return nil
}

func (d *Demuxer) flushAU() error {
	if !d.inAU || len(d.au) == 0 {
		d.au = d.au[:0]
		return nil
	}
	err := d.onAU(AU{Data: d.au, PTS: d.auPTS, HasPTS: d.auHasPTS})
	d.au = d.au[:0]
	d.auHasPTS = false
	return err
}

// parsePESHeader returns the stream id, the total PES header length, the PTS,
// and whether the packet is a usable PES start. The caller decides which
// stream ids it accepts — video is 0xE0-0xEF, Opus rides private_stream_1.
//
// PES_packet_length is 0 for video by convention — an unbounded PES, ended by
// the next one — which is precisely why the boundary comes from PUSI rather
// than from a length field. (Audio does carry a length, and this deliberately
// ignores it for the same reason: one boundary rule, not two.)
func parsePESHeader(p []byte) (streamID byte, hdrLen int, pts uint64, hasPTS bool, ok bool) {
	if len(p) < 9 || p[0] != 0x00 || p[1] != 0x00 || p[2] != 0x01 {
		return 0, 0, 0, false, false
	}
	streamID = p[3]
	// p[6] must start with '10' for a normal PES header.
	if p[6]&0xc0 != 0x80 {
		return 0, 0, 0, false, false
	}
	ptsDTSFlags := (p[7] >> 6) & 0x03
	pesHeaderDataLen := int(p[8])
	hdrLen = 9 + pesHeaderDataLen
	if hdrLen > len(p) {
		return 0, 0, 0, false, false
	}
	if ptsDTSFlags&0x02 != 0 && pesHeaderDataLen >= 5 {
		pts = parsePTS(p[9:14])
		hasPTS = true
	}
	return streamID, hdrLen, pts, hasPTS, true
}

// parsePTS reads the 33-bit, 90 kHz timestamp out of its five marker-bit-
// interleaved bytes.
func parsePTS(b []byte) uint64 {
	return uint64(b[0]&0x0e)<<29 |
		uint64(b[1])<<22 |
		uint64(b[2]&0xfe)<<14 |
		uint64(b[3])<<7 |
		uint64(b[4])>>1
}
