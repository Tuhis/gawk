//! Process memory, measured (R76, docs/77): what the broadcaster costs the
//! machine it runs on, and which part of it is whose.
//!
//! Three sources, each reported only where it is real (an absent number and
//! a zero must stay distinguishable, as everywhere in [`crate::stats`]):
//!
//! - **The process footprint** from the OS, in the figure the OS's own task
//!   manager shows (D2): Windows' private working set (Task Manager's
//!   "Memory"), Apple's `phys_footprint` (Activity Monitor's "Memory", and
//!   what iOS's jetsam limit is charged against), Linux's anonymous plus
//!   shared resident pages (what a desktop's system monitor calls private).
//! - **The Rust heap**, counted by [`CountingAlloc`] when a shell installs it
//!   as its `#[global_allocator]` (D3). Footprint minus heap is memory the
//!   app reaches only through a library: the GPU driver, Media Foundation,
//!   GStreamer, ScreenCaptureKit, the UI toolkit's renderer.
//! - **Components**: bytes each subsystem knows it holds, reported where it
//!   allocates them (D4). Their sum against the footprint is what is still
//!   unattributed.
//!
//! Everything here is process-wide, like the memory it measures: the
//! counters are statics, not per session.

use std::alloc::{GlobalAlloc, Layout, System};
use std::sync::atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering};
use std::sync::{Mutex, OnceLock};

// ---------------------------------------------------------------------------
// The Rust heap (D3)
// ---------------------------------------------------------------------------

static HEAP_LIVE: AtomicUsize = AtomicUsize::new(0);
static HEAP_PEAK: AtomicUsize = AtomicUsize::new(0);

/// The system allocator, counting live bytes. A shell opts in with
///
/// ```ignore
/// #[global_allocator]
/// static ALLOC: gawk_engine::memory::CountingAlloc = gawk_engine::memory::CountingAlloc;
/// ```
///
/// Two relaxed atomics per allocation; nothing else changes. Without it
/// [`heap`] reports nothing, which is what the tests and any shell that has
/// not opted in see.
pub struct CountingAlloc;

impl CountingAlloc {
    fn grew(by: usize) {
        let now = HEAP_LIVE.fetch_add(by, Ordering::Relaxed) + by;
        HEAP_PEAK.fetch_max(now, Ordering::Relaxed);
    }
    fn shrank(by: usize) {
        HEAP_LIVE.fetch_sub(by, Ordering::Relaxed);
    }
}

// SAFETY: every call is forwarded to `System` unchanged; the counters only
// observe sizes the caller already passed.
unsafe impl GlobalAlloc for CountingAlloc {
    unsafe fn alloc(&self, layout: Layout) -> *mut u8 {
        let p = unsafe { System.alloc(layout) };
        if !p.is_null() {
            Self::grew(layout.size());
        }
        p
    }

    unsafe fn alloc_zeroed(&self, layout: Layout) -> *mut u8 {
        let p = unsafe { System.alloc_zeroed(layout) };
        if !p.is_null() {
            Self::grew(layout.size());
        }
        p
    }

    unsafe fn dealloc(&self, ptr: *mut u8, layout: Layout) {
        unsafe { System.dealloc(ptr, layout) };
        Self::shrank(layout.size());
    }

    unsafe fn realloc(&self, ptr: *mut u8, layout: Layout, new_size: usize) -> *mut u8 {
        let p = unsafe { System.realloc(ptr, layout, new_size) };
        if !p.is_null() {
            if new_size >= layout.size() {
                Self::grew(new_size - layout.size());
            } else {
                Self::shrank(layout.size() - new_size);
            }
        }
        p
    }
}

/// Live and peak Rust heap bytes, or `None` when [`CountingAlloc`] is not
/// the global allocator. Installed, it has counted something long before
/// anyone can ask: the runtime allocates before `main`.
pub fn heap() -> Option<(u64, u64)> {
    let peak = HEAP_PEAK.load(Ordering::Relaxed);
    if peak == 0 {
        return None;
    }
    Some((HEAP_LIVE.load(Ordering::Relaxed) as u64, peak as u64))
}

// ---------------------------------------------------------------------------
// Components (D4)
// ---------------------------------------------------------------------------

