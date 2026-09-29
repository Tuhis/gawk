//! GStreamer encode decisions with GStreamer taken out (R56, docs/58 D4/D5):
//! the R14 candidate table, the capture ladder, and the element plan for the
//! live and trial pipelines. Pure and portable, in the `vt_policy` pattern,
//! so every rule is a host test — the way Go's `BuildPipeline` string tests
//! were; `gst` (Linux) only builds what this module plans.
//!
//! The plan is typed elements, never a `parse_launch` string: each
//! [`Element`] is a factory name plus properties, and a capsfilter is an
//! element like any other. Property values stay strings here because that is
//! how GStreamer's own tooling spells them (`gst-inspect-1.0`), and
//! `gst::Element::set_property_from_str` parses them against the real
//! property type at build time — so an enum nick like `cbr` means exactly
//! what it meant in the Go app's `gst-launch-1.0` arguments.

/// One element to build: a factory, an optional name, and its properties in
/// the order they are applied.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Element {
    pub factory: &'static str,
    pub name: Option<&'static str>,
    pub props: Vec<(&'static str, String)>,
}

impl Element {
    pub fn new(factory: &'static str) -> Self {
        Self {
            factory,
            name: None,
            props: Vec::new(),
        }
    }

    pub fn named(mut self, name: &'static str) -> Self {
        self.name = Some(name);
        self
    }

    pub fn prop(mut self, key: &'static str, value: impl ToString) -> Self {
        self.props.push((key, value.to_string()));
        self
    }

    pub fn caps(caps: impl ToString) -> Self {
        Self::new("capsfilter").prop("caps", caps)
    }

    /// The property's planned value, if set.
    pub fn get(&self, key: &str) -> Option<&str> {
        self.props
            .iter()
            .find(|(k, _)| *k == key)
            .map(|(_, v)| v.as_str())
    }
}

/// The appsink the encoded video leaves through.
pub const VIDEO_SINK: &str = "video";
/// The appsink the 1 Hz thumbnail leaves through.
pub const THUMB_SINK: &str = "thumb";
/// The encoder element's name in every plan, so the live pipeline can find
/// it for forced IDRs and error attribution.
pub const ENCODER: &str = "encoder";
/// The capture source's name in the live plan.
pub const SOURCE: &str = "capture";

/// One hardware encoder in the cascade (docs/19 D4, docs/58 D5). There is no
/// software rung, by decision: the browser covers software encode on Linux,
/// and this app exists to do hardware encode.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Candidate {
    /// Vulkan Video: the target encode API (docs/19 D21) — the only one
    /// spanning RADV, ANV and NVIDIA — so it leads (owner, 2026-07-15).
    Vulkan,
    /// NVENC through CUDA memory.
    Nvenc,
    /// VA-API: Intel and AMD. Last, because it structurally excludes NVIDIA.
    Va,
}

/// The cascade, in preference order.
pub const CASCADE: [Candidate; 3] = [Candidate::Vulkan, Candidate::Nvenc, Candidate::Va];

