//! The one D3D11 device (docs/38 D9's zero-copy rule) and the VideoProcessor
//! convert/scale pass (D6/D11): BGRA capture textures → NV12 for the MFT,
//! plus the 1 Hz thumbnail downscale (D12.5). Frames never round-trip
//! through system memory on the live path; the staging readbacks here exist
//! for the WARP unit test and the thumbnail only.

use windows::Win32::Graphics::Direct3D::{D3D_DRIVER_TYPE_HARDWARE, D3D_DRIVER_TYPE_WARP};
use windows::Win32::Graphics::Direct3D11::{
    D3D11_BIND_RENDER_TARGET, D3D11_BIND_SHADER_RESOURCE, D3D11_CPU_ACCESS_READ,
    D3D11_CREATE_DEVICE_BGRA_SUPPORT, D3D11_CREATE_DEVICE_FLAG, D3D11_CREATE_DEVICE_VIDEO_SUPPORT,
    D3D11_MAP_READ, D3D11_SDK_VERSION, D3D11_TEX2D_VPIV, D3D11_TEX2D_VPOV, D3D11_TEXTURE2D_DESC,
    D3D11_USAGE_DEFAULT, D3D11_USAGE_STAGING, D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE,
    D3D11_VIDEO_PROCESSOR_CONTENT_DESC, D3D11_VIDEO_PROCESSOR_INPUT_VIEW_DESC,
    D3D11_VIDEO_PROCESSOR_INPUT_VIEW_DESC_0, D3D11_VIDEO_PROCESSOR_OUTPUT_VIEW_DESC,
    D3D11_VIDEO_PROCESSOR_OUTPUT_VIEW_DESC_0, D3D11_VIDEO_PROCESSOR_STREAM,
    D3D11_VIDEO_USAGE_PLAYBACK_NORMAL, D3D11_VPIV_DIMENSION_TEXTURE2D,
    D3D11_VPOV_DIMENSION_TEXTURE2D, D3D11CreateDevice, ID3D11Device, ID3D11DeviceContext,
    ID3D11Multithread, ID3D11Texture2D, ID3D11VideoContext, ID3D11VideoContext1, ID3D11VideoDevice,
    ID3D11VideoProcessor, ID3D11VideoProcessorEnumerator, ID3D11VideoProcessorInputView,
    ID3D11VideoProcessorOutputView,
};
use windows::Win32::Graphics::Dxgi::Common::{
    DXGI_COLOR_SPACE_RGB_FULL_G22_NONE_P709, DXGI_COLOR_SPACE_YCBCR_STUDIO_G22_LEFT_P709,
    DXGI_FORMAT_B8G8R8A8_UNORM, DXGI_FORMAT_NV12, DXGI_SAMPLE_DESC,
};
use windows::Win32::Graphics::Dxgi::IDXGIDevice;
use windows::Win32::System::WinRT::Direct3D11::CreateDirect3D11DeviceFromDXGIDevice;
use windows::core::{Interface, Result};

/// The shared device: WGC's frame pool, the VideoProcessor and the MFT all
/// live on this one device so textures pass by handle, never by copy.
#[derive(Clone)]
pub struct GpuDevice {
    pub device: ID3D11Device,
    pub context: ID3D11DeviceContext,
    /// The WinRT projection of the same device, for the WGC frame pool.
    pub winrt: windows::Graphics::DirectX::Direct3D11::IDirect3DDevice,
}

// The raw COM pointers are freely shareable; the multithread protection
// below is what makes concurrent context use defined.
unsafe impl Send for GpuDevice {}
unsafe impl Sync for GpuDevice {}

impl GpuDevice {
    /// The production device: hardware, with video (VideoProcessor + MFT
    /// DXGI manager) and BGRA support, multithread-protected because WGC's
    /// pool thread, the encoder drain and the thumbnail all touch it.
    pub fn hardware() -> Result<Self> {
        Self::create(D3D_DRIVER_TYPE_HARDWARE)
    }

    /// WARP: the software rasterizer, for CI-runnable conversion tests.
    pub fn warp() -> Result<Self> {
        Self::create(D3D_DRIVER_TYPE_WARP)
    }

