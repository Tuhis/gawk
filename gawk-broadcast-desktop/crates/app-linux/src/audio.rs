//! Audio for a Linux broadcast (docs/58 D7/D8, docs/39 D5/D6): strictly
//! subordinate — every failure here ends in system audio or silence, never a
//! failed broadcast, and with audio off the wire is byte-identical to a
//! video-only broadcaster.

use gawk_audio::gstsrc::{self, Capture, Source};
use gawk_audio::lane::{Block, Lane};
use gawk_audio::pwctl::{Notice, PwCtl};
use gawk_audio::pwgraph::Event;
use gawk_capture::pwclock::Mapper;
use gawk_engine::clock::Clock;
use gawk_engine::media::AUDIO_SAMPLE_RATE;
use gawk_engine::sender::Sender;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

/// What Start decided about audio, on the GUI thread.
pub enum AudioPlan {
    /// The config's switch is off.
    Off,
    /// "No audio" in the whose-audio step (a choice, not a failure).
    NoneByChoice,
    /// Whole-system audio through the cascade, or the device pin.
    System { device: String, last_good: String },
    /// One application's audio through the control plane (docs/39 D3).
    App {
        ctl: PwCtl,
        binary: String,
        last_good: String,
    },
}

/// The silence hint's patience (docs/39 D6): links at zero this long.
const SILENCE_HINT_AFTER: Duration = Duration::from_secs(10);

/// What the pre-flight chose.
pub struct Chosen {
    source: Option<Source>,
    /// The control plane, when app audio is live.
    ctl: Option<PwCtl>,
    app: Option<String>,
    state: &'static str,
}

/// The pre-flight: trials within one budget, the app sink when an app was
/// chosen. Never fails.
pub fn select(plan: &AudioPlan) -> Chosen {
    // `plan` is consumed by `AudioPart::start`; the control plane moves
    // there. Selection only reads it.
    match plan {
        AudioPlan::Off => Chosen::off("off"),
        AudioPlan::NoneByChoice => {
            log::info!("broadcasting this window without audio, by choice");
            Chosen::off("off")
        }
        AudioPlan::System { device, last_good } => system(device, last_good),
        AudioPlan::App {
            ctl,
            binary,
            last_good,
        } => match ctl.capture(Some(binary)) {
            Ok(serial) => {
                let src = Source::AppSinkMonitor(serial);
                match gstsrc::trial(&src, Instant::now() + gstsrc::PROBE_BUDGET) {
                    Ok(()) => {
                        log::info!(
                            "capturing one application's audio: {binary} (sink serial {serial})"
                        );
                        Chosen {
                            source: Some(src),
                            ctl: None,
                            app: Some(binary.clone()),
                            state: "active",
                        }
                    }
                    Err(e) => {
                        log::warn!(
                            "could not capture {binary}'s audio ({e}); using system audio instead"
                        );
                        system("", last_good)
                    }
                }
            }
            Err(e) => {
                log::warn!("per-application audio unavailable ({e}); using system audio instead");
                system("", last_good)
            }
        },
    }
}

fn system(device: &str, last_good: &str) -> Chosen {
    if !device.trim().is_empty() {
        log::info!(
            "capturing the named audio device {device:?}; the per-application step is skipped"
        );
    }
    match gstsrc::select(&gstsrc::order(device, last_good)) {
        Ok(src) => {
            log::info!("capturing system audio: {}", src.name());
            Chosen {
                source: Some(src),
                ctl: None,
                app: None,
                state: "active",
            }
        }
        Err(tried) => {
            for t in &tried {
                log::warn!("audio source rejected: {t}");
            }
            log::warn!("no system-audio source on this machine; publishing video only");
            Chosen::off("unavailable")
        }
    }
}

impl Chosen {
    fn off(state: &'static str) -> Self {
        Self {
            source: None,
            ctl: None,
            app: None,
            state,
        }
    }
}

struct Shared {
    lane: Mutex<Option<Lane>>,
    state: Mutex<String>,
}

