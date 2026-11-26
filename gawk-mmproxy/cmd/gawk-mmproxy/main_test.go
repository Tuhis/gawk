package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	for _, c := range []struct {
		args []string
		code int
		out  string
	}{
		{nil, 2, "usage"},
		{[]string{"nope"}, 2, "usage"},
		{[]string{"version"}, 0, "gawk-mmproxy dev"},
		{[]string{"serve", "-max-conns", "0"}, 2, "invalid -max-conns"},
		{[]string{"routes", "-route-table", "255"}, 2, "invalid -route-table"},
		{[]string{"serve", "-h"}, 0, "-require-routes"},
	} {
		var b bytes.Buffer
		if code := run(c.args, func(string) string { return "" }, &b); code != c.code || !strings.Contains(b.String(), c.out) {
			t.Errorf("%v: %d %q, want %d %q", c.args, code, b.String(), c.code, c.out)
		}
	}
}

// `routes` always exits 0 (docs/92 D4, D15), even when it can't install.
func TestRoutesAlwaysExitsZero(t *testing.T) {
	var b bytes.Buffer
	if code := run([]string{"routes", "-route-table", "4244"}, func(string) string { return "" }, &b); code != 0 {
		t.Fatalf("exit %d: %s", code, b.String())
	}
}
