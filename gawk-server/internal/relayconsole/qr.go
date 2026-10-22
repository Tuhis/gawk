package relayconsole

// A minimal QR Code encoder (ISO/IEC 18004): byte mode, error-correction
// level M, versions 1–40, the penalty-chosen mask. The console draws its join
// code with it, with no network and no JavaScript library — the console must
// work at a party with no internet at all (docs/76 §6.1 item 6) — and
// without adding a module dependency to gawk-server, whose go.mod every
// other Go module in the repo pulls through its replace directive.
//
// The structure follows Project Nayuki's reference implementation (MIT);
// the tables are the standard's.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

type eccLevel int

const (
	eccLow eccLevel = iota
	eccMedium
)

// formatID is the level's two format-information bits (L=01, M=00).
var formatID = [...]int{eccLow: 1, eccMedium: 0}

// eccPerBlock and eccBlocks are the standard's table 9, indexed by version
// (index 0 unused).
var eccPerBlock = [...][41]int{
	eccLow:    {-1, 7, 10, 15, 20, 26, 18, 20, 24, 30, 18, 20, 24, 26, 30, 22, 24, 28, 30, 28, 28, 28, 28, 30, 30, 26, 28, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30},
	eccMedium: {-1, 10, 16, 26, 18, 24, 16, 18, 22, 22, 26, 30, 22, 22, 24, 24, 28, 28, 26, 26, 26, 26, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28},
}

var eccBlocks = [...][41]int{
	eccLow:    {-1, 1, 1, 1, 1, 1, 2, 2, 2, 2, 4, 4, 4, 4, 4, 6, 6, 6, 6, 7, 8, 8, 9, 9, 10, 12, 12, 12, 13, 14, 15, 16, 17, 18, 19, 19, 20, 21, 22, 24, 25},
	eccMedium: {-1, 1, 1, 1, 2, 2, 4, 4, 4, 5, 5, 5, 8, 9, 9, 10, 10, 11, 13, 14, 16, 17, 17, 18, 20, 21, 23, 25, 26, 28, 29, 31, 33, 35, 37, 38, 40, 43, 45, 47, 49},
}

// ErrQRTooLong is returned for a payload no version can hold.
var ErrQRTooLong = errors.New("relayconsole: payload too long for a QR code")

// QR is an encoded symbol: Size×Size modules, without the quiet zone.
type QR struct {
	Version int
	Size    int
	mods    []bool // row-major, true = dark
	fn      []bool // function modules (not data)
}

// Dark reports the module at column x, row y.
func (q *QR) Dark(x, y int) bool { return q.mods[y*q.Size+x] }

// EncodeQR encodes data in byte mode at level M in the smallest version
// that holds it.
func EncodeQR(data []byte) (*QR, error) {
	const ecl = eccMedium
	ver := 0
	for v := 1; v <= 40; v++ {
		if len(data) <= byteCapacity(v, ecl) {
			ver = v
			break
		}
	}
	if ver == 0 {
		return nil, ErrQRTooLong
	}

	// The bit stream: mode 0100, the count, the bytes, a terminator, then
	// pad to a byte and fill with the alternating pad codewords.
	var bb bitBuf
	bb.put(0b0100, 4)
	bb.put(len(data), countBits(ver))
	for _, b := range data {
		bb.put(int(b), 8)
	}
	capBits := dataCodewords(ver, ecl) * 8
	bb.put(0, min(4, capBits-bb.n))
	bb.put(0, (8-bb.n%8)%8)
	for pad := 0xEC; bb.n < capBits; pad ^= 0xEC ^ 0x11 {
		bb.put(pad, 8)
	}

	q := &QR{Version: ver, Size: ver*4 + 17}
	q.mods = make([]bool, q.Size*q.Size)
	q.fn = make([]bool, q.Size*q.Size)
	q.drawFunctionPatterns()
	q.drawCodewords(addECCAndInterleave(bb.bytes, ver, ecl))

	best, bestPenalty := 0, -1
	for m := 0; m < 8; m++ {
		q.applyMask(m)
		q.drawFormat(ecl, m)
		if p := q.penalty(); bestPenalty < 0 || p < bestPenalty {
			best, bestPenalty = m, p
		}
		q.applyMask(m) // XOR undoes it
	}
	q.applyMask(best)
	q.drawFormat(ecl, best)
	return q, nil
}

// byteCapacity is how many payload bytes a version holds in byte mode.
func byteCapacity(ver int, ecl eccLevel) int {
	return (dataCodewords(ver, ecl)*8 - 4 - countBits(ver)) / 8
}

func countBits(ver int) int {
	if ver <= 9 {
		return 8
	}
	return 16
}

// rawModules is the number of data+EC modules in a version: the symbol less
// every function pattern.
func rawModules(ver int) int {
	r := (16*ver+128)*ver + 64
	if ver >= 2 {
		n := ver/7 + 2
		r -= (25*n-10)*n - 55
		if ver >= 7 {
			r -= 36
		}
	}
	return r
}