/// The running audio side of a broadcast.
pub struct AudioPart {
    shared: Arc<Shared>,
    capture: Mutex<Option<Capture>>,
    ctl: Mutex<Option<PwCtl>>,
    app: Mutex<Option<String>>,
    source: Mutex<Option<Source>>,
    zero_links_since: Mutex<Option<Instant>>,
    sender: Arc<Sender>,
    mapper: Mapper,
    clock: Arc<dyn Clock>,
}

impl AudioPart {
    pub fn start(
        plan: AudioPlan,
        chosen: Chosen,
        sender: Arc<Sender>,
        mapper: Mapper,
        clock: Arc<dyn Clock>,
    ) -> Self {
        let mut ctl = match plan {
            AudioPlan::App { ctl, .. } => Some(ctl),
            _ => None,
        };
        // No app in the end (system chosen, or the app path failed): no
        // reason to hold a PipeWire connection and registry watch for hours
        // (docs/39 F10).
        if chosen.app.is_none()
            && let Some(c) = ctl.take()
        {
            c.stop();
        }
        let part = Self {
            shared: Arc::new(Shared {
                lane: Mutex::new(None),
                state: Mutex::new(chosen.state.into()),
            }),
            capture: Mutex::new(None),
            ctl: Mutex::new(ctl.or(chosen.ctl)),
            app: Mutex::new(chosen.app),
            source: Mutex::new(None),
            zero_links_since: Mutex::new(None),
            sender,
            mapper,
            clock,
        };
        if let Some(src) = chosen.source {
            part.run(src);
        }
        part
    }