/// A subsystem that reports the bytes it holds. The set is closed on
/// purpose: each is a typed telemetry field (`mem<Name>Bytes`), and a
/// component is only worth a field once it can say what it holds.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Component {
    /// Captured frames held between capture and the encoder, wherever their
    /// pixels live: the capture frame pool, conversion targets, queued
    /// frames. Mostly GPU textures and IOSurfaces, counted at pixel size, so
    /// this explains the GPU figure more than the footprint (D4).
    Capture,
    /// Encoded access units between the encoder and the sender (the engine's
    /// `FrameGate`).
    Encode,
    /// Encoded video the sender holds: carrier records queued for the uplink
    /// and the keyframe waiting for its stream (docs/75 D3, F-12). QUIC's own
    /// send buffers are not visible to the app and are not in here.
    Send,
    /// The Ready page's source preview and the UI's picture buffers.
    Preview,
    /// Telemetry samples, events and batches not yet posted.
    Telemetry,
}

impl Component {
    pub const ALL: [Component; 5] = [
        Component::Capture,
        Component::Encode,
        Component::Send,
        Component::Preview,
        Component::Telemetry,
    ];

    fn index(self) -> usize {
        self as usize
    }

    /// The diagnostics row label.
    pub fn label(self) -> &'static str {
        match self {
            Component::Capture => "capture",
            Component::Encode => "encode",
            Component::Send => "send",
            Component::Preview => "preview",
            Component::Telemetry => "telemetry",
        }
    }
}

const N: usize = Component::ALL.len();
static GAUGES: [AtomicU64; N] = [const { AtomicU64::new(0) }; N];
static REPORTED: [AtomicBool; N] = [const { AtomicBool::new(false) }; N];

/// Sets a component's holding outright: for a pool or buffer whose size is
/// known when it is (re)built.
pub fn set(c: Component, bytes: u64) {
    REPORTED[c.index()].store(true, Ordering::Relaxed);
    GAUGES[c.index()].store(bytes, Ordering::Relaxed);
}

/// Adds to a component's holding: for a queue, at the push.
pub fn add(c: Component, bytes: u64) {
    REPORTED[c.index()].store(true, Ordering::Relaxed);
    GAUGES[c.index()].fetch_add(bytes, Ordering::Relaxed);
}

/// Takes back what [`add`] gave, saturating at zero: for a queue, at the pop
/// or the shed.
pub fn sub(c: Component, bytes: u64) {
    let _ = GAUGES[c.index()].fetch_update(Ordering::Relaxed, Ordering::Relaxed, |v| {
        Some(v.saturating_sub(bytes))
    });
}

/// Declares that a component reports, at zero, before it holds anything: a
/// queue that is empty is measured at 0, not unmeasured.
pub fn declare(c: Component) {
    REPORTED[c.index()].store(true, Ordering::Relaxed);
}

/// A component's holding, or `None` when it has never reported.
pub fn component(c: Component) -> Option<u64> {
    REPORTED[c.index()]
        .load(Ordering::Relaxed)
        .then(|| GAUGES[c.index()].load(Ordering::Relaxed))
}

/// Bytes accounted to a component for as long as this lives: one queued
/// item, subtracted when it is sent, shed or dropped — whichever path drops
/// it, which is what keeps a queue's gauge from drifting.
#[derive(Debug)]
pub struct Held {
    c: Component,
    bytes: u64,
}

impl Held {
    pub fn new(c: Component, bytes: u64) -> Self {
        add(c, bytes);
        Self { c, bytes }
    }
}

impl Drop for Held {
    fn drop(&mut self) {
        sub(self.c, self.bytes);
    }
}

// ---------------------------------------------------------------------------
// GPU (D5)
// ---------------------------------------------------------------------------

type GpuProbe = Box<dyn Fn() -> Option<u64> + Send + Sync>;

fn gpu_probe() -> &'static Mutex<Option<GpuProbe>> {
    static P: OnceLock<Mutex<Option<GpuProbe>>> = OnceLock::new();
    P.get_or_init(|| Mutex::new(None))
}

/// Installs the shell's GPU-memory probe: the bytes of video memory this
/// process holds, read from the adapter the shell captures and encodes on
/// (Windows: DXGI's per-process `QueryVideoMemoryInfo`). Replaces any
/// earlier probe; `None` from the probe means "not measurable right now".
/// Apple platforms need none: the kernel's graphics ledger is read here.
pub fn set_gpu_probe(probe: impl Fn() -> Option<u64> + Send + Sync + 'static) {
    *gpu_probe().lock().unwrap() = Some(Box::new(probe));
}

// ---------------------------------------------------------------------------
// The process (D2)
// ---------------------------------------------------------------------------

