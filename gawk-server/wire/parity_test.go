package wire

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math/rand"
	"testing"
)

// --- GF(2^8) arithmetic ----------------------------------------------------

func TestGFMulIdentitiesAndInverse(t *testing.T) {
	for a := 0; a < 256; a++ {
		if got := gfMul(uint8(a), 0); got != 0 {
			t.Fatalf("gfMul(%d, 0) = %d, want 0", a, got)
		}
		if got := gfMul(uint8(a), 1); got != uint8(a) {
			t.Fatalf("gfMul(%d, 1) = %d, want %d", a, got, a)
		}
	}
	// Every non-zero element has a multiplicative inverse.
	for a := 1; a < 256; a++ {
		inv := gfDiv(1, uint8(a))
		if got := gfMul(uint8(a), inv); got != 1 {
			t.Fatalf("a=%d * inv=%d = %d, want 1", a, inv, got)
		}
	}
}

func TestGFPowIsDistinctBelow255(t *testing.T) {
	// The Q coefficients are g^i. They must be distinct over the supported
	// data-chunk range or the 2-erasure solve divides by zero. This is
	// exactly why MaxParityDataChunks is 255.
	seen := make(map[uint8]int, 255)
	for i := 0; i < MaxParityDataChunks; i++ {
		c := gfPow2(i)
		if prev, dup := seen[c]; dup {
			t.Fatalf("g^%d == g^%d == %d: coefficients repeat inside the supported range", i, prev, c)
		}
		seen[c] = i
	}
	// And it genuinely wraps right after, which is the guard's justification.
	if gfPow2(0) != gfPow2(255) {
		t.Fatal("expected g^0 == g^255 (period 255) — the guard rationale is wrong")
	}
}

// --- Parity computation ----------------------------------------------------

func TestComputeParityShape(t *testing.T) {
	chunks := [][]byte{{1, 2, 3, 4}, {5, 6, 7, 8}, {9, 10}}
	p, err := ComputeParity(chunks, 2)
	if err != nil {
		t.Fatalf("ComputeParity: %v", err)
	}
	if len(p) != 2 {
		t.Fatalf("got %d symbols, want 2", len(p))
	}
	// Symbols are as long as the LONGEST chunk (short chunks zero-pad), which
	// is what lets the viewer derive the chunk length from a parity payload.
	for i, s := range p {
		if len(s) != 4 {
			t.Fatalf("symbol %d has length %d, want 4", i, len(s))
		}
	}
	// P is the plain XOR — the prefix property the whole per-subscriber
	// design rests on (docs/34 §4.1).
	wantP := []byte{1 ^ 5 ^ 9, 2 ^ 6 ^ 10, 3 ^ 7, 4 ^ 8}
	if !bytes.Equal(p[0], wantP) {
		t.Fatalf("P = %x, want %x", p[0], wantP)
	}
}

func TestComputeParityPrefixProperty(t *testing.T) {
	chunks := [][]byte{{0xde, 0xad}, {0xbe, 0xef}, {0x12, 0x34}}
	one, err := ComputeParity(chunks, 1)
	if err != nil {
		t.Fatalf("ComputeParity(k=1): %v", err)
	}
	two, err := ComputeParity(chunks, 2)
	if err != nil {
		t.Fatalf("ComputeParity(k=2): %v", err)
	}
	if !bytes.Equal(one[0], two[0]) {
		t.Fatalf("k=1 symbol %x != k=2 symbol 0 %x: prefix property broken", one[0], two[0])
	}
}

func TestComputeParitySingleChunkEmitsOneSymbol(t *testing.T) {
	// n=1: P duplicates the chunk (genuine protection against its loss); a
	// second symbol would add nothing, so min(k, n) applies.
	chunks := [][]byte{{7, 7, 7}}
	p, err := ComputeParity(chunks, 2)
	if err != nil {
		t.Fatalf("ComputeParity: %v", err)
	}
	if len(p) != 1 {
		t.Fatalf("got %d symbols for n=1, want 1", len(p))
	}
	if !bytes.Equal(p[0], chunks[0]) {
		t.Fatalf("P = %x, want the chunk itself %x", p[0], chunks[0])
	}
}

