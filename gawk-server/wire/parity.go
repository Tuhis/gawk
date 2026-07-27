package wire

// Forward parity for the datagram delta path (R29, docs/34).
//
// A delta frame split into n data chunks gets up to two parity symbols:
//
//	P = d0 ^ d1 ^ ... ^ d(n-1)
//	Q = (g^0 * d0) ^ (g^1 * d1) ^ ... ^ (g^(n-1) * d(n-1))     g = 2 in GF(2^8)
//
// This is the RAID-6 P/Q scheme. It is MDS for k <= 2 — ANY two erasures
// among the n+2 transmitted chunks reconstruct the frame — while needing only
// two 256-entry tables, where a general Reed-Solomon implementation mirrored
// in three languages would be several times the code for no extra recovery at
// k = 2 (docs/34 Decision 4.1).
//
// P alone IS the k=1 code. That prefix property is load-bearing: it is what
// lets one computation at the fleet's parity level serve subscribers at every
// level below it, which is what makes per-subscriber k free on the producer
// (docs/34 §5.1).

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	// ParityChunkHeaderSize is the fixed header size of a ParityChunk
	// datagram: version, type, frameID, parityIndex, chunkCount, frameBytes.
	//
	// It is deliberately 13 and not 20. A parity symbol is as long as the
	// longest data chunk, i.e. up to MaxChunkPayload (1180), so a 20-byte
	// header carrying a timestamp would produce a 1201-byte datagram and
	// breach MaxDatagramSize. The cost of omitting the timestamp is that
	// reconstruction needs at least one surviving data chunk to source it
	// from — which holds whenever recovery is possible at all, except the
	// degenerate n <= k case that is deliberately not recovered.
	ParityChunkHeaderSize = 13

	// MaxParitySymbols is the largest k the P/Q scheme supports.
	MaxParitySymbols = 2

	// MaxParityDataChunks bounds n. g^i has period 255, so at n > 255 the Q
	// coefficients repeat, two data chunks share a coefficient, and the
	// 2-erasure solve divides by zero — the code silently stops being MDS.
	// A delta needing more than 255 chunks would be ~300 KB, which is
	// unreachable at any sane bitrate, and keyframes never carry parity
	// (they ride reliable streams already). This is an explicit guard rather
	// than an assumption.
	MaxParityDataChunks = 255

	// RelayCapabilitiesSize is the exact size of a RelayCapabilities message:
	// version, type, flags (uint16 BE), parity level.
	RelayCapabilitiesSize = 5
)

// Capability flags carried by RelayCapabilities. Append, never renumber.
const (
	// CapParityChunks means the relay understands ParityChunk datagrams and
	// will filter them per subscriber. A producer that does not see this bit
	// sends no parity, so a new broadcaster against an old relay stays
	// byte-identical to pre-R29 (docs/34 §4.4).
	CapParityChunks uint16 = 1 << 0
)

var (
	// ErrParityUnsupported reports a frame shape parity cannot cover.
	ErrParityUnsupported = errors.New("wire: parity unsupported for this frame")
	// ErrParityUnrecoverable reports more erasures than the parity present
	// can repair. It is an expected outcome on a lossy link, not a fault.
	ErrParityUnrecoverable = errors.New("wire: too many erasures to recover")
)

// --- GF(2^8), primitive polynomial 0x11D, generator 2 ----------------------

var (
	gfExp [512]uint8
	gfLog [256]uint8
)

func init() {
	x := uint8(1)
	for i := 0; i < 255; i++ {
		gfExp[i] = x
		gfLog[x] = uint8(i)
		// Multiply by the generator (2), reducing modulo 0x11D.
		hi := x&0x80 != 0
		x <<= 1
		if hi {
			x ^= 0x1d
		}
	}
	// Duplicate the cycle so exponent sums up to 508 need no modulo.
	for i := 255; i < 512; i++ {
		gfExp[i] = gfExp[i-255]
	}
}

func gfMul(a, b uint8) uint8 {
	if a == 0 || b == 0 {
		return 0
	}
	return gfExp[int(gfLog[a])+int(gfLog[b])]
}

func gfDiv(a, b uint8) uint8 {
	if b == 0 {
		panic("wire: division by zero in GF(2^8)")
	}
	if a == 0 {
		return 0
	}
	return gfExp[int(gfLog[a])-int(gfLog[b])+255]
}

