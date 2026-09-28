package api

// The R49 merged room view (docs/50 D3, D4): the Room CR list — durable, the
// whole truth for static facts — joined with the fleet scan's home-pod rows,
// the only place the live roster exists (docs/44 D5).
//
// The merge renders the UNION, with `live` saying which half a row came from:
// a room whose home pod did not answer, or that no pod has homed, renders from
// its CR with `live: false` and no roster, rather than an empty roster that
// would read as "nobody is here". A room some pod is home for but that has no
// CR — a relay running rooms without the CRD, or a mint racing the CR list —
// renders from the scan alone, so the operator still sees it.

import (
	"context"
	"sort"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/kube"
	"github.com/Tuhis/gawk/gawk-admin/internal/relayscan"
	"github.com/Tuhis/gawk/gawk-server/rooms"
)

// liveRooms returns the fleet's home-pod rows by normalized code. Degrades
// rather than failing: with no scanner, or a fleet that cannot be enumerated,
// every room renders from its CR as not live — the rooms view is still an
// operator's way to END a room, so it must work while the relays are in
// trouble.
func (a *API) liveRooms(ctx context.Context) map[string]relayscan.RoomAggregate {
	out := map[string]relayscan.RoomAggregate{}
	if a.opts.Fleet == nil {
		return out
	}
	snap, err := a.opts.Fleet.Snapshot(ctx)
	if err != nil {
		a.log.Warn("relay enumeration failed; rendering rooms from their CRs only", "err", err)
		return out
	}
	for _, r := range snap.Rooms {
		out[r.Code] = r
	}
	return out
}

// mergeRooms renders every room in the union of the CR list and the scan.
// detail selects the projection: the list omits the attachment and
// participant arrays, which is what keeps a list of rooms from being the
// detail of every room (D4).
func (a *API) mergeRooms(list []kube.RoomObject, live map[string]relayscan.RoomAggregate, detail bool) []roomJSON {
	out := make([]roomJSON, 0, len(list)+len(live))
	seen := make(map[string]bool, len(list))
	for _, obj := range list {
		seen[obj.Name] = true
		if obj.Err != nil {
			a.log.Warn("room CR could not be decoded; listing it by name only", "crName", obj.Name, "err", obj.Err)
			out = append(out, roomJSON{Name: obj.Name, Code: obj.Name})
			continue
		}
		row := renderRoom(obj)
		if l, ok := live[obj.Name]; ok {
			a.applyLive(&row, l, detail)
		} else {
			a.applyCRAttachments(&row, obj.Room.Status.Attachments, detail)
		}
		out = append(out, row)
	}
	for code, l := range live {
		if seen[code] {
			continue
		}
		row := roomJSON{
			Name: l.Code, Kind: l.Kind, Code: l.DisplayCode, DisplayName: l.DisplayName,
			HomeHolder: l.Pod, Key: l.Key, CreatedAt: formatTime(l.CreatedAt),
		}
		if l.EmptySince != nil {
			row.EmptySince = formatTime(*l.EmptySince)
		}
		a.applyLive(&row, l, detail)
		out = append(out, row)
	}
	for i := range out {
		out[i].Links = a.roomLinks(out[i].Code)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			// Static rooms first: they are the ones an operator manages;
			// dynamic ones come and go.
			return out[i].Kind == rooms.KindStatic
		}
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// applyLive fills a row from its home pod's answer.
func (a *API) applyLive(row *roomJSON, l relayscan.RoomAggregate, detail bool) {
	row.Live = true
	if row.HomeHolder == "" {
		row.HomeHolder = l.Pod
	}
	if row.Key == "" {
		row.Key = l.Key
	}
	row.Counts = roomCountsJSON{Participants: len(l.Participants), Attachments: len(l.Attachments)}
	for _, p := range l.Participants {
		if p.Streaming {
			row.Counts.Streaming++
		} else {
			row.Counts.Watching++
		}
	}
	if !detail {
		return
	}
	atts := make([]roomAttachmentJSON, 0, len(l.Attachments))
	for _, at := range l.Attachments {
		live, viewers := at.Live, at.Viewers
		atts = append(atts, roomAttachmentJSON{
			BroadcastID: at.BroadcastID, Label: at.Label, Live: &live, Viewers: &viewers,
			AttachedAt: formatTime(at.AttachedAt), Links: a.attachmentLinks(at.BroadcastID),
		})
	}
	parts := make([]roomParticipantJSON, 0, len(l.Participants))
	for _, p := range l.Participants {
		parts = append(parts, roomParticipantJSON{
			ID: p.ID, Nickname: p.Nickname, ClientKind: p.ClientKind,
			Streaming: p.Streaming, Speaking: p.Speaking, Identity: p.Identity,
		})
	}
	row.Attachments, row.Participants = &atts, &parts
}

// applyCRAttachments fills a row nobody reachable is home for. The CR's
// attachment list is kept current by the home pod on every attach and
// detach, so it is right about WHAT is attached; it cannot say whether a
// broadcast is live or watched, and those fields are absent rather than
// false. The roster is empty because it is unknown, and `live: false` says so.
func (a *API) applyCRAttachments(row *roomJSON, crAtts []rooms.Attachment, detail bool) {
	row.Counts = roomCountsJSON{Attachments: len(crAtts)}
	if !detail {
		return
	}
	atts := make([]roomAttachmentJSON, 0, len(crAtts))
	for _, at := range crAtts {
		j := roomAttachmentJSON{BroadcastID: at.BroadcastID, Label: at.Label, Links: a.attachmentLinks(at.BroadcastID)}
		if at.AttachedAt != nil {
			j.AttachedAt = formatTime(at.AttachedAt.Time)
		}
		atts = append(atts, j)
	}
	parts := []roomParticipantJSON{}
	row.Attachments, row.Participants = &atts, &parts
}

// roomLinks is `<appBaseUrl>/#/room/<displayCode>`, omitted without a base
// URL — the rule links.watch already follows on broadcasts (D4).
func (a *API) roomLinks(displayCode string) *roomLinksJSON {
	if a.opts.Config.AppBaseURL == "" || displayCode == "" {
		return nil
	}
	return &roomLinksJSON{Join: a.opts.Config.AppBaseURL + "/#/room/" + displayCode}
}

func (a *API) attachmentLinks(id string) *attachmentLinksJSON {
	if a.opts.Config.AppBaseURL == "" || id == "" {
		return nil
	}
	return &attachmentLinksJSON{Watch: a.opts.Config.AppBaseURL + "/#/view/" + id}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