/// What the OS says the process holds.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct Process {
    /// The task manager's figure (module doc).
    pub footprint: u64,
    /// The OS's own peak of `footprint`, where it keeps one (Apple); else 0
    /// and [`snapshot`] keeps the peak it has seen.
    pub footprint_peak: u64,
    /// Every resident page, shared and file-backed ones included: Windows'
    /// working set, the resident size elsewhere.
    pub resident: u64,
    /// Committed private memory, resident or paged out (Windows' "Commit
    /// size"); 0 where the OS has no such figure. A leak that pages out
    /// leaves the working set and stays here.
    pub commit: u64,
    /// Graphics memory charged to the process by the kernel (Apple's
    /// graphics ledger); 0 elsewhere, where the shell's GPU probe answers.
    pub graphics: u64,
    /// Bytes left before the OS kills the process for memory (iOS jetsam);
    /// 0 where there is no such limit.
    pub limit_remaining: u64,
}

/// Reads the process's memory from the OS, or `None` where it cannot.
pub fn process() -> Option<Process> {
    os::process()
}

#[cfg(windows)]
mod os {
    use super::Process;

    #[repr(C)]
    #[allow(non_snake_case)]
    #[derive(Default)]
    struct ProcessMemoryCountersEx2 {
        cb: u32,
        PageFaultCount: u32,
        PeakWorkingSetSize: usize,
        WorkingSetSize: usize,
        QuotaPeakPagedPoolUsage: usize,
        QuotaPagedPoolUsage: usize,
        QuotaPeakNonPagedPoolUsage: usize,
        QuotaNonPagedPoolUsage: usize,
        PagefileUsage: usize,
        PeakPagefileUsage: usize,
        PrivateUsage: usize,
        PrivateWorkingSetSize: usize,
        SharedCommitUsage: u64,
    }

    #[link(name = "kernel32")]
    unsafe extern "system" {
        fn GetCurrentProcess() -> isize;
        fn K32GetProcessMemoryInfo(
            process: isize,
            counters: *mut core::ffi::c_void,
            cb: u32,
        ) -> i32;
    }

    pub fn process() -> Option<Process> {
        let mut c = ProcessMemoryCountersEx2::default();
        let full = std::mem::size_of::<ProcessMemoryCountersEx2>() as u32;
        c.cb = full;
        // SAFETY: `c` is a writable PROCESS_MEMORY_COUNTERS_EX2 of `cb` bytes.
        let mut ok =
            unsafe { K32GetProcessMemoryInfo(GetCurrentProcess(), (&raw mut c).cast(), full) } != 0;
        let mut private_ws = ok;
        if !ok {
            // Before Windows 10 1809 there is no EX2: ask for EX, which ends
            // at PrivateUsage, and stand that in for the private working set.
            let ex = std::mem::offset_of!(ProcessMemoryCountersEx2, PrivateWorkingSetSize) as u32;
            c = ProcessMemoryCountersEx2 {
                cb: ex,
                ..Default::default()
            };
            // SAFETY: as above; `ex` is no larger than the struct.
            ok = unsafe { K32GetProcessMemoryInfo(GetCurrentProcess(), (&raw mut c).cast(), ex) }
                != 0;
            private_ws = false;
        }
        if !ok {
            return None;
        }
        Some(Process {
            footprint: if private_ws {
                c.PrivateWorkingSetSize as u64
            } else {
                c.PrivateUsage as u64
            },
            footprint_peak: 0,
            resident: c.WorkingSetSize as u64,
            commit: c.PrivateUsage as u64,
            graphics: 0,
            limit_remaining: 0,
        })
    }
}

#[cfg(any(target_os = "macos", target_os = "ios"))]
mod os {
    use super::Process;

    /// `task_vm_info` through revision 4 (xnu `osfmk/mach/task_info.h`),
    /// declared under `#pragma pack(4)` there and so here. Asking for fewer
    /// fields than the kernel has is how the ABI is meant to be used: the
    /// kernel fills what `count` covers.
    #[repr(C, packed(4))]
    #[derive(Default)]
    struct TaskVmInfo {
        virtual_size: u64,
        region_count: i32,
        page_size: i32,
        resident_size: u64,
        resident_size_peak: u64,
        device: u64,
        device_peak: u64,
        internal: u64,
        internal_peak: u64,
        external: u64,
        external_peak: u64,
        reusable: u64,
        reusable_peak: u64,
        purgeable_volatile_pmap: u64,
        purgeable_volatile_resident: u64,
        purgeable_volatile_virtual: u64,
        compressed: u64,
        compressed_peak: u64,
        compressed_lifetime: u64,
        // rev 1
        phys_footprint: u64,
        // rev 2
        min_address: u64,
        max_address: u64,
        // rev 3
        ledger_phys_footprint_peak: i64,
        ledger_purgeable_nonvolatile: i64,
        ledger_purgeable_novolatile_compressed: i64,
        ledger_purgeable_volatile: i64,
        ledger_purgeable_volatile_compressed: i64,
        ledger_tag_network_nonvolatile: i64,
        ledger_tag_network_nonvolatile_compressed: i64,
        ledger_tag_network_volatile: i64,
        ledger_tag_network_volatile_compressed: i64,
        ledger_tag_media_footprint: i64,
        ledger_tag_media_footprint_compressed: i64,
        ledger_tag_media_nofootprint: i64,
        ledger_tag_media_nofootprint_compressed: i64,
        ledger_tag_graphics_footprint: i64,
        ledger_tag_graphics_footprint_compressed: i64,
        ledger_tag_graphics_nofootprint: i64,
        ledger_tag_graphics_nofootprint_compressed: i64,
        ledger_tag_neural_footprint: i64,
        ledger_tag_neural_footprint_compressed: i64,
        ledger_tag_neural_nofootprint: i64,
        ledger_tag_neural_nofootprint_compressed: i64,
        // rev 4
        limit_bytes_remaining: u64,
    }

