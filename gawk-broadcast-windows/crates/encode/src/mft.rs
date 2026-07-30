//! Media Foundation: hardware-only enumeration, the encoder session, and
//! the real trial runner behind the cascade gate (WB4, docs/38 D9).
//!
//! Hardware H.264 encoder MFTs are async MFTs: they emit
//! `METransformNeedInput` / `METransformHaveOutput` events and we react —
//! one pump thread per session runs that loop. Software MFTs are never
//! requested and never used (G3).

use crate::cascade::{Candidate, TrialAu, TrialRun, TrialRunner};
use std::sync::mpsc;
use windows::Win32::Graphics::Direct3D11::{
    D3D11_BIND_SHADER_RESOURCE, D3D11_SUBRESOURCE_DATA, D3D11_TEXTURE2D_DESC, D3D11_USAGE_DEFAULT,
    ID3D11Device, ID3D11Texture2D,
};
use windows::Win32::Graphics::Dxgi::Common::{DXGI_FORMAT_NV12, DXGI_SAMPLE_DESC};
use windows::Win32::Media::MediaFoundation::{
    CODECAPI_AVEncCommonMaxBitRate, CODECAPI_AVEncCommonMeanBitRate,
    CODECAPI_AVEncCommonRateControlMode, CODECAPI_AVEncMPVDefaultBPictureCount,
    CODECAPI_AVEncMPVGOPSize, CODECAPI_AVEncVideoForceKeyFrame, CODECAPI_AVLowLatencyMode,
    ICodecAPI, IMFActivate, IMFDXGIDeviceManager, IMFMediaEvent, IMFMediaEventGenerator, IMFSample,
    IMFTransform, METransformDrainComplete, METransformHaveOutput, METransformNeedInput,
    MF_E_TRANSFORM_STREAM_CHANGE, MF_EVENT_FLAG_NONE, MF_MT_AVG_BITRATE, MF_MT_FRAME_RATE,
    MF_MT_FRAME_SIZE, MF_MT_INTERLACE_MODE, MF_MT_MAJOR_TYPE, MF_MT_MPEG_SEQUENCE_HEADER,
    MF_MT_SUBTYPE, MF_TRANSFORM_ASYNC_UNLOCK, MF_VERSION, MFCreateDXGIDeviceManager,
    MFCreateDXGISurfaceBuffer, MFCreateMediaType, MFCreateSample, MFMediaType_Video,
    MFSTARTUP_FULL, MFStartup, MFT_CATEGORY_VIDEO_ENCODER, MFT_ENUM_FLAG_HARDWARE,
    MFT_ENUM_FLAG_SORTANDFILTER, MFT_FRIENDLY_NAME_Attribute, MFT_MESSAGE_COMMAND_DRAIN,
    MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, MFT_MESSAGE_NOTIFY_END_OF_STREAM,
    MFT_MESSAGE_NOTIFY_START_OF_STREAM, MFT_MESSAGE_SET_D3D_MANAGER, MFT_OUTPUT_DATA_BUFFER,
    MFT_REGISTER_TYPE_INFO, MFTEnumEx, MFVideoFormat_H264, MFVideoFormat_NV12,
    MFVideoInterlace_Progressive, eAVEncCommonRateControlMode_PeakConstrainedVBR,
};
use windows::Win32::System::Variant::VARIANT;
use windows::core::{GUID, Interface, Result};

/// One-time Media Foundation startup; safe to call repeatedly.
pub fn startup() -> Result<()> {
    static ONCE: std::sync::Once = std::sync::Once::new();
    let mut result = Ok(());
    ONCE.call_once(|| {
        result = unsafe { MFStartup(MF_VERSION, MFSTARTUP_FULL) };
    });
    result
}

/// A hardware H.264 encoder MFT, enumerated but NOT yet accepted — the
/// trial gate decides that (enumeration is not acceptance, D9).
pub struct MftEntry {
    pub activate: IMFActivate,
    pub name: String,
}

// IMFActivate is a free-threaded factory object.
unsafe impl Send for MftEntry {}

