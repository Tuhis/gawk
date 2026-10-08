package engine

import (
	"bytes"
	"testing"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

func patternBytes(n, seed int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i*31 + seed)
	}
	return out
}

// parity_test uses the real constructor (send_test.go's newTestSender) and
// then applies capabilities, so the gate under test is the one production
// goes through rather than a hand-built struct.
func senderAtParity(parityLevel int) *sender {
	s := newTestSender(newFakeSession())
	if parityLevel > 0 {
		s.applyCapabilities(wire.RelayCapabilities{Flags: wire.CapParityChunks, ParityLevel: uint8(parityLevel)})
	}
	return s
}

// TestChunkParityMatchesBrowserProducer is the cross-producer guard: the two
// producers must emit byte-identical parity for the same frame, or a viewer
// that works against one silently mis-repairs against the other. The expected
// symbols are computed from the shared wire codec over the chunk PAYLOADS,
// which is the same rule packetizer.ts follows.
func TestChunkParityMatchesBrowserProducer(t *testing.T) {
	s := senderAtParity(2)
	au := AccessUnit{Data: patternBytes(5000, 3), TimestampUs: 123456}
	dgrams, parity, err := s.chunkWithParity(42, au)
	if err != nil {
		t.Fatalf("chunkWithParity: %v", err)
	}
	if len(parity) != 2 {
		t.Fatalf("got %d parity symbols, want 2", len(parity))
	}

	payloads := make([][]byte, len(dgrams))
	for i, d := range dgrams {
		_, p, err := wire.ParseVideoChunk(d)
		if err != nil {
			t.Fatalf("ParseVideoChunk: %v", err)
		}
		payloads[i] = p
	}
	want, err := wire.ComputeParity(payloads, 2)
	if err != nil {
		t.Fatalf("ComputeParity: %v", err)
	}
	for i, p := range parity {
		h, got, err := wire.ParseParityChunk(p)
		if err != nil {
			t.Fatalf("ParseParityChunk: %v", err)
		}
		if h.FrameID != 42 || int(h.ParityIndex) != i || int(h.ChunkCount) != len(dgrams) || int(h.FrameBytes) != len(au.Data) {
			t.Fatalf("symbol %d header = %+v, want frame 42 / index %d / count %d / bytes %d",
				i, h, i, len(dgrams), len(au.Data))
		}
		if !bytes.Equal(got, want[i]) {
			t.Fatalf("symbol %d payload drifted from the shared codec", i)
		}
	}
}

func TestChunkParityRepairsTwoLostChunks(t *testing.T) {
	s := senderAtParity(2)
	au := AccessUnit{Data: patternBytes(5000, 11), TimestampUs: 1}
	dgrams, parity, err := s.chunkWithParity(7, au)
	if err != nil {
		t.Fatalf("chunkWithParity: %v", err)
	}
	payloads := make([][]byte, len(dgrams))
	for i, d := range dgrams {
		_, p, _ := wire.ParseVideoChunk(d)
		payloads[i] = p
	}
	n := len(payloads)
	payloads[1] = nil
	payloads[n-1] = nil // include the short final chunk

	symbols := make([][]byte, len(parity))
	for i, p := range parity {
		_, sym, _ := wire.ParseParityChunk(p)
		symbols[i] = sym
	}
	if err := wire.RecoverChunks(payloads, symbols, len(au.Data)); err != nil {
		t.Fatalf("RecoverChunks: %v", err)
	}
	var rebuilt []byte
	for _, p := range payloads {
		rebuilt = append(rebuilt, p...)
	}
	if !bytes.Equal(rebuilt, au.Data) {
		t.Fatal("recovered frame does not match the original")
	}
}

func TestChunkParityLevelZeroIsIdentical(t *testing.T) {
	au := AccessUnit{Data: patternBytes(3000, 5), TimestampUs: 9}
	plain, err := senderAtParity(0).chunk(1, au)
	if err != nil {
		t.Fatalf("chunk: %v", err)
	}
	dgrams, parity, err := senderAtParity(0).chunkWithParity(1, au)
	if err != nil {
		t.Fatalf("chunkWithParity: %v", err)
	}
	if len(parity) != 0 {
		t.Fatalf("got %d parity symbols at level 0, want none", len(parity))
	}
	if len(dgrams) != len(plain) {
		t.Fatalf("got %d datagrams, want %d", len(dgrams), len(plain))
	}
	for i := range plain {
		if !bytes.Equal(dgrams[i], plain[i]) {
			t.Fatalf("datagram %d differs at parity level 0", i)
		}
	}
}

func TestChunkParitySingleChunkEmitsOneSymbol(t *testing.T) {
	dgrams, parity, err := senderAtParity(2).chunkWithParity(1, AccessUnit{Data: []byte{1, 2, 3}})
	if err != nil {
		t.Fatalf("chunkWithParity: %v", err)
	}
	if len(dgrams) != 1 || len(parity) != 1 {
		t.Fatalf("got %d datagrams / %d symbols, want 1 / 1", len(dgrams), len(parity))
	}
}

// A frame past the code's MDS range must degrade to plain datagrams rather
// than failing the broadcast.
func TestChunkParitySkippedForOversizeFrame(t *testing.T) {
	s := senderAtParity(2)
	au := AccessUnit{Data: patternBytes(wire.MaxChunkPayload*300, 6)}
	dgrams, parity, err := s.chunkWithParity(1, au)
	if err != nil {
		t.Fatalf("chunkWithParity: %v", err)
	}
	if len(dgrams) <= wire.MaxParityDataChunks {
		t.Fatalf("test frame only produced %d chunks", len(dgrams))
	}
	if len(parity) != 0 {
		t.Fatalf("got %d parity symbols for an oversize frame, want none", len(parity))
	}
}

func TestChunkParityFitsDatagramCap(t *testing.T) {
	s := senderAtParity(2)
	au := AccessUnit{Data: patternBytes(wire.MaxChunkPayload*9, 7)}
	dgrams, parity, err := s.chunkWithParity(1, au)
	if err != nil {
		t.Fatalf("chunkWithParity: %v", err)
	}
	for _, d := range append(append([][]byte{}, dgrams...), parity...) {
		if len(d) > wire.MaxDatagramSize {
			t.Fatalf("datagram of %d bytes exceeds the %d cap", len(d), wire.MaxDatagramSize)
		}
	}
}

// The capability gate: a relay that never advertises parity (or advertises it
// off) must leave this producer emitting nothing, which is what keeps a new
// broadcaster against an old relay byte-identical to pre-R29.
func TestApplyRelayCapabilitiesGatesParity(t *testing.T) {
	s := senderAtParity(0)
	if s.parityLevel != 0 {
		t.Fatal("parity should start disabled")
	}
	s.applyCapabilities(wire.RelayCapabilities{Flags: wire.CapParityChunks, ParityLevel: 2})
	if s.parityLevel != 2 {
		t.Fatalf("parityLevel = %d, want 2 after the relay advertised it", s.parityLevel)
	}
	// Flag clear means the relay does not filter parity per subscriber, so
	// emitting would spray chunks at viewers that cannot use them.
	s.applyCapabilities(wire.RelayCapabilities{Flags: 0, ParityLevel: 2})
	if s.parityLevel != 0 {
		t.Fatalf("parityLevel = %d, want 0 when CapParityChunks is clear", s.parityLevel)
	}
}
