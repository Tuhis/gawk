//go:build ignore

// Command genaudio turns an MPEG-TS carrying Opus into the length-prefixed
// packet file internal/fixture embeds. It is run by hand, not by CI — the
// fixture is committed bytes on purpose (docs/28 Decision 12).
//
// See ../README-audio.md for the full recipe, including the ffmpeg invocation
// that produces the input.
//
//	go run genaudio/main.go tone.ts sample-audio.opus
package main

import (
	"encoding/binary"
	"fmt"
	"os"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/mpegts"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/opus"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: genaudio <in.ts> <out.opus>")
		os.Exit(2)
	}
	ts, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}

	var out []byte
	n := 0
	d := mpegts.NewDemuxer(8<<20, func(mpegts.AU) error { return nil })
	d.OnAudioPacket(func(p mpegts.AudioPacket) {
		toc, ok := opus.ParseTOC(p.Data)
		if !ok {
			panic("packet with no readable TOC")
		}
		if !toc.Stereo || toc.FrameDurationUs != 20_000 || toc.Frames != 1 {
			panic(fmt.Sprintf("packet %d is %+v, want one stereo 20 ms frame", n, toc))
		}
		if len(p.Data) > 0xffff {
			panic("packet does not fit a uint16 length prefix")
		}
		out = binary.BigEndian.AppendUint16(out, uint16(len(p.Data)))
		out = append(out, p.Data...)
		n++
	})
	if _, err := d.Write(ts); err != nil {
		panic(err)
	}
	if err := d.Close(); err != nil {
		panic(err)
	}
	if n == 0 {
		panic("no Opus packets in the input")
	}
	if err := os.WriteFile(os.Args[2], out, 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("%d packets, %d bytes, %d ms\n", n, len(out), n*20)
}
