package bridge

import "time"

// Callback states (docs/89 D15, D16): /statusz's ice.callbacks.
const (
	cbRegistered  = "registered"
	cbUnreachable = "unreachable"
	cbProbing     = "probing"
	cbUnsafe      = "unsafe"
)

// cbMaxUnanswered is D15's bound: a pod never holds more than two
// registrations murmur has not answered with an event, so each loss of
// reachability costs murmur at most two F15 freezes.
const cbMaxUnanswered = 2

// cbAction is what a transition asks of murmur.
type cbAction struct {
	add    bool // Server.addCallback
	remove bool // Server.removeCallback
}

// callbackFSM is D15's per-process state machine for the pod's one
// ServerCallback registration. It is pure: the caller feeds it connects,
// events, drifting reconciles and the clock, and performs the actions it
// returns. The state, the count and the backoff live for the process; only
// an event clears them.
type callbackFSM struct {
	state string
	// unanswered counts the registration that delivered the last event (if
	// any) plus every addCallback call since.
	unanswered int
	// backoff is the wait before the next probe; base and max bound it.
	backoff, base, max time.Duration
	probeAt            time.Time
	// notRun counts probes in a row a reconnect ended before they could
	// answer; two count as one failed probe.
	notRun int
}

func newCallbackFSM(probe time.Duration) *callbackFSM {
	return &callbackFSM{state: cbRegistered, backoff: probe, base: probe, max: 2 * time.Hour}
}

// add is D15's one rule for every addCallback call site except a probe:
// only while fewer than two registrations are unanswered.
func (f *callbackFSM) add() cbAction {
	if f.unanswered >= cbMaxUnanswered {
		return cbAction{}
	}
	f.unanswered++
	return cbAction{add: true}
}

// onConnect is a connect or a reconnect of the Ice client, after
// getVersion (D11, D16).
func (f *callbackFSM) onConnect(unsafe bool, now time.Time) cbAction {
	if unsafe {
		f.state = cbUnsafe
		return cbAction{}
	}
	if f.state == cbUnsafe {
		// An upgraded murmur: start as a fresh process would.
		f.state, f.unanswered = cbRegistered, 0
	}
	switch f.state {
	case cbRegistered:
		return f.add()
	case cbProbing:
		// A reconnect ends the probe as not run; two in a row count as
		// one failed probe.
		f.state = cbUnreachable
		f.unanswered = min(f.unanswered, cbMaxUnanswered) // D12: 3 only during a probe
		f.notRun++
		if f.notRun >= 2 {
			f.notRun = 0
			f.backoff = min(2*f.backoff, f.max)
		}
		f.probeAt = now.Add(f.backoff)
		return cbAction{remove: true}
	}
	return cbAction{} // unreachable: no addCallback at all
}

// onEvent is any event delivered to the endpoint: the registration that
// delivered it is live in murmur.
func (f *callbackFSM) onEvent() {
	if f.state == cbUnsafe {
		return
	}
	f.state, f.unanswered = cbRegistered, 1
	f.backoff, f.notRun = f.base, 0
}

// onDrift is a periodic reconcile whose read differed from the mirror: the
// only sign murmur dropped the registration (F13). It reports whether the
// pod just gave up on callbacks.
func (f *callbackFSM) onDrift(now time.Time) (cbAction, bool) {
	switch f.state {
	case cbRegistered:
		if a := f.add(); a.add {
			return a, false
		}
		f.state = cbUnreachable
		f.probeAt = now.Add(f.backoff)
		return cbAction{remove: true}, true
	case cbProbing:
		// The probe's registration drifted with no event: failed.
		f.state = cbUnreachable
		f.unanswered = min(f.unanswered, cbMaxUnanswered) // D12: 3 only during a probe
		f.notRun = 0
		f.backoff = min(2*f.backoff, f.max)
		f.probeAt = now.Add(f.backoff)
		return cbAction{remove: true}, false
	}
	return cbAction{}, false
}

// probeDue is when the next probe is due, if the pod is unreachable.
func (f *callbackFSM) probeDue() (time.Time, bool) {
	return f.probeAt, f.state == cbUnreachable
}

// onProbe starts a probe once it is due: exactly one addCallback, whatever
// the count, which it increments.
func (f *callbackFSM) onProbe(now time.Time) cbAction {
	if f.state != cbUnreachable || now.Before(f.probeAt) {
		return cbAction{}
	}
	f.state = cbProbing
	f.unanswered++
	return cbAction{add: true}
}
