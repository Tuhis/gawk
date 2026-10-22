package main

import (
	"bytes"
	"testing"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// The PoP sender's deltas are what a broadcaster sends: VideoChunks that
// reassemble to the frame's size, and parity that recovers a lost chunk.
func TestLegSendDeltasCarryRecoverableParity(t *testing.T) {
	dgrams, parity, err := deltaDatagrams(7, 1000, 2500, 1100, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(dgrams) != 3 || len(parity) != 2 {
		t.Fatalf("%d chunks, %d parity; want 3 and 2", len(dgrams), len(parity))
	}
	payloads := make([][]byte, len(dgrams))
	total := 0
	for i, d := range dgrams {
		h, p, err := wire.ParseVideoChunk(d)
		if err != nil || h.FrameID != 7 || int(h.ChunkIndex) != i || h.ChunkCount != 3 {
			t.Fatalf("chunk %d: %+v %v", i, h, err)
		}
		payloads[i] = append([]byte(nil), p...)
		total += len(p)
	}
	if total != 2500 {
		t.Fatalf("payload bytes %d, want 2500", total)
	}
	syms := make([][]byte, len(parity))
	for i, d := range parity {
		h, p, err := wire.ParseParityChunk(d)
		if err != nil || h.FrameID != 7 || int(h.ParityIndex) != i || h.ChunkCount != 3 || h.FrameBytes != 2500 {
			t.Fatalf("parity %d: %+v %v", i, h, err)
		}
		syms[i] = p
	}
	want := payloads[1]
	lost := append([][]byte(nil), payloads...)
	lost[1] = nil
	if err := wire.RecoverChunks(lost, syms, 2500); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(lost[1], want) {
		t.Fatal("parity did not recover the lost chunk")
	}
	if _, parity, _ := deltaDatagrams(8, 0, 2500, 1100, 0); parity != nil {
		t.Fatal("-parity 0 sent parity")
	}
}
