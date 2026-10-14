package main

import (
	"strings"
	"testing"

	"github.com/Tuhis/gawk/gawk-server/internal/pubsim"
)

// Every -fixture name resolves to a loop that starts on a keyframe, and the
// R40 magenta loop is the two long phases (docs/71 CF8).
func TestFixtureAUs(t *testing.T) {
	for _, name := range []string{"default", "large", "magenta"} {
		aus, err := fixtureAUs(name)
		if err != nil {
			t.Fatalf("fixtureAUs(%q): %v", name, err)
		}
		if len(aus) == 0 || !aus[0].Keyframe {
			t.Fatalf("fixtureAUs(%q) does not start with a keyframe", name)
		}
	}
	if aus, _ := fixtureAUs("magenta"); len(aus) != 2*pubsim.MagentaPhaseFrames {
		t.Fatalf("magenta loop is %d AUs, want %d", len(aus), 2*pubsim.MagentaPhaseFrames)
	}
	if _, err := fixtureAUs("nope"); err == nil {
		t.Fatal("an unknown fixture was accepted")
	}
}

// The -room-new stdout contract the browser E2E scrapes: one line,
// `ROOM <CODE> <creatorTokenHex>`, code upper-case, newline-terminated.
func TestRoomLineFormat(t *testing.T) {
	tok := strings.Repeat("ab", 16)
	got := roomLine("ab2cd3", tok)
	if got != "ROOM AB2CD3 "+tok+"\n" {
		t.Errorf("roomLine = %q", got)
	}
	fields := strings.Fields(got)
	if len(fields) != 3 || fields[0] != "ROOM" || len(fields[2]) != 32 {
		t.Errorf("roomLine fields = %q, want ROOM <CODE> <32 hex>", fields)
	}
}