func TestComputeParityRejectsTooManyChunks(t *testing.T) {
	chunks := make([][]byte, MaxParityDataChunks+1)
	for i := range chunks {
		chunks[i] = []byte{1}
	}
	if _, err := ComputeParity(chunks, 2); !errors.Is(err, ErrParityUnsupported) {
		t.Fatalf("err = %v, want ErrParityUnsupported for n > %d", err, MaxParityDataChunks)
	}
}

func TestComputeParityRejectsBadK(t *testing.T) {
	chunks := [][]byte{{1}, {2}}
	for _, k := range []int{-1, MaxParitySymbols + 1} {
		if _, err := ComputeParity(chunks, k); err == nil {
			t.Fatalf("k=%d accepted, want error", k)
		}
	}
	if p, err := ComputeParity(chunks, 0); err != nil || p != nil {
		t.Fatalf("k=0 should be a clean no-op, got %v / %v", p, err)
	}
}

// --- Recovery: the exhaustive property test --------------------------------

// TestRecoverAllErasurePairs is the load-bearing correctness proof: for every
// supported n and EVERY pair of erasure positions among the n+2 transmitted
// chunks, recovery must reproduce the original bytes exactly.
func TestRecoverAllErasurePairs(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	for _, n := range []int{1, 2, 3, 9, 17, 64, 254, 255} {
		orig, frameBytes := makeFrame(rnd, n, 32)
		k := 2
		if n < 2 {
			k = 1
		}
		parity, err := ComputeParity(orig, k)
		if err != nil {
			t.Fatalf("n=%d: ComputeParity: %v", n, err)
		}
		total := n + len(parity)
		for a := 0; a < total; a++ {
			for b := a; b < total; b++ {
				// a == b models a single erasure.
				chunks, par := eraseAt(orig, parity, a, b)
				lost := 0
				for _, c := range chunks {
					if c == nil {
						lost++
					}
				}
				// Recoverability is data erasures vs. SURVIVING parity, not
				// vs. the parity originally sent — erasing a symbol removes
				// exactly as much repair capacity as erasing a data chunk
				// consumes.
				survivingParity := 0
				for _, p := range par {
					if p != nil {
						survivingParity++
					}
				}
				err := RecoverChunks(chunks, par, frameBytes)
				if lost > survivingParity {
					if err == nil {
						t.Fatalf("n=%d erasures (%d,%d): recovered %d data losses with %d surviving symbols", n, a, b, lost, survivingParity)
					}
					continue
				}
				if err != nil {
					t.Fatalf("n=%d erasures (%d,%d): RecoverChunks: %v", n, a, b, err)
				}
				for i := range orig {
					if !bytes.Equal(chunks[i], orig[i]) {
						t.Fatalf("n=%d erasures (%d,%d): chunk %d = %x, want %x", n, a, b, i, chunks[i], orig[i])
					}
				}
			}
		}
	}
}

func TestRecoverPreservesShortFinalChunk(t *testing.T) {
	// The last chunk is shorter than the rest. frameBytes is the only thing
	// that says by how much, so losing exactly that chunk is the case the
	// header field exists for.
	orig := [][]byte{{1, 2, 3, 4}, {5, 6, 7, 8}, {9}}
	frameBytes := 9
	parity, err := ComputeParity(orig, 2)
	if err != nil {
		t.Fatalf("ComputeParity: %v", err)
	}
	chunks := [][]byte{orig[0], orig[1], nil}
	if err := RecoverChunks(chunks, parity, frameBytes); err != nil {
		t.Fatalf("RecoverChunks: %v", err)
	}
	if !bytes.Equal(chunks[2], []byte{9}) {
		t.Fatalf("recovered final chunk = %x, want 09 (padding must be trimmed)", chunks[2])
	}
}

func TestRecoverFailsCleanlyWithoutParity(t *testing.T) {
	orig := [][]byte{{1, 2}, {3, 4}}
	chunks := [][]byte{nil, orig[1]}
	if err := RecoverChunks(chunks, nil, 4); err == nil {
		t.Fatal("recovery without any parity should fail")
	}
	// A frame with nothing missing needs no parity and must not error.
	full := [][]byte{orig[0], orig[1]}
	if err := RecoverChunks(full, nil, 4); err != nil {
		t.Fatalf("complete frame: %v", err)
	}
}

