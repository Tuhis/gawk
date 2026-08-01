// Test support for *other* packages.
//
// cmd/gawk-broadcast-gui's layout tests have to put the window into states this
// package owns — R35's whose-audio card most of all, since it is a whole set of
// widgets that only exists mid-start. A `_test.go` file cannot help them: it is
// invisible across a package boundary. So these two doors are ordinary code,
// kept deliberately thin — the card's own rules are tested inside this package,
// and nothing here is logic.
package app

import (
	"context"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/engine"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/gst"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/pwproto"
)

// The two helpers below exist for cmd/gawk-broadcast-gui's layout tests, which
// live in another package and must be able to put the window into the
// whose-audio state (R35 AS5). They are deliberately thin: the card's own
// logic is tested inside this package, and this is only the door.

// AppAudioOfferForTest builds an offer without the caller importing internal/gst.
func AppAudioOfferForTest(apps []pwproto.App, err error) gst.AppAudioOffer {
	return gst.AppAudioOffer{Apps: apps, Err: err}
}

// ChooseAudioTargetForTest opens the whose-audio card and blocks, exactly as
// the engine does.
func (a *App) ChooseAudioTargetForTest(offer gst.AppAudioOffer) engine.AudioTarget {
	return a.chooseAudioTarget(context.Background(), offer)
}
