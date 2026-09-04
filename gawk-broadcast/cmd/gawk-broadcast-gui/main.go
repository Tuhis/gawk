// Command gawk-broadcast-gui is the native Linux broadcaster's window
// (R14 V5/V6, docs/19).
//
// It is deliberately thin: everything worth testing lives in internal/app, and
// this file is layout plus event wiring. The engine is consumed unmodified from
// V1 — adding a GUI required no engine change, which is the whole reason the
// engine was a package from the start rather than a main full of flags.
//
// What it deliberately does not have:
//
//   - **A source picker.** Your desktop's portal already shows you one, on the
//     first run only (Decision 5). We don't draw it and we don't theme it.
//   - **A preview.** You are looking at your own screen. The browser has one
//     only because a tab isn't your screen. What people actually need is "am I
//     live and are frames moving", which the code and a sent-fps readout
//     answer (Decision 16).
//   - **A tray.** The window *is* the app: close it and nothing is publishing.
//     No background presence, no hidden state (Decision 15).
package main

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"gioui.org/app"
	"gioui.org/io/clipboard"
	"gioui.org/io/input"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	gawkapp "github.com/Tuhis/gawk/gawk-broadcast/internal/app"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/config"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/engine"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/gst"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/notify"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/pwproto"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/version"
)

// The window's palette: the web app's monochrome-restrained design system
// (R6) in Gio form — dark surfaces, light text, a light *inverted* primary
// button, green reserved for "live" and red for danger. These feed the Gio
// theme too: material.NewTheme() is a light theme, and any widget that takes
// its color from the theme (text fields, checkbox labels, button text) used
// to render near-black on the dark background.
var (
	colBg      = rgb(0x14, 0x15, 0x18) // window
	colCard    = rgb(0x1c, 0x1e, 0x23) // cards
	colText    = rgb(0xdc, 0xe0, 0xe8) // body text
	colBright  = rgb(0xf2, 0xf3, 0xf5) // headings, values
	colMuted   = rgb(0x8a, 0x90, 0x9c) // secondary text
	colFaint   = rgb(0x6b, 0x70, 0x7a) // labels, hints
	colButton  = rgb(0x2a, 0x2d, 0x34) // secondary buttons
	colPrimary = rgb(0x3f, 0x51, 0xb5) // Gio's stock material blue — Start wore it before the dark-palette rework, and it was missed
	colLive    = rgb(0x3d, 0xd6, 0x8c) // the heartbeat dot
	colResume  = rgb(0xe8, 0xb3, 0x39) // the heartbeat dot while reconnecting
	colDanger  = rgb(0x8b, 0x2c, 0x2c) // stop
	colError   = rgb(0xff, 0xa5, 0x9e) // error text
)

func main() {
	cfgPath, err := config.DefaultPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		// A corrupt config must not stop the app starting; it shows up in the
		// window instead.
		fmt.Fprintln(os.Stderr, err)
	}

	go func() {
		w := new(app.Window)
		w.Option(
			app.Title("gawk broadcast"),
			app.Size(unit.Dp(460), unit.Dp(560)),
			app.MinSize(unit.Dp(380), unit.Dp(460)),
		)
		if err := loop(w, cfg); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}()
	app.Main()
}

func loop(w *app.Window, cfg *config.Config) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	n := notify.New()

	a := gawkapp.New(gawkapp.Options{
		Config:   cfg,
		Notifier: n,
		Log:      log,
		// The engine's callbacks arrive on its own goroutines; every state
		// change has to poke the UI thread or the window shows stale state
		// until the user moves the mouse.
		Invalidate: w.Invalidate,
	})
	// Decision 15: closing the window ends the broadcast. Nothing survives it.
	defer a.Quit()

	ui := newUI(a, cfg)
	var ops op.Ops
	for {
		switch e := w.Event().(type) {
		case app.DestroyEvent:
			return e.Err
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			ui.layout(gtx)
			e.Frame(gtx.Ops)
		}
	}
}

// ui holds the widget state.
type ui struct {
	app *gawkapp.App
	cfg *config.Config
	th  *material.Theme

	start   widget.Clickable
	stop    widget.Clickable
	resume  widget.Clickable
	copyLnk widget.Clickable
	copyDia widget.Clickable
	details widget.Bool

	// R37 SP8: the server picker. serverSel is the profile whose fields the
	// editor currently shows — 0 is the pinned default, i>0 is
	// cfg.CustomServers()[i-1]. relay/secret below edit the selected profile;
	// the default's URL is built in and not editable (its credential slot is,
	// docs/40 F4).
	serverPick dropdown
	serverName textInput
	addSrv     widget.Clickable
	removeSrv  widget.Clickable
	serverSel  int

	relay     textInput
	appURL    textInput
	secret    textInput
	bitrate   textInput
	telemetry textInput

	resPick dropdown
	fpsPick dropdown

	// R42's room card (docs/44 §4.8): code-or-link and attach-secret
	// fields, the tile label and nickname, and the four actions.
	roomCode   textInput
	roomSecret textInput
	roomLabel  textInput
	nickname   textInput
	roomAttach widget.Clickable
	roomDetach widget.Clickable
	roomNew    widget.Clickable
	roomOpen   widget.Clickable
	// openURL launches the browser; a seam so tests never spawn one.
	openURL func(string)

	// R35's whose-audio card. appBtns is one clickable per listed application,
	// grown as the live list does; the two fixed rows and the mid-session
	// switch have their own.
	appBtns    []*widget.Clickable
	sysAudio   widget.Clickable
	noAudio    widget.Clickable
	switchSys  widget.Clickable
	audioApps  []pwproto.App
	audioReady bool

	list   widget.List
	copied string
}

