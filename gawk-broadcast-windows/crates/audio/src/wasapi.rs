//! WASAPI capture, both modes (docs/38 D8):
//!
//! - **Mode 1** — process loopback via `ActivateAudioInterfaceAsync` with
//!   `PROCESS_LOOPBACK` activation params, include-target-process-tree.
//!   The capture format is OURS to specify: the mix is rendered into
//!   48 kHz / stereo / f32 directly, no resampler dependency. Capture
//!   follows the *process*, so a headphone/speaker switch is a non-event
//!   (V-3b verifies on hardware).
//! - **Mode 2** — endpoint loopback on the default render device with
//!   `AUTOCONVERTPCM | SRC_DEFAULT_QUALITY` requesting the same format.
//!   Default-device changes are followed via `IMMNotificationClient`:
//!   tear down and re-open on the new default; the re-open gap is dropped
//!   packets, which the wire model already treats as truth.
//!
//! Audio never fails a broadcast: every error here surfaces as a state
//! change the shell reports, video keeps running (R25 Decision 6).

use crate::AudioMode;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, mpsc};
use windows::Win32::Foundation::WAIT_OBJECT_0;
use windows::Win32::Media::Audio::{
    AUDCLNT_BUFFERFLAGS_SILENT, AUDCLNT_BUFFERFLAGS_TIMESTAMP_ERROR, AUDCLNT_SHAREMODE_SHARED,
    AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM, AUDCLNT_STREAMFLAGS_LOOPBACK,
    AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY, AUDIOCLIENT_ACTIVATION_PARAMS,
    AUDIOCLIENT_ACTIVATION_PARAMS_0, AUDIOCLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK,
    AUDIOCLIENT_PROCESS_LOOPBACK_PARAMS, EDataFlow, ERole, IActivateAudioInterfaceAsyncOperation,
    IActivateAudioInterfaceCompletionHandler, IActivateAudioInterfaceCompletionHandler_Impl,
    IAudioCaptureClient, IAudioClient, IMMDevice, IMMDeviceEnumerator, IMMNotificationClient,
    IMMNotificationClient_Impl, MMDeviceEnumerator,
    PROCESS_LOOPBACK_MODE_INCLUDE_TARGET_PROCESS_TREE, VIRTUAL_AUDIO_DEVICE_PROCESS_LOOPBACK,
    WAVEFORMATEX, eConsole, eRender,
};
use windows::Win32::Media::Multimedia::WAVE_FORMAT_IEEE_FLOAT;
use windows::Win32::System::Com::StructuredStorage::PROPVARIANT;
use windows::Win32::System::Com::{
    BLOB, CLSCTX_ALL, COINIT_MULTITHREADED, CoCreateInstance, CoInitializeEx,
};
use windows::core::{Interface, PCWSTR, Result, implement};

use gawk_engine::media::{AUDIO_CHANNELS, AUDIO_SAMPLE_RATE};

/// One capture packet as delivered, pre-framing. QPC is `None` when the
/// device flags a timestamp error or reports zero (the V-4 hedge).
pub struct CapturePacket {
    pub interleaved: Vec<f32>,
    pub qpc_100ns: Option<i64>,
    /// Device-buffered frames ahead of (and including) this packet at read
    /// time — the arrival-stamping fallback's subtraction term.
    pub buffered_ahead_samples: usize,
}

/// The requested capture format: 48 kHz stereo f32, R25's contract.
fn capture_format() -> WAVEFORMATEX {
    let block_align = u16::from(AUDIO_CHANNELS) * 4;
    WAVEFORMATEX {
        wFormatTag: WAVE_FORMAT_IEEE_FLOAT as u16,
        nChannels: u16::from(AUDIO_CHANNELS),
        nSamplesPerSec: AUDIO_SAMPLE_RATE,
        nAvgBytesPerSec: AUDIO_SAMPLE_RATE * u32::from(block_align),
        nBlockAlign: block_align,
        wBitsPerSample: 32,
        cbSize: 0,
    }
}

