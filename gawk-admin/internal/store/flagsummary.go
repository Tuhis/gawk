package store

import (
	"fmt"
	"time"

	"github.com/Tuhis/gawk/gawk-server/events"
)

// The sentences of the R40 events (docs/71 D16). They name the broadcast by its
// raw ID, as every summary does since docs/52 D9, and never anything a
// delivery does not already carry.

// SummarizeContentFlagRaised is the page: what tripped, and what to do.
func SummarizeContentFlagRaised(broadcastID string, flagged, samples int, peak float64, returned bool) string {
	what := broadcastName(broadcastID)
	if returned {
		// docs/72 D4: said first, because it changes what the page asks for.
		what += ", which returned after review"
	}
	return fmt.Sprintf("content flag raised on %s: %d of %d samples flagged, peak score %.2f — join and judge it",
		what, flagged, samples, peak)
}

// SummarizeContentFlagObserved is the calibration notice (D19).
func SummarizeContentFlagObserved(broadcastID string, flagged, samples int, peak float64) string {
	return fmt.Sprintf("observe mode would have flagged %s: %d of %d samples, peak score %.2f",
		broadcastName(broadcastID), flagged, samples, peak)
}

// SummarizeContentFlagReminded is the re-page (D9). The signal is spelled out
// because "the sampler has gone quiet" asks for a different response from
// "it is still on screen".
func SummarizeContentFlagReminded(broadcastID string, reminder int, age time.Duration, signal string) string {
	state := "the signal is still live"
	if signal == events.ContentFlagSignalUnknown {
		state = "the sampler has gone quiet"
	}
	return fmt.Sprintf("content flag on %s is still unacknowledged after %s (reminder %d; %s)",
		broadcastName(broadcastID), age.Round(time.Minute), reminder, state)
}

// SummarizeContentFlagResolved is how a flag ended.
func SummarizeContentFlagResolved(broadcastID, resolution, reason, actor string) string {
	what := "content flag on " + broadcastName(broadcastID)
	switch resolution {
	case events.ContentFlagResolutionCleared:
		return what + " resolved: the signal cleared"
	case events.ContentFlagResolutionDismissed:
		return what + " was dismissed by " + actorOrOperator(actor)
	case events.ContentFlagResolutionKilled:
		return what + " resolved: the broadcast was killed by " + actorOrOperator(actor)
	case events.ContentFlagResolutionAutoKilled:
		if reason != "" {
			return what + " resolved: killed automatically (" + reason + ")"
		}
		return what + " resolved: killed automatically"
	case events.ContentFlagResolutionEnded:
		return what + " resolved: the broadcast ended"
	default:
		return what + " resolved"
	}
}

// SummarizeRoomScreening is room.screening_changed's sentence (D11).
func SummarizeRoomScreening(kind, name, actor string, exempt bool) string {
	what := "a room"
	switch {
	case kind != "" && name != "":
		what = kind + " room " + name
	case name != "":
		what = "room " + name
	}
	if exempt {
		return what + " was exempted from content screening by " + actorOrOperator(actor)
	}
	return what + " is screened again, by " + actorOrOperator(actor)
}

func broadcastName(id string) string {
	if id == "" {
		return "a broadcast"
	}
	return "broadcast " + id
}