// gfPow2 returns g^i for g = 2, the Q coefficient of data chunk i.
func gfPow2(i int) uint8 { return gfExp[i%255] }

// --- Parity computation ----------------------------------------------------

// ComputeParity returns min(k, len(chunks)) parity symbols over chunks, each
// as long as the longest chunk (shorter chunks are treated as zero-padded).
// k == 0 returns nil. Chunks are not modified.
func ComputeParity(chunks [][]byte, k int) ([][]byte, error) {
	if k < 0 || k > MaxParitySymbols {
		return nil, fmt.Errorf("%w: k=%d, max %d", ErrParityUnsupported, k, MaxParitySymbols)
	}
	n := len(chunks)
	if k == 0 || n == 0 {
		return nil, nil
	}
	if n > MaxParityDataChunks {
		return nil, fmt.Errorf("%w: %d chunks, max %d", ErrParityUnsupported, n, MaxParityDataChunks)
	}
	// n == 1: P duplicates the chunk, and a second symbol would duplicate it
	// again. min(k, n) keeps that from being wire waste.
	if k > n {
		k = n
	}

	width := 0
	for _, c := range chunks {
		if len(c) > width {
			width = len(c)
		}
	}
	if width > MaxChunkPayload {
		return nil, fmt.Errorf("%w: chunk of %d bytes, max %d", ErrParityUnsupported, width, MaxChunkPayload)
	}

	out := make([][]byte, k)
	for j := range out {
		out[j] = make([]byte, width)
	}
	for i, c := range chunks {
		p := out[0]
		for b, v := range c {
			p[b] ^= v
		}
		if k > 1 {
			coeff := gfPow2(i)
			q := out[1]
			for b, v := range c {
				q[b] ^= gfMul(coeff, v)
			}
		}
	}
	return out, nil
}

// --- Recovery --------------------------------------------------------------