func newUI(a *gawkapp.App, cfg *config.Config) *ui {
	u := &ui{app: a, cfg: cfg, th: material.NewTheme()}
	// A dark theme, not a dark rectangle: every themed widget (text fields,
	// checkbox, button text) draws from this palette, and the material
	// default is a light theme — black-on-dark without it.
	u.th.Palette = material.Palette{
		Bg:         colBg,
		Fg:         colText,
		ContrastBg: colPrimary, // the stock material blue, back by request
		ContrastFg: colBright,
	}
	u.list.Axis = layout.Vertical
	// A config built by hand (tests, embedding) may still carry the pre-R37
	// flat relay/secret pair; fold it into the profiles the picker edits.
	// Load already did this for the real config file — Migrate is idempotent.
	cfg.Migrate()
	u.serverSel = selectedServerIndex(cfg)
	u.rebuildServerPick()
	u.loadServerFields()
	u.appURL.SetText(cfg.AppURL)
	u.secret.ed.Mask = '•'
	u.telemetry.SetText(cfg.TelemetryURL)
	u.roomCode.SetText(cfg.Room)
	u.roomSecret.SetText(cfg.RoomAttachSecret)
	u.roomSecret.ed.Mask = '•'
	u.roomLabel.SetText(cfg.RoomLabel)
	u.nickname.SetText(cfg.Nickname)
	u.openURL = openInBrowser
	if cfg.BitrateBps > 0 {
		u.bitrate.SetText(strconv.FormatFloat(float64(cfg.BitrateBps)/1e6, 'f', -1, 64))
	}
	u.resPick = newDropdown("Resolution", resLabels(), resIndex(cfg.Width, cfg.Height))
	if cfg.Width > 0 && u.resPick.sel < 0 {
		// A rung the CLI set that the picker does not offer: show it honestly
		// until the user picks one of ours.
		u.resPick.custom = fmt.Sprintf("%d×%d (custom)", cfg.Width, cfg.Height)
	}
	u.fpsPick = newDropdown("Framerate", fpsLabels(), fpsIndex(cfg.Fps))
	if cfg.Fps > 0 && u.fpsPick.sel < 0 {
		u.fpsPick.custom = fmt.Sprintf("%d fps (custom)", cfg.Fps)
	}
	return u
}

// selectedServerIndex maps cfg.SelectedServer to the picker's index space:
// 0 for the default, 1+i for a custom profile; unknown selections degrade to
// the default, same as the config's own resolution rule.
func selectedServerIndex(cfg *config.Config) int {
	if name := cfg.SelectedServer; name != "" && name != config.DefaultServerName {
		for i, p := range cfg.CustomServers() {
			if p.Name == name {
				return i + 1
			}
		}
	}
	return 0
}

// selectionName is the inverse: what to store in cfg.SelectedServer for a
// picker index.
func selectionName(cfg *config.Config, sel int) string {
	if custom := cfg.CustomServers(); sel > 0 && sel <= len(custom) {
		return custom[sel-1].Name
	}
	return config.DefaultServerName
}

// rebuildServerPick regenerates the dropdown from the profile list. Called
// whenever the list or a display name changes — the dropdown holds its labels
// by value.
func (u *ui) rebuildServerPick() {
	labels := []string{"Default relay"}
	for _, p := range u.cfg.CustomServers() {
		labels = append(labels, p.Name)
	}
	u.serverPick = newDropdown("Server", labels, u.serverSel)
}

// loadServerFields points the editor fields at the selected profile.
func (u *ui) loadServerFields() {
	if u.serverSel == 0 {
		// The default's identity is built in (docs/40 D5) — only its
		// credential slot is editable (F4).
		u.serverName.SetText("")
		u.relay.SetText("")
		u.secret.SetText(u.cfg.DefaultSecret())
		return
	}
	p := u.cfg.CustomServers()[u.serverSel-1]
	u.serverName.SetText(p.Name)
	u.relay.SetText(p.URL)
	u.secret.SetText(p.PublishSecret)
}

// commitServerFields writes the editor fields back into the profile they show.
// The config package owns the invariants (unique names, the reserved
// "default", the URL-keyed default record).
func (u *ui) commitServerFields() {
	if u.serverSel == 0 {
		u.cfg.SetDefaultSecret(u.secret.Text())
		return
	}
	u.cfg.UpdateCustomServer(u.serverSel-1, config.ServerProfile{
		Name:          strings.TrimSpace(u.serverName.Text()),
		URL:           strings.TrimSpace(u.relay.Text()),
		PublishSecret: u.secret.Text(),
	})
}

// The rungs mirror the browser broadcaster's pickers (R3), minus its
// auto/native entries — auto by request, native because this pipeline pins
// concrete encoder caps. 120 is the one addition: 240 Hz-class displays make
// it a real choice here where the browser's "native" used to cover it.
var resRungs = []struct {
	label string
	w, h  int
}{
	{"2560×1440", 2560, 1440},
	{"1920×1080 (default)", 1920, 1080},
	{"1280×720", 1280, 720},
	{"854×480", 854, 480},
}

var fpsRungs = []struct {
	label string
	fps   int
}{
	{"120 fps", 120},
	{"60 fps (default)", 60},
	{"30 fps", 30},
	{"5 fps", 5},
}

func resLabels() []string {
	out := make([]string, len(resRungs))
	for i, r := range resRungs {
		out[i] = r.label
	}
	return out
}

func fpsLabels() []string {
	out := make([]string, len(fpsRungs))
	for i, r := range fpsRungs {
		out[i] = r.label
	}
	return out
}

// resIndex maps a configured rung back to a picker entry; 0,0 (the default)
// selects 1920×1080, anything unlisted returns -1 (custom).
func resIndex(w, h int) int {
	if w == 0 && h == 0 {
		return 1
	}
	for i, r := range resRungs {
		if r.w == w && r.h == h {
			return i
		}
	}
	return -1
}

func fpsIndex(fps int) int {
	if fps == 0 {
		return 1
	}
	for i, r := range fpsRungs {
		if r.fps == fps {
			return i
		}
	}
	return -1
}

