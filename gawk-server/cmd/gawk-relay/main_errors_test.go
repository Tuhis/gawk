package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/relaystate"
)

// runArgs is startRelay's flag set, for a run that is expected to stop on
// its own (an error, or an already-cancelled context).
func runArgs(t *testing.T, extra ...string) []string {
	return append([]string{
		"-addr", fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t)),
		"-metrics-addr", "off",
		"-lan-address", "127.0.0.1",
		"-console-addr", "127.0.0.1:0",
		"-log-level", "error",
	}, extra...)
}

// Every startup step that cannot complete stops the relay with its error;
// none of them falls back to a half-configured relay.
func TestRelayStartupFailures(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	corruptKeys := t.TempDir()
	if err := os.WriteFile(filepath.Join(corruptKeys, relaystate.ResumeTokenKeyFile), []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	blockedCerts := t.TempDir()
	if err := os.WriteFile(filepath.Join(blockedCerts, certsSubdir), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"no state dir and no HOME", nil, map[string]string{}, "pass -state-dir"},
		{"state dir under a file", nil, map[string]string{"GAWK_STATE_DIR": filepath.Join(notADir, "state")}, "relaystate"},
		{"corrupt persisted key", nil, map[string]string{"GAWK_STATE_DIR": corruptKeys}, "delete it to generate a new one"},
		{"unreadable explicit certificate", []string{"-cert-file", "/nonexistent/tls.crt", "-key-file", "/nonexistent/tls.key"},
			map[string]string{"GAWK_STATE_DIR": t.TempDir()}, "load certificate"},
		{"rolling certificate dir blocked", nil, map[string]string{"GAWK_STATE_DIR": blockedCerts}, "tlsutil"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- run(ctx, runArgs(t, tc.args...), envOf(tc.env)) }()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("run = %v, want an error containing %q", err, tc.want)
				}
			case <-time.After(10 * time.Second):
				cancel()
				<-done
				t.Fatal("the relay started despite the broken step")
			}
		})
	}
}

// -dev-cert is ignored: gawk-relay still serves its rolling pinned
// certificate, so a cancelled start leaves the pinned pair behind.
func TestRelayIgnoresDevCert(t *testing.T) {
	stateDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, runArgs(t, "-dev-cert"), envOf(map[string]string{"GAWK_STATE_DIR": stateDir})); err != nil {
		t.Fatalf("run = %v", err)
	}
	if n := len(pinnedHashes(t, stateDir)); n != 2 {
		t.Errorf("%d pinned certificates with -dev-cert, want the rolling pair (2)", n)
	}
}