/// Hardware-only enumeration: the software flag is never passed, so a
/// machine with no hardware encoder enumerates EMPTY and the cascade
/// refuses — it cannot fall back to software by construction.
pub fn enumerate_hardware() -> Result<Vec<MftEntry>> {
    startup()?;
    let output = MFT_REGISTER_TYPE_INFO {
        guidMajorType: MFMediaType_Video,
        guidSubtype: MFVideoFormat_H264,
    };
    let mut activates: *mut Option<IMFActivate> = std::ptr::null_mut();
    let mut count = 0u32;
    unsafe {
        MFTEnumEx(
            MFT_CATEGORY_VIDEO_ENCODER,
            MFT_ENUM_FLAG_HARDWARE | MFT_ENUM_FLAG_SORTANDFILTER,
            None,
            Some(&output),
            &mut activates,
            &mut count,
        )?;
    }
    let mut out = Vec::with_capacity(count as usize);
    for i in 0..count as usize {
        let slot = unsafe { &*activates.add(i) };
        if let Some(activate) = slot.clone() {
            let name = unsafe {
                let mut buf = [0u16; 256];
                let mut len = 0u32;
                match activate.GetString(&MFT_FRIENDLY_NAME_Attribute, &mut buf, Some(&mut len)) {
                    Ok(()) => String::from_utf16_lossy(&buf[..(len as usize).min(buf.len())]),
                    Err(_) => format!("hardware H.264 MFT #{i}"),
                }
            };
            out.push(MftEntry { activate, name });
        }
    }
    unsafe {
        windows::Win32::System::Com::CoTaskMemFree(Some(activates as *const _));
    }
    Ok(out)
}

/// The cascade's candidate list (portable ids) for the enumerated MFTs.
pub fn candidates(entries: &[MftEntry]) -> Vec<Candidate> {
    entries
        .iter()
        .map(|e| Candidate { id: e.name.clone() })
        .collect()
}

/// The rung an encoder session runs at.
#[derive(Debug, Clone, Copy)]
pub struct EncoderParams {
    pub width: u32,
    pub height: u32,
    pub fps: u32,
    /// User-facing peak (D9: peak-constrained VBR, mean = 75 %).
    pub peak_bitrate_bps: u32,
    /// GOP length in frames (fps/2 for the 500 ms target).
    pub gop_frames: u32,
}

/// One encoded access unit off the MFT, timestamped in µs on whatever
/// timeline the inputs were stamped with (VFR pass-through).
pub struct EncodedAu {
    pub data: Vec<u8>,
    pub time_us: u64,
    pub keyframe: bool,
}

/// One frame into the encoder.
pub struct EncoderInput {
    pub texture: ID3D11Texture2D,
    pub time_us: u64,
    pub duration_us: u64,
    /// Resume re-prime (D5): ask the encoder for an IDR on this frame.
    pub force_idr: bool,
}

// Textures on the shared multithread-protected device.
unsafe impl Send for EncoderInput {}

/// A live hardware encoder session: one pump thread reacting to the async
/// MFT's events, frames in through [`EncoderSession::send`], AUs out
/// through the callback (which runs on the pump thread).
pub struct EncoderSession {
    input_tx: Option<mpsc::Sender<EncoderInput>>,
    pump: Option<std::thread::JoinHandle<Result<()>>>,
    /// SPS/PPS from the negotiated output type ("" when absent) — the
    /// prepend cache and codec-string fallback.
    pub sequence_header: Vec<u8>,
    codec_api: ICodecAPI,
}

// ICodecAPI on hardware MFTs is free-threaded; force-IDR is a single
// property store write.
unsafe impl Send for EncoderSession {}

// Hardware encoder MFTs are free-threaded COM objects and the pump thread
// is the ONLY thread touching the transform after start; ICodecAPI's
// force-IDR write is a property-store set, safe cross-thread.
unsafe impl Send for Pump {}

struct Pump {
    transform: IMFTransform,
    events: IMFMediaEventGenerator,
    codec_api: ICodecAPI,
    input_rx: mpsc::Receiver<EncoderInput>,
    on_output: Box<dyn FnMut(EncodedAu) + Send>,
}