#[implement(IActivateAudioInterfaceCompletionHandler)]
struct ActivateHandler {
    tx: std::sync::Mutex<Option<mpsc::Sender<()>>>,
}

impl IActivateAudioInterfaceCompletionHandler_Impl for ActivateHandler_Impl {
    fn ActivateCompleted(
        &self,
        _op: windows::core::Ref<'_, IActivateAudioInterfaceAsyncOperation>,
    ) -> Result<()> {
        if let Some(tx) = self.tx.lock().unwrap().take() {
            let _ = tx.send(());
        }
        Ok(())
    }
}

/// Watches the default render endpoint (mode 2's device-follow); sets its
/// flag when the default changes so the pump re-opens.
#[implement(IMMNotificationClient)]
struct DeviceWatch {
    changed: Arc<AtomicBool>,
}

impl IMMNotificationClient_Impl for DeviceWatch_Impl {
    fn OnDeviceStateChanged(
        &self,
        _id: &PCWSTR,
        _state: windows::Win32::Media::Audio::DEVICE_STATE,
    ) -> Result<()> {
        Ok(())
    }
    fn OnDeviceAdded(&self, _id: &PCWSTR) -> Result<()> {
        Ok(())
    }
    fn OnDeviceRemoved(&self, _id: &PCWSTR) -> Result<()> {
        Ok(())
    }
    fn OnDefaultDeviceChanged(&self, flow: EDataFlow, role: ERole, _id: &PCWSTR) -> Result<()> {
        if flow == eRender && role == eConsole {
            self.changed.store(true, Ordering::SeqCst);
        }
        Ok(())
    }
    fn OnPropertyValueChanged(
        &self,
        _id: &PCWSTR,
        _key: &windows::Win32::Foundation::PROPERTYKEY,
    ) -> Result<()> {
        Ok(())
    }
}

/// Opens the audio client for `mode`. Blocking (the async activation is
/// waited out); call off the GUI thread.
fn open_client(mode: AudioMode) -> std::result::Result<IAudioClient, String> {
    match mode {
        AudioMode::ProcessLoopback { pid } => open_process_loopback(pid),
        AudioMode::SystemLoopback => open_endpoint_loopback(),
        AudioMode::Off => Err("audio is off".into()),
    }
}

fn open_process_loopback(pid: u32) -> std::result::Result<IAudioClient, String> {
    let params = AUDIOCLIENT_ACTIVATION_PARAMS {
        ActivationType: AUDIOCLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK,
        Anonymous: AUDIOCLIENT_ACTIVATION_PARAMS_0 {
            ProcessLoopbackParams: AUDIOCLIENT_PROCESS_LOOPBACK_PARAMS {
                TargetProcessId: pid,
                ProcessLoopbackMode: PROCESS_LOOPBACK_MODE_INCLUDE_TARGET_PROCESS_TREE,
            },
        },
    };
    let blob = BLOB {
        cbSize: std::mem::size_of::<AUDIOCLIENT_ACTIVATION_PARAMS>() as u32,
        pBlobData: &params as *const _ as *mut u8,
    };
    let prop = PROPVARIANT {
        Anonymous: windows::Win32::System::Com::StructuredStorage::PROPVARIANT_0 {
            Anonymous: std::mem::ManuallyDrop::new(
                windows::Win32::System::Com::StructuredStorage::PROPVARIANT_0_0 {
                    vt: windows::Win32::System::Variant::VT_BLOB,
                    Anonymous: windows::Win32::System::Com::StructuredStorage::PROPVARIANT_0_0_0 {
                        blob,
                    },
                    ..Default::default()
                },
            ),
        },
    };

    let (tx, rx) = mpsc::channel();
    let handler: IActivateAudioInterfaceCompletionHandler = ActivateHandler {
        tx: std::sync::Mutex::new(Some(tx)),
    }
    .into();

    let op = unsafe {
        windows::Win32::Media::Audio::ActivateAudioInterfaceAsync(
            VIRTUAL_AUDIO_DEVICE_PROCESS_LOOPBACK,
            &IAudioClient::IID,
            Some(&prop),
            &handler,
        )
    }
    .map_err(|e| format!("process-loopback activation: {e}"))?;

    rx.recv_timeout(std::time::Duration::from_secs(5))
        .map_err(|_| "process-loopback activation timed out".to_string())?;

    let mut hr = windows::core::HRESULT(0);
    let mut unk: Option<windows::core::IUnknown> = None;
    unsafe {
        op.GetActivateResult(&mut hr, &mut unk)
            .map_err(|e| format!("process-loopback activate result: {e}"))?;
    }
    hr.ok()
        .map_err(|e| format!("process-loopback activation failed: {e}"))?;
    let client: IAudioClient = unk
        .ok_or("activation returned no interface")?
        .cast()
        .map_err(|e| format!("activation interface cast: {e}"))?;

    let fmt = capture_format();
    unsafe {
        client
            .Initialize(
                AUDCLNT_SHAREMODE_SHARED,
                AUDCLNT_STREAMFLAGS_LOOPBACK,
                2_000_000, // 200 ms device buffer
                0,
                &fmt,
                None,
            )
            .map_err(|e| format!("process-loopback initialize: {e}"))?;
    }
    Ok(client)
}