impl Candidate {
    /// The GStreamer element — also the stable id the last-good cache and the
    /// `encoder` pin store, exactly the Go app's values (docs/58 D10).
    pub fn element(self) -> &'static str {
        match self {
            Self::Vulkan => "vulkanh264enc",
            Self::Nvenc => "nvh264enc",
            Self::Va => "vah264enc",
        }
    }

    /// The encode API, for the header line: "which driver stack am I
    /// debugging?" rather than "which element?".
    pub fn api(self) -> &'static str {
        match self {
            Self::Vulkan => "Vulkan Video",
            Self::Nvenc => "NVENC",
            Self::Va => "VA-API",
        }
    }

    /// The candidate's GPU memory caps feature: pinned on the encoder caps in
    /// zero-copy capture so the converter→encoder handoff never falls to
    /// system memory.
    pub fn memory(self) -> &'static str {
        match self {
            Self::Vulkan => "memory:VulkanImage",
            Self::Nvenc => "memory:CUDAMemory",
            Self::Va => "memory:VAMemory",
        }
    }

    /// Where a missing element comes from, for a sentence instead of a trace.
    pub fn package(self) -> &'static str {
        match self {
            Self::Vulkan => "gstreamer1.0-plugins-bad (vulkan)",
            Self::Nvenc => "gstreamer1.0-plugins-bad (nvcodec)",
            Self::Va => "gstreamer1.0-plugins-bad (va)",
        }
    }

    /// The elements between the capture and the encoder. They keep the frame
    /// on the GPU: the conversion is a GPU operation, not a readback.
    pub fn convert(self) -> Vec<Element> {
        match self {
            Self::Vulkan => vec![
                Element::new("vulkanupload"),
                Element::new("vulkancolorconvert"),
            ],
            Self::Nvenc => vec![Element::new("cudaupload"), Element::new("cudaconvertscale")],
            Self::Va => vec![Element::new("vapostproc")],
        }
    }

    /// The element that brings a frame in this candidate's memory back to
    /// system memory — the thumbnail branch's first hop on a zero-copy path.
    pub fn download(self) -> Element {
        match self {
            Self::Vulkan => Element::new("vulkandownload"),
            Self::Nvenc => Element::new("cudadownload"),
            // vapostproc outputs system memory when the caps after it ask.
            Self::Va => Element::new("vapostproc"),
        }
    }

    /// The encoder and its properties (docs/58 D5's table, the Go
    /// `cascade.go` values).
    pub fn encoder(self, fps: u32, peak_bps: u32) -> Element {
        let peak_kbps = peak_bps / 1000;
        let gop = gop_frames(fps);
        let enc = Element::new(self.element()).named(ENCODER);
        match self {
            // CBR: the element has no VBR mode. No GOP or B-frame property is
            // pinned — its keyframe-interval surface varies across driver and
            // GStreamer versions, and a wrong name would fail the lead
            // candidate at launch. The cadence is forced instead (see
            // `KeyframeCadence`), and no B-frames is its profile's default;
            // both are verified by the trial, not assumed.
            Self::Vulkan => enc.prop("rate-control", "cbr").prop("bitrate", peak_kbps),
            // nvh264enc: `bitrate` is the TARGET, `max-bitrate` the ceiling.
            Self::Nvenc => enc
                .prop("rc-mode", "vbr")
                .prop("zerolatency", "true")
                .prop("bitrate", peak_kbps * VBR_TARGET_PERCENT / 100)
                .prop("max-bitrate", peak_kbps)
                .prop("gop-size", gop)
                .prop("bframes", 0),
            // vah264enc: `bitrate` is the CEILING, target-percentage the
            // target fraction of it.
            Self::Va => enc
                .prop("rate-control", "vbr")
                .prop("bitrate", peak_kbps)
                .prop("target-percentage", VBR_TARGET_PERCENT)
                .prop("key-int-max", gop)
                .prop("b-frames", 0),
        }
    }

    /// Whether the encoder element honours its GOP property. Where it does
    /// not (Vulkan, above), the pipeline forces every `fps/2`-th frame with a
    /// force-key-unit event instead — counting frames at the encoder input,
    /// as the VideoToolbox path does (docs/54 D7).
    pub fn needs_forced_cadence(self) -> bool {
        matches!(self, Self::Vulkan)
    }
}

/// VBR target as a percentage of the configured ceiling: typical motion
/// averages ~75 % of the cap, complex scenes burst to it.
pub const VBR_TARGET_PERCENT: u32 = 75;

/// Frames per 500 ms GOP (docs/58 D5: `fps/2`, never 0).
pub fn gop_frames(fps: u32) -> u32 {
    (fps / 2).max(1)
}

/// The cascade order for a start: the pin alone when one is set (the user
/// asked; running a different encoder than the one named would be worse
/// than failing), else the full cascade. `Err` names an unknown pin.
pub fn cascade_for(pin: &str) -> Result<Vec<Candidate>, String> {
    let pin = pin.trim();
    if pin.is_empty() {
        return Ok(CASCADE.to_vec());
    }
    find(pin).map(|c| vec![c]).ok_or_else(|| {
        format!(
            "unknown encoder \"{pin}\" in the config: choose one of {}",
            CASCADE.map(Candidate::element).join(", ")
        )
    })
}

/// A cascade entry by element name.
pub fn find(element: &str) -> Option<Candidate> {
    CASCADE.into_iter().find(|c| c.element() == element)
}