func (u *ui) layout(gtx layout.Context) layout.Dimensions {
	// A plain dark background: this window is a control panel, not a design
	// exercise — the production UI (R6) is where taste lives.
	paint.FillShape(gtx.Ops, colBg, clip.Rect{Max: gtx.Constraints.Max}.Op())

	u.handleEvents(gtx)

	inset := layout.UniformInset(unit.Dp(20))
	return inset.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return material.List(u.th, &u.list).Layout(gtx, 1, func(gtx layout.Context, _ int) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(u.header),
				layout.Rigid(spacer(16)),
				layout.Rigid(u.controls),
				layout.Rigid(spacer(16)),
				layout.Rigid(u.audioCard),
				layout.Rigid(u.code),
				layout.Rigid(spacer(12)),
				layout.Rigid(u.roomCard),
				layout.Rigid(spacer(12)),
				layout.Rigid(u.errorBox),
				layout.Rigid(spacer(12)),
				layout.Rigid(u.settings),
				layout.Rigid(spacer(12)),
				layout.Rigid(u.stats),
			)
		})
	})
}

func (u *ui) handleEvents(gtx layout.Context) {
	// R37 SP8: server picker actions. A dropdown choice made during the last
	// frame's layout is applied here: commit the fields of the profile being
	// left, then load the newly selected one. Selection persists immediately —
	// choosing a server is a deliberate act, not a pending edit.
	if sel := u.serverPick.sel; sel >= 0 && sel != u.serverSel {
		u.commitServerFields()
		u.serverSel = sel
		u.cfg.SelectedServer = selectionName(u.cfg, sel)
		u.rebuildServerPick() // a rename on the profile just left may have changed its label
		u.loadServerFields()
		u.saveCfg()
	}
	if u.addSrv.Clicked(gtx) {
		u.commitServerFields()
		p := u.cfg.AddCustomServer()
		u.cfg.SelectedServer = p.Name
		u.serverSel = len(u.cfg.CustomServers())
		u.rebuildServerPick()
		u.loadServerFields()
		u.saveCfg()
	}
	if u.removeSrv.Clicked(gtx) && u.serverSel > 0 {
		u.cfg.RemoveCustomServer(u.serverSel - 1)
		u.serverSel = 0
		u.cfg.SelectedServer = config.DefaultServerName
		u.rebuildServerPick()
		u.loadServerFields()
		u.saveCfg()
	}
	if u.start.Clicked(gtx) {
		u.save()
		u.copied = ""
		u.app.Start(context.Background(), "")
	}
	if u.resume.Clicked(gtx) {
		u.save()
		u.copied = ""
		u.app.Start(context.Background(), u.cfg.LastBroadcastID)
	}
	if u.stop.Clicked(gtx) {
		u.app.Stop()
	}
	if u.copyLnk.Clicked(gtx) {
		if link := u.app.JoinLink(); link != "" {
			gtx.Execute(clipboard.WriteCmd{Type: "application/text", Data: io.NopCloser(strings.NewReader(link))})
			u.copied = "Link copied"
		} else if id := u.app.BroadcastID(); id != "" {
			gtx.Execute(clipboard.WriteCmd{Type: "application/text", Data: io.NopCloser(strings.NewReader(id))})
			u.copied = "Code copied"
		}
	}
	// R35: the whose-audio card. Answering it unblocks the engine goroutine
	// that is waiting inside Start.
	if prompt := u.app.AudioPromptState(); prompt != nil {
		for i, b := range u.appBtns {
			if i < len(u.audioApps) && b.Clicked(gtx) {
				u.app.AnswerAudioPrompt(engine.AudioTarget{
					Mode:   engine.AudioTargetApp,
					Binary: u.audioApps[i].Binary,
				})
			}
		}
		if u.sysAudio.Clicked(gtx) {
			u.app.AnswerAudioPrompt(engine.AudioTarget{Mode: engine.AudioTargetSystem})
		}
		if u.noAudio.Clicked(gtx) {
			u.app.AnswerAudioPrompt(engine.AudioTarget{Mode: engine.AudioTargetNone})
		}
	}
	if u.switchSys.Clicked(gtx) {
		u.app.SwitchToSystemAudio(context.Background())
	}
	// R42: the room card. Attach persists the room (so the next start
	// attaches too) and joins now when live; Detach forgets it; New room
	// mints from the live broadcast; Open room view hands the grant to the
	// SPA through the browser.
	if u.roomAttach.Clicked(gtx) {
		u.saveRoomFields()
		u.app.AttachRoom(u.roomCode.Text(), u.roomSecret.Text())
	}
	if u.roomDetach.Clicked(gtx) {
		u.app.DetachRoom()
		u.roomCode.SetText("")
	}
	if u.roomNew.Clicked(gtx) {
		u.saveRoomFields()
		u.app.NewRoom()
	}
	if u.roomOpen.Clicked(gtx) {
		if link := u.app.RoomViewLink(); link != "" {
			u.openURL(link)
		}
	}
	if u.copyDia.Clicked(gtx) {
		gtx.Execute(clipboard.WriteCmd{Type: "application/text", Data: io.NopCloser(strings.NewReader(u.app.Diagnostics()))})
		u.copied = "Diagnostics copied"
	}
}

// save writes the settings fields back to the config.
func (u *ui) save() {
	u.commitServerFields()
	u.cfg.SelectedServer = selectionName(u.cfg, u.serverSel)
	u.rebuildServerPick() // a rename may have changed the selected label
	// The flat relay/secret slots are per-run overrides (-url/GAWK_URL) since
	// R37; the GUI's server choice lives in the profiles, and persisting a
	// flat copy would shadow every future selection.
	u.cfg.RelayURL, u.cfg.PublishSecret = "", ""
	u.cfg.AppURL = strings.TrimSpace(u.appURL.Text())
	u.cfg.BitrateBps = parseBitrateMbps(u.bitrate.Text())
	// Stored verbatim, blanks and all: blank means "follow the default" and
	// baking today's default in would pin this user to it forever.
	u.cfg.TelemetryURL = strings.TrimSpace(u.telemetry.Text())
	if i := u.resPick.sel; i >= 0 {
		u.cfg.Width, u.cfg.Height = resRungs[i].w, resRungs[i].h
	}
	if i := u.fpsPick.sel; i >= 0 {
		u.cfg.Fps = fpsRungs[i].fps
	}
	// R42: a room typed into the card but not yet attached still means
	// "attach on start" — the CLI's -room, the profile's `room` field.
	u.cfg.Room = gawkapp.RoomCodeFromInput(u.roomCode.Text())
	u.cfg.RoomAttachSecret = strings.TrimSpace(u.roomSecret.Text())
	u.applyRoomFields()
	u.saveCfg()
}