fn open_endpoint_loopback() -> std::result::Result<IAudioClient, String> {
    unsafe {
        let enumerator: IMMDeviceEnumerator =
            CoCreateInstance(&MMDeviceEnumerator, None, CLSCTX_ALL)
                .map_err(|e| format!("device enumerator: {e}"))?;
        let device: IMMDevice = enumerator
            .GetDefaultAudioEndpoint(eRender, eConsole)
            .map_err(|e| format!("no default render device: {e}"))?;
        let client: IAudioClient = device
            .Activate(CLSCTX_ALL, None)
            .map_err(|e| format!("audio client activate: {e}"))?;
        let fmt = capture_format();
        client
            .Initialize(
                AUDCLNT_SHAREMODE_SHARED,
                AUDCLNT_STREAMFLAGS_LOOPBACK
                    | AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM
                    | AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY,
                2_000_000,
                0,
                &fmt,
                None,
            )
            .map_err(|e| format!("endpoint loopback initialize: {e}"))?;
        Ok(client)
    }
}

/// A running capture: one pump thread polling the capture client, packets
/// out through the callback. Mode 2 re-opens on default-device change.
pub struct LoopbackCapture {
    stop: Arc<AtomicBool>,
    thread: Option<std::thread::JoinHandle<()>>,
}

impl LoopbackCapture {
    /// `on_packet` runs on the pump thread; `on_error` fires once if the
    /// capture dies (audio subordination: the shell drops audio, video
    /// runs on).
    pub fn start(
        mode: AudioMode,
        on_packet: Box<dyn FnMut(CapturePacket) + Send>,
        on_error: Box<dyn FnOnce(String) + Send>,
    ) -> std::result::Result<Self, String> {
        let stop = Arc::new(AtomicBool::new(false));
        let stop2 = stop.clone();
        let thread = std::thread::Builder::new()
            .name("wasapi-pump".into())
            .spawn(move || {
                unsafe {
                    let _ = CoInitializeEx(None, COINIT_MULTITHREADED);
                }
                if let Err(e) = pump(mode, &stop2, on_packet) {
                    on_error(e);
                }
            })
            .map_err(|e| format!("spawn wasapi pump: {e}"))?;
        Ok(Self {
            stop,
            thread: Some(thread),
        })
    }

    pub fn stop(mut self) {
        self.halt();
    }

    fn halt(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        if let Some(t) = self.thread.take() {
            let _ = t.join();
        }
    }
}

impl Drop for LoopbackCapture {
    fn drop(&mut self) {
        self.halt();
    }
}

