package opusfixture

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	in := [][]byte{{1}, {2, 3}, bytes.Repeat([]byte{4}, 300)}
	var b bytes.Buffer
	if err := Write(&b, in); err != nil {
		t.Fatal(err)
	}
	out, err := Read(&b)
	if err != nil || len(out) != 3 || !bytes.Equal(out[2], in[2]) {
		t.Fatalf("%v %v", out, err)
	}
	if err := Write(&b, [][]byte{{}}); err == nil {
		t.Fatal("empty packet written")
	}
	if _, err := Read(bytes.NewReader([]byte{0, 0})); err == nil {
		t.Fatal("empty packet read")
	}
	if _, err := Read(bytes.NewReader([]byte{0, 5, 1})); err == nil {
		t.Fatal("truncated read")
	}
}

func oggPage(packets ...[]byte) []byte {
	var segs []byte
	var body []byte
	for _, p := range packets {
		n := len(p)
		for n >= 255 {
			segs = append(segs, 255)
			n -= 255
		}
		segs = append(segs, byte(n))
		body = append(body, p...)
	}
	h := make([]byte, 27)
	copy(h, "OggS")
	binary.LittleEndian.PutUint32(h[18:], 0)
	h[26] = byte(len(segs))
	return append(append(h, segs...), body...)
}

func TestFromOgg(t *testing.T) {
	big := bytes.Repeat([]byte{7}, 300)
	stream := append(oggPage([]byte("OpusHead........")), oggPage([]byte("OpusTags...."))...)
	stream = append(stream, oggPage([]byte{1, 2}, big)...)
	out, err := FromOgg(bytes.NewReader(stream))
	if err != nil || len(out) != 2 || !bytes.Equal(out[1], big) {
		t.Fatalf("%d packets, %v", len(out), err)
	}
	if _, err := FromOgg(bytes.NewReader([]byte("nope nope nope nope nope nope"))); err == nil {
		t.Fatal("garbage accepted")
	}
	if _, err := FromOgg(bytes.NewReader(nil)); err == nil {
		t.Fatal("empty accepted")
	}
}

// The committed fixture is ≥ 2 s of 10 ms frames.
func TestCommittedToneFixture(t *testing.T) {
	f, err := os.Open("../../testdata/tone-440hz-10ms.opus")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ps, err := Read(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) < 200 {
		t.Fatalf("%d packets: under 2 s of 10 ms frames", len(ps))
	}
	for i, p := range ps {
		// TOC config 15/19/23/27/31 ⇔ 10 ms (RFC 6716 §3.1): CELT-only
		// configs 16-31 encode 2.5/5/10/20 ms as config%4 = 0..3.
		if cfg := p[0] >> 3; !(cfg >= 16 && cfg%4 == 2) {
			t.Fatalf("packet %d TOC config %d is not a 10 ms CELT frame", i, cfg)
		}
	}
}
