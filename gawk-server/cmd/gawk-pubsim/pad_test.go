package main

import (
	"bytes"
	"testing"

	"github.com/Tuhis/gawk/gawk-server/publisher"
)

// R87 SG0: padding grows each AU to its target with one filler NAL and leaves
// the original bytes, the keyframe flag and short-enough targets untouched.
func TestPadAUs(t *testing.T) {
	kf := publisher.AccessUnit{Data: []byte{0, 0, 0, 1, 0x65, 1, 2}, Keyframe: true}
	delta := publisher.AccessUnit{Data: []byte{0, 0, 0, 1, 0x41, 3}}
	tests := []struct {
		name              string
		kfBytes, dBytes   int
		wantKf, wantDelta int
	}{
		{"off", 0, 0, len(kf.Data), len(delta.Data)},
		{"both padded", 100, 50, 100, 50},
		{"target below the AU", 3, 3, len(kf.Data), len(delta.Data)},
		{"keyframes only", 100, 0, 100, len(delta.Data)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := padAUs([]publisher.AccessUnit{kf, delta}, tc.kfBytes, tc.dBytes)
			for i, want := range []int{tc.wantKf, tc.wantDelta} {
				orig := []publisher.AccessUnit{kf, delta}[i]
				got := out[i]
				if len(got.Data) != want {
					t.Errorf("AU %d: %d bytes, want %d", i, len(got.Data), want)
				}
				if got.Keyframe != orig.Keyframe || !bytes.HasPrefix(got.Data, orig.Data) {
					t.Errorf("AU %d: original bytes or keyframe flag changed", i)
				}
				if len(got.Data) > len(orig.Data) {
					filler := got.Data[len(orig.Data):]
					if !bytes.HasPrefix(filler, []byte{0, 0, 0, 1, 0x0C}) || filler[len(filler)-1] != 0x80 {
						t.Errorf("AU %d: padding is not one filler NAL ending in the stop bit", i)
					}
				}
			}
		})
	}
}