func dataCodewords(ver int, ecl eccLevel) int {
	return rawModules(ver)/8 - eccPerBlock[ecl][ver]*eccBlocks[ecl][ver]
}

// alignmentPositions is the standard's table E.1, computed.
func alignmentPositions(ver int) []int {
	if ver == 1 {
		return nil
	}
	n := ver/7 + 2
	step := (ver*8 + n*3 + 5) / (n*4 - 4) * 2
	out := make([]int, n)
	out[0] = 6
	for i, pos := n-1, ver*4+17-7; i >= 1; i, pos = i-1, pos-step {
		out[i] = pos
	}
	return out
}

type bitBuf struct {
	bytes []byte
	n     int
}

func (b *bitBuf) put(v, bits int) {
	for i := bits - 1; i >= 0; i-- {
		if b.n%8 == 0 {
			b.bytes = append(b.bytes, 0)
		}
		if v>>i&1 != 0 {
			b.bytes[b.n/8] |= 0x80 >> (b.n % 8)
		}
		b.n++
	}
}

// addECCAndInterleave splits the data codewords into the version's blocks,
// appends each block's Reed–Solomon codewords and interleaves them.
func addECCAndInterleave(data []byte, ver int, ecl eccLevel) []byte {
	nBlocks := eccBlocks[ecl][ver]
	eccLen := eccPerBlock[ecl][ver]
	raw := rawModules(ver) / 8
	nShort := nBlocks - raw%nBlocks
	shortData := raw/nBlocks - eccLen
	div := rsDivisor(eccLen)

	blocks := make([][]byte, nBlocks)
	eccs := make([][]byte, nBlocks)
	k := 0
	for i := range blocks {
		n := shortData
		if i >= nShort {
			n++
		}
		blocks[i] = data[k : k+n]
		k += n
		eccs[i] = rsRemainder(blocks[i], div)
	}
	out := make([]byte, 0, raw)
	for i := 0; i <= shortData; i++ {
		for _, b := range blocks {
			if i < len(b) {
				out = append(out, b[i])
			}
		}
	}
	for i := 0; i < eccLen; i++ {
		for _, e := range eccs {
			out = append(out, e[i])
		}
	}
	return out
}

// gfMul multiplies in GF(2^8) modulo x^8+x^4+x^3+x^2+1.
func gfMul(x, y byte) byte {
	var z int
	for i := 7; i >= 0; i-- {
		z = (z << 1) ^ ((z >> 7) * 0x11D)
		z ^= int(y>>i&1) * int(x)
	}
	return byte(z)
}

// rsDivisor is the generator polynomial of the given degree, highest
// coefficient (always 1) dropped.
func rsDivisor(degree int) []byte {
	out := make([]byte, degree)
	out[degree-1] = 1
	root := byte(1)
	for i := 0; i < degree; i++ {
		for j := range out {
			out[j] = gfMul(out[j], root)
			if j+1 < len(out) {
				out[j] ^= out[j+1]
			}
		}
		root = gfMul(root, 0x02)
	}
	return out
}

func rsRemainder(data, div []byte) []byte {
	out := make([]byte, len(div))
	for _, b := range data {
		factor := b ^ out[0]
		copy(out, out[1:])
		out[len(out)-1] = 0
		for i, d := range div {
			out[i] ^= gfMul(d, factor)
		}
	}
	return out
}

func (q *QR) set(x, y int, dark bool) {
	q.mods[y*q.Size+x] = dark
	q.fn[y*q.Size+x] = true
}

func (q *QR) drawFunctionPatterns() {
	for i := 0; i < q.Size; i++ {
		q.set(6, i, i%2 == 0)
		q.set(i, 6, i%2 == 0)
	}
	q.drawFinder(3, 3)
	q.drawFinder(q.Size-4, 3)
	q.drawFinder(3, q.Size-4)
	pos := alignmentPositions(q.Version)
	last := len(pos) - 1
	for i := range pos {
		for j := range pos {
			if (i == 0 && j == 0) || (i == 0 && j == last) || (i == last && j == 0) {
				continue
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					q.set(pos[i]+dx, pos[j]+dy, max(abs(dx), abs(dy)) != 1)
				}
			}
		}
	}
	q.drawFormat(eccMedium, 0) // reserves the area; redrawn per mask
	if q.Version >= 7 {
		bits := versionBits(q.Version)
		for i := 0; i < 18; i++ {
			dark := bits>>i&1 != 0
			a, b := q.Size-11+i%3, i/3
			q.set(a, b, dark)
			q.set(b, a, dark)
		}
	}
}

func (q *QR) drawFinder(cx, cy int) {
	for dy := -4; dy <= 4; dy++ {
		for dx := -4; dx <= 4; dx++ {
			x, y := cx+dx, cy+dy
			if x < 0 || y < 0 || x >= q.Size || y >= q.Size {
				continue
			}
			d := max(abs(dx), abs(dy))
			q.set(x, y, d != 2 && d != 4)
		}
	}
}

