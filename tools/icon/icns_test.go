package main

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestICNSRoundTripsEveryType(t *testing.T) {
	images := testImages(ICNSSizes...)
	data, err := WriteICNS(images)
	if err != nil {
		t.Fatal(err)
	}
	if string(data[:4]) != "icns" {
		t.Fatalf("magic %q", data[:4])
	}
	if n := binary.BigEndian.Uint32(data[4:]); int(n) != len(data) {
		t.Fatalf("header length %d, file is %d", n, len(data))
	}
	// Walk the elements by hand, not with ParseICNS, so the writer is not
	// its own witness: every payload is a PNG of the type's pixel size.
	seen := map[string]bool{}
	for p := 8; p < len(data); {
		typ := string(data[p : p+4])
		n := int(binary.BigEndian.Uint32(data[p+4:]))
		payload := data[p+8 : p+n]
		img, err := png.Decode(bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("%s: not PNG: %v", typ, err)
		}
		want, ok := icnsSize(typ)
		if !ok {
			t.Fatalf("unexpected type %q", typ)
		}
		sameImage(t, toNRGBA(img), images[want])
		seen[typ] = true
		p += n
	}
	for _, e := range icnsTypes {
		if !seen[e.Type] {
			t.Errorf("type %s missing", e.Type)
		}
	}

	parsed, err := ParseICNS(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed) != len(icnsTypes) {
		t.Fatalf("parsed %d elements, want %d", len(parsed), len(icnsTypes))
	}
	for _, e := range icnsTypes {
		sameImage(t, parsed[e.Type], images[e.Size])
	}
}

func TestICNSCarriesTheRetinaDockSizes(t *testing.T) {
	// The Dock draws at up to 512 pt, so a Retina display needs the 1024 px
	// ic10; without it macOS upscales the 512 and the icon looks soft.
	for typ, want := range map[string]int{"ic09": 512, "ic10": 1024, "ic07": 128, "icp4": 16} {
		if got, ok := icnsSize(typ); !ok || got != want {
			t.Errorf("%s: %d px (known %v), want %d", typ, got, ok, want)
		}
	}
}

func TestWriteICNSRejectsAMissingSize(t *testing.T) {
	images := testImages(ICNSSizes...)
	delete(images, 1024)
	if _, err := WriteICNS(images); err == nil {
		t.Fatal("wrote an .icns without the 1024 px image")
	}
}

func TestParseICNSRejectsGarbage(t *testing.T) {
	good, err := WriteICNS(testImages(ICNSSizes...))
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(f func([]byte) []byte) []byte {
		return f(append([]byte(nil), good...))
	}
	for name, data := range map[string][]byte{
		"empty":    {},
		"not icns": []byte("\x89PNG\r\n\x1a\n"),
		"bad total": mutate(func(b []byte) []byte {
			binary.BigEndian.PutUint32(b[4:], uint32(len(b)+1))
			return b
		}),
		"past end": mutate(func(b []byte) []byte {
			binary.BigEndian.PutUint32(b[12:], uint32(len(b)))
			return b
		}),
		"unknown type": mutate(func(b []byte) []byte {
			copy(b[8:], "zzzz")
			return b
		}),
		"mislabelled": mutate(func(b []byte) []byte {
			// The first element is icp4 (16 px); call it ic10 (1024 px).
			copy(b[8:], "ic10")
			return b
		}),
		"truncated": good[:len(good)-1],
	} {
		if _, err := ParseICNS(data); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

func TestCheckFailsOnAMissingICNS(t *testing.T) {
	dir := copyIconDir(t)
	if err := os.Remove(filepath.Join(dir, icnsName)); err != nil {
		t.Fatal(err)
	}
	if err := Check(dir); err == nil {
		t.Fatal("a missing .icns passed the check")
	}
}

func TestCheckFailsOnAStaleICNSEntry(t *testing.T) {
	dir := copyIconDir(t)
	images, err := renderSource(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A blank 1024 px image: structurally valid, visibly wrong.
	images[1024] = testImages(1024)[1024]
	data, err := WriteICNS(images)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, icnsName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Check(dir); err == nil {
		t.Fatal("a stale .icns entry passed the check")
	}
}