func TestRecoverRejectsInconsistentFrameBytes(t *testing.T) {
	orig := [][]byte{{1, 2, 3, 4}, {5, 6}}
	parity, _ := ComputeParity(orig, 2)
	chunks := [][]byte{nil, orig[1]}
	// frameBytes far larger than n * chunkLen is structurally impossible.
	if err := RecoverChunks(chunks, parity, 9999); err == nil {
		t.Fatal("inconsistent frameBytes accepted, want error")
	}
}

// --- Wire encoding ---------------------------------------------------------

func TestParityChunkRoundTrip(t *testing.T) {
	h := ParityChunkHeader{FrameID: 0x01020304, ParityIndex: 1, ChunkCount: 9, FrameBytes: 8640}
	payload := bytes.Repeat([]byte{0xa5}, 64)
	dgram, err := AppendParityChunk(nil, h, payload)
	if err != nil {
		t.Fatalf("AppendParityChunk: %v", err)
	}
	if len(dgram) != ParityChunkHeaderSize+len(payload) {
		t.Fatalf("len = %d, want %d", len(dgram), ParityChunkHeaderSize+len(payload))
	}
	got, gotPayload, err := ParseParityChunk(dgram)
	if err != nil {
		t.Fatalf("ParseParityChunk: %v", err)
	}
	if got != h {
		t.Fatalf("header = %+v, want %+v", got, h)
	}
	if !bytes.Equal(gotPayload, payload) {
		t.Fatalf("payload = %x, want %x", gotPayload, payload)
	}
}

// TestParityChunkGoldenVector pins the exact bytes. The TS and gawk-broadcast
// mirrors assert the same hex.
func TestParityChunkGoldenVector(t *testing.T) {
	h := ParityChunkHeader{FrameID: 0x01020304, ParityIndex: 1, ChunkCount: 9, FrameBytes: 8640}
	dgram, err := AppendParityChunk(nil, h, []byte{0xde, 0xad, 0xbe, 0xef})
	if err != nil {
		t.Fatalf("AppendParityChunk: %v", err)
	}
	const want = "010e01020304010009000021c0deadbeef"
	if got := hex.EncodeToString(dgram); got != want {
		t.Fatalf("golden vector mismatch:\n got %s\nwant %s", got, want)
	}
}

// TestParityChunkFullPayloadBoundary pins the common case, not an edge case:
// a parity symbol over full-size data chunks is MaxChunkPayload bytes, and
// the resulting datagram must fit MaxDatagramSize. This is why the header is
// 13 bytes and not 20 (docs/34 §4.2).
func TestParityChunkFullPayloadBoundary(t *testing.T) {
	payload := bytes.Repeat([]byte{0x5a}, MaxChunkPayload)
	dgram, err := AppendParityChunk(nil, ParityChunkHeader{FrameID: 1, ChunkCount: 9, FrameBytes: 9 * MaxChunkPayload}, payload)
	if err != nil {
		t.Fatalf("AppendParityChunk: %v", err)
	}
	if len(dgram) > MaxDatagramSize {
		t.Fatalf("full-payload parity datagram is %d bytes, exceeds MaxDatagramSize %d", len(dgram), MaxDatagramSize)
	}
	if len(dgram) != ParityChunkHeaderSize+MaxChunkPayload {
		t.Fatalf("len = %d, want %d", len(dgram), ParityChunkHeaderSize+MaxChunkPayload)
	}
	if _, p, err := ParseParityChunk(dgram); err != nil || len(p) != MaxChunkPayload {
		t.Fatalf("round trip at boundary failed: %v, payload %d", err, len(p))
	}
}

func TestParityChunkRejectsMalformed(t *testing.T) {
	good, err := AppendParityChunk(nil, ParityChunkHeader{FrameID: 1, ChunkCount: 4, FrameBytes: 16}, []byte{1, 2, 3, 4})
	if err != nil {
		t.Fatalf("AppendParityChunk: %v", err)
	}
	for name, mutate := range map[string]func([]byte) []byte{
		"short":        func(b []byte) []byte { return b[:ParityChunkHeaderSize-1] },
		"bad version":  func(b []byte) []byte { c := clone(b); c[0] = 0x02; return c },
		"bad type":     func(b []byte) []byte { c := clone(b); c[1] = TypeVideoChunk; return c },
		"zero count":   func(b []byte) []byte { c := clone(b); c[7], c[8] = 0, 0; return c },
		"count > max":  func(b []byte) []byte { c := clone(b); c[7], c[8] = 0xff, 0xff; return c },
		"bad index":    func(b []byte) []byte { c := clone(b); c[6] = MaxParitySymbols; return c },
		"oversize pay": func(b []byte) []byte { return append(clone(b), make([]byte, MaxDatagramSize)...) },
	} {
		if _, _, err := ParseParityChunk(mutate(good)); err == nil {
			t.Fatalf("%s: accepted, want error", name)
		}
	}
}