// applyRoomFields copies the label and nickname fields into the config.
func (u *ui) applyRoomFields() {
	u.cfg.RoomLabel = strings.TrimSpace(u.roomLabel.Text())
	u.cfg.Nickname = strings.TrimSpace(u.nickname.Text())
}

// saveRoomFields persists the label and nickname ahead of a room action, so
// the engine's next attach or mint carries what the card shows.
func (u *ui) saveRoomFields() {
	u.applyRoomFields()
	u.saveCfg()
}

// openInBrowser launches the system browser at url, the Windows sibling's
// open_in_browser in Go: xdg-open on Linux, open on macOS. Fire and
// forget — a browser that fails to start is not this window's error.
func openInBrowser(url string) {
	if url == "" {
		return
	}
	cmd := "xdg-open"
	if runtime.GOOS == "darwin" {
		cmd = "open"
	}
	if err := exec.Command(cmd, url).Start(); err != nil {
		fmt.Fprintln(os.Stderr, "could not open the browser:", err)
	}
}

// saveCfg persists the config, non-fatally: the broadcast can still run, the
// settings just won't survive a restart.
func (u *ui) saveCfg() {
	if err := u.cfg.Save(); err != nil {
		fmt.Fprintln(os.Stderr, "could not save settings:", err)
	}
}

// parseBitrateMbps turns the bitrate field into bps. Blank or unparseable
// means "use the default" (0), mirroring the CLI's -bitrate; values are
// clamped to [1, 100] Mbps — outside that range the number is a typo, and a
// 1600 Mbps broadcast would only fail somewhere less obvious.
func parseBitrateMbps(s string) int {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", "."))
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return 0
	}
	f = min(max(f, 1), 100)
	return int(f * 1e6)
}

func (u *ui) header(gtx layout.Context) layout.Dimensions {
	state, status := u.app.State()
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			// Title left, build version right. The version is here rather than
			// buried in Details because the question it answers — "is the
			// binary you are looking at the one I fixed?" — is asked about a
			// screenshot, and a screenshot only ever shows the top of the
			// window.
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Baseline}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					t := material.H6(u.th, "gawk broadcast")
					t.Color = colBright
					return t.Layout(gtx)
				}),
				// Claims the slack so the version sits against the right edge.
				// Returning the allocated Min (not layout.Spacer{}, which
				// reports its own fixed size) is what makes the next child land
				// where the flex actually put it.
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return layout.Dimensions{Size: gtx.Constraints.Min}
				}),
				layout.Rigid(caption(u.th, "v"+version.String())),
			)
		}),
		layout.Rigid(spacer(4)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					// The heartbeat: "am I live" at a glance (Decision 16 —
					// this is what a preview would have been for). Amber while
					// auto-resume reclaims the broadcast: the code is still
					// ours, but nothing is reaching viewers yet.
					c := colFaint
					if state == gawkapp.StateLive {
						c = colLive
						if u.app.Resuming() {
							c = colResume
						}
					}
					return dot(gtx, c)
				}),
				layout.Rigid(spacerW(8)),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					t := material.Body2(u.th, status)
					t.Color = rgb(0xa8, 0xad, 0xb8)
					return t.Layout(gtx)
				}),
			)
		}),
		// The encode line: which API is doing the work (Vulkan Video / NVENC /
		// VA-API — the status already says so, but this one adds *how frames
		// travel* and at what rung), visible without opening Details.
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			s := u.app.Stats()
			if state != gawkapp.StateLive || s.Encoder == "" {
				return layout.Dimensions{}
			}
			line := fmt.Sprintf("%dx%d@%d · %.0f Mbps", s.Width, s.Height, s.Fps, float64(s.BitrateBps)/1e6)
			// R35: the dimensions above are the *fitted* ones, so saying a
			// window is being shared is what makes "1542x1080" read as
			// deliberate rather than as a bug.
			if s.ShareMode == "window" {
				line = "window · " + line
			}
			if s.CapturePath != "" {
				line += " · " + s.CapturePath + " capture"
			}
			// R25: whether sound is going out is worth a glance without
			// opening Details — a broadcast that is silently silent is the
			// failure this line exists to prevent.
			line += " · " + gawkapp.AudioStatus(s)
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(spacer(4)),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					t := material.Body2(u.th, line)
					t.Color = colMuted
					return t.Layout(gtx)
				}),
			)
		}),
		// R18: the live audience figure — the relay pushes it (~1 s cadence);
		// hidden until the first push (an old relay never sends one).
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			s := u.app.Stats()
			if state != gawkapp.StateLive || !s.ViewerCountAvailable {
				return layout.Dimensions{}
			}
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(spacer(4)),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					t := material.Body2(u.th, fmt.Sprintf("%d watching", s.ViewerCount))
					t.Color = colText
					return t.Layout(gtx)
				}),
			)
		}),
	)
}

// secondaryBtn is a dark button with light text. The theme's ContrastFg is
// dark (the primary button is light-inverted), so any button with a dark
// background must say so about its text too.
func (u *ui) secondaryBtn(c *widget.Clickable, label string, bg color.NRGBA) material.ButtonStyle {
	b := material.Button(u.th, c, label)
	b.Background = bg
	b.Color = colBright
	return b
}

func (u *ui) controls(gtx layout.Context) layout.Dimensions {
	state, _ := u.app.State()
	switch state {
	case gawkapp.StateLive, gawkapp.StateStarting:
		btn := u.secondaryBtn(&u.stop, "Stop broadcast", colDanger)
		if state == gawkapp.StateStarting {
			btn.Text = "Starting…"
		}
		return btn.Layout(gtx)
	default:
		return layout.Flex{}.Layout(gtx,
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return material.Button(u.th, &u.start, "Start broadcast").Layout(gtx)
			}),
			// Resume is offered only when there is a code to reclaim. Per
			// Decision 5 it no longer re-prompts the picker, so it is cheap.
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if u.cfg.LastBroadcastID == "" {
					return layout.Dimensions{}
				}
				return layout.Flex{}.Layout(gtx,
					layout.Rigid(spacerW(8)),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.secondaryBtn(&u.resume, "Resume "+u.cfg.LastBroadcastID, colButton).Layout(gtx)
					}),
				)
			}),
		)
	}
}