impl EncoderSession {
    /// Activates and configures `entry` on `device`, applying the D9
    /// invariant knobs before streaming starts. `on_output` runs on the
    /// pump thread — hand off, don't block.
    pub fn start(
        entry: &MftEntry,
        device: &ID3D11Device,
        params: EncoderParams,
        on_output: Box<dyn FnMut(EncodedAu) + Send>,
    ) -> Result<Self> {
        startup()?;
        let transform: IMFTransform = unsafe { entry.activate.ActivateObject()? };

        // Async MFTs must be explicitly unlocked; that this is required is
        // also the "it really is a hardware MFT" sanity check.
        let attrs = unsafe { transform.GetAttributes()? };
        unsafe { attrs.SetUINT32(&MF_TRANSFORM_ASYNC_UNLOCK, 1)? };

        // The shared D3D device: zero-copy input (D9).
        let mut reset_token = 0u32;
        let mut manager: Option<IMFDXGIDeviceManager> = None;
        unsafe {
            MFCreateDXGIDeviceManager(&mut reset_token, &mut manager)?;
        }
        let manager = manager.expect("MFCreateDXGIDeviceManager succeeded without a manager");
        unsafe {
            manager.ResetDevice(device, reset_token)?;
            transform.ProcessMessage(MFT_MESSAGE_SET_D3D_MANAGER, manager.as_raw() as usize)?;
        }

        // The CodecAPI knobs — each row of the D9 table. Set BEFORE the
        // output type so rate control shapes negotiation.
        let codec_api: ICodecAPI = transform.cast()?;
        unsafe {
            codec_api.SetValue(
                &CODECAPI_AVEncCommonRateControlMode,
                &VARIANT::from(eAVEncCommonRateControlMode_PeakConstrainedVBR.0 as u32),
            )?;
            codec_api.SetValue(
                &CODECAPI_AVEncCommonMaxBitRate,
                &VARIANT::from(params.peak_bitrate_bps),
            )?;
            codec_api.SetValue(
                &CODECAPI_AVEncCommonMeanBitRate,
                &VARIANT::from(params.peak_bitrate_bps / 4 * 3),
            )?;
            codec_api.SetValue(&CODECAPI_AVEncMPVGOPSize, &VARIANT::from(params.gop_frames))?;
            // Best-effort: not every vendor exposes every knob; B-frames
            // and latency are VERIFIED by the trial gate regardless.
            let _ =
                codec_api.SetValue(&CODECAPI_AVEncMPVDefaultBPictureCount, &VARIANT::from(0u32));
            let _ = codec_api.SetValue(&CODECAPI_AVLowLatencyMode, &VARIANT::from(true));
        }

        // Output type first (encoder MFT convention), then input.
        unsafe {
            let out_type = MFCreateMediaType()?;
            out_type.SetGUID(&MF_MT_MAJOR_TYPE, &MFMediaType_Video)?;
            out_type.SetGUID(&MF_MT_SUBTYPE, &MFVideoFormat_H264)?;
            out_type.SetUINT32(&MF_MT_AVG_BITRATE, params.peak_bitrate_bps / 4 * 3)?;
            out_type.SetUINT64(
                &MF_MT_FRAME_SIZE,
                (u64::from(params.width) << 32) | u64::from(params.height),
            )?;
            // Nominal fps for rate-control budgeting ONLY; real timestamps
            // ride each sample (the Linux caps-carry-framerate lesson).
            out_type.SetUINT64(&MF_MT_FRAME_RATE, (u64::from(params.fps) << 32) | 1)?;
            out_type.SetUINT32(&MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive.0 as u32)?;
            transform.SetOutputType(0, &out_type, 0)?;

            let in_type = MFCreateMediaType()?;
            in_type.SetGUID(&MF_MT_MAJOR_TYPE, &MFMediaType_Video)?;
            in_type.SetGUID(&MF_MT_SUBTYPE, &MFVideoFormat_NV12)?;
            in_type.SetUINT64(
                &MF_MT_FRAME_SIZE,
                (u64::from(params.width) << 32) | u64::from(params.height),
            )?;
            in_type.SetUINT64(&MF_MT_FRAME_RATE, (u64::from(params.fps) << 32) | 1)?;
            transform.SetInputType(0, &in_type, 0)?;
        }

        let sequence_header = read_sequence_header(&transform).unwrap_or_default();

        let events: IMFMediaEventGenerator = transform.cast()?;
        unsafe {
            transform.ProcessMessage(MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, 0)?;
            transform.ProcessMessage(MFT_MESSAGE_NOTIFY_START_OF_STREAM, 0)?;
        }

        let (input_tx, input_rx) = mpsc::channel();
        let pump = Pump {
            transform,
            events,
            codec_api: codec_api.clone(),
            input_rx,
            on_output,
        };
        let handle = std::thread::Builder::new()
            .name("mft-pump".into())
            .spawn(move || pump.run())
            .expect("spawn mft pump");

        Ok(Self {
            input_tx: Some(input_tx),
            pump: Some(handle),
            sequence_header,
            codec_api,
        })
    }

    /// Queues one frame. Returns false when the pump has died — the caller
    /// surfaces that as a session error (cascade advance / broadcast end).
    pub fn send(&self, input: EncoderInput) -> bool {
        self.input_tx
            .as_ref()
            .is_some_and(|tx| tx.send(input).is_ok())
    }

