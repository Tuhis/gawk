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
//   - **A viewer count.** Nothing on the wire tells a publisher about
//     subscribers — the browser broadcaster doesn't know either (Decision 18).
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
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
	"gioui.org/x/component"

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
	colBg     = rgb(0x14, 0x15, 0x18) // window
	colCard   = rgb(0x1c, 0x1e, 0x23) // cards
	colText   = rgb(0xdc, 0xe0, 0xe8) // body text
	colBright = rgb(0xf2, 0xf3, 0xf5) // headings, values
	colMuted  = rgb(0x8a, 0x90, 0x9c) // secondary text
	colFaint  = rgb(0x6b, 0x70, 0x7a) // labels, hints
	colButton = rgb(0x2a, 0x2d, 0x34) // secondary buttons
	colLive   = rgb(0x3d, 0xd6, 0x8c) // the heartbeat dot
	colDanger = rgb(0x8b, 0x2c, 0x2c) // stop
	colError  = rgb(0xff, 0xa5, 0x9e) // error text
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

	relay      component.TextField
	appURL     component.TextField
	secret     component.TextField
	bitrate    component.TextField
	resolution component.TextField
	framerate  component.TextField

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
		ContrastBg: colBright, // primary button: light surface…
		ContrastFg: colBg,     // …with dark text (R6's inverted monochrome)
	}
	u.list.Axis = layout.Vertical
	u.relay.SetText(cfg.RelayURL)
	u.appURL.SetText(cfg.AppURL)
	u.secret.SetText(cfg.PublishSecret)
	u.secret.Mask = '•'
	if cfg.BitrateBps > 0 {
		u.bitrate.SetText(strconv.FormatFloat(float64(cfg.BitrateBps)/1e6, 'f', -1, 64))
	}
	if cfg.Width > 0 && cfg.Height > 0 {
		u.resolution.SetText(fmt.Sprintf("%dx%d", cfg.Width, cfg.Height))
	}
	if cfg.Fps > 0 {
		u.framerate.SetText(strconv.Itoa(cfg.Fps))
	}
	return u
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
	u.cfg.Width, u.cfg.Height = parseResolutionField(u.resolution.Text())
	u.cfg.Fps = parseFramerateField(u.framerate.Text())
	if err := u.cfg.Save(); err != nil {
		// Non-fatal: the broadcast can still run, the settings just won't
		// survive a restart.
		fmt.Fprintln(os.Stderr, "could not save settings:", err)
	}
}

// parseResolutionField turns the resolution field into a rung. Blank or
// unparseable means "use the default" (0,0), like the other fields; the
// parser itself is the engine's, shared with the CLI's -resolution flag.
func parseResolutionField(s string) (int, int) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0
	}
	w, h, err := engine.ParseResolution(s)
	if err != nil {
		return 0, 0
	}
	return w, h
}

// parseFramerateField turns the framerate field into fps. Blank or
// unparseable means "use the default" (0); clamped to [1, 240] — the highest
// refresh rate any of our displays actually has, and beyond it the number is
// a typo.
func parseFramerateField(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0
	}
	return min(n, 240)
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
					// this is what a preview would have been for).
					c := colFaint
					if state == gawkapp.StateLive {
						c = colLive
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
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(spacer(4)),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					t := material.Body2(u.th, line)
					t.Color = colMuted
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
						return u.relay.Layout(gtx, u.th, "Relay URL (https://…)")
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
						return u.resolution.Layout(gtx, u.th, "Stream resolution WxH (blank = 1920x1080)")
					}),
					layout.Rigid(spacer(8)),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.framerate.Layout(gtx, u.th, "Framerate cap in fps (blank = 60)")
					}),
				)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if !live {
					return layout.Dimensions{}
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(spacer(6)),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						t := material.Caption(u.th, "Settings apply to the next broadcast.")
						t.Color = colFaint
						return t.Layout(gtx)
					}),
				)
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
				rows := [][2]string{
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
