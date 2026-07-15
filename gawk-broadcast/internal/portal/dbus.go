package portal

import (
	"context"
	"fmt"
	"os"

	"github.com/godbus/dbus/v5"
)

// dbusCaller is the production Caller: a real session-bus connection.
type dbusCaller struct {
	conn    *dbus.Conn
	obj     dbus.BusObject
	signals chan *dbus.Signal
}

func dialSessionBus() (Caller, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("%w: cannot reach the session bus: %v", ErrUnavailable, err)
	}
	c := &dbusCaller{
		conn: conn,
		obj:  conn.Object(busName, objectPath),
		// Buffered: responses are rare, but a blocked signal channel makes
		// godbus drop messages, and the one we drop would be the one we want.
		signals: make(chan *dbus.Signal, 16),
	}
	conn.Signal(c.signals)
	return c, nil
}

func (c *dbusCaller) Close() {
	if c.conn != nil {
		c.conn.RemoveSignal(c.signals)
		_ = c.conn.Close()
		c.conn = nil
	}
}

func (c *dbusCaller) ScreenCastVersion(ctx context.Context) (uint32, error) {
	v, err := c.obj.GetProperty(scIface + ".version")
	if err != nil {
		return 0, err
	}
	version, ok := v.Value().(uint32)
	if !ok {
		return 0, fmt.Errorf("portal: version property has unexpected type %T", v.Value())
	}
	return version, nil
}

// Call invokes a ScreenCast method and waits for its Request response.
//
// The subscribe-before-call ordering is load-bearing: the portal answers on a
// signal to an object path derived from our handle_token, and a fast portal
// (or a restored grant, which needs no user interaction at all) can respond
// before the method call even returns. Subscribing afterwards would miss it
// and hang until the context expired.
func (c *dbusCaller) Call(ctx context.Context, method string, opts map[string]dbus.Variant, args ...any) (uint32, map[string]dbus.Variant, error) {
	token, ok := opts["handle_token"].Value().(string)
	if !ok || token == "" {
		token = newToken("gawk_req")
		opts["handle_token"] = dbus.MakeVariant(token)
	}
	want := requestPath(c.conn.Names()[0], token)

	if call := c.conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0,
		fmt.Sprintf("type='signal',interface='org.freedesktop.portal.Request',member='Response',path='%s'", want),
	); call.Err != nil {
		return 0, nil, fmt.Errorf("portal: cannot watch for the response: %w", call.Err)
	}

	callArgs := append(append([]any{}, args...), opts)
	var handle dbus.ObjectPath
	if err := c.obj.CallWithContext(ctx, scIface+"."+method, 0, callArgs...).Store(&handle); err != nil {
		return 0, nil, err
	}
	// The portal is allowed to choose a different Request path than the one we
	// predicted. If it did, watch that one instead — the prediction is an
	// optimization for ordering, not a contract.
	if handle != want {
		if call := c.conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0,
			fmt.Sprintf("type='signal',interface='org.freedesktop.portal.Request',member='Response',path='%s'", handle),
		); call.Err != nil {
			return 0, nil, fmt.Errorf("portal: cannot watch for the response: %w", call.Err)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		case sig := <-c.signals:
			if sig == nil || sig.Name != "org.freedesktop.portal.Request.Response" {
				continue
			}
			if sig.Path != handle && sig.Path != want {
				continue // another request's response
			}
			if len(sig.Body) < 2 {
				return 0, nil, fmt.Errorf("portal: malformed Response signal")
			}
			resp, ok := sig.Body[0].(uint32)
			if !ok {
				return 0, nil, fmt.Errorf("portal: Response code has unexpected type %T", sig.Body[0])
			}
			results, ok := sig.Body[1].(map[string]dbus.Variant)
			if !ok {
				results = map[string]dbus.Variant{}
			}
			return resp, results, nil
		}
	}
}

func (c *dbusCaller) OpenPipeWireRemote(ctx context.Context, session dbus.ObjectPath) (*os.File, error) {
	var fd dbus.UnixFD
	err := c.obj.CallWithContext(ctx, scIface+".OpenPipeWireRemote", 0,
		session, map[string]dbus.Variant{}).Store(&fd)
	if err != nil {
		return nil, err
	}
	// godbus hands over an already-dup'd descriptor; wrapping it in an os.File
	// gives it an owner and a finalizer.
	return os.NewFile(uintptr(fd), "pipewire-remote"), nil
}