    /// Asks for an IDR outside the frame flow (resume re-prime).
    pub fn force_idr(&self) {
        unsafe {
            let _ = self
                .codec_api
                .SetValue(&CODECAPI_AVEncVideoForceKeyFrame, &VARIANT::from(1u32));
        }
    }

    /// Closes the input, drains the encoder and joins the pump.
    pub fn finish(mut self) -> Result<()> {
        drop(self.input_tx.take());
        match self.pump.take() {
            Some(h) => h.join().unwrap_or(Ok(())),
            None => Ok(()),
        }
    }
}

impl Drop for EncoderSession {
    fn drop(&mut self) {
        drop(self.input_tx.take());
        if let Some(h) = self.pump.take() {
            let _ = h.join();
        }
    }
}

impl Pump {
    // The async-MFT contract: never ProcessInput before NeedInput, never
    // ProcessOutput before HaveOutput. Input exhaustion (channel closed)
    // triggers the drain sequence; DrainComplete ends the thread.
    fn run(mut self) -> Result<()> {
        let mut draining = false;
        loop {
            let event: IMFMediaEvent = unsafe { self.events.GetEvent(MF_EVENT_FLAG_NONE)? };
            let ty = unsafe { event.GetType()? } as i32;
            if ty == METransformNeedInput.0 {
                if draining {
                    continue;
                }
                match self.input_rx.recv() {
                    Ok(input) => self.feed(input)?,
                    Err(_) => {
                        // Input closed: drain what the encoder holds.
                        draining = true;
                        unsafe {
                            self.transform
                                .ProcessMessage(MFT_MESSAGE_NOTIFY_END_OF_STREAM, 0)?;
                            self.transform
                                .ProcessMessage(MFT_MESSAGE_COMMAND_DRAIN, 0)?;
                        }
                    }
                }
            } else if ty == METransformHaveOutput.0 {
                self.deliver_output()?;
            } else if ty == METransformDrainComplete.0 {
                return Ok(());
            }
        }
    }

    fn feed(&mut self, input: EncoderInput) -> Result<()> {
        if input.force_idr {
            unsafe {
                let _ = self
                    .codec_api
                    .SetValue(&CODECAPI_AVEncVideoForceKeyFrame, &VARIANT::from(1u32));
            }
        }
        unsafe {
            let buffer = MFCreateDXGISurfaceBuffer(
                &ID3D11Texture2D::IID as *const GUID,
                &input.texture,
                0,
                false,
            )?;
            let sample: IMFSample = MFCreateSample()?;
            sample.AddBuffer(&buffer)?;
            sample.SetSampleTime(input.time_us as i64 * 10)?;
            sample.SetSampleDuration(input.duration_us.max(1) as i64 * 10)?;
            self.transform.ProcessInput(0, &sample, 0)?;
        }
        Ok(())
    }

    fn deliver_output(&mut self) -> Result<()> {
        let mut buffers = [MFT_OUTPUT_DATA_BUFFER::default()];
        let mut status = 0u32;
        let result = unsafe { self.transform.ProcessOutput(0, &mut buffers, &mut status) };
        if let Err(e) = &result {
            // Mid-stream format change: renegotiate and continue; the next
            // HaveOutput carries the data.
            if e.code() == MF_E_TRANSFORM_STREAM_CHANGE {
                unsafe {
                    let new_type = self.transform.GetOutputAvailableType(0, 0)?;
                    self.transform.SetOutputType(0, &new_type, 0)?;
                }
                return Ok(());
            }
            return result;
        }
        let buffer = &mut buffers[0];
        if let Some(sample) = std::mem::ManuallyDrop::into_inner(buffer.pSample.clone()) {
            let time_us = unsafe { sample.GetSampleTime()? } as u64 / 10;
            let data = unsafe {
                let contiguous = sample.ConvertToContiguousBuffer()?;
                let mut ptr = std::ptr::null_mut();
                let mut len = 0u32;
                contiguous.Lock(&mut ptr, None, Some(&mut len))?;
                let bytes = std::slice::from_raw_parts(ptr, len as usize).to_vec();
                contiguous.Unlock()?;
                bytes
            };
            // Keyframe = AU contains an IDR NAL — the same classification
            // Linux uses; never the CleanPoint attribute (D10).
            let keyframe = crate::h264::has_idr(&data);
            (self.on_output)(EncodedAu {
                data,
                time_us,
                keyframe,
            });
        }
        // Balance the ManuallyDrop clone and release any events.
        unsafe {
            std::mem::ManuallyDrop::drop(&mut buffer.pSample);
            std::mem::ManuallyDrop::drop(&mut buffer.pEvents);
        }
        Ok(())
    }
}

