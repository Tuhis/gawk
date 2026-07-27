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

	relay     textInput
	appURL    textInput
	secret    textInput
	bitrate   textInput
	telemetry textInput

	resPick dropdown
	fpsPick dropdown

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
	u.relay.SetText(cfg.RelayURL)
	u.appURL.SetText(cfg.AppURL)
	u.secret.SetText(cfg.PublishSecret)
	u.secret.ed.Mask = '•'
	u.telemetry.SetText(cfg.TelemetryURL)
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
				layout.Rigid(u.code),
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
	if u.copyDia.Clicked(gtx) {
		gtx.Execute(clipboard.WriteCmd{Type: "application/text", Data: io.NopCloser(strings.NewReader(u.app.Diagnostics()))})
		u.copied = "Diagnostics copied"
	}
}

// save writes the settings fields back to the config.
func (u *ui) save() {
	u.cfg.RelayURL = strings.TrimSpace(u.relay.Text())
	u.cfg.AppURL = strings.TrimSpace(u.appURL.Text())
	u.cfg.PublishSecret = u.secret.Text()
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
	if err := u.cfg.Save(); err != nil {
		// Non-fatal: the broadcast can still run, the settings just won't
		// survive a restart.
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
			t := material.H6(u.th, "gawk broadcast")
			t.Color = colBright
			return t.Layout(gtx)
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
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.relay.Layout(gtx, u.th, "Relay URL (blank = the default relay)")
					}),
					layout.Rigid(spacer(8)),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.appURL.Layout(gtx, u.th, "App URL, for join links (https://…)")
					}),
					layout.Rigid(spacer(8)),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.secret.Layout(gtx, u.th, "Publish secret (if the relay needs one)")
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
				relay := config.ResolveRelayURL(u.relay.Text())
				ingest := config.ResolveTelemetryURL(u.relay.Text(), u.telemetry.Text())
				if ingest == "" {
					ingest = "off — nothing is sent"
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

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
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