    fn create(driver: windows::Win32::Graphics::Direct3D::D3D_DRIVER_TYPE) -> Result<Self> {
        let mut device: Option<ID3D11Device> = None;
        let mut context: Option<ID3D11DeviceContext> = None;
        unsafe {
            D3D11CreateDevice(
                None,
                driver,
                windows::Win32::Foundation::HMODULE::default(),
                D3D11_CREATE_DEVICE_FLAG(
                    D3D11_CREATE_DEVICE_BGRA_SUPPORT.0 | D3D11_CREATE_DEVICE_VIDEO_SUPPORT.0,
                ),
                None,
                D3D11_SDK_VERSION,
                Some(&mut device),
                None,
                Some(&mut context),
            )?;
        }
        let device = device.expect("D3D11CreateDevice succeeded without a device");
        let context = context.expect("D3D11CreateDevice succeeded without a context");

        // Defined concurrent access from the WGC pool thread + encoder
        // drain + GUI thumbnail; without this, corruption not errors.
        let mt: ID3D11Multithread = context.cast()?;
        unsafe {
            let _ = mt.SetMultithreadProtected(true);
        }

        let dxgi: IDXGIDevice = device.cast()?;
        let winrt = unsafe { CreateDirect3D11DeviceFromDXGIDevice(&dxgi)? }.cast()?;

        Ok(Self {
            device,
            context,
            winrt,
        })
    }
}

/// One VideoProcessor pass: BGRA in at a fixed source size, NV12 (and
/// optionally a BGRA thumbnail) out at fixed target sizes. Recreated when
/// the source size changes — WGC window capture resizes with the window.
pub struct Converter {
    gpu: GpuDevice,
    video: ID3D11VideoDevice,
    vctx: ID3D11VideoContext,
    enumerator: ID3D11VideoProcessorEnumerator,
    processor: ID3D11VideoProcessor,
    in_width: u32,
    in_height: u32,
    /// The NV12 output the encoder consumes, reused frame to frame.
    nv12: ID3D11Texture2D,
    nv12_view: ID3D11VideoProcessorOutputView,
    out_width: u32,
    out_height: u32,
}

// Guarded by the device's multithread protection (see GpuDevice::create).
unsafe impl Send for Converter {}

impl Converter {
    pub fn new(
        gpu: &GpuDevice,
        in_width: u32,
        in_height: u32,
        out_width: u32,
        out_height: u32,
    ) -> Result<Self> {
        let video: ID3D11VideoDevice = gpu.device.cast()?;
        let vctx: ID3D11VideoContext = gpu.context.cast()?;

        let desc = D3D11_VIDEO_PROCESSOR_CONTENT_DESC {
            InputFrameFormat: D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE,
            InputWidth: in_width,
            InputHeight: in_height,
            OutputWidth: out_width,
            OutputHeight: out_height,
            Usage: D3D11_VIDEO_USAGE_PLAYBACK_NORMAL,
            ..Default::default()
        };
        let enumerator = unsafe { video.CreateVideoProcessorEnumerator(&desc)? };
        let processor = unsafe { video.CreateVideoProcessor(&enumerator, 0)? };

        // Color spaces stated explicitly, not defaulted: capture is full-
        // range sRGB, H.264 consumers assume studio-range BT.709. The _1
        // variants (Win10+) take DXGI color-space enums instead of the
        // legacy bitfield struct.
        if let Ok(vctx1) = gpu.context.cast::<ID3D11VideoContext1>() {
            unsafe {
                vctx1.VideoProcessorSetStreamColorSpace1(
                    &processor,
                    0,
                    DXGI_COLOR_SPACE_RGB_FULL_G22_NONE_P709,
                );
                vctx1.VideoProcessorSetOutputColorSpace1(
                    &processor,
                    DXGI_COLOR_SPACE_YCBCR_STUDIO_G22_LEFT_P709,
                );
            }
        }

        let nv12 = new_texture(
            &gpu.device,
            out_width,
            out_height,
            DXGI_FORMAT_NV12,
            D3D11_USAGE_DEFAULT,
            D3D11_BIND_RENDER_TARGET.0 as u32,
            0,
        )?;
        let nv12_view = unsafe {
            let mut view = None;
            video.CreateVideoProcessorOutputView(
                &nv12,
                &enumerator,
                &D3D11_VIDEO_PROCESSOR_OUTPUT_VIEW_DESC {
                    ViewDimension: D3D11_VPOV_DIMENSION_TEXTURE2D,
                    Anonymous: D3D11_VIDEO_PROCESSOR_OUTPUT_VIEW_DESC_0 {
                        Texture2D: D3D11_TEX2D_VPOV { MipSlice: 0 },
                    },
                },
                Some(&mut view),
            )?;
            view.expect("output view create succeeded without a view")
        };

        Ok(Self {
            gpu: gpu.clone(),
            video,
            vctx,
            enumerator,
            processor,
            in_width,
            in_height,
            nv12,
            nv12_view,
            out_width,
            out_height,
        })
    }