// audioCard is R35's whose-audio step, and — while live — the status line it
// collapses into.
//
// Linux structurally cannot have the Windows sibling's one-picker flow: the
// portal never says which application owns the window that was picked
// (docs/39 §1). So rather than guessing — a wrong guess silently streams the
// wrong application's sound, the worst failure this feature can have — gawk
// asks, once, and says plainly why.
func (u *ui) audioCard(gtx layout.Context) layout.Dimensions {
	if prompt := u.app.AudioPromptState(); prompt != nil {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.audioPrompt(gtx, prompt)
			}),
			layout.Rigid(spacer(16)),
		)
	}
	hint := u.app.AudioSilenceHint()
	if hint == "" {
		return layout.Dimensions{}
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return card(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(label(u.th, "Audio")),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						t := material.Body2(u.th, hint)
						t.Color = colText
						return t.Layout(gtx)
					}),
					layout.Rigid(spacer(8)),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.secondaryBtn(&u.switchSys, "Switch to whole-system audio", colButton).Layout(gtx)
					}),
				)
			})
		}),
		layout.Rigid(spacer(16)),
	)
}

// audioPrompt draws the card itself.
func (u *ui) audioPrompt(gtx layout.Context, prompt *gawkapp.AudioPrompt) layout.Dimensions {
	u.audioApps = prompt.Apps
	for len(u.appBtns) < len(u.audioApps) {
		u.appBtns = append(u.appBtns, new(widget.Clickable))
	}

	children := []layout.FlexChild{
		layout.Rigid(label(u.th, "Whose audio?")),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			t := material.Body2(u.th, "You are sharing one window. Choose whose sound goes out with it.")
			t.Color = colText
			return t.Layout(gtx)
		}),
		layout.Rigid(spacer(10)),
	}

	if prompt.Err != nil {
		// D6's first row: per-application audio is unavailable on this
		// machine. Say what and why in one sentence, then offer what works.
		children = append(children,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				t := material.Body2(u.th, gawkapp.Message(prompt.Err))
				t.Color = colError
				return t.Layout(gtx)
			}),
			layout.Rigid(spacer(10)),
		)
	} else if len(u.audioApps) == 0 {
		// The card's one honest caveat. Streams only exist once an application
		// opens them, and the list updates live — so this is a wait, not a
		// dead end.
		children = append(children,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				t := material.Body2(u.th,
					"Apps appear here when they play sound — start the game's audio first if the list looks empty.")
				t.Color = colMuted
				return t.Layout(gtx)
			}),
			layout.Rigid(spacer(10)),
		)
	}

	for i, appRow := range u.audioApps {
		i, appRow := i, appRow
		children = append(children,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				text := appRow.Name
				if appRow.Binary != "" && appRow.Binary != appRow.Name {
					text += "  ·  " + appRow.Binary
				}
				if appRow.Streams > 1 {
					text += fmt.Sprintf("  (%d streams)", appRow.Streams)
				}
				bg := colButton
				if appRow.Binary == prompt.Preselect {
					// AD3: the last-used application, highlighted only when it
					// is present and emitting, so the common restart-the-same-
					// game case is one click.
					bg = colPrimary
					text += "   ← last used"
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.secondaryBtn(u.appBtns[i], text, bg).Layout(gtx)
					}),
					layout.Rigid(spacer(6)),
				)
			}),
		)
	}

	children = append(children,
		layout.Rigid(spacer(4)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.secondaryBtn(&u.sysAudio, "Whole system", colButton).Layout(gtx)
		}),
		layout.Rigid(spacer(6)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.secondaryBtn(&u.noAudio, "No audio", colButton).Layout(gtx)
		}),
	)
	return card(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
	})
}

func (u *ui) code(gtx layout.Context) layout.Dimensions {
	id := u.app.BroadcastID()
	if id == "" {
		return layout.Dimensions{}
	}
	return card(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(label(u.th, "Broadcast code")),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				t := material.H4(u.th, id)
				t.Color = colBright
				// Monospace: this is read aloud and typed in by hand.
				t.Font.Typeface = "monospace"
				return t.Layout(gtx)
			}),
			layout.Rigid(spacer(8)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if link := u.app.JoinLink(); link != "" {
					t := material.Body2(u.th, link)
					t.Color = colMuted
					return t.Layout(gtx)
				}
				t := material.Body2(u.th, "Set the app URL below to get a join link.")
				t.Color = colFaint
				return t.Layout(gtx)
			}),
			layout.Rigid(spacer(8)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.secondaryBtn(&u.copyLnk, "Copy link", colButton).Layout(gtx)
					}),
					layout.Rigid(spacerW(10)),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if u.copied == "" {
							return layout.Dimensions{}
						}
						t := material.Body2(u.th, u.copied)
						t.Color = colLive
						return t.Layout(gtx)
					}),
				)
			}),
		)
	})
}