    const TASK_VM_INFO: i32 = 22;
    /// `TASK_VM_INFO_REV1_COUNT`: through `phys_footprint`.
    const REV1_COUNT: u32 = (std::mem::offset_of!(TaskVmInfo, min_address) / 4) as u32;
    const REV3_COUNT: u32 = (std::mem::offset_of!(TaskVmInfo, limit_bytes_remaining) / 4) as u32;

    unsafe extern "C" {
        static mach_task_self_: u32;
        fn task_info(task: u32, flavor: i32, info: *mut i32, count: *mut u32) -> i32;
    }

    pub fn process() -> Option<Process> {
        let mut info = TaskVmInfo::default();
        let mut count = (std::mem::size_of::<TaskVmInfo>() / 4) as u32;
        // SAFETY: `info` is writable for `count` natural_t words.
        let kr = unsafe {
            task_info(
                mach_task_self_,
                TASK_VM_INFO,
                (&raw mut info).cast(),
                &mut count,
            )
        };
        if kr != 0 || count < REV1_COUNT {
            return None;
        }
        let rev3 = count >= REV3_COUNT;
        let rev4 = count >= (std::mem::size_of::<TaskVmInfo>() / 4) as u32;
        let graphics = if rev3 {
            (info.ledger_tag_graphics_footprint + info.ledger_tag_graphics_footprint_compressed)
                .max(0) as u64
        } else {
            0
        };
        Some(Process {
            footprint: info.phys_footprint,
            footprint_peak: if rev3 {
                info.ledger_phys_footprint_peak.max(0) as u64
            } else {
                0
            },
            resident: info.resident_size,
            commit: 0,
            graphics,
            limit_remaining: if rev4 { info.limit_bytes_remaining } else { 0 },
        })
    }
}

#[cfg(target_os = "linux")]
mod os {
    use super::Process;

    pub fn process() -> Option<Process> {
        parse_status(&std::fs::read_to_string("/proc/self/status").ok()?)
    }

    /// `/proc/self/status`: RssAnon + RssShmem is the private footprint
    /// (memfd and DMA-BUF-backed PipeWire buffers are shmem), VmRSS adds the
    /// file-backed pages, the libraries' code above all.
    pub(super) fn parse_status(s: &str) -> Option<Process> {
        let kb = |key: &str| -> Option<u64> {
            let line = s.lines().find(|l| l.starts_with(key))?;
            let v = line[key.len()..].trim().trim_end_matches("kB").trim();
            v.parse::<u64>().ok().map(|k| k * 1024)
        };
        let anon = kb("RssAnon:")?;
        Some(Process {
            footprint: anon + kb("RssShmem:").unwrap_or(0),
            footprint_peak: 0,
            resident: kb("VmRSS:").unwrap_or(0),
            commit: 0,
            graphics: 0,
            limit_remaining: 0,
        })
    }
}

#[cfg(not(any(windows, target_os = "macos", target_os = "ios", target_os = "linux")))]
mod os {
    pub fn process() -> Option<super::Process> {
        None
    }
}

// ---------------------------------------------------------------------------
// The sample (D6)
// ---------------------------------------------------------------------------

/// One reading of everything above, as [`crate::stats::Stats`] carries it
/// ([`crate::stats::Stats::apply_memory`]).
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Snapshot {
    pub process: Option<Process>,
    /// The peak footprint: the OS's where it keeps one, else the highest
    /// this process has sampled.
    pub footprint_peak: u64,
    pub heap: Option<(u64, u64)>,
    pub gpu: Option<u64>,
    pub components: [Option<u64>; N],
}