/// How frames may cross the `pipewiresrc` boundary — the Go capture ladder
/// (docs/19, `pipeline.go`), walked per encoder, in this order.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum CaptureRung {
    /// Zero-copy with the stream rate requested at the source, so a 240 Hz
    /// desktop stops costing four converts per kept frame. A compositor that
    /// rejects the field fails this rung's preroll and the ladder moves on.
    AutoCapped,
    /// Free zero-copy negotiation, the converter adjacent to the source so
    /// the DMA-BUF allocation query reaches the element that can import it.
    Auto,
    /// Plain system-memory frames: one copy per frame, immune to modifier
    /// negotiation (the 2026-07-16 "unhandled format" class).
    SystemMemory,
}

pub const RUNGS: [CaptureRung; 3] = [
    CaptureRung::AutoCapped,
    CaptureRung::Auto,
    CaptureRung::SystemMemory,
];

impl CaptureRung {
    pub fn label(self) -> &'static str {
        match self {
            Self::AutoCapped => "auto-capped",
            Self::Auto => "auto",
            Self::SystemMemory => "system-memory",
        }
    }

    /// What the header line calls the path.
    pub fn path(self) -> &'static str {
        match self {
            Self::AutoCapped => "zero-copy (capped)",
            Self::Auto => "zero-copy",
            Self::SystemMemory => "system-memory",
        }
    }
}

/// Whether the 1 Hz thumbnail branch exists on this path (OD13). A tee
/// before a GPU encoder puts a second consumer on the converter's pool, and
/// on a zero-copy path that may cost the zero-copy itself — which the
/// thumbnail must never do. Until the on-hardware pass measures it per path
/// (V-3), only system-memory capture carries one: there the frame is already
/// CPU-visible and the branch is a scale of a buffer we hold anyway.
pub fn thumbnail_on(rung: CaptureRung) -> bool {
    rung == CaptureRung::SystemMemory
}

/// Thumbnail width; the height follows the aspect.
pub const THUMB_WIDTH: u32 = 320;

/// A live capture's inputs.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct LiveParams {
    /// The portal's PipeWire remote, as a raw fd number.
    pub fd: i32,
    /// The granted node's global id (`path`, not `target-object` — see
    /// [`live_plan`]).
    pub node_id: u32,
    /// Fitted, even encode dimensions (docs/39 D2).
    pub width: u32,
    pub height: u32,
    pub fps: u32,
    pub peak_bps: u32,
}

/// The live pipeline plan (docs/58 D4): a chain from the source to the video
/// appsink, and — when [`thumbnail_on`] — a second branch off a tee, planned
/// separately and linked to the tee by the builder.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LivePlan {
    pub video: Vec<Element>,
    /// Starts at the tee's second pad; empty when the path has no thumbnail.
    pub thumb: Vec<Element>,
    /// The tee's name when `thumb` is non-empty.
    pub tee: Option<&'static str>,
}

/// The rate gate: drop-only, never a CFR conversion. Portal capture is
/// damage-driven, and a CFR converter holds the last frame on a still screen
/// and then bursts stale duplicates.
fn rate_gate(fps: u32) -> Element {
    Element::new("videorate")
        .prop("drop-only", "true")
        .prop("max-rate", fps)
}

/// The caps in front of the encoder: the fitted size, the nominal framerate
/// (rate control's budget, not CFR — vah264enc assumes 30 fps without it),
/// and on a zero-copy rung the candidate's memory feature.
pub fn encoder_caps(c: Candidate, rung: CaptureRung, w: u32, h: u32, fps: u32) -> String {
    let media = if rung == CaptureRung::SystemMemory {
        "video/x-raw".to_owned()
    } else {
        format!("video/x-raw({})", c.memory())
    };
    format!(
        "{media},width={},height={},framerate={fps}/1",
        w & !1,
        h & !1
    )
}

/// The parse + appsink tail every plan shares: SPS/PPS before every IDR
/// (`config-interval=-1` — load-bearing, extradata is empty on this path),
/// Annex-B access units, and a sink that never syncs to the clock.
pub fn h264_tail() -> Vec<Element> {
    vec![
        Element::new("h264parse").prop("config-interval", -1),
        Element::caps("video/x-h264,stream-format=byte-stream,alignment=au"),
        Element::new("appsink")
            .named(VIDEO_SINK)
            .prop("sync", "false")
            .prop("max-buffers", 2)
            .prop("drop", "false")
            .prop("emit-signals", "false"),
    ]
}

