// Package opusfixture reads and writes the e2e voice fixture: a sequence of
// Opus packets, each prefixed by its length as a big-endian uint16. It also
// extracts the packets from an Ogg Opus stream (what ffmpeg's libopus
// writes), for testdata/gen-tone.sh. Fixture audio is a generated test tone,
// never anyone's voice.
package opusfixture

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Read returns every packet in a fixture.
func Read(r io.Reader) ([][]byte, error) {
	var out [][]byte
	var hdr [2]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, err
		}
		n := binary.BigEndian.Uint16(hdr[:])
		if n == 0 {
			return nil, errors.New("opusfixture: empty packet")
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(r, p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
}

// Write writes packets as a fixture.
func Write(w io.Writer, packets [][]byte) error {
	for _, p := range packets {
		if len(p) == 0 || len(p) > 0xFFFF {
			return fmt.Errorf("opusfixture: %d-byte packet", len(p))
		}
		var hdr [2]byte
		binary.BigEndian.PutUint16(hdr[:], uint16(len(p)))
		if _, err := w.Write(hdr[:]); err != nil {
			return err
		}
		if _, err := w.Write(p); err != nil {
			return err
		}
	}
	return nil
}

// FromOgg extracts the Opus packets of an Ogg Opus stream (RFC 7845),
// skipping the OpusHead and OpusTags headers.
func FromOgg(r io.Reader) ([][]byte, error) {
	var out [][]byte
	var partial []byte
	for {
		var hdr [27]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		if string(hdr[:4]) != "OggS" {
			return nil, errors.New("opusfixture: not an Ogg page")
		}
		segs := make([]byte, hdr[26])
		if _, err := io.ReadFull(r, segs); err != nil {
			return nil, err
		}
		for _, l := range segs {
			seg := make([]byte, l)
			if _, err := io.ReadFull(r, seg); err != nil {
				return nil, err
			}
			partial = append(partial, seg...)
			if l < 255 {
				if len(partial) > 0 && !bytes.HasPrefix(partial, []byte("OpusHead")) && !bytes.HasPrefix(partial, []byte("OpusTags")) {
					out = append(out, partial)
				}
				partial = nil
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("opusfixture: no Opus packets")
	}
	return out, nil
}