func TestAppendParityChunkRejectsOversizePayload(t *testing.T) {
	_, err := AppendParityChunk(nil, ParityChunkHeader{FrameID: 1, ChunkCount: 2, FrameBytes: 4}, make([]byte, MaxChunkPayload+1))
	if !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("err = %v, want ErrPayloadTooLarge", err)
	}
}

// --- RelayCapabilities -----------------------------------------------------

func TestRelayCapabilitiesRoundTrip(t *testing.T) {
	c := RelayCapabilities{Flags: CapParityChunks, ParityLevel: 2}
	dgram, err := AppendRelayCapabilities(nil, c)
	if err != nil {
		t.Fatalf("AppendRelayCapabilities: %v", err)
	}
	if len(dgram) != RelayCapabilitiesSize {
		t.Fatalf("len = %d, want %d", len(dgram), RelayCapabilitiesSize)
	}
	got, err := ParseRelayCapabilities(dgram)
	if err != nil {
		t.Fatalf("ParseRelayCapabilities: %v", err)
	}
	if got != c {
		t.Fatalf("got %+v, want %+v", got, c)
	}
}

func TestRelayCapabilitiesGoldenVector(t *testing.T) {
	dgram, err := AppendRelayCapabilities(nil, RelayCapabilities{Flags: CapParityChunks, ParityLevel: 2})
	if err != nil {
		t.Fatalf("AppendRelayCapabilities: %v", err)
	}
	const want = "010f000102"
	if got := hex.EncodeToString(dgram); got != want {
		t.Fatalf("golden vector mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestRelayCapabilitiesRejectsMalformed(t *testing.T) {
	good, _ := AppendRelayCapabilities(nil, RelayCapabilities{Flags: CapParityChunks, ParityLevel: 1})
	for name, bad := range map[string][]byte{
		"short":       good[:RelayCapabilitiesSize-1],
		"long":        append(clone(good), 0),
		"bad version": {0x02, TypeRelayCapabilities, 0, 1, 1},
		"bad type":    {Version, TypeVideoChunk, 0, 1, 1},
		"bad level":   {Version, TypeRelayCapabilities, 0, 1, MaxParitySymbols + 1},
	} {
		if _, err := ParseRelayCapabilities(bad); err == nil {
			t.Fatalf("%s: accepted, want error", name)
		}
	}
}

func TestParityFuzzNeverPanics(t *testing.T) {
	rnd := rand.New(rand.NewSource(7))
	buf := make([]byte, 64)
	for i := 0; i < 20000; i++ {
		n := rnd.Intn(len(buf))
		rnd.Read(buf[:n])
		_, _, _ = ParseParityChunk(buf[:n])
		_, _ = ParseRelayCapabilities(buf[:n])
	}
}

// --- helpers ---------------------------------------------------------------

func clone(b []byte) []byte { return append([]byte(nil), b...) }

// makeFrame builds n chunks of chunkLen bytes with a short final chunk, and
// returns the total frame length they encode.
func makeFrame(rnd *rand.Rand, n, chunkLen int) ([][]byte, int) {
	chunks := make([][]byte, n)
	for i := range chunks {
		l := chunkLen
		if i == n-1 {
			l = chunkLen/2 + 1
		}
		chunks[i] = make([]byte, l)
		rnd.Read(chunks[i])
	}
	return chunks, (n-1)*chunkLen + len(chunks[n-1])
}

// eraseAt returns copies of chunks/parity with positions a and b removed,
// where positions [0,n) index data chunks and [n, n+k) index parity symbols.
func eraseAt(chunks, parity [][]byte, a, b int) ([][]byte, [][]byte) {
	n := len(chunks)
	c := append([][]byte(nil), chunks...)
	p := append([][]byte(nil), parity...)
	for _, pos := range []int{a, b} {
		if pos < n {
			c[pos] = nil
		} else {
			p[pos-n] = nil
		}
	}
	return c, p
}