    pub fn matches_input(&self, width: u32, height: u32) -> bool {
        self.in_width == width && self.in_height == height
    }

    /// Converts one BGRA texture into the reused NV12 output and returns it.
    /// The caller must be done with the previous frame's output — the MFT
    /// copies/consumes the sample before the next convert (one pump, no
    /// pipelining, which is exactly the ≤1-frame-latency posture).
    pub fn convert(&self, input: &ID3D11Texture2D) -> Result<ID3D11Texture2D> {
        let input_view = self.input_view(input)?;
        self.blt(&input_view, &self.nv12_view)?;
        Ok(self.nv12.clone())
    }

    /// A downscaled BGRA readback of `input` for the GUI thumbnail
    /// (docs/38 D12.5: ~1 Hz, reusing a frame already in hand; this staging
    /// round-trip is deliberately NOT on the encode path).
    pub fn thumbnail_rgba(
        &self,
        input: &ID3D11Texture2D,
        max_width: u32,
    ) -> Result<(u32, u32, Vec<u8>)> {
        let w = self.in_width.min(max_width).max(1);
        let h = (u64::from(w) * u64::from(self.in_height) / u64::from(self.in_width.max(1))).max(1)
            as u32;

        let target = new_texture(
            &self.gpu.device,
            w,
            h,
            DXGI_FORMAT_B8G8R8A8_UNORM,
            D3D11_USAGE_DEFAULT,
            D3D11_BIND_RENDER_TARGET.0 as u32,
            0,
        )?;
        let out_view = unsafe {
            let mut view = None;
            self.video.CreateVideoProcessorOutputView(
                &target,
                &self.enumerator,
                &D3D11_VIDEO_PROCESSOR_OUTPUT_VIEW_DESC {
                    ViewDimension: D3D11_VPOV_DIMENSION_TEXTURE2D,
                    Anonymous: D3D11_VIDEO_PROCESSOR_OUTPUT_VIEW_DESC_0 {
                        Texture2D: D3D11_TEX2D_VPOV { MipSlice: 0 },
                    },
                },
                Some(&mut view),
            )?;
            view.expect("output view create succeeded without a view")
        };
        let input_view = self.input_view(input)?;
        self.blt(&input_view, &out_view)?;

        let (row_pitch, bytes) = read_texture(&self.gpu, &target, w, h, 4)?;
        // Tightly pack, and swizzle BGRA → RGBA for the GUI's image type.
        let mut rgba = vec![0u8; (w * h * 4) as usize];
        for y in 0..h as usize {
            let src = &bytes[y * row_pitch..y * row_pitch + (w as usize) * 4];
            let dst = &mut rgba[y * (w as usize) * 4..(y + 1) * (w as usize) * 4];
            for x in 0..w as usize {
                dst[x * 4] = src[x * 4 + 2];
                dst[x * 4 + 1] = src[x * 4 + 1];
                dst[x * 4 + 2] = src[x * 4];
                dst[x * 4 + 3] = 0xff;
            }
        }
        Ok((w, h, rgba))
    }

    /// Staging readback of the NV12 output — WARP-test support (the
    /// conversion-correctness pin) and never the live path.
    pub fn read_nv12(&self) -> Result<Vec<u8>> {
        let (row_pitch, bytes) =
            read_texture(&self.gpu, &self.nv12, self.out_width, self.out_height, 1)?;
        // NV12: H rows of Y then H/2 rows of interleaved UV, both W wide.
        let w = self.out_width as usize;
        let h = self.out_height as usize;
        let mut out = Vec::with_capacity(w * h * 3 / 2);
        for y in 0..h + h / 2 {
            out.extend_from_slice(&bytes[y * row_pitch..y * row_pitch + w]);
        }
        Ok(out)
    }

    fn input_view(&self, input: &ID3D11Texture2D) -> Result<ID3D11VideoProcessorInputView> {
        unsafe {
            let mut view = None;
            self.video.CreateVideoProcessorInputView(
                input,
                &self.enumerator,
                &D3D11_VIDEO_PROCESSOR_INPUT_VIEW_DESC {
                    FourCC: 0,
                    ViewDimension: D3D11_VPIV_DIMENSION_TEXTURE2D,
                    Anonymous: D3D11_VIDEO_PROCESSOR_INPUT_VIEW_DESC_0 {
                        Texture2D: D3D11_TEX2D_VPIV {
                            MipSlice: 0,
                            ArraySlice: 0,
                        },
                    },
                },
                Some(&mut view),
            )?;
            Ok(view.expect("input view create succeeded without a view"))
        }
    }

