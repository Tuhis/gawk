package wire

// R84 BR1 (docs/88 D2, §5.2): the keyframe carrier, K3's wire form. A
// publisher's GOP stream whose prologue is Version‖TypeKeyframeCarrier, then
// one StreamFrame message exactly as a keyframe stream carries it, then R73's
// carrier records to the FIN. The TS (wire.test.ts, parity.test.ts) and Rust
// (crates/wire/tests/golden.rs) mirrors pin the identical hex.

import (
	"bytes"
	"encoding/hex"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	// The keyframe carrier's prologue.
	//
	//   01   version
	//   1f   type = KeyframeCarrier
	goldenKeyframeCarrierPrologueHex = "011f"

	// The head of a keyframe carrier: the prologue, the golden StreamFrame
	// header (ConfigLen 6, PayloadLen 3), its six config and three payload
	// bytes, then one record framing the golden VideoChunk.
	goldenKeyframeCarrierHeadHex = goldenKeyframeCarrierPrologueHex +
		goldenStreamFrameHeaderHex + "a1a2a3a4a5a6" + "b1b2b3" + goldenCarrierRecordHex

	// RelayCapabilities with every bit through CapKeyframeCarriers and
	// parity level 2.
	//
	//   01      version
	//   0f      type = RelayCapabilities
	//   00 0f   flags = parity | striped | uplink carriers | keyframe carriers
	//   02      parity level
	goldenCapabilitiesKeyframeCarriersHex = "010f000f02"
)

func TestKeyframeCarrierConstants(t *testing.T) {
	if TypeKeyframeCarrier != 0x1F {
		t.Fatalf("TypeKeyframeCarrier = 0x%02x, the mirrors pin 0x1F", TypeKeyframeCarrier)
	}
	if CapKeyframeCarriers != 1<<3 {
		t.Fatalf("CapKeyframeCarriers = %#x, the mirrors pin 1<<3", CapKeyframeCarriers)
	}
}

func TestGoldenKeyframeCarrierPrologue(t *testing.T) {
	got := AppendKeyframeCarrierPrologue(nil)
	if want := mustHex(t, goldenKeyframeCarrierPrologueHex); !bytes.Equal(got, want) {
		t.Fatalf("keyframe carrier prologue = %x, want %x", got, want)
	}
	if err := ParseKeyframeCarrierPrologue(got); err != nil {
		t.Fatalf("ParseKeyframeCarrierPrologue(golden) = %v", err)
	}
	// The two prologues are told apart by their type byte alone.
	if err := ParseCarrierPrologue(got); !errors.Is(err, ErrBadType) {
		t.Fatalf("ParseCarrierPrologue(keyframe carrier) = %v, want ErrBadType", err)
	}
	if err := ParseKeyframeCarrierPrologue(AppendCarrierPrologue(nil)); !errors.Is(err, ErrBadType) {
		t.Fatalf("ParseKeyframeCarrierPrologue(carrier) = %v, want ErrBadType", err)
	}
}

func TestKeyframeCarrierPrologueRejects(t *testing.T) {
	for name, tc := range map[string]struct {
		buf  []byte
		want error
	}{
		"short":        {[]byte{Version}, ErrShortDatagram},
		"empty":        {nil, ErrShortDatagram},
		"bad version":  {[]byte{0x02, TypeKeyframeCarrier}, ErrBadVersion},
		"stream frame": {[]byte{Version, TypeStreamFrame}, ErrBadType},
	} {
		if err := ParseKeyframeCarrierPrologue(tc.buf); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

// The order on the stream is the contract: prologue, the keyframe's
// StreamFrame (header, config, payload), then records.
func TestGoldenKeyframeCarrierHead(t *testing.T) {
	hdr, err := AppendStreamFrameHeader(nil, StreamFrameHeader{
		Keyframe: true, FrameID: 0x01020304, TimestampUs: 0x0000005D21DBA5F0, ConfigLen: 6, PayloadLen: 3,
	})
	if err != nil {
		t.Fatalf("AppendStreamFrameHeader: %v", err)
	}
	buf := AppendKeyframeCarrierPrologue(nil)
	buf = append(buf, hdr...)
	buf = append(buf, 0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xb1, 0xb2, 0xb3)
	if buf, err = AppendCarrierRecord(buf, mustHex(t, goldenVideoChunkHex)); err != nil {
		t.Fatalf("AppendCarrierRecord: %v", err)
	}
	if got := hex.EncodeToString(buf); got != goldenKeyframeCarrierHeadHex {
		t.Fatalf("keyframe carrier head:\n got %s\nwant %s", got, goldenKeyframeCarrierHeadHex)
	}

	if err := ParseKeyframeCarrierPrologue(buf); err != nil {
		t.Fatalf("prologue: %v", err)
	}
	rest := buf[CarrierPrologueSize:]
	h, err := ParseStreamFrameHeader(rest)
	if err != nil {
		t.Fatalf("ParseStreamFrameHeader: %v", err)
	}
	if !h.Keyframe || h.FrameID != 0x01020304 {
		t.Fatalf("header = %+v", h)
	}
	rest = rest[StreamFrameHeaderSize+int(h.ConfigLen)+int(h.PayloadLen):]
	rec, rest, err := ParseCarrierRecord(rest)
	if err != nil {
		t.Fatalf("ParseCarrierRecord: %v", err)
	}
	if !bytes.Equal(rec, mustHex(t, goldenVideoChunkHex)) || len(rest) != 0 {
		t.Fatalf("record %x rest %x", rec, rest)
	}
}

func TestRelayCapabilitiesKeyframeCarriersGoldenVector(t *testing.T) {
	dgram, err := AppendRelayCapabilities(nil, RelayCapabilities{
		Flags:       CapParityChunks | CapStripedDelivery | CapUplinkCarriers | CapKeyframeCarriers,
		ParityLevel: 2,
	})
	if err != nil {
		t.Fatalf("AppendRelayCapabilities: %v", err)
	}
	if got := hex.EncodeToString(dgram); got != goldenCapabilitiesKeyframeCarriersHex {
		t.Fatalf("golden vector mismatch:\n got %s\nwant %s", got, goldenCapabilitiesKeyframeCarriersHex)
	}
	c, err := ParseRelayCapabilities(dgram)
	if err != nil {
		t.Fatalf("ParseRelayCapabilities: %v", err)
	}
	if c.Flags&CapKeyframeCarriers == 0 || c.Flags&CapUplinkCarriers == 0 {
		t.Fatalf("flags = %#04x, want both carrier bits", c.Flags)
	}
}

// Every Type* constant in this package's sources has its own value. Types are
// allocated beside their messages in several files (wire.go's header names
// them), so a new one taken from the header alone could collide (#729 found
// 0x13–0x1D allocated outside it).
func TestWireTypesAreDistinct(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen := map[int64]string{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, id := range vs.Names {
					if !strings.HasPrefix(id.Name, "Type") || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.INT {
						continue
					}
					v, err := strconv.ParseInt(lit.Value, 0, 64)
					if err != nil {
						t.Fatalf("%s: %v", id.Name, err)
					}
					if prev, dup := seen[v]; dup {
						t.Errorf("0x%02x is both %s and %s (%s)", v, prev, id.Name, name)
					}
					seen[v] = id.Name
				}
			}
		}
	}
	if seen[TypeKeyframeCarrier] != "TypeKeyframeCarrier" {
		t.Fatalf("TypeKeyframeCarrier not found by the source walk: %v", seen)
	}
}