/// Plans one live attempt: `c` on `rung`.
///
/// `pipewiresrc` selects the granted node by its global id through `path`,
/// NOT `target-object`: the portal's `Start` gives the global id, and
/// `target-object` matches a name or `object.serial` ("target not found").
/// `do-timestamp=true` stamps buffers on the pipeline clock at arrival, which
/// is the one clock (D4).
pub fn live_plan(c: Candidate, rung: CaptureRung, p: LiveParams) -> LivePlan {
    let mut v = vec![
        Element::new("pipewiresrc")
            .named(SOURCE)
            .prop("fd", p.fd)
            .prop("path", p.node_id)
            .prop("do-timestamp", "true"),
    ];
    match rung {
        CaptureRung::AutoCapped | CaptureRung::Auto => {
            if rung == CaptureRung::AutoCapped {
                // Features ANY: a memory constraint here forbids DMA-BUF.
                v.push(Element::caps(format!(
                    "video/x-raw(ANY),max-framerate={}/1",
                    p.fps
                )));
            }
            v.extend(c.convert());
            v.push(rate_gate(p.fps));
        }
        CaptureRung::SystemMemory => {
            v.push(Element::caps("video/x-raw"));
            // The gate first: a dropped frame is never CPU-converted.
            v.push(rate_gate(p.fps));
            v.push(Element::new("videoconvert"));
            v.extend(c.convert());
        }
    }
    v.push(Element::caps(encoder_caps(
        c, rung, p.width, p.height, p.fps,
    )));
    let thumb = if thumbnail_on(rung) {
        v.push(Element::new("tee").named("t"));
        thumb_branch(c, rung)
    } else {
        Vec::new()
    };
    // The encoder branch gets its own queue after a tee so the thumbnail can
    // never back-pressure it (and vice versa).
    if !thumb.is_empty() {
        v.push(
            Element::new("queue")
                .prop("max-size-buffers", 2)
                .prop("max-size-time", 0)
                .prop("max-size-bytes", 0),
        );
    }
    v.push(c.encoder(p.fps, p.peak_bps));
    v.extend(h264_tail());
    LivePlan {
        video: v,
        tee: (!thumb.is_empty()).then_some("t"),
        thumb,
    }
}

/// The thumbnail branch: leaky, 1 Hz, drop-only, scaled down to RGBA. It can
/// never block the encoder branch.
fn thumb_branch(c: Candidate, rung: CaptureRung) -> Vec<Element> {
    let mut b = vec![
        Element::new("queue")
            .prop("leaky", "downstream")
            .prop("max-size-buffers", 1)
            .prop("max-size-time", 0)
            .prop("max-size-bytes", 0),
        Element::new("videorate")
            .prop("drop-only", "true")
            .prop("max-rate", 1),
    ];
    if rung != CaptureRung::SystemMemory {
        b.push(c.download());
    }
    b.push(Element::new("videoconvertscale"));
    b.push(Element::caps(format!(
        "video/x-raw,format=RGBA,width={THUMB_WIDTH},pixel-aspect-ratio=1/1"
    )));
    b.push(
        Element::new("appsink")
            .named(THUMB_SINK)
            .prop("sync", "false")
            .prop("drop", "true")
            .prop("max-buffers", 1)
            .prop("emit-signals", "false"),
    );
    b
}

/// The trial's size: "does this element encode at all on this device", not
/// "how fast" — small enough to be quick, large enough for every encoder's
/// minimum.
pub const TRIAL_WIDTH: u32 = 640;
pub const TRIAL_HEIGHT: u32 = 360;

/// The trial pipeline (docs/58 D5): `videotestsrc` — never the portal, so a
/// probe can never pop a share dialog — through the candidate's convert and
/// encoder into the same tail the live path uses. `is-live` so frames arrive
/// paced like capture and the latency check means something.
pub fn trial_plan(c: Candidate, fps: u32, peak_bps: u32) -> Vec<Element> {
    let mut v = vec![
        Element::new("videotestsrc")
            .prop("is-live", "true")
            .prop("pattern", "ball"),
        Element::caps(format!(
            "video/x-raw,format=NV12,width={TRIAL_WIDTH},height={TRIAL_HEIGHT},framerate={fps}/1"
        )),
    ];
    v.extend(c.convert());
    v.push(Element::caps(encoder_caps(
        c,
        CaptureRung::Auto,
        TRIAL_WIDTH,
        TRIAL_HEIGHT,
        fps,
    )));
    v.push(c.encoder(fps, peak_bps));
    v.extend(h264_tail());
    v
}