static PEAK_SEEN: AtomicU64 = AtomicU64::new(0);

/// Reads everything above once. The peak footprint is the OS's where it
/// keeps one, else the highest any snapshot has seen: the shells read at
/// their 1 Hz tick, so a spike between two reads is missed — the cost of
/// reading once a second, named in docs/77 D6.
pub fn snapshot() -> Snapshot {
    let process = process();
    let mut footprint_peak = 0;
    if let Some(p) = &process {
        let seen = PEAK_SEEN
            .fetch_max(p.footprint, Ordering::Relaxed)
            .max(p.footprint);
        footprint_peak = p.footprint_peak.max(seen);
    }
    let gpu = match process {
        Some(p) if p.graphics > 0 => Some(p.graphics),
        _ => gpu_probe()
            .lock()
            .unwrap()
            .as_ref()
            .and_then(|probe| probe()),
    };
    Snapshot {
        process,
        footprint_peak,
        heap: heap(),
        gpu,
        components: Component::ALL.map(component),
    }
}

/// Mebibytes, for a log line or a diagnostics row.
pub fn mib(bytes: u64) -> f64 {
    bytes as f64 / (1024.0 * 1024.0)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_process_is_measured_on_every_desktop_os() {
        let p = process().expect("this OS reports process memory");
        assert!(p.footprint > 0, "{p:?}");
        assert!(p.resident >= p.footprint / 2, "{p:?}");
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn linux_footprint_is_anonymous_plus_shared() {
        let s = "Name:\tx\nVmHWM:\t  9000 kB\nVmRSS:\t  8000 kB\nRssAnon:\t  3000 kB\nRssFile:\t  4000 kB\nRssShmem:\t  1000 kB\n";
        let p = os::parse_status(s).unwrap();
        assert_eq!(p.footprint, 4000 * 1024);
        assert_eq!(p.resident, 8000 * 1024);
        assert_eq!(
            os::parse_status("Name:\tx\n"),
            None,
            "no RssAnon, no reading"
        );
    }

    // The gauges are process-wide statics shared with every test in this
    // binary: these use the two only a shell reports (Capture, Preview).
    #[test]
    fn a_component_is_absent_until_it_reports_and_a_held_item_gives_back() {
        assert_eq!(component(Component::Capture), None);
        declare(Component::Capture);
        assert_eq!(component(Component::Capture), Some(0));
        {
            let _a = Held::new(Component::Capture, 1000);
            let _b = Held::new(Component::Capture, 24);
            assert_eq!(component(Component::Capture), Some(1024));
        }
        assert_eq!(component(Component::Capture), Some(0));
        sub(Component::Capture, 5);
        assert_eq!(
            component(Component::Capture),
            Some(0),
            "saturates, never wraps"
        );
    }

    #[test]
    fn set_replaces_and_add_accumulates() {
        set(Component::Preview, 500);
        add(Component::Preview, 20);
        assert_eq!(component(Component::Preview), Some(520));
        set(Component::Preview, 7);
        assert_eq!(component(Component::Preview), Some(7));
    }

    #[test]
    fn the_snapshot_keeps_the_highest_footprint_it_saw() {
        let first = snapshot();
        let now = first.process.expect("measured").footprint;
        assert!(first.footprint_peak >= now);
        PEAK_SEEN.fetch_max(now * 4, Ordering::Relaxed);
        assert!(
            snapshot().footprint_peak >= now * 4,
            "an earlier, higher reading stays the peak"
        );
    }

    // One test for both, because both read the same statics.
    #[test]
    fn the_heap_is_unmeasured_without_the_counting_allocator_which_balances() {
        // The test binary runs on the plain system allocator.
        assert_eq!(heap(), None);
        let a = CountingAlloc;
        let before = HEAP_LIVE.load(Ordering::Relaxed);
        let layout = Layout::from_size_align(4096, 8).unwrap();
        // SAFETY: a matched alloc/realloc/dealloc of the same block.
        unsafe {
            let p = a.alloc(layout);
            assert!(!p.is_null());
            assert_eq!(HEAP_LIVE.load(Ordering::Relaxed) - before, 4096);
            let p = a.realloc(p, layout, 100);
            assert_eq!(HEAP_LIVE.load(Ordering::Relaxed) - before, 100);
            a.dealloc(p, Layout::from_size_align(100, 8).unwrap());
        }
        assert_eq!(HEAP_LIVE.load(Ordering::Relaxed), before);
        assert!(HEAP_PEAK.load(Ordering::Relaxed) >= 4096);
    }
}
