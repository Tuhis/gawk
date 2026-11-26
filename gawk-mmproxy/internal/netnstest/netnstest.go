// Package netnstest runs a test inside a fresh network namespace, so the
// code that installs policy routes, binds spoofed source addresses and
// lists sockets runs against a real kernel without touching the host.
//
// The test binary re-executes itself in a new user and network namespace
// (unshare(CLONE_NEWUSER|CLONE_NEWNET), mapped to root inside), where the
// test holds CAP_NET_ADMIN and CAP_NET_RAW over that namespace only. Where
// the kernel or a sandbox refuses unprivileged user namespaces, Run skips,
// unless GAWK_MMPROXY_REQUIRE_NETNS is set (CI sets it, so a runner that
// can't run them fails instead of passing vacuously).
package netnstest

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

const childEnv = "GAWK_MMPROXY_NETNS_CHILD"

// InChild reports whether this process is the namespaced re-execution.
func InChild() bool { return os.Getenv(childEnv) != "" }

// InPlace reports GAWK_MMPROXY_NETNS_INPLACE: the whole process already
// runs in a disposable network namespace with CAP_NET_ADMIN (CI's coverage
// container), so fn runs in this process, where its coverage is measured.
// Packages then share that namespace: run them with -p 1, and never set it
// on a machine whose namespace you care about.
func InPlace() bool { return os.Getenv("GAWK_MMPROXY_NETNS_INPLACE") != "" }

// Run runs fn in a fresh network namespace with loopback up. In the parent
// it re-executes the test binary for t's test alone and fails t if the
// child does; fn runs only in the child. With InPlace, fn runs here.
func Run(t *testing.T, fn func(t *testing.T)) {
	t.Helper()
	if InPlace() {
		if err := LoopbackUp(); err != nil {
			t.Fatalf("loopback up: %v", err)
		}
		fn(t)
		return
	}
	if os.Getenv(childEnv) == t.Name() {
		if err := LoopbackUp(); err != nil {
			t.Fatalf("loopback up: %v", err)
		}
		fn(t)
		return
	}
	if InChild() {
		return // another test's child: not ours to run
	}
	cmd := exec.Command(os.Args[0], "-test.run", "^"+strings.ReplaceAll(t.Name(), "/", "$/^")+"$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), childEnv+"="+t.Name())
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		if os.Getenv("GAWK_MMPROXY_REQUIRE_NETNS") != "" {
			t.Fatalf("can't create a user and network namespace (GAWK_MMPROXY_REQUIRE_NETNS is set): %v", err)
		}
		t.Skipf("no unprivileged user namespaces here: %v", err)
	}
	err := cmd.Wait()
	if s := out.String(); testing.Verbose() || err != nil {
		t.Log("namespaced run:\n" + s)
	}
	if err != nil {
		t.Fatalf("the test failed in its namespace: %v", err)
	}
	if !strings.Contains(out.String(), "--- PASS: "+t.Name()) && !strings.Contains(out.String(), "PASS") {
		t.Fatal("the namespaced run reported no PASS")
	}
}

// LoopbackUp sets lo up in the current network namespace.
func LoopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return fmt.Errorf("SIOCGIFFLAGS: %w", err)
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		return fmt.Errorf("SIOCSIFFLAGS: %w", err)
	}
	return nil
}