// roomCard is R42's room control (docs/44 §4.8): where this broadcast sits
// in a room, and the four things a broadcaster can do about it. The status
// line is derived in internal/app from the relay's own events, so
// "attached", "away" and "ended" mean what the relay says, not what was
// clicked.
func (u *ui) roomCard(gtx layout.Context) layout.Dimensions {
	state, _ := u.app.State()
	live := state == gawkapp.StateLive
	r := u.app.Room()
	inRoom := r.Status != gawkapp.RoomNone && r.Status != gawkapp.RoomError && r.Status != gawkapp.RoomEnded
	return card(gtx, func(gtx layout.Context) layout.Dimensions {
		children := []layout.FlexChild{
			layout.Rigid(label(u.th, "Room")),
			layout.Rigid(spacer(4)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				line := r.Status.String()
				if r.Code != "" && r.Status != gawkapp.RoomNone {
					line += " · " + r.Code
				}
				if inRoom && r.Status != gawkapp.RoomJoining {
					line += fmt.Sprintf(" · %d broadcast(s), %d participant(s)", r.Broadcasts, r.Participants)
				}
				if r.Status == gawkapp.RoomNone && r.Configured != "" && !live {
					line = "Will attach to " + r.Configured + " on start"
				}
				t := material.Body2(u.th, line)
				t.Color = colText
				if r.Status == gawkapp.RoomAttached {
					t.Color = colLive
				}
				return t.Layout(gtx)
			}),
		}
		if r.Error != "" {
			children = append(children,
				layout.Rigid(spacer(4)),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					t := material.Body2(u.th, r.Error)
					t.Color = colError
					return t.Layout(gtx)
				}),
			)
		}
		children = append(children,
			layout.Rigid(spacer(8)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.roomCode.Layout(gtx, u.th, "Room code, slug, or a room link")
			}),
			layout.Rigid(spacer(8)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.roomSecret.Layout(gtx, u.th, "Attach secret (static rooms only)")
			}),
			layout.Rigid(spacer(8)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return u.roomLabel.Layout(gtx, u.th, "Tile label")
					}),
					layout.Rigid(spacerW(8)),
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return u.nickname.Layout(gtx, u.th, "Nickname")
					}),
				)
			}),
			layout.Rigid(spacer(10)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				row := []layout.FlexChild{}
				if inRoom {
					row = append(row, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.secondaryBtn(&u.roomDetach, "Detach", colDanger).Layout(gtx)
					}))
				} else {
					row = append(row, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return material.Button(u.th, &u.roomAttach, "Attach").Layout(gtx)
					}))
				}
				if live && !inRoom {
					// A room is minted *from* a live broadcast; idle there is
					// nothing to mint from.
					row = append(row,
						layout.Rigid(spacerW(8)),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return u.secondaryBtn(&u.roomNew, "New room", colButton).Layout(gtx)
						}),
					)
				}
				if inRoom && u.app.RoomViewLink() != "" {
					row = append(row,
						layout.Rigid(spacerW(8)),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return u.secondaryBtn(&u.roomOpen, "Open room view", colButton).Layout(gtx)
						}),
					)
				}
				return layout.Flex{}.Layout(gtx, row...)
			}),
		)
		if inRoom && u.app.RoomViewLink() == "" {
			children = append(children,
				layout.Rigid(spacer(6)),
				layout.Rigid(caption(u.th, "Set the app URL below to open the room view.")),
			)
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
	})
}

func (u *ui) errorBox(gtx layout.Context) layout.Dimensions {
	msg := u.app.LastError()
	if msg == "" {
		return layout.Dimensions{}
	}
	return card(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				// Sentences, not Go error strings — internal/app owns that
				// mapping, including the HTTP-status distinctions the browser
				// broadcaster structurally cannot make.
				t := material.Body2(u.th, msg)
				t.Color = colError
				return t.Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if !u.app.CanMint() {
					return layout.Dimensions{}
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(spacer(8)),
					// Offered only for connect-phase failures. A capture-phase
					// failure had a live publisher session on that ID, and
					// silently minting a new one is R1's bug.
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return material.Button(u.th, &u.start, "Start a new broadcast instead").Layout(gtx)
					}),
				)
			}),
		)
	})
}

func (u *ui) settings(gtx layout.Context) layout.Dimensions {
	state, _ := u.app.State()
	live := state != gawkapp.StateIdle
	return card(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(label(u.th, "Settings")),
			layout.Rigid(spacer(8)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if live {
					// Decision 9: no mid-session changes. Rather than let the
					// user type into fields that will not take effect, the
					// panel says so.
					gtx = gtx.Disabled()
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					// R37 SP8: which server the next broadcast dials, with
					// add/edit/remove for custom profiles. The default's
					// identity is built in; only its secret is editable (F4).
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.dropdownRow(gtx, &u.serverPick)
					}),
					layout.Rigid(spacer(6)),
					layout.Rigid(u.serverEditor),
					layout.Rigid(spacer(8)),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.appURL.Layout(gtx, u.th, "App URL, for join links (https://…)")
					}),
					layout.Rigid(spacer(8)),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.bitrate.Layout(gtx, u.th, "Peak bitrate in Mbps (blank = 16)")
					}),
					layout.Rigid(spacer(8)),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.telemetry.Layout(gtx, u.th, `Telemetry URL (blank = default, "off" = send nothing)`)
					}),
					layout.Rigid(spacer(10)),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.dropdownRow(gtx, &u.resPick)
					}),
					layout.Rigid(spacer(8)),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.dropdownRow(gtx, &u.fpsPick)
					}),
				)
			}),
			// Both fields default to something rather than to nothing, so blank
			// is not self-explanatory: this says where the next broadcast
			// actually goes and where its diagnostics actually go, computed
			// from what is typed right now rather than from what was last
			// saved. A default that sends data should be visible without
			// reading the README.
			layout.Rigid(spacer(8)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				// The default profile's URL is not typed anywhere: blank
				// resolves to it, exactly like the config does.
				relayRaw := ""
				if u.serverSel > 0 {
					relayRaw = u.relay.Text()
				}
				relay := config.ResolveRelayURL(relayRaw)
				ingest := config.ResolveTelemetryURL(relayRaw, u.telemetry.Text())
				if ingest == "" {
					if u.serverSel > 0 && !config.TelemetryOff(u.telemetry.Text()) {
						// The §4.10 rule made visible: on a foreign server the
						// destination is the relay's to name (wire 0x12), and
						// with no advertisement nothing is sent at all.
						ingest = "wherever this server advertises — otherwise nothing is sent"
					} else {
						ingest = "off — nothing is sent"
					}
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(caption(u.th, "Broadcasting to "+relay)),
					layout.Rigid(caption(u.th, "Diagnostics to "+ingest)),
				)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if !live {
					return layout.Dimensions{}
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(spacer(6)),
					layout.Rigid(caption(u.th, "Settings apply to the next broadcast.")),
				)
			}),
			// R23 (docs/29 TC5): a non-gating reference to the operator's terms,
			// with the link when an app URL is set (derived like the join link).
			layout.Rigid(spacer(10)),
			layout.Rigid(caption(u.th, "By broadcasting you accept the operator's terms of use.")),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				link := u.app.TermsLink()
				if link == "" {
					return layout.Dimensions{}
				}
				return caption(u.th, link)(gtx)
			}),
		)
	})
}

