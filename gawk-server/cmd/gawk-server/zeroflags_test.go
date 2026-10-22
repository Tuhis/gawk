package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// R74 PP2 (docs/76 §6.1 item 1, docs/41 §4.2.1): gawk-relay self-signs, and
// gawk-server must still refuse to start without a certificate. A deployment
// whose certificate Secret failed to mount has to crash, never self-sign, so
// the shared serve package must not have leaked the relay's default here.
func TestZeroFlagsRefuse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := run(ctx, nil, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "no certificate configured") {
		t.Fatalf("run with zero flags = %v, want the no-certificate refusal", err)
	}
}

// The same through the real process: main exits non-zero.
func TestZeroFlagsExitNonZero(t *testing.T) {
	if os.Getenv("GAWK_ZEROFLAGS_CHILD") == "1" {
		// Strip every GAWK_* variable the parent test environment might
		// carry, so this is truly zero configuration.
		for _, kv := range os.Environ() {
			if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "GAWK_") {
				os.Unsetenv(k)
			}
		}
		os.Args = []string{"gawk-server"}
		main()
		return // unreachable when main refuses; reaching here is the failure
	}
	// Bounded: a regression would start a relay that never exits.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestZeroFlagsExitNonZero$")
	cmd.Env = append(os.Environ(), "GAWK_ZEROFLAGS_CHILD=1")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() == 0 {
		t.Fatalf("gawk-server with zero flags: err=%v, output:\n%s", err, out)
	}
	if !strings.Contains(string(out), "no certificate configured") {
		t.Errorf("exit without the refusal message:\n%s", out)
	}
}
