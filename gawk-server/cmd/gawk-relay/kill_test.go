package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"
)

// buildRelay compiles this binary once per test run.
func buildRelay(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and runs the gawk-relay binary")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH")
	}
	exe := filepath.Join(t.TempDir(), "gawk-relay")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	cmd := exec.Command(goBin, "build", "-o", exe, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return exe
}

// startProcess starts the binary with a state directory and a loopback port.
func startProcess(t *testing.T, exe, stateDir string, port int) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(exe,
		"-addr", fmt.Sprintf("127.0.0.1:%d", port),
		"-metrics-addr", "off",
		"-lan-address", "127.0.0.1",
		"-console-addr", "127.0.0.1:0",
		"-log-level", "warn")
	cmd.Env = append(os.Environ(), "GAWK_STATE_DIR="+stateDir)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

func kill9(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	// SIGKILL on Unix, TerminateProcess on Windows: no CONNECTION_CLOSE
	// leaves the process, exactly as a crash.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
}

// probeUntilDead sends datagrams on sess until it dies or the window ends;
// it returns the error and how long after the first probe it arrived.
//
// The probe is deliberately large: quic-go answers an unknown connection ID
// with a stateless reset only when the triggering packet is longer than the
// smallest reset it can send (42 bytes), so that a reset can never amplify;
// a five-byte datagram's packet is below that and draws nothing.
func probeUntilDead(ctx context.Context, sess *webtransport.Session, window time.Duration) (error, time.Duration) {
	probe := make([]byte, 600)
	start := time.Now()
	for time.Since(start) < window {
		if err := sess.SendDatagram(probe); err != nil {
			return err, time.Since(start)
		}
		rctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		_, err := sess.ReceiveDatagram(rctx)
		cancel()
		if err != nil && rctx.Err() == nil {
			return err, time.Since(start)
		}
	}
	return nil, time.Since(start)
}

// docs/76 PP2: after a kill -9 and a restart, a connected client gets a
// stateless reset within about one RTT instead of waiting out the idle
// timeout — because the stateless-reset key is persisted in the state
// directory (gawk-server leaves it off by default). On loopback one RTT is
// well under a millisecond, so the bound below is generous scheduling slack,
// not a measurement; the control proves the reset comes from the persisted
// key and not from something else killing the session.
func TestRelayKill9RestartSendsStatelessReset(t *testing.T) {
	exe := buildRelay(t)
	for _, tc := range []struct {
		name      string
		sameState bool
	}{
		{"same state directory: reset", true},
		{"fresh state directory: no reset (control)", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			stateDir := t.TempDir()
			port := freeUDPPort(t)

			first := startProcess(t, exe, stateDir, port)
			d := dialer(t, pinnedTLS(t, stateDir))
			sess := dialRetry(t, ctx, d, fmt.Sprintf("https://127.0.0.1:%d/echo", port))
			if err := sess.SendDatagram([]byte("ping")); err != nil {
				t.Fatal(err)
			}
			rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
			if _, err := sess.ReceiveDatagram(rctx); err != nil {
				rcancel()
				t.Fatalf("echo before the kill: %v", err)
			}
			rcancel()

			kill9(t, first)
			restartDir := stateDir
			if !tc.sameState {
				restartDir = t.TempDir()
			}
			startProcess(t, exe, restartDir, port)
			// Wait until the new process answers a fresh dial, so the probe
			// window below measures the reset and not the restart.
			fresh := dialRetry(t, ctx, dialer(t, pinnedTLS(t, restartDir)), fmt.Sprintf("https://127.0.0.1:%d/echo", port))
			fresh.CloseWithError(0, "")

			err, after := probeUntilDead(ctx, sess, 2*time.Second)
			if !tc.sameState {
				if err != nil {
					t.Fatalf("session died (%v) with a different reset key; the control is broken", err)
				}
				return
			}
			if err == nil {
				t.Fatal("no stateless reset after a kill -9 and a restart on the same state directory")
			}
			// The type does not always survive webtransport-go's session
			// teardown (the CONNECT stream's cancellation can surface first),
			// so the control, not the error's type, is what ties the death to
			// the reset key: a relay with another key leaves the same session
			// alive through the same probes.
			var reset *quic.StatelessResetError
			if errors.As(err, &reset) || strings.Contains(err.Error(), "stateless reset") {
				t.Logf("session error is the stateless reset itself: %v", err)
			} else {
				t.Logf("session error (teardown surfaced first): %v", err)
			}
			if after > 500*time.Millisecond {
				t.Errorf("stateless reset after %v, want within a few RTTs on loopback", after)
			}
			// A near-zero value means the session had already been reset by
			// an earlier client packet (an ACK or retransmission while the
			// relay restarted), which is the same mechanism, only sooner.
			t.Logf("session ended by a stateless reset %v after the first probe", after)
		})
	}
}
