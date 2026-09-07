package main

import (
	"strings"
	"testing"
)

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