// serverEditor renders the selected profile's fields: name/URL/secret for a
// custom server, secret-only (identity built in, docs/40 D5/F4) for the
// default — plus add/remove.
func (u *ui) serverEditor(gtx layout.Context) layout.Dimensions {
	var children []layout.FlexChild
	if u.serverSel == 0 {
		children = append(children,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return caption(u.th, "Relay URL: "+config.DefaultRelayURL+" (built in)")(gtx)
			}),
			layout.Rigid(spacer(6)),
		)
	} else {
		children = append(children,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.serverName.Layout(gtx, u.th, "Server name")
			}),
			layout.Rigid(spacer(8)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.relay.Layout(gtx, u.th, "Relay URL (https://…)")
			}),
			layout.Rigid(spacer(8)),
		)
	}
	children = append(children,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.secret.Layout(gtx, u.th, "Publish secret for this server (if it needs one)")
		}),
		layout.Rigid(spacer(8)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			row := []layout.FlexChild{
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return u.secondaryBtn(&u.addSrv, "Add server", colButton).Layout(gtx)
				}),
			}
			if u.serverSel > 0 {
				row = append(row,
					layout.Rigid(spacerW(8)),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.secondaryBtn(&u.removeSrv, "Remove server", colDanger).Layout(gtx)
					}),
				)
			}
			return layout.Flex{}.Layout(gtx, row...)
		}),
	)
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

func (u *ui) stats(gtx layout.Context) layout.Dimensions {
	s := u.app.Stats()
	return card(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return material.CheckBox(u.th, &u.details, "Details").Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if !u.details.Value {
					return layout.Dimensions{}
				}
				rtt := "n/a"
				if s.TimeSyncAvailable {
					rtt = fmt.Sprintf("%.1f ms", s.TimeSyncRttMs)
				}
				kf := "n/a"
				if s.KeyframeIntervalAvailable {
					kf = fmt.Sprintf("%.0f ms (target 500)", s.KeyframeIntervalMs)
				}
				encoder := "—"
				if s.Encoder != "" {
					encoder = gst.EncoderAPI(s.Encoder) + " (" + s.Encoder + ")"
				}
				watching := "n/a (relay predates R18)"
				if s.ViewerCountAvailable {
					watching = fmt.Sprintf("%d", s.ViewerCount)
				}
				rows := [][2]string{
					{"Watching", watching},
					// R35: what was actually shared. "Rung" below carries the
					// *fitted* dimensions, so a window's odd numbers are
					// explained by the row above them.
					{"Sharing", shareMode(s)},
					{"Encoder", encoder},
					{"Capture path", orDash(s.CapturePath)},
					{"Codec", orDash(s.Codec)},
					{"Rung", fmt.Sprintf("%dx%d@%d · %.1f Mbps", s.Width, s.Height, s.Fps, float64(s.BitrateBps)/1e6)},
					// Decision 20: the GStreamer child owns capture, so this
					// stage is genuinely unobservable from here. "n/a" is the
					// honest answer; a number from the requested rate would
					// answer "is the source keeping up?" wrongly.
					{"Capture fps", "n/a (the encoder child owns capture)"},
					{"Encode fps", fmt.Sprintf("%.1f", s.EncoderFps)},
					{"Sent fps", fmt.Sprintf("%.1f", s.SentFps)},
					{"Keyframes", fmt.Sprintf("%d sent · %d failed · %d superseded",
						s.KeyframeStreamsSent, s.KeyframeStreamsFailed, s.KeyframeStreamsSuperseded)},
					{"Keyframe interval", kf},
					{"Dropped at send", fmt.Sprintf("%d", s.FramesDroppedAtSend)},
					// "none" on a healthy session, and the answer to "why did
					// it hitch?" on any other: each rebuild is a capture death
					// the broadcast survived, and a freeze viewers saw
					// (docs/39 D2). Nothing else on screen would say so.
					{"Capture rebuilds", captureRebuilds(s)},
					{"Datagrams", fmt.Sprintf("%d · %.1f MB", s.DatagramsSent, float64(s.BytesSent)/1e6)},
					{"RTT (time-sync)", rtt},
					{"Audio", gawkapp.AudioStatus(s)},
					{"Audio format", audioFormat(s)},
					{"Audio packets", fmt.Sprintf("%d sent · %d configs · %d dropped · %.2f MB",
						s.AudioPacketsSent, s.AudioConfigsSent, s.AudioPacketsDropped, float64(s.AudioBytesSent)/1e6)},
				}
				children := make([]layout.FlexChild, 0, len(rows)+2)
				children = append(children, layout.Rigid(spacer(8)))
				for _, r := range rows {
					children = append(children, layout.Rigid(statRow(u.th, r[0], r[1])))
				}
				children = append(children,
					layout.Rigid(spacer(10)),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.secondaryBtn(&u.copyDia, "Copy diagnostics", colButton).Layout(gtx)
					}),
				)
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
			}),
		)
	})
}

// textInput is a labelled single-line field: a caption over a bordered
// editor, in the same hand-rolled spirit as dropdown below.
//
// It replaces gioui.org/x's component.TextField, which asked for an immediate
// redraw on *every* frame a field held text — its floating-label animation
// restarts from a state the field never leaves while it has contents. Since
// newUI pre-fills these from the saved config, the window free-ran from launch
// and burned 20-30 % of a core sitting idle (docs/19 finding 12; the spin was
// invisible during a broadcast only because the panel is laid out disabled
// then, and a disabled context drops the invalidate). Nothing here animates,
// so an idle window schedules no frames at all — main_test.go holds that line
// for the whole window, not just for this widget.
type textInput struct {
	ed widget.Editor
}

func (t *textInput) SetText(s string) { t.ed.SetText(s) }
func (t *textInput) Text() string     { return t.ed.Text() }

