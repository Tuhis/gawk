package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestSubcommands(t *testing.T) {
	var out bytes.Buffer
	env := func(string) string { return "" }
	if err := run([]string{"version"}, env, &out); err != nil || strings.TrimSpace(out.String()) != version {
		t.Fatalf("version: %q %v", out.String(), err)
	}
	if err := run([]string{"bogus"}, env, &out); err == nil || !strings.Contains(err.Error(), "unknown subcommand") {
		t.Fatalf("bogus: %v", err)
	}
	// The service refuses to start without its required configuration — and
	// says which knob, before touching the model or the network.
	if err := run(nil, env, &out); err == nil || !strings.Contains(err.Error(), "-admin-url") {
		t.Fatalf("run without config: %v", err)
	}
	if err := run([]string{"-mode", "flag"}, env, &out); err == nil {
		t.Fatal("run with only -mode started")
	}
}
