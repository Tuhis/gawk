package main

/*
#cgo pkg-config: libpipewire-0.3
#include "shim.h"
#include <stdlib.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/pwgraph"
)

// proxyPtr is a handle to an object we asked the daemon to create. It is a
// type alias so the rest of the package — which must not import "C" — can hold
// one without knowing what it is.
type proxyPtr = *C.struct_pw_proxy

// conn is a live connection to the PipeWire daemon.
//
// Everything the daemon tells us arrives through registry callbacks on the
// loop thread and is enqueued; everything we tell the daemon goes out under
// the loop lock. The two never overlap, which is the only concurrency rule
// this file has.
type conn struct {
	pw *C.struct_gawk_pw

	mu sync.Mutex
	// order preserves the interleaving of adds and removes. PipeWire reuses
	// ids, so "add 33, remove 33, add 33" collapsed into two lists in the
	// wrong order is a graph that thinks a dead node is alive.
	order []queued

	wake chan struct{}
	done chan int
	// fatal carries a core error. Buffered so the loop thread never blocks on
	// a Go reader that has already gone away.
	fatal chan error
}

// queued is one registry change, in arrival order.
type queued struct {
	// kind is what to do with it.
	op     queuedOp
	global pwgraph.Global
	id     uint32
	props  map[string]string
}

type queuedOp int

const (
	queuedAdd queuedOp = iota
	queuedRemove
	// queuedMerge is a bound object's fuller property list arriving behind its
	// registry global (see pwgraph.Merge).
	queuedMerge
)

// active is the one live connection. libpipewire's callbacks carry a void*
// user pointer, but cgo forbids passing a Go pointer into C and holding it, so
// the helper — which is a single-connection process by construction — resolves
// callbacks through this instead of through a handle table.
var (
	activeMu sync.Mutex
	active   *conn
)

func connect() (*conn, error) {
	var cerr *C.char
	pw := C.gawk_pw_new(&cerr)
	if pw == nil {
		msg := "could not connect to PipeWire"
		if cerr != nil {
			msg = C.GoString(cerr)
			C.free(unsafe.Pointer(cerr))
		}
		return nil, errors.New(msg)
	}
	c := &conn{
		pw:    pw,
		wake:  make(chan struct{}, 1),
		done:  make(chan int, 64),
		fatal: make(chan error, 4),
	}
	activeMu.Lock()
	active = c
	activeMu.Unlock()
	return c, nil
}

func (c *conn) close() {
	activeMu.Lock()
	active = nil
	activeMu.Unlock()
	// Stops the loop and drops every proxy: the sink and all links go with it.
	C.gawk_pw_free(c.pw)
	c.pw = nil
}

func libraryVersion() string { return C.GoString(C.gawk_pw_version()) }

// Wake is closed-on-change signalling: one buffered slot, so a burst of a
// hundred registry events costs one wakeup rather than a hundred.
func (c *conn) Wake() <-chan struct{} { return c.wake }

// Fatal reports a core-level error from the daemon.
func (c *conn) Fatal() <-chan error { return c.fatal }

func (c *conn) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// drain hands over everything the loop thread has queued since the last call,
// in arrival order.
func (c *conn) drain() []queued {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.order) == 0 {
		return nil
	}
	out := c.order
	c.order = nil
	return out
}

// roundtrip waits for the daemon to have processed everything we have sent.
//
// It is what turns "we asked for a sink" into "the sink exists and its global
// has been announced", which is the difference between a helper that works and
// one that races the daemon on every start.
func (c *conn) roundtrip(timeout time.Duration) error {
	C.gawk_pw_lock(c.pw)
	seq := int(C.gawk_pw_sync(c.pw))
	C.gawk_pw_unlock(c.pw)

	deadline := time.After(timeout)
	for {
		select {
		case got := <-c.done:
			// Sequence numbers advance; anything at or past ours means our
			// request has been processed.
			if got >= seq {
				return nil
			}
		case err := <-c.fatal:
			return err
		case <-deadline:
			return fmt.Errorf("timed out waiting for PipeWire (%s)", timeout)
		}
	}
}

// createSink asks the daemon for the capture sink. The proxy comes back
// immediately; the *node* appears in the registry a round-trip later, which is
// how the caller learns its id and serial.
func (c *conn) createSink(name, desc, positions string, channels int) (proxyPtr, error) {
	cname, cdesc, cpos := C.CString(name), C.CString(desc), C.CString(positions)
	defer func() {
		C.free(unsafe.Pointer(cname))
		C.free(unsafe.Pointer(cdesc))
		C.free(unsafe.Pointer(cpos))
	}()

	C.gawk_pw_lock(c.pw)
	proxy := C.gawk_pw_create_sink(c.pw, cname, cdesc, cpos, C.int(channels))
	C.gawk_pw_unlock(c.pw)
	if proxy == nil {
		return nil, errors.New("PipeWire refused to create the capture sink")
	}
	return proxy, nil
}

func (c *conn) createLink(l pwgraph.Link) (proxyPtr, error) {
	C.gawk_pw_lock(c.pw)
	proxy := C.gawk_pw_create_link(c.pw,
		C.uint32_t(l.OutNode), C.uint32_t(l.OutPort),
		C.uint32_t(l.InNode), C.uint32_t(l.InPort))
	C.gawk_pw_unlock(c.pw)
	if proxy == nil {
		return nil, fmt.Errorf("PipeWire refused to link port %d into port %d", l.OutPort, l.InPort)
	}
	return proxy, nil
}

func (c *conn) destroy(proxy proxyPtr) {
	if proxy == nil {
		return
	}
	C.gawk_pw_lock(c.pw)
	C.gawk_pw_destroy_proxy(proxy)
	C.gawk_pw_unlock(c.pw)
}

//export gawkOnGlobal
func gawkOnGlobal(id C.uint, kind C.int, props *C.struct_spa_dict) {
	c := currentConn()
	if c == nil {
		return
	}
	m := dictToMap(props)
	c.mu.Lock()
	c.order = append(c.order, queued{
		op:     queuedAdd,
		global: pwgraph.Global{ID: uint32(id), Kind: pwgraph.Kind(kind), Props: m},
	})
	c.mu.Unlock()
	c.signal()
}

//export gawkOnObjectProps
func gawkOnObjectProps(id C.uint, kind C.int, props *C.struct_spa_dict) {
	c := currentConn()
	if c == nil {
		return
	}
	c.mu.Lock()
	c.order = append(c.order, queued{op: queuedMerge, id: uint32(id), props: dictToMap(props)})
	c.mu.Unlock()
	c.signal()
}

// dictToMap copies a spa_dict into Go memory. The dict is only valid for the
// duration of the callback, so this is a copy rather than a view.
func dictToMap(props *C.struct_spa_dict) map[string]string {
	n := int(C.gawk_dict_n(props))
	m := make(map[string]string, n)
	for i := 0; i < n; i++ {
		k := C.gawk_dict_key(props, C.uint(i))
		v := C.gawk_dict_value(props, C.uint(i))
		if k == nil || v == nil {
			continue
		}
		m[C.GoString(k)] = C.GoString(v)
	}
	return m
}

//export gawkOnGlobalRemove
func gawkOnGlobalRemove(id C.uint) {
	c := currentConn()
	if c == nil {
		return
	}
	c.mu.Lock()
	c.order = append(c.order, queued{op: queuedRemove, id: uint32(id)})
	c.mu.Unlock()
	c.signal()
}

//export gawkOnCoreDone
func gawkOnCoreDone(seq C.int) {
	c := currentConn()
	if c == nil {
		return
	}
	select {
	case c.done <- int(seq):
	default:
		// A round-trip nobody is waiting for. Dropping it is correct: the
		// waiter reads the *next* one, and every sync we send has a reader.
	}
}

//export gawkOnCoreError
func gawkOnCoreError(id C.uint, res C.int, message *C.char) {
	c := currentConn()
	if c == nil {
		return
	}
	msg := ""
	if message != nil {
		msg = C.GoString(message)
	}
	// Errors against objects other than the core are per-object failures — a
	// link the daemon refused, most often — and must not tear the helper down:
	// the plan is recomputed on the next registry change anyway.
	if uint32(id) != 0 {
		return
	}
	select {
	case c.fatal <- fmt.Errorf("PipeWire error %d: %s", int(res), msg):
	default:
	}
}

func currentConn() *conn {
	activeMu.Lock()
	defer activeMu.Unlock()
	return active
}