/// Which part of the pipeline a bus error came from (D4's attribution),
/// by the source element's factory name.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Culprit {
    /// `pipewiresrc`, a converter, the rate gate: capture's problem — D6's
    /// rebuild.
    Capture,
    /// The encoder, or the parser behind it: the cascade advances while in
    /// the live-probe window; a session error after it.
    Encode,
    /// Anything else (the thumbnail branch, a sink).
    Other,
}

/// Attributes an error to its element (D4).
pub fn culprit(c: Candidate, factory: &str) -> Culprit {
    if factory == c.element() || factory == "h264parse" {
        return Culprit::Encode;
    }
    let capture = ["pipewiresrc", "videorate", "videoconvert", "capsfilter"];
    if capture.contains(&factory) || c.convert().iter().any(|e| e.factory == factory) {
        return Culprit::Capture;
    }
    Culprit::Other
}

/// Whether every failure of a pass died inside the capture source — then the
/// encoders are innocent (no frame reached one) and the diagnosis is the
/// capture-format sentence, not "no hardware encoder".
pub fn all_inside_pipewiresrc(failures: &[String]) -> bool {
    !failures.is_empty()
        && failures
            .iter()
            .all(|f| f.to_ascii_lowercase().contains("pipewiresrc"))
}

/// What a user sees when every live pipeline died inside `pipewiresrc`
/// (the Go `CaptureFormatMessage`, trimmed to the card).
pub const CAPTURE_FORMAT_MESSAGE: &str = "Screen capture failed: your compositor's screencast \
stream and GStreamer's pipewiresrc could not agree on a frame format. The GPU encoder is \
fine — the pipeline died before any frame reached it. gawk-broadcast already retried with \
plain system-memory capture. Check that pipewire and the gstreamer packages come from the \
same era, and whether another portal capture (OBS, Kooha) works on this PC.";

/// The live start is the final probe (docs/19 D4): a pipeline must survive
/// this long to be believed.
pub const LIVE_PROBE: std::time::Duration = std::time::Duration::from_secs(3);

/// Mid-session rebuilds (D6): at most this many per rolling window, then the
/// broadcast ends — a backstop against a hot loop, not a session total.
pub const REBUILD_BUDGET: usize = 60;
pub const REBUILD_WINDOW_US: u64 = 30_000_000;

/// D6's rate limiter: the rebuild timestamps inside the window, pruned on
/// every consultation, so a broadcast that recovers occasionally over hours
/// never runs out.
#[derive(Debug, Default)]
pub struct RebuildLimiter {
    stamps: Vec<u64>,
    budget: usize,
}

impl RebuildLimiter {
    pub fn new(budget: usize) -> Self {
        Self {
            stamps: Vec::new(),
            budget,
        }
    }