// formatBits is the 15-bit BCH-coded, masked format information.
func formatBits(ecl eccLevel, mask int) int {
	data := formatID[ecl]<<3 | mask
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	return (data<<10 | rem) ^ 0x5412
}

// versionBits is the 18-bit BCH-coded version information.
func versionBits(ver int) int {
	rem := ver
	for i := 0; i < 12; i++ {
		rem = (rem << 1) ^ ((rem >> 11) * 0x1F25)
	}
	return ver<<12 | rem
}

func (q *QR) drawFormat(ecl eccLevel, mask int) {
	bits := formatBits(ecl, mask)
	bit := func(i int) bool { return bits>>i&1 != 0 }
	for i := 0; i <= 5; i++ {
		q.set(8, i, bit(i))
	}
	q.set(8, 7, bit(6))
	q.set(8, 8, bit(7))
	q.set(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		q.set(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		q.set(q.Size-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		q.set(8, q.Size-15+i, bit(i))
	}
	q.set(8, q.Size-8, true)
}

// drawCodewords places the codeword bits in the zigzag order, two columns
// at a time from the right, skipping the vertical timing column.
func (q *QR) drawCodewords(cw []byte) {
	i := 0
	for right := q.Size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < q.Size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				y := vert
				if (right+1)&2 == 0 {
					y = q.Size - 1 - vert
				}
				if q.fn[y*q.Size+x] || i >= len(cw)*8 {
					continue
				}
				q.mods[y*q.Size+x] = cw[i>>3]>>(7-i&7)&1 != 0
				i++
			}
		}
	}
}

func (q *QR) applyMask(m int) {
	for y := 0; y < q.Size; y++ {
		for x := 0; x < q.Size; x++ {
			var inv bool
			switch m {
			case 0:
				inv = (x+y)%2 == 0
			case 1:
				inv = y%2 == 0
			case 2:
				inv = x%3 == 0
			case 3:
				inv = (x+y)%3 == 0
			case 4:
				inv = (x/3+y/2)%2 == 0
			case 5:
				inv = x*y%2+x*y%3 == 0
			case 6:
				inv = (x*y%2+x*y%3)%2 == 0
			case 7:
				inv = ((x+y)%2+x*y%3)%2 == 0
			}
			if inv && !q.fn[y*q.Size+x] {
				q.mods[y*q.Size+x] = !q.mods[y*q.Size+x]
			}
		}
	}
}

// penalty is the standard's mask evaluation (N1–N4). Any mask decodes; this
// only picks the one readers find easiest.
func (q *QR) penalty() int {
	n := q.Size
	p := 0
	line := func(get func(i int) bool) {
		run := 1
		for i := 1; i <= n; i++ {
			if i < n && get(i) == get(i-1) {
				run++
				continue
			}
			if run >= 5 {
				p += 3 + run - 5
			}
			run = 1
		}
		// 1:1:3:1:1 finder-like runs with four light modules on a side.
		pat := []bool{true, false, true, true, true, false, true}
		for i := 0; i+7 <= n; i++ {
			match := true
			for k, v := range pat {
				if get(i+k) != v {
					match = false
					break
				}
			}
			if !match {
				continue
			}
			lightRun := func(from, to int) bool {
				for k := from; k < to; k++ {
					if k >= 0 && k < n && get(k) {
						return false
					}
				}
				return true
			}
			if lightRun(i-4, i) || lightRun(i+7, i+11) {
				p += 40
			}
		}
	}
	for y := 0; y < n; y++ {
		line(func(i int) bool { return q.Dark(i, y) })
	}
	for x := 0; x < n; x++ {
		line(func(i int) bool { return q.Dark(x, i) })
	}
	dark := 0
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			c := q.Dark(x, y)
			if c {
				dark++
			}
			if x+1 < n && y+1 < n && c == q.Dark(x+1, y) && c == q.Dark(x, y+1) && c == q.Dark(x+1, y+1) {
				p += 3
			}
		}
	}
	total := n * n
	k := (abs(dark*20-total*10)+total-1)/total - 1
	return p + k*10
}

// SVG renders the symbol with a 4-module quiet zone, one unit per module.
// Scale it with CSS; shape-rendering keeps the edges sharp.
func (q *QR) SVG() []byte {
	const quiet = 4
	w := q.Size + 2*quiet
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges">`, w, w)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, w, w)
	for y := 0; y < q.Size; y++ {
		for x := 0; x < q.Size; x++ {
			if q.Dark(x, y) {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return []byte(b.String())
}

// fingerprint pins a symbol in tests: a hash of its modules, row by row.
func (q *QR) fingerprint() string {
	h := sha256.New()
	for y := 0; y < q.Size; y++ {
		for x := 0; x < q.Size; x++ {
			if q.Dark(x, y) {
				h.Write([]byte{'1'})
			} else {
				h.Write([]byte{'0'})
			}
		}
		h.Write([]byte{'\n'})
	}
	return fmt.Sprintf("v%d-%s", q.Version, hex.EncodeToString(h.Sum(nil))[:16])
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