// RecoverChunks reconstructs missing data chunks in place. A nil entry in
// chunks or parity means "not received". frameBytes is the total encoded
// frame length, which is the only thing that says how long the final
// (short) chunk is.
//
// The symbol width is taken from a surviving parity symbol, not inferred:
// every parity symbol is exactly as long as the longest data chunk, and a
// parity symbol is present whenever recovery is possible at all.
//
// Returns ErrParityUnrecoverable when there are more data erasures than
// usable parity symbols. That is a routine outcome on a lossy link and the
// caller is expected to count it, not treat it as a fault.
func RecoverChunks(chunks [][]byte, parity [][]byte, frameBytes int) error {
	n := len(chunks)
	if n == 0 {
		return fmt.Errorf("%w: no chunks", ErrParityUnsupported)
	}
	if n > MaxParityDataChunks {
		return fmt.Errorf("%w: %d chunks, max %d", ErrParityUnsupported, n, MaxParityDataChunks)
	}

	missing := make([]int, 0, 2)
	for i, c := range chunks {
		if c == nil {
			missing = append(missing, i)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	var haveP, haveQ []byte
	if len(parity) > 0 {
		haveP = parity[0]
	}
	if len(parity) > 1 {
		haveQ = parity[1]
	}
	avail := 0
	if haveP != nil {
		avail++
	}
	if haveQ != nil {
		avail++
	}
	if len(missing) > avail {
		return fmt.Errorf("%w: %d data erasures, %d parity symbols", ErrParityUnrecoverable, len(missing), avail)
	}

	width := 0
	switch {
	case haveP != nil:
		width = len(haveP)
	case haveQ != nil:
		width = len(haveQ)
	}
	if width == 0 {
		return fmt.Errorf("%w: no parity symbol to size the block", ErrParityUnrecoverable)
	}
	if haveP != nil && haveQ != nil && len(haveP) != len(haveQ) {
		return fmt.Errorf("%w: parity symbols disagree on width (%d vs %d)", ErrParityUnsupported, len(haveP), len(haveQ))
	}

	// frameBytes must be consistent with n full-width chunks minus the final
	// chunk's shortfall, or the header is lying and reconstruction would
	// silently produce garbage.
	lastLen := frameBytes - (n-1)*width
	if lastLen <= 0 || lastLen > width {
		return fmt.Errorf("%w: frameBytes %d inconsistent with %d chunks of width %d",
			ErrParityUnsupported, frameBytes, n, width)
	}
	for i, c := range chunks {
		if c == nil {
			continue
		}
		want := width
		if i == n-1 {
			want = lastLen
		}
		if len(c) != want {
			return fmt.Errorf("%w: chunk %d has length %d, want %d", ErrParityUnsupported, i, len(c), want)
		}
	}

	// Work on zero-padded copies so the GF arithmetic sees a rectangle.
	padded := make([][]byte, n)
	for i, c := range chunks {
		buf := make([]byte, width)
		if c != nil {
			copy(buf, c)
		}
		padded[i] = buf
	}

	switch len(missing) {
	case 1:
		x := missing[0]
		if haveP != nil {
			// d_x = P ^ (XOR of survivors)
			acc := make([]byte, width)
			copy(acc, haveP)
			for i, c := range padded {
				if i == x {
					continue
				}
				for b := range acc {
					acc[b] ^= c[b]
				}
			}
			padded[x] = acc
		} else {
			// Only Q survived: d_x = (Q ^ sum(g^i * d_i, i != x)) / g^x
			acc := make([]byte, width)
			copy(acc, haveQ)
			for i, c := range padded {
				if i == x {
					continue
				}
				coeff := gfPow2(i)
				for b := range acc {
					acc[b] ^= gfMul(coeff, c[b])
				}
			}
			inv := gfPow2(x)
			for b := range acc {
				acc[b] = gfDiv(acc[b], inv)
			}
			padded[x] = acc
		}
	case 2:
		// Both P and Q are required (checked above via avail).
		x, y := missing[0], missing[1]
		pm := make([]byte, width) // d_x ^ d_y
		qm := make([]byte, width) // g^x*d_x ^ g^y*d_y
		copy(pm, haveP)
		copy(qm, haveQ)
		for i, c := range padded {
			if i == x || i == y {
				continue
			}
			coeff := gfPow2(i)
			for b := range pm {
				pm[b] ^= c[b]
				qm[b] ^= gfMul(coeff, c[b])
			}
		}
		gx, gy := gfPow2(x), gfPow2(y)
		den := gx ^ gy
		if den == 0 {
			// Unreachable while n <= MaxParityDataChunks, which is exactly
			// what that bound is for.
			return fmt.Errorf("%w: coefficients for chunks %d and %d collide", ErrParityUnsupported, x, y)
		}
		dx := make([]byte, width)
		for b := range dx {
			dx[b] = gfDiv(gfMul(gy, pm[b])^qm[b], den)
		}
		dy := make([]byte, width)
		for b := range dy {
			dy[b] = pm[b] ^ dx[b]
		}
		padded[x], padded[y] = dx, dy
	}

	for _, i := range missing {
		out := padded[i]
		if i == n-1 {
			out = out[:lastLen]
		}
		chunks[i] = out
	}
	return nil
}

// --- ParityChunk wire format ----------------------------------------------

// ParityChunkHeader is the header of a ParityChunk datagram.
type ParityChunkHeader struct {
	FrameID     uint32
	ParityIndex uint8  // 0 = P, 1 = Q
	ChunkCount  uint16 // n, the DATA chunk count of the frame
	FrameBytes  uint32 // total encoded frame length
}

// AppendParityChunk appends a ParityChunk datagram to dst.
func AppendParityChunk(dst []byte, h ParityChunkHeader, payload []byte) ([]byte, error) {
	if len(payload) > MaxChunkPayload {
		return nil, fmt.Errorf("%w: %d bytes, max %d", ErrPayloadTooLarge, len(payload), MaxChunkPayload)
	}
	if h.ChunkCount == 0 || h.ChunkCount > MaxParityDataChunks {
		return nil, fmt.Errorf("%w: count %d, max %d", ErrBadChunkCount, h.ChunkCount, MaxParityDataChunks)
	}
	if h.ParityIndex >= MaxParitySymbols {
		return nil, fmt.Errorf("%w: parity index %d, max %d", ErrBadChunkCount, h.ParityIndex, MaxParitySymbols-1)
	}
	dst = append(dst, Version, TypeParityChunk)
	dst = binary.BigEndian.AppendUint32(dst, h.FrameID)
	dst = append(dst, h.ParityIndex)
	dst = binary.BigEndian.AppendUint16(dst, h.ChunkCount)
	dst = binary.BigEndian.AppendUint32(dst, h.FrameBytes)
	dst = append(dst, payload...)
	return dst, nil
}

// ParseParityChunk parses a ParityChunk datagram. The returned payload
// aliases dgram (no copy).
func ParseParityChunk(dgram []byte) (h ParityChunkHeader, payload []byte, err error) {
	if len(dgram) < ParityChunkHeaderSize {
		return ParityChunkHeader{}, nil, fmt.Errorf("%w: %d bytes, need at least %d for parity chunk",
			ErrShortDatagram, len(dgram), ParityChunkHeaderSize)
	}
	if len(dgram) > MaxDatagramSize {
		return ParityChunkHeader{}, nil, fmt.Errorf("%w: %d bytes, max %d", ErrPayloadTooLarge, len(dgram), MaxDatagramSize)
	}
	if dgram[0] != Version {
		return ParityChunkHeader{}, nil, fmt.Errorf("%w: 0x%02x", ErrBadVersion, dgram[0])
	}
	if dgram[1] != TypeParityChunk {
		return ParityChunkHeader{}, nil, fmt.Errorf("%w: got 0x%02x, want parity chunk 0x%02x",
			ErrBadType, dgram[1], TypeParityChunk)
	}
	h = ParityChunkHeader{
		FrameID:     binary.BigEndian.Uint32(dgram[2:6]),
		ParityIndex: dgram[6],
		ChunkCount:  binary.BigEndian.Uint16(dgram[7:9]),
		FrameBytes:  binary.BigEndian.Uint32(dgram[9:13]),
	}
	if h.ChunkCount == 0 || h.ChunkCount > MaxParityDataChunks {
		return ParityChunkHeader{}, nil, fmt.Errorf("%w: count %d (max %d)", ErrBadChunkCount, h.ChunkCount, MaxParityDataChunks)
	}
	if h.ParityIndex >= MaxParitySymbols {
		return ParityChunkHeader{}, nil, fmt.Errorf("%w: parity index %d (max %d)", ErrBadChunkCount, h.ParityIndex, MaxParitySymbols-1)
	}
	return h, dgram[ParityChunkHeaderSize:], nil
}

// --- RelayCapabilities wire format -----------------------------------------

// RelayCapabilities is what the relay tells a client about optional features
// it supports, sent once per session at session start on both routes.
//
// It is a separate message rather than extra fields on BroadcastAnnounce for
// two reasons already settled elsewhere: the parsers are strict, so appending
// bytes to an existing message breaks old readers; and the browser
// WebTransport API exposes no HTTP response headers, so a capability cannot
// ride the connect response (docs/34 §4.4).
type RelayCapabilities struct {
	Flags       uint16
	ParityLevel uint8 // the fleet parity level producers should emit
}

// AppendRelayCapabilities appends a RelayCapabilities message to dst.
func AppendRelayCapabilities(dst []byte, c RelayCapabilities) ([]byte, error) {
	if c.ParityLevel > MaxParitySymbols {
		return nil, fmt.Errorf("%w: parity level %d, max %d", ErrParityUnsupported, c.ParityLevel, MaxParitySymbols)
	}
	dst = append(dst, Version, TypeRelayCapabilities)
	dst = binary.BigEndian.AppendUint16(dst, c.Flags)
	dst = append(dst, c.ParityLevel)
	return dst, nil
}

// ParseRelayCapabilities parses a RelayCapabilities message.
func ParseRelayCapabilities(b []byte) (RelayCapabilities, error) {
	if len(b) != RelayCapabilitiesSize {
		return RelayCapabilities{}, fmt.Errorf("%w: %d bytes, want exactly %d",
			ErrShortDatagram, len(b), RelayCapabilitiesSize)
	}
	if b[0] != Version {
		return RelayCapabilities{}, fmt.Errorf("%w: 0x%02x", ErrBadVersion, b[0])
	}
	if b[1] != TypeRelayCapabilities {
		return RelayCapabilities{}, fmt.Errorf("%w: got 0x%02x, want relay capabilities 0x%02x",
			ErrBadType, b[1], TypeRelayCapabilities)
	}
	c := RelayCapabilities{
		Flags:       binary.BigEndian.Uint16(b[2:4]),
		ParityLevel: b[4],
	}
	if c.ParityLevel > MaxParitySymbols {
		return RelayCapabilities{}, fmt.Errorf("%w: parity level %d (max %d)", ErrParityUnsupported, c.ParityLevel, MaxParitySymbols)
	}
	return c, nil
}