fn pump(
    mode: AudioMode,
    stop: &AtomicBool,
    mut on_packet: Box<dyn FnMut(CapturePacket) + Send>,
) -> std::result::Result<(), String> {
    // Device-follow only applies to mode 2 (mode 1 follows the process).
    let device_changed = Arc::new(AtomicBool::new(false));
    let mut _watch_registration: Option<(IMMDeviceEnumerator, IMMNotificationClient)> = None;
    if mode == AudioMode::SystemLoopback {
        let enumerator: IMMDeviceEnumerator =
            unsafe { CoCreateInstance(&MMDeviceEnumerator, None, CLSCTX_ALL) }
                .map_err(|e| format!("device enumerator: {e}"))?;
        let watch: IMMNotificationClient = DeviceWatch {
            changed: device_changed.clone(),
        }
        .into();
        unsafe {
            enumerator
                .RegisterEndpointNotificationCallback(&watch)
                .map_err(|e| format!("device-change registration: {e}"))?;
        }
        _watch_registration = Some((enumerator, watch));
    }

    'reopen: loop {
        let client = open_client(mode)?;
        let capture: IAudioCaptureClient =
            unsafe { client.GetService() }.map_err(|e| format!("capture client: {e}"))?;
        unsafe {
            client.Start().map_err(|e| format!("capture start: {e}"))?;
        }

        loop {
            if stop.load(Ordering::SeqCst) {
                unsafe {
                    let _ = client.Stop();
                }
                return Ok(());
            }
            if device_changed.swap(false, Ordering::SeqCst) {
                // Mode 2: the default sink moved — capture the new one.
                // The gap is dropped packets, deliberately.
                unsafe {
                    let _ = client.Stop();
                }
                continue 'reopen;
            }

            let buffered = unsafe { client.GetCurrentPadding() }.unwrap_or(0) as usize;
            loop {
                let next = unsafe { capture.GetNextPacketSize() }
                    .map_err(|e| format!("capture next-packet: {e}"))?;
                if next == 0 {
                    break;
                }
                let mut data: *mut u8 = std::ptr::null_mut();
                let mut frames = 0u32;
                let mut flags = 0u32;
                let mut qpc = 0u64;
                unsafe {
                    capture
                        .GetBuffer(&mut data, &mut frames, &mut flags, None, Some(&mut qpc))
                        .map_err(|e| format!("capture get-buffer: {e}"))?;
                }
                let n = frames as usize * usize::from(AUDIO_CHANNELS);
                let interleaved = if flags & AUDCLNT_BUFFERFLAGS_SILENT.0 as u32 != 0 {
                    vec![0f32; n]
                } else {
                    unsafe { std::slice::from_raw_parts(data as *const f32, n) }.to_vec()
                };
                let qpc_ok = flags & AUDCLNT_BUFFERFLAGS_TIMESTAMP_ERROR.0 as u32 == 0 && qpc != 0;
                on_packet(CapturePacket {
                    interleaved,
                    qpc_100ns: qpc_ok.then_some(qpc as i64),
                    buffered_ahead_samples: buffered,
                });
                unsafe {
                    capture
                        .ReleaseBuffer(frames)
                        .map_err(|e| format!("capture release-buffer: {e}"))?;
                }
            }
            std::thread::sleep(std::time::Duration::from_millis(5));
        }
    }
}

/// The R25 Decision 6 probe: a short self-contained open+encode trial — no
/// picker, no GPU — before the broadcast goes live. Deliberately does NOT
/// require packets: loopback delivers nothing while the source is silent,
/// and silence is not a failure.
pub fn probe(mode: AudioMode) -> std::result::Result<(), String> {
    unsafe {
        let _ = CoInitializeEx(None, COINIT_MULTITHREADED);
    }
    let client = open_client(mode)?;
    unsafe {
        client.Start().map_err(|e| format!("probe start: {e}"))?;
        std::thread::sleep(std::time::Duration::from_millis(50));
        let _ = client.Stop();
    }
    // And the encoder half of the trial.
    let mut enc = crate::opusenc::OpusEncoder::new()?;
    enc.encode(&vec![0f32; crate::opusenc::FRAME_INTERLEAVED_LEN])?;
    Ok(())
}

// Silence "unused" for the wait constant pulled in for future event-driven
// capture; polling is the shipped model.
const _: u32 = WAIT_OBJECT_0.0;