fn read_sequence_header(transform: &IMFTransform) -> Result<Vec<u8>> {
    unsafe {
        let out_type = transform.GetOutputCurrentType(0)?;
        let len = out_type.GetBlobSize(&MF_MT_MPEG_SEQUENCE_HEADER)?;
        let mut buf = vec![0u8; len as usize];
        out_type.GetBlob(&MF_MT_MPEG_SEQUENCE_HEADER, &mut buf, None)?;
        Ok(buf)
    }
}

/// The real trial gate (D9): ~30 synthetic NV12 frames through a fresh
/// session — never the capture session — collected into a [`TrialRun`] for
/// the portable invariant checks. A forced IDR lands mid-trial so the
/// resume re-prime is proven per candidate.
pub struct MftTrialRunner<'a> {
    pub device: &'a ID3D11Device,
    pub params: EncoderParams,
    pub entries: &'a [MftEntry],
}

const TRIAL_FRAMES: usize = 30;
const TRIAL_FORCED_IDR_AT: usize = 20;

impl TrialRunner for MftTrialRunner<'_> {
    fn run(&mut self, candidate: &Candidate) -> std::result::Result<TrialRun, String> {
        let entry = self
            .entries
            .iter()
            .find(|e| e.name == candidate.id)
            .ok_or_else(|| {
                format!(
                    "candidate {} vanished between enumeration and trial",
                    candidate.id
                )
            })?;
        trial_encode(entry, self.device, self.params).map_err(|e| e.to_string())
    }
}

fn trial_encode(
    entry: &MftEntry,
    device: &ID3D11Device,
    params: EncoderParams,
) -> Result<TrialRun> {
    let (out_tx, out_rx) = mpsc::channel();
    let session = EncoderSession::start(
        entry,
        device,
        params,
        Box::new(move |au| {
            let _ = out_tx.send(au);
        }),
    )?;
    let sequence_header = session.sequence_header.clone();

    let frame_us = 1_000_000 / u64::from(params.fps.max(1));
    let mut input_times = Vec::with_capacity(TRIAL_FRAMES);
    for i in 0..TRIAL_FRAMES {
        // Moving-bar synthetic content: rate control sees motion, and the
        // texture is fresh per frame (the MFT may hold a reference).
        let tex = synthetic_nv12(device, params.width, params.height, i)?;
        let time_us = i as u64 * frame_us;
        input_times.push(time_us as i64 * 10);
        session.send(EncoderInput {
            texture: tex,
            time_us,
            duration_us: frame_us,
            force_idr: i == TRIAL_FORCED_IDR_AT,
        });
    }
    session.finish()?;

    let mut aus = Vec::new();
    while let Ok(au) = out_rx.try_recv() {
        aus.push(TrialAu {
            data: au.data,
            time_100ns: au.time_us as i64 * 10,
        });
    }
    Ok(TrialRun {
        inputs_fed: TRIAL_FRAMES,
        aus,
        input_times_100ns: input_times,
        forced_idr_at: Some(TRIAL_FORCED_IDR_AT),
        sequence_header,
    })
}

/// A gray NV12 frame with a moving white bar.
fn synthetic_nv12(
    device: &ID3D11Device,
    width: u32,
    height: u32,
    frame_index: usize,
) -> Result<ID3D11Texture2D> {
    let w = width as usize;
    let h = height as usize;
    let mut data = vec![128u8; w * h * 3 / 2];
    let bar_x = (frame_index * 16) % w.max(1);
    let bar_end = (bar_x + 32).min(w);
    for row in data.chunks_exact_mut(w).take(h) {
        for px in &mut row[bar_x..bar_end] {
            *px = 235;
        }
    }
    let desc = D3D11_TEXTURE2D_DESC {
        Width: width,
        Height: height,
        MipLevels: 1,
        ArraySize: 1,
        Format: DXGI_FORMAT_NV12,
        SampleDesc: DXGI_SAMPLE_DESC {
            Count: 1,
            Quality: 0,
        },
        Usage: D3D11_USAGE_DEFAULT,
        BindFlags: D3D11_BIND_SHADER_RESOURCE.0 as u32,
        ..Default::default()
    };
    let init = D3D11_SUBRESOURCE_DATA {
        pSysMem: data.as_ptr() as *const _,
        SysMemPitch: width,
        SysMemSlicePitch: 0,
    };
    unsafe {
        let mut tex = None;
        device.CreateTexture2D(&desc, Some(&init), Some(&mut tex))?;
        Ok(tex.expect("CreateTexture2D succeeded without a texture"))
    }
}