    /// Starts capture from `src` into a lane. A lane that will not open, or
    /// a capture that will not start, leaves the broadcast video-only.
    fn run(&self, src: Source) {
        let mut lane_slot = self.shared.lane.lock().unwrap();
        if lane_slot.is_none() {
            match Lane::new(src.name()) {
                Ok(l) => *lane_slot = Some(l),
                Err(e) => {
                    log::warn!("opus encoder: {e}; broadcasting without audio");
                    *self.shared.state.lock().unwrap() = "unavailable".into();
                    return;
                }
            }
        }
        drop(lane_slot);
        let on_pcm = {
            let shared = self.shared.clone();
            let sender = self.sender.clone();
            let mapper = self.mapper;
            let clock = self.clock.clone();
            move |pcm: gstsrc::Pcm<'_>| {
                // One clock (D4): the same mapper as video. A buffer with no
                // time is stamped on arrival minus its own duration.
                let ts = pcm
                    .capture_ns
                    .map(|ns| mapper.to_session_us(ns))
                    .unwrap_or_else(|| {
                        clock.now_us().saturating_sub(
                            pcm.frames as u64 * 1_000_000 / u64::from(AUDIO_SAMPLE_RATE),
                        )
                    });
                let buffers = [pcm.bytes];
                let out = match shared.lane.lock() {
                    Ok(mut lane) => match lane.as_mut() {
                        Some(l) => l.feed(Ok(Block::f32_stereo(&buffers, pcm.frames, ts))),
                        None => return,
                    },
                    Err(_) => return,
                };
                if let Some(format) = out.advertise {
                    sender.set_audio_format(format);
                }
                for p in out.packets {
                    sender.send_audio(p);
                }
                if let Some(why) = out.failed {
                    log::warn!("audio: {why}; broadcast continues without audio");
                    *shared.state.lock().unwrap() = "error".into();
                }
            }
        };
        let on_error = {
            let shared = self.shared.clone();
            move |e: String| {
                log::warn!("audio capture failed: {e}; video continues");
                *shared.state.lock().unwrap() = "error".into();
            }
        };
        match Capture::start(&src, on_pcm, on_error) {
            Ok(c) => {
                *self.capture.lock().unwrap() = Some(c);
                *self.shared.state.lock().unwrap() = "active".into();
                *self.source.lock().unwrap() = Some(src);
            }
            Err(e) => {
                log::warn!("audio capture would not start: {e}; broadcasting without audio");
                *self.shared.state.lock().unwrap() = "unavailable".into();
            }
        }
    }

    pub fn state(&self) -> String {
        self.shared.state.lock().unwrap().clone()
    }

    /// A poisoned lane (a panic inside it) reads as silence, never as a
    /// UI-thread panic.
    pub fn level(&self) -> f32 {
        match self.shared.lane.lock() {
            Ok(l) => l.as_ref().map_or(0.0, |l| l.level().level()),
            Err(_) => 0.0,
        }
    }

    /// The docs/39 D6 hint, driven by the LINK COUNT rather than a level
    /// meter: the chosen app has had no stream linked for 10 s. Drains the
    /// control plane's notices as it goes (the shell calls this every tick).
    pub fn silence_hint(&self) -> bool {
        let ctl = self.ctl.lock().unwrap();
        let Some(c) = ctl.as_ref() else {
            return false;
        };
        let mut zero = self.zero_links_since.lock().unwrap();
        for n in c.drain() {
            match n {
                Notice::Event(Event::Links { links, binary, .. }) if !binary.is_empty() => {
                    if links == 0 {
                        zero.get_or_insert_with(Instant::now);
                    } else {
                        *zero = None;
                    }
                }
                Notice::Event(Event::Links { .. }) => *zero = None,
                Notice::Fatal(e) => {
                    // The control plane died: the sink went with its
                    // connection, so this app's audio has stopped. Video
                    // does not notice; the switch still works (it
                    // re-captures the system cascade).
                    log::warn!(
                        "the PipeWire control plane died ({e}); app audio stopped, video continues"
                    );
                    *self.shared.state.lock().unwrap() = "error".into();
                }
                _ => {}
            }
        }
        self.app.lock().unwrap().is_some()
            && zero.is_some_and(|since| since.elapsed() >= SILENCE_HINT_AFTER)
    }

    /// The one permitted mid-session audio change (docs/39 D5): a RE-LINK
    /// of the same sink onto the system's monitors — same pipeline, same
    /// Opus stream, same sequence space. With the control plane gone, a new
    /// capture of the system cascade into the same lane.
    pub fn switch_to_system(&self) {
        let relinked = self.ctl.lock().unwrap().as_ref().map(|c| c.capture(None));
        match relinked {
            Some(Ok(_)) => {
                log::info!("switched to whole-system audio mid-broadcast (re-link)");
                *self.app.lock().unwrap() = None;
                *self.zero_links_since.lock().unwrap() = None;
            }
            other => {
                if let Some(Err(e)) = other {
                    log::warn!("re-link to system audio failed ({e}); recapturing instead");
                }
                if let Some(c) = self.capture.lock().unwrap().take() {
                    c.stop();
                }
                if let Some(c) = self.ctl.lock().unwrap().take() {
                    c.stop();
                }
                *self.app.lock().unwrap() = None;
                if let Ok(src) = gstsrc::select(&gstsrc::order("", "")) {
                    self.run(src);
                }
            }
        }
    }

    pub fn app(&self) -> Option<String> {
        self.app.lock().unwrap().clone()
    }

    /// The system cascade's winner is worth caching; a pin or an app sink
    /// is not.
    pub fn cacheable_source(&self) -> Option<String> {
        self.source
            .lock()
            .unwrap()
            .as_ref()
            .and_then(|s| Source::cached(s.name()).map(|s| s.name().to_owned()))
    }

    /// Both pipelines to NULL and the control connection closed before
    /// returning (docs/58 §6 "Stopping").
    pub fn stop(&self) {
        if let Some(c) = self.capture.lock().unwrap().take() {
            c.stop();
        }
        if let Some(c) = self.ctl.lock().unwrap().take() {
            c.stop();
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use gawk_engine::clock::MonotonicClock;
    use std::time::Duration;

    use crate::pwtest::{daemon, wait_for};

    fn part(plan: AudioPlan) -> (AudioPart, Arc<Sender>) {
        let chosen = select(&plan);
        let clock: Arc<dyn Clock> = Arc::new(MonotonicClock::new());
        let sender = Arc::new(Sender::new(
            Arc::new(crate::pipeline::tests::NullRelay),
            clock.clone(),
        ));
        let mapper = gawk_capture::pwclock::mapper(&*clock);
        (
            AudioPart::start(plan, chosen, sender.clone(), mapper, clock),
            sender,
        )
    }

    #[test]
    fn off_and_no_audio_leave_the_wire_video_only() {
        let (p, s) = part(AudioPlan::Off);
        assert_eq!(p.state(), "off");
        let (q, _) = part(AudioPlan::NoneByChoice);
        assert_eq!(q.state(), "off");
        assert_eq!(p.level(), 0.0);
        assert!(!p.silence_hint());
        assert_eq!(p.cacheable_source(), None);
        assert_eq!(s.stats().audio_packets_sent, 0);
        p.stop();
        q.stop();
    }

    /// Whole-system audio through the cascade into the shared lane: Opus
    /// packets reach the sender, and the winner is the one to cache.
    #[test]
    fn system_audio_reaches_the_sender_through_the_lane() {
        let Some((_d, _g)) = daemon() else { return };
        let (p, sender) = part(AudioPlan::System {
            device: String::new(),
            last_good: String::new(),
        });
        assert_eq!(p.state(), "active");
        wait_for("audio packets", || sender.stats().audio_packets_sent > 10);
        assert_eq!(p.cacheable_source().as_deref(), Some("pipewire-monitor"));
        assert_eq!(p.app(), None);
        // With no control plane, the switch recaptures the cascade into the
        // same lane — audio carries on.
        p.switch_to_system();
        let n = sender.stats().audio_packets_sent;
        wait_for("audio after the switch", || {
            sender.stats().audio_packets_sent > n + 10
        });
        assert_eq!(p.state(), "active");
        p.stop();
    }

    /// One application's audio: the control plane links it, the app sink's
    /// monitor feeds the lane, the link count drives the silence hint's
    /// clock, and the mid-session switch is a re-link.
    #[test]
    fn app_audio_follows_the_app_and_switches_by_relinking() {
        let Some((d, _g)) = daemon() else { return };
        let game = d.emitter("lane-game", 440, 2);
        let ctl = PwCtl::start().expect("control plane");
        wait_for("the app in the list", || {
            ctl.watch();
            std::thread::sleep(Duration::from_millis(50));
            ctl.drain().iter().any(|n| {
                matches!(n, Notice::Event(Event::Apps(a)) if a.iter().any(|a| a.binary == "lane-game"))
            })
        });
        let (p, sender) = part(AudioPlan::App {
            ctl,
            binary: "lane-game".into(),
            last_good: String::new(),
        });
        assert_eq!(p.state(), "active");
        assert_eq!(p.app().as_deref(), Some("lane-game"));
        assert_eq!(p.cacheable_source(), None, "an app sink is not cacheable");
        wait_for("audio packets", || sender.stats().audio_packets_sent > 10);
        wait_for("a level", || p.level() > 0.0);

        // The app goes quiet: the link count drops to zero and the hint's
        // clock starts (the 10 s itself is policy, not waited for here).
        game.kill();
        wait_for("the zero-link clock", || {
            let _ = p.silence_hint();
            p.zero_links_since.lock().unwrap().is_some()
        });

        // "Use whole-system audio": a re-link into the same sink.
        p.switch_to_system();
        assert_eq!(p.app(), None);
        assert!(p.zero_links_since.lock().unwrap().is_none());
        let n = sender.stats().audio_packets_sent;
        wait_for("audio after the switch", || {
            sender.stats().audio_packets_sent > n + 10
        });
        p.stop();
        wait_for("no gawk sink after stop", || d.gawk_objects().is_empty());
    }

    #[test]
    fn an_app_that_cannot_be_captured_falls_back_to_system_audio() {
        let Some((_d, _g)) = daemon() else { return };
        let ctl = PwCtl::start().expect("control plane");
        ctl.stop();
        // A dead control plane cannot capture: the pre-flight degrades to the
        // system cascade, never to a failed start (docs/39 D6).
        let ctl = PwCtl::start().expect("control plane");
        let chosen = {
            let plan = AudioPlan::App {
                ctl,
                binary: String::new(),
                last_good: String::new(),
            };
            let c = select(&plan);
            if let AudioPlan::App { ctl, .. } = plan {
                ctl.stop();
            }
            c
        };
        assert_eq!(chosen.app, None);
        assert_eq!(chosen.state, "active");
        assert_eq!(chosen.source, Some(Source::PipewireMonitor));
    }
}
