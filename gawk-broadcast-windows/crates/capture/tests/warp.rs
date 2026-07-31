//! WB3 acceptance: "VideoProcessor BGRA→NV12 (+scale) correct on a
//! synthetic texture via WARP" — the software rasterizer gives CI a real
//! D3D11 video pipeline with no GPU. Windows-only by nature; the file
//! compiles empty elsewhere.
#![cfg(windows)]

use gawk_capture::d3d::{Converter, GpuDevice, texture_from_bgra};

/// A WARP device with video support, or a loud skip: some hosted-runner
/// images ship a WARP without D3D11 video (device creation fails with
/// DXGI_ERROR_UNSUPPORTED there — measured on windows-latest, 2026-07).
/// Any real Windows machine runs these; the self-skip mirrors the
/// drain-restart test's cfg(unix) posture rather than failing CI on a
/// runner-image capability.
fn warp_or_skip() -> Option<GpuDevice> {
    match GpuDevice::warp() {
        Ok(gpu) => Some(gpu),
        Err(e) => {
            eprintln!("SKIP: WARP device without D3D11 video on this host: {e}");
            None
        }
    }
}

fn solid_bgra(w: u32, h: u32, b: u8, g: u8, r: u8) -> Vec<u8> {
    let mut v = Vec::with_capacity((w * h * 4) as usize);
    for _ in 0..w * h {
        v.extend_from_slice(&[b, g, r, 0xff]);
    }
    v
}

/// Mean of a plane region, for tolerance-window asserts.
fn mean(bytes: &[u8]) -> f64 {
    bytes.iter().map(|&b| f64::from(b)).sum::<f64>() / bytes.len() as f64
}

#[test]
fn bgra_to_nv12_converts_solid_red_plausibly() {
    let Some(gpu) = warp_or_skip() else { return };
    let (w, h) = (64u32, 64u32);
    let conv = Converter::new(&gpu, w, h, w, h).expect("converter");
    let tex = texture_from_bgra(&gpu, w, h, &solid_bgra(w, h, 0, 0, 255)).unwrap();
    conv.convert(&tex).expect("convert");
    let nv12 = conv.read_nv12().expect("readback");
    assert_eq!(nv12.len(), (w * h * 3 / 2) as usize);

    let y = mean(&nv12[..(w * h) as usize]);
    let uv = &nv12[(w * h) as usize..];
    let u = mean(&uv.iter().step_by(2).copied().collect::<Vec<_>>());
    let v = mean(&uv.iter().skip(1).step_by(2).copied().collect::<Vec<_>>());
    // Red in YCbCr: Y low-ish, U below center, V near max. Windows wide
    // enough to accept BT.709 or BT.601 matrices and either range — the
    // pin is "this is red", not a colorimetry certification.
    assert!((45.0..95.0).contains(&y), "Y {y}");
    assert!((80.0..120.0).contains(&u), "U {u}");
    assert!((220.0..256.0).contains(&v), "V {v}");
}

#[test]
fn scaling_halves_dimensions_and_keeps_content() {
    let Some(gpu) = warp_or_skip() else { return };
    let (in_w, in_h, out_w, out_h) = (128u32, 128u32, 64u32, 64u32);
    let conv = Converter::new(&gpu, in_w, in_h, out_w, out_h).expect("converter");
    // Left half black, right half white.
    let mut bgra = Vec::with_capacity((in_w * in_h * 4) as usize);
    for _ in 0..in_h {
        for x in 0..in_w {
            let c = if x < in_w / 2 { 0u8 } else { 255u8 };
            bgra.extend_from_slice(&[c, c, c, 0xff]);
        }
    }
    let tex = texture_from_bgra(&gpu, in_w, in_h, &bgra).unwrap();
    conv.convert(&tex).expect("convert");
    let nv12 = conv.read_nv12().expect("readback");
    assert_eq!(nv12.len(), (out_w * out_h * 3 / 2) as usize);

    // Sample away from the seam: left quarter dark, right quarter bright.
    let row = &nv12[..out_w as usize];
    let left = mean(&row[..(out_w / 4) as usize]);
    let right = mean(&row[(out_w * 3 / 4) as usize..]);
    assert!(left < 60.0, "left {left}");
    assert!(right > 180.0, "right {right}");
}

#[test]
fn thumbnail_downscales_and_swizzles_to_rgba() {
    let Some(gpu) = warp_or_skip() else { return };
    let (w, h) = (256u32, 128u32);
    let conv = Converter::new(&gpu, w, h, w, h).expect("converter");
    // Solid green, in BGRA.
    let tex = texture_from_bgra(&gpu, w, h, &solid_bgra(w, h, 0, 255, 0)).unwrap();
    let (tw, th, rgba) = conv.thumbnail_rgba(&tex, 96).expect("thumbnail");
    assert_eq!(tw, 96);
    assert_eq!(th, 48); // aspect preserved
    assert_eq!(rgba.len(), (tw * th * 4) as usize);
    let px = &rgba[..4];
    assert!(px[0] < 40, "R {}", px[0]);
    assert!(px[1] > 200, "G {}", px[1]);
    assert!(px[2] < 40, "B {}", px[2]);
    assert_eq!(px[3], 0xff);
}
