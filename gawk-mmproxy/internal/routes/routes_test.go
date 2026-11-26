package routes

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/Tuhis/gawk/gawk-mmproxy/internal/netnstest"
)

func TestInstallIsIdempotentAndCheckSeesIt(t *testing.T) {
	netnstest.Run(t, func(t *testing.T) {
		st, err := Check(DefaultTable)
		if err != nil {
			t.Fatal(err)
		}
		if (st.V4 || st.V6) && !netnstest.InPlace() { // in place, another package may have installed them
			t.Fatalf("a fresh namespace already has the routes: %+v", st)
		}
		for i := 0; i < 2; i++ {
			for _, r := range Install(DefaultTable) {
				if r.Err != nil {
					t.Fatalf("install %d, %s: %v", i+1, r.Family, r.Err)
				}
			}
		}
		st, err = Check(DefaultTable)
		if err != nil {
			t.Fatal(err)
		}
		if !st.V4 || !st.V6 {
			t.Fatalf("after install: %+v, want both families", st)
		}
		// Exactly one copy of each rule after two installs (F12).
		if out, err := exec.Command("ip", "-4", "rule").CombinedOutput(); err == nil {
			if n := strings.Count(string(out), "lookup 64738"); n != 1 {
				t.Errorf("ip -4 rule shows %d copies of the rule:\n%s", n, out)
			}
			r6, _ := exec.Command("ip", "-6", "rule").CombinedOutput()
			if n := strings.Count(string(r6), "lookup 64738"); n != 1 {
				t.Errorf("ip -6 rule shows %d copies of the rule:\n%s", n, r6)
			}
			rt, _ := exec.Command("ip", "route", "show", "table", "64738").CombinedOutput()
			if !strings.Contains(string(rt), "local default dev lo") {
				t.Errorf("table 64738: %s", rt)
			}
		} else {
			t.Logf("no ip(8) to cross-check: %v", err)
		}
		// Another table is not ours.
		if st, err := Check(4242); err != nil || st.V4 || st.V6 {
			t.Errorf("Check(4242) = %+v, %v", st, err)
		}
	})
}

// A small table (< 256) is carried in the header byte too.
func TestSmallTable(t *testing.T) {
	netnstest.Run(t, func(t *testing.T) {
		for _, r := range Install(100) {
			if r.Err != nil {
				t.Fatalf("%s: %v", r.Family, r.Err)
			}
		}
		if st, err := Check(100); err != nil || !st.V4 || !st.V6 {
			t.Fatalf("Check(100) = %+v, %v", st, err)
		}
	})
}

// Without CAP_NET_ADMIN, Install fails and says so; Check still works.
func TestInstallWithoutCapability(t *testing.T) {
	if netnstest.InChild() {
		return
	}
	st, err := Check(DefaultTable)
	if err != nil {
		t.Fatalf("Check needs no capability: %v", err)
	}
	_ = st
	// Never install into the caller's own namespace when it could succeed:
	// under sudo that would leave routes on the developer's machine.
	if os.Geteuid() == 0 || hasNetAdmin(t) {
		t.Skip("this process can change this namespace's routes; not testing the refusal here")
	}
	failed := false
	for _, r := range Install(4243) {
		if r.Err != nil {
			failed = true
		}
	}
	if !failed {
		t.Fatal("Install succeeded without CAP_NET_ADMIN")
	}
}

// hasNetAdmin reads CAP_NET_ADMIN (bit 12) from this process's effective set.
func hasNetAdmin(t *testing.T) bool {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return true // can't tell: be safe
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "CapEff:"); ok {
			n, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
			return err != nil || n&(1<<12) != 0
		}
	}
	return true
}
