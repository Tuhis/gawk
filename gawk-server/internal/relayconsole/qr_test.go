package relayconsole

import (
	"bytes"
	"strings"
	"testing"
)

// The widely restated 1-M "HELLO WORLD" codeword block: sixteen data
// codewords and the ten EC codewords they divide to.
func TestReedSolomonKnownVector(t *testing.T) {
	data := []byte{32, 91, 11, 120, 209, 114, 220, 77, 67, 64, 236, 17, 236, 17, 236, 17}
	want := []byte{196, 35, 39, 119, 235, 215, 231, 226, 93, 23}
	got := rsRemainder(data, rsDivisor(len(want)))
	if !bytes.Equal(got, want) {
		t.Fatalf("EC codewords = %v, want %v", got, want)
	}
}

// The format string for level M, mask 5 is a published constant
// (100000011001110), and every version from 7 carries its BCH-coded number
// (version 7 is 000111110010010100).
func TestFormatAndVersionBits(t *testing.T) {
	if got := formatBits(eccMedium, 5); got != 0b100000011001110 {
		t.Errorf("format(M,5) = %015b", got)
	}
	if got := versionBits(7); got != 0b000111110010010100 {
		t.Errorf("version(7) = %018b", got)
	}
}

// Alignment centres for a few versions, against the standard's table.
func TestAlignmentPositions(t *testing.T) {
	for ver, want := range map[int][]int{
		1: nil, 2: {6, 18}, 7: {6, 22, 38}, 14: {6, 26, 46, 66},
		22: {6, 26, 50, 74, 98}, 32: {6, 34, 60, 86, 112, 138}, 40: {6, 30, 58, 86, 114, 142, 170},
	} {
		got := alignmentPositions(ver)
		if len(got) != len(want) {
			t.Errorf("v%d: %v, want %v", ver, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("v%d: %v, want %v", ver, got, want)
				break
			}
		}
	}
}

// Byte-mode capacity at level M, against the standard's capacity table.
func TestCapacityAtLevelM(t *testing.T) {
	for ver, want := range map[int]int{1: 14, 2: 26, 5: 84, 10: 213, 15: 412, 20: 666, 40: 2331} {
		if got := byteCapacity(ver, eccMedium); got != want {
			t.Errorf("v%d-M holds %d bytes, want %d", ver, got, want)
		}
	}
}

// EncodeQR picks the smallest version that fits, refuses what fits none,
// and draws the function patterns where a reader looks for them.
func TestEncodeShape(t *testing.T) {
	q, err := EncodeQR([]byte(strings.Repeat("x", 213)))
	if err != nil {
		t.Fatal(err)
	}
	if q.Version != 10 || q.Size != 57 {
		t.Fatalf("213 bytes: version %d size %d, want 10 / 57", q.Version, q.Size)
	}
	if q2, _ := EncodeQR([]byte(strings.Repeat("x", 214))); q2 == nil || q2.Version != 11 {
		t.Fatalf("214 bytes: want version 11, got %+v", q2)
	}
	if _, err := EncodeQR(make([]byte, 2332)); err == nil {
		t.Fatal("2332 bytes fit no version at level M; want an error")
	}
	for _, c := range [][2]int{{3, 3}, {q.Size - 4, 3}, {3, q.Size - 4}} {
		for d := -3; d <= 3; d++ {
			if !q.Dark(c[0]+d, c[1]-3) || !q.Dark(c[0]-3, c[1]+d) {
				t.Fatalf("finder at %v has a light outer ring", c)
			}
		}
		if q.Dark(c[0]-2, c[1]) || !q.Dark(c[0], c[1]) {
			t.Fatalf("finder at %v is not ring/centre", c)
		}
	}
	// The timing patterns alternate, starting dark.
	for i := 8; i < q.Size-8; i++ {
		if q.Dark(i, 6) != (i%2 == 0) || q.Dark(6, i) != (i%2 == 0) {
			t.Fatalf("timing pattern broken at %d", i)
		}
	}
	// The always-dark module beside the lower-left format copy.
	if !q.Dark(8, q.Size-8) {
		t.Fatal("dark module missing")
	}
}

// The symbols are pinned: the encoder is deterministic, and these were
// checked by decoding them with an independent reader (ZXing's Go port,
// github.com/makiuchi-d/gozxing) when the encoder was written. A change
// here changes every QR code the console shows; re-verify before
// re-pinning.
func TestEncodeGolden(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"https://gawk.ioio.fi", qrGoldenShort},
		{"https://gawk.ioio.fi/#/?relay=https%3A%2F%2F192.168.1.20%3A4433&certHash=" + strings.Repeat("a1", 32) + "," + strings.Repeat("b2", 32), qrGoldenStandalone},
	} {
		q, err := EncodeQR([]byte(c.in))
		if err != nil {
			t.Fatal(err)
		}
		if got := q.fingerprint(); got != c.want {
			t.Errorf("%q: fingerprint %s, want %s", c.in, got, c.want)
		}
	}
}

func TestSVG(t *testing.T) {
	q, err := EncodeQR([]byte("hi"))
	if err != nil {
		t.Fatal(err)
	}
	svg := string(q.SVG())
	if !strings.HasPrefix(svg, "<svg ") || !strings.Contains(svg, `viewBox="0 0 29 29"`) {
		t.Fatalf("a v1 symbol with a 4-module quiet zone is 29 wide:\n%s", svg)
	}
}