    fn blt(
        &self,
        input: &ID3D11VideoProcessorInputView,
        output: &ID3D11VideoProcessorOutputView,
    ) -> Result<()> {
        let mut stream = D3D11_VIDEO_PROCESSOR_STREAM {
            Enable: true.into(),
            pInputSurface: std::mem::ManuallyDrop::new(Some(input.clone())),
            ..Default::default()
        };
        let result = unsafe {
            self.vctx
                .VideoProcessorBlt(&self.processor, output, 0, std::slice::from_ref(&stream))
        };
        // Balance the clone above — the struct field is ManuallyDrop only
        // because the C ABI owns nothing.
        unsafe { std::mem::ManuallyDrop::drop(&mut stream.pInputSurface) };
        result
    }
}

/// Creates a BGRA texture from raw pixels — WARP-test support.
pub fn texture_from_bgra(
    gpu: &GpuDevice,
    width: u32,
    height: u32,
    bgra: &[u8],
) -> Result<ID3D11Texture2D> {
    assert_eq!(bgra.len(), (width * height * 4) as usize);
    let desc = D3D11_TEXTURE2D_DESC {
        Width: width,
        Height: height,
        MipLevels: 1,
        ArraySize: 1,
        Format: DXGI_FORMAT_B8G8R8A8_UNORM,
        SampleDesc: DXGI_SAMPLE_DESC {
            Count: 1,
            Quality: 0,
        },
        Usage: D3D11_USAGE_DEFAULT,
        BindFlags: D3D11_BIND_SHADER_RESOURCE.0 as u32,
        ..Default::default()
    };
    let init = windows::Win32::Graphics::Direct3D11::D3D11_SUBRESOURCE_DATA {
        pSysMem: bgra.as_ptr() as *const _,
        SysMemPitch: width * 4,
        SysMemSlicePitch: 0,
    };
    unsafe {
        let mut tex = None;
        gpu.device
            .CreateTexture2D(&desc, Some(&init), Some(&mut tex))?;
        Ok(tex.expect("CreateTexture2D succeeded without a texture"))
    }
}

fn new_texture(
    device: &ID3D11Device,
    width: u32,
    height: u32,
    format: windows::Win32::Graphics::Dxgi::Common::DXGI_FORMAT,
    usage: windows::Win32::Graphics::Direct3D11::D3D11_USAGE,
    bind: u32,
    cpu: u32,
) -> Result<ID3D11Texture2D> {
    let desc = D3D11_TEXTURE2D_DESC {
        Width: width,
        Height: height,
        MipLevels: 1,
        ArraySize: 1,
        Format: format,
        SampleDesc: DXGI_SAMPLE_DESC {
            Count: 1,
            Quality: 0,
        },
        Usage: usage,
        BindFlags: bind,
        CPUAccessFlags: cpu,
        ..Default::default()
    };
    unsafe {
        let mut tex = None;
        device.CreateTexture2D(&desc, None, Some(&mut tex))?;
        Ok(tex.expect("CreateTexture2D succeeded without a texture"))
    }
}

/// Copies a texture through a staging texture and maps it out. Returns
/// (row pitch, bytes). `bytes_per_texel` sizes the staging copy only via the
/// source format, which the staging texture inherits.
fn read_texture(
    gpu: &GpuDevice,
    src: &ID3D11Texture2D,
    width: u32,
    height: u32,
    _bytes_per_texel: u32,
) -> Result<(usize, Vec<u8>)> {
    let mut desc = D3D11_TEXTURE2D_DESC::default();
    unsafe { src.GetDesc(&mut desc) };
    let staging = new_texture(
        &gpu.device,
        width,
        height,
        desc.Format,
        D3D11_USAGE_STAGING,
        0,
        D3D11_CPU_ACCESS_READ.0 as u32,
    )?;
    unsafe {
        gpu.context.CopyResource(&staging, src);
        let mut mapped = windows::Win32::Graphics::Direct3D11::D3D11_MAPPED_SUBRESOURCE::default();
        gpu.context
            .Map(&staging, 0, D3D11_MAP_READ, 0, Some(&mut mapped))?;
        // NV12 maps as one plane of H*3/2 rows; BGRA as H rows.
        let rows = match desc.Format {
            DXGI_FORMAT_NV12 => height as usize * 3 / 2,
            _ => height as usize,
        };
        let len = mapped.RowPitch as usize * rows;
        let bytes = std::slice::from_raw_parts(mapped.pData as *const u8, len).to_vec();
        gpu.context.Unmap(&staging, 0);
        Ok((mapped.RowPitch as usize, bytes))
    }
}