    /// Whether a rebuild may start at `now_us`; records it when it may.
    pub fn admit(&mut self, now_us: u64) -> bool {
        self.stamps
            .retain(|&at| now_us >= at && now_us - at < REBUILD_WINDOW_US);
        if self.stamps.len() >= self.budget {
            return false;
        }
        self.stamps.push(now_us);
        true
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn params() -> LiveParams {
        LiveParams {
            fd: 7,
            node_id: 42,
            width: 1920,
            height: 1080,
            fps: 60,
            peak_bps: 12_000_000,
        }
    }

    fn factories(v: &[Element]) -> Vec<&str> {
        v.iter().map(|e| e.factory).collect()
    }

    fn caps_of(v: &[Element]) -> Vec<&str> {
        v.iter()
            .filter(|e| e.factory == "capsfilter")
            .filter_map(|e| e.get("caps"))
            .collect()
    }

    #[test]
    fn the_cascade_is_r14s_order_and_ids() {
        assert_eq!(
            CASCADE.map(Candidate::element),
            ["vulkanh264enc", "nvh264enc", "vah264enc"]
        );
        assert_eq!(
            CASCADE.map(Candidate::api),
            ["Vulkan Video", "NVENC", "VA-API"]
        );
        assert_eq!(find("nvh264enc"), Some(Candidate::Nvenc));
        assert_eq!(find("x264enc"), None, "no software rung, ever");
    }

    #[test]
    fn a_pin_is_the_only_candidate_and_an_unknown_one_is_a_sentence() {
        assert_eq!(cascade_for("").unwrap(), CASCADE.to_vec());
        assert_eq!(cascade_for(" vah264enc ").unwrap(), vec![Candidate::Va]);
        let err = cascade_for("x264enc").unwrap_err();
        assert!(err.contains("vulkanh264enc, nvh264enc, vah264enc"), "{err}");
    }

    #[test]
    fn encoder_properties_are_the_go_tables() {
        let v = Candidate::Vulkan.encoder(60, 12_000_000);
        assert_eq!(v.get("rate-control"), Some("cbr"));
        assert_eq!(v.get("bitrate"), Some("12000"));
        assert_eq!(v.get("gop-size"), None, "no unverifiable GOP property");

        let nv = Candidate::Nvenc.encoder(60, 12_000_000);
        assert_eq!(nv.get("rc-mode"), Some("vbr"));
        assert_eq!(nv.get("zerolatency"), Some("true"));
        assert_eq!(nv.get("bitrate"), Some("9000"), "75 % target");
        assert_eq!(nv.get("max-bitrate"), Some("12000"), "the ceiling");
        assert_eq!(nv.get("gop-size"), Some("30"), "fps/2");
        assert_eq!(nv.get("bframes"), Some("0"));

        let va = Candidate::Va.encoder(120, 16_000_000);
        assert_eq!(va.get("rate-control"), Some("vbr"));
        assert_eq!(
            va.get("bitrate"),
            Some("16000"),
            "va's bitrate is the ceiling"
        );
        assert_eq!(va.get("target-percentage"), Some("75"));
        assert_eq!(va.get("key-int-max"), Some("60"));
        assert_eq!(va.get("b-frames"), Some("0"));

        for c in CASCADE {
            assert_eq!(c.encoder(60, 1).name, Some(ENCODER));
        }
        assert!(Candidate::Vulkan.needs_forced_cadence());
        assert!(!Candidate::Nvenc.needs_forced_cadence());
        assert_eq!(gop_frames(1), 1);
    }

    #[test]
    fn auto_puts_the_converter_on_the_source_and_pins_gpu_memory() {
        let plan = live_plan(Candidate::Va, CaptureRung::Auto, params());
        assert_eq!(
            factories(&plan.video),
            [
                "pipewiresrc",
                "vapostproc",
                "videorate",
                "capsfilter",
                "vah264enc",
                "h264parse",
                "capsfilter",
                "appsink"
            ]
        );
        let src = &plan.video[0];
        assert_eq!(src.get("path"), Some("42"), "the global id, via path");
        assert_eq!(src.get("target-object"), None);
        assert_eq!(src.get("fd"), Some("7"));
        assert_eq!(src.get("do-timestamp"), Some("true"));
        assert_eq!(plan.video[2].get("drop-only"), Some("true"));
        assert_eq!(
            caps_of(&plan.video)[0],
            "video/x-raw(memory:VAMemory),width=1920,height=1080,framerate=60/1"
        );
        assert!(plan.thumb.is_empty() && plan.tee.is_none());
    }

    #[test]
    fn auto_capped_asks_the_compositor_for_the_stream_rate_with_any_features() {
        let plan = live_plan(Candidate::Nvenc, CaptureRung::AutoCapped, params());
        assert_eq!(
            &factories(&plan.video)[..5],
            [
                "pipewiresrc",
                "capsfilter",
                "cudaupload",
                "cudaconvertscale",
                "videorate"
            ]
        );
        assert_eq!(
            caps_of(&plan.video)[0],
            "video/x-raw(ANY),max-framerate=60/1"
        );
    }

    #[test]
    fn system_memory_gates_before_converting_and_keeps_bare_caps() {
        let plan = live_plan(Candidate::Vulkan, CaptureRung::SystemMemory, params());
        let f = factories(&plan.video);
        assert_eq!(
            &f[..6],
            [
                "pipewiresrc",
                "capsfilter",
                "videorate",
                "videoconvert",
                "vulkanupload",
                "vulkancolorconvert"
            ]
        );
        let caps = caps_of(&plan.video);
        assert_eq!(caps[0], "video/x-raw");
        assert_eq!(caps[1], "video/x-raw,width=1920,height=1080,framerate=60/1");
    }

    #[test]
    fn the_tail_puts_headers_before_every_idr_into_an_unsynced_appsink() {
        for rung in RUNGS {
            let plan = live_plan(Candidate::Va, rung, params());
            let n = plan.video.len();
            assert_eq!(plan.video[n - 3].factory, "h264parse");
            assert_eq!(plan.video[n - 3].get("config-interval"), Some("-1"));
            assert_eq!(
                plan.video[n - 2].get("caps"),
                Some("video/x-h264,stream-format=byte-stream,alignment=au")
            );
            let sink = &plan.video[n - 1];
            assert_eq!(sink.name, Some(VIDEO_SINK));
            assert_eq!(sink.get("sync"), Some("false"));
            assert_eq!(sink.get("drop"), Some("false"), "never drops encoded AUs");
        }
    }

    #[test]
    fn caps_carry_the_fitted_even_size() {
        let mut p = params();
        p.width = 1279;
        p.height = 721;
        let plan = live_plan(Candidate::Va, CaptureRung::Auto, p);
        assert!(caps_of(&plan.video)[0].contains("width=1278,height=720"));
    }

    #[test]
    fn the_thumbnail_rides_only_system_memory_and_never_blocks_the_encoder() {
        assert!(!thumbnail_on(CaptureRung::Auto));
        assert!(!thumbnail_on(CaptureRung::AutoCapped));
        let plan = live_plan(Candidate::Nvenc, CaptureRung::SystemMemory, params());
        assert_eq!(plan.tee, Some("t"));
        let t = plan.video.iter().position(|e| e.factory == "tee").unwrap();
        assert_eq!(plan.video[t + 1].factory, "queue", "encoder branch queue");
        assert_eq!(plan.video[t + 2].factory, "nvh264enc");
        let q = &plan.thumb[0];
        assert_eq!(q.factory, "queue");
        assert_eq!(q.get("leaky"), Some("downstream"));
        assert_eq!(q.get("max-size-buffers"), Some("1"));
        assert_eq!(plan.thumb[1].get("max-rate"), Some("1"));
        assert_eq!(plan.thumb.last().unwrap().name, Some(THUMB_SINK));
        assert_eq!(plan.thumb.last().unwrap().get("drop"), Some("true"));
        assert!(
            !factories(&plan.thumb).contains(&"cudadownload"),
            "system memory needs no download"
        );
    }

    #[test]
    fn the_trial_never_touches_the_portal() {
        for c in CASCADE {
            let t = trial_plan(c, 60, 12_000_000);
            assert!(!factories(&t).contains(&"pipewiresrc"));
            assert_eq!(t[0].factory, "videotestsrc");
            assert_eq!(t[0].get("is-live"), Some("true"));
            assert!(factories(&t).contains(&c.element()));
            assert_eq!(t.last().unwrap().name, Some(VIDEO_SINK));
        }
    }

    #[test]
    fn errors_are_attributed_by_element() {
        let c = Candidate::Nvenc;
        assert_eq!(culprit(c, "pipewiresrc"), Culprit::Capture);
        assert_eq!(culprit(c, "cudaupload"), Culprit::Capture);
        assert_eq!(culprit(c, "videorate"), Culprit::Capture);
        assert_eq!(culprit(c, "nvh264enc"), Culprit::Encode);
        assert_eq!(culprit(c, "h264parse"), Culprit::Encode);
        assert_eq!(culprit(c, "appsink"), Culprit::Other);
        assert_eq!(
            culprit(c, "vapostproc"),
            Culprit::Other,
            "not this candidate's"
        );
    }

    #[test]
    fn a_pass_that_died_only_in_pipewiresrc_is_a_capture_diagnosis() {
        assert!(!all_inside_pipewiresrc(&[]));
        assert!(all_inside_pipewiresrc(&[
            "nvh264enc (capture auto): pipewiresrc: stream error".into(),
            "vah264enc (capture system-memory): PipeWireSrc not negotiated".into(),
        ]));
        assert!(!all_inside_pipewiresrc(&[
            "nvh264enc (capture auto): pipewiresrc: stream error".into(),
            "vah264enc (capture auto): vah264enc: no encode entrypoint".into(),
        ]));
    }

    #[test]
    fn the_rebuild_limiter_is_a_rate_not_a_total() {
        let mut l = RebuildLimiter::new(3);
        assert!(l.admit(0));
        assert!(l.admit(1_000_000));
        assert!(l.admit(2_000_000));
        assert!(!l.admit(3_000_000), "fourth inside the window");
        // Once the first stamp ages out, one more fits.
        assert!(l.admit(REBUILD_WINDOW_US));
        // Hours of occasional rebuilds never run out.
        let mut l = RebuildLimiter::new(REBUILD_BUDGET);
        for i in 0..1000u64 {
            assert!(l.admit(i * 60_000_000));
        }
    }
}