func (t *textInput) Layout(gtx layout.Context, th *material.Theme, hint string) layout.Dimensions {
	t.ed.SingleLine = true
	// The settings panel is laid out through gtx.Disabled() while live
	// (Decision 9), which is a zero input.Source. Say so visually — otherwise
	// a field that silently ignores typing is the only feedback.
	disabled := gtx.Source == (input.Source{})
	textCol, borderCol := colText, colFaint
	switch {
	case disabled:
		textCol, borderCol = colMuted, colButton
	case gtx.Source.Focused(&t.ed):
		borderCol = colPrimary
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			c := material.Caption(th, hint)
			c.Color = colFaint
			return c.Layout(gtx)
		}),
		layout.Rigid(spacer(4)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return widget.Border{Color: borderCol, CornerRadius: unit.Dp(6), Width: unit.Dp(1)}.Layout(gtx,
				func(gtx layout.Context) layout.Dimensions {
					return layout.UniformInset(unit.Dp(8)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						e := material.Editor(th, &t.ed, "")
						e.Color = textCol
						return e.Layout(gtx)
					})
				})
		}),
	)
}

// dropdown is a picker built from stock buttons: the field toggles an inline
// list of options (Gio has no combobox, and an overlay menu is a lot of
// machinery for a settings card — inline expansion behaves the same and
// cannot mis-anchor). sel is the chosen index, or -1 for a value configured
// outside the picker's rungs, shown via custom.
type dropdown struct {
	title  string
	labels []string
	custom string
	sel    int
	open   bool
	field  widget.Clickable
	items  []widget.Clickable
}

func newDropdown(title string, labels []string, sel int) dropdown {
	return dropdown{title: title, labels: labels, sel: sel, items: make([]widget.Clickable, len(labels))}
}

func (u *ui) dropdownRow(gtx layout.Context, d *dropdown) layout.Dimensions {
	if d.field.Clicked(gtx) {
		d.open = !d.open
	}
	for i := range d.items {
		if d.items[i].Clicked(gtx) {
			d.sel, d.open = i, false
		}
	}
	current := d.custom
	if d.sel >= 0 {
		current = d.labels[d.sel]
	}
	arrow := "▾"
	if d.open {
		arrow = "▴"
	}
	children := []layout.FlexChild{
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.secondaryBtn(&d.field, d.title+": "+current+"  "+arrow, colButton).Layout(gtx)
		}),
	}
	if d.open {
		for i := range d.labels {
			children = append(children,
				layout.Rigid(spacer(4)),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Left: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						b := u.secondaryBtn(&d.items[i], d.labels[i], colCard)
						if i == d.sel {
							b.Color = colBright
						} else {
							b.Color = colMuted
						}
						return b.Layout(gtx)
					})
				}),
			)
		}
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

// --- small helpers -------------------------------------------------------

func statRow(th *material.Theme, k, v string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{}.Layout(gtx,
			layout.Flexed(0.45, func(gtx layout.Context) layout.Dimensions {
				t := material.Body2(th, k)
				t.Color = colMuted
				return t.Layout(gtx)
			}),
			layout.Flexed(0.55, func(gtx layout.Context) layout.Dimensions {
				t := material.Body2(th, v)
				t.Color = colText
				t.Alignment = text.End
				return t.Layout(gtx)
			}),
		)
	}
}

// caption is the settings card's small faint line.
func caption(th *material.Theme, s string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		t := material.Caption(th, s)
		t.Color = colFaint
		return t.Layout(gtx)
	}
}

func label(th *material.Theme, s string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		t := material.Caption(th, strings.ToUpper(s))
		t.Color = colFaint
		return t.Layout(gtx)
	}
}

func card(gtx layout.Context, w layout.Widget) layout.Dimensions {
	macro := op.Record(gtx.Ops)
	dims := layout.UniformInset(unit.Dp(14)).Layout(gtx, w)
	call := macro.Stop()

	rr := gtx.Dp(unit.Dp(10))
	defer clip.RRect{Rect: image.Rect(0, 0, gtx.Constraints.Max.X, dims.Size.Y), SE: rr, SW: rr, NW: rr, NE: rr}.Push(gtx.Ops).Pop()
	paint.Fill(gtx.Ops, colCard)
	call.Add(gtx.Ops)
	return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, dims.Size.Y)}
}

func dot(gtx layout.Context, c color.NRGBA) layout.Dimensions {
	sz := gtx.Dp(unit.Dp(9))
	defer clip.Ellipse{Max: image.Pt(sz, sz)}.Push(gtx.Ops).Pop()
	paint.Fill(gtx.Ops, c)
	return layout.Dimensions{Size: image.Pt(sz, sz)}
}

func spacer(dp int) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Spacer{Height: unit.Dp(dp)}.Layout(gtx)
	}
}

func spacerW(dp int) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Spacer{Width: unit.Dp(dp)}.Layout(gtx)
	}
}

func rgb(r, g, b uint8) color.NRGBA { return color.NRGBA{R: r, G: g, B: b, A: 0xff} }

// shareMode renders what the desktop's picker returned, in the user's
// vocabulary. Empty until capture starts, which is honest rather than a guess:
// the picker is what decides, and it has not been answered yet.
func shareMode(s engine.Stats) string {
	switch s.ShareMode {
	case "window":
		if s.AudioApp != "" {
			return "One window · audio from " + s.AudioApp
		}
		return "One window · whole-system audio"
	case "screen":
		return "Whole screen"
	default:
		return "—"
	}
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// captureRebuilds renders the mid-broadcast capture restarts (docs/39 D2).
// "none" rather than "0": the row exists to answer a question someone is
// already asking about a hitch they saw, and a bare zero reads like a number
// nobody has looked at.
func captureRebuilds(s engine.Stats) string {
	if s.CaptureRestarts == 0 {
		return "none"
	}
	return fmt.Sprintf("%d (capture died and was rebuilt; each one was a freeze)", s.CaptureRestarts)
}

// audioFormat renders the format actually advertised to viewers, or a dash
// when there is no lane. Read from the stats rather than from the constants:
// what matters is what the AudioConfig on the wire says, not what the code
// intended it to say.
func audioFormat(s engine.Stats) string {
	if s.AudioCodec == "" {
		return "—"
	}
	return fmt.Sprintf("%s · %d Hz · %d ch · %.0f kbps",
		s.AudioCodec, s.AudioSampleRate, s.AudioChannels, float64(s.AudioBitrateBps)/1000)
}
