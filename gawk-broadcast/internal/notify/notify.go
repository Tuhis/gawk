// Package notify sends desktop notifications over D-Bus, with urgency
// (R14 Decision 17, docs/19).
//
// Urgency is load-bearing here, not decoration, and the reason is a trap the
// design walked into before it was reviewed: **KDE's portal inhibits
// notifications while a ScreenCast session is active**
// (xdg-desktop-portal-kde MR !33), and Plasma can auto-enable do-not-disturb
// for fullscreen apps. So on KDE, the act of broadcasting suppressed the only
// signal a fullscreen broadcaster could ever receive — by default.
//
// The freedesktop spec's **critical** urgency bypasses DND on Plasma and is
// shown over fullscreen on GNOME. Hence the split:
//
//   - normal:   went live, frames flowing. Fine if they are swallowed mid-game.
//   - critical: the child died, publishing failed, the broadcast ended
//     unexpectedly. A child-process death you do not learn about for an hour is
//     precisely the failure mode notifications exist to prevent.
//
// gioui.org/x/notify is rejected for this: experimental *and* no urgency
// control. The D-Bus call is a page of code behind the same interface, and
// godbus is already a dependency for the portal — so the dependency delta of
// doing it properly is zero.
package notify

import (
	"github.com/godbus/dbus/v5"
)

// Urgency levels from the freedesktop notification spec.
type Urgency byte

const (
	// UrgencyLow is unused today; here so the hint's meaning is legible.
	UrgencyLow Urgency = 0
	// UrgencyNormal may be swallowed while screen casting on KDE. Use it for
	// things that do not matter mid-game.
	UrgencyNormal Urgency = 1
	// UrgencyCritical bypasses do-not-disturb on Plasma and shows over
	// fullscreen on GNOME. Use it only for things that end the broadcast.
	UrgencyCritical Urgency = 2
)

const (
	busName    = "org.freedesktop.Notifications"
	objectPath = "/org/freedesktop/Notifications"
	iface      = "org.freedesktop.Notifications"

	// appName is what the notification is attributed to.
	appName = "gawk-broadcast"
	// icon is a stock freedesktop icon name; no assets to ship.
	icon = "video-display"
)

// Notifier sends desktop notifications. One method, so the transport is
// swappable without touching call sites (V6's criterion) and so a GUI test
// does not need a session bus.
type Notifier interface {
	Notify(summary, body string, urgency Urgency)
	Close()
}

// dbusNotifier is the real one.
type dbusNotifier struct {
	conn *dbus.Conn
	obj  dbus.BusObject
	// id lets us replace our own previous notification rather than stack them
	// up: a broadcaster who starts and stops five times should not accumulate
	// five popups.
	id uint32
}

// New connects to the session bus. It returns a no-op Notifier rather than an
// error when there is no bus: a missing notification daemon must never stop a
// broadcast, and the CLI has no use for popups at all.
func New() Notifier {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return Discard{}
	}
	return &dbusNotifier{conn: conn, obj: conn.Object(busName, objectPath)}
}

func (n *dbusNotifier) Notify(summary, body string, urgency Urgency) {
	hints := map[string]dbus.Variant{
		"urgency": dbus.MakeVariant(byte(urgency)),
	}
	// A critical notification stays until it is dismissed; a normal one uses
	// the desktop's default timeout. Expiry -1 = default, 0 = never.
	expire := int32(-1)
	if urgency == UrgencyCritical {
		expire = 0
	}
	call := n.obj.Call(iface+".Notify", 0,
		appName,
		n.id, // replaces_id
		icon,
		summary,
		body,
		[]string{}, // actions
		hints,
		expire,
	)
	if call.Err != nil {
		return // a failed popup is never worth surfacing
	}
	var id uint32
	if err := call.Store(&id); err == nil {
		n.id = id
	}
}

func (n *dbusNotifier) Close() {
	if n.conn != nil {
		_ = n.conn.Close()
		n.conn = nil
	}
}

// Discard drops notifications. It is what a machine with no notification
// daemon gets, and what tests use.
type Discard struct{}

func (Discard) Notify(string, string, Urgency) {}
func (Discard) Close()                         {}
