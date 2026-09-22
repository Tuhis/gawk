//! Aspect-preserving fit (docs/38 D11 amendment, 2026-07-31): the encode
//! resolution is the configured rung box shrunk to the SOURCE's aspect
//! ratio — one side matches the box, the other is smaller, nothing is ever
//! stretched. Portable and pure: the geometry is unit-tested here; the GPU
//! pass (`d3d::Converter`) and the pipeline consume the results.

/// Fits `src` into `bounds` preserving the source aspect ratio exactly:
/// one output side equals the corresponding bound, the other is ≤ its
/// bound. Results are floored to even (NV12 4:2:0 subsampling needs even
/// dimensions) with a floor of 2. Degenerate inputs (any zero) fall back
/// to the bounds themselves.
pub fn fit_within(src_w: u32, src_h: u32, box_w: u32, box_h: u32) -> (u32, u32) {
    if src_w == 0 || src_h == 0 || box_w == 0 || box_h == 0 {
        return (even_floor(box_w), even_floor(box_h));
    }
    let (src_w, src_h, box_w, box_h) = (
        u64::from(src_w),
        u64::from(src_h),
        u64::from(box_w),
        u64::from(box_h),
    );
    // Source wider (relative to the box) ⇒ width pins to the box and the
    // height derives; otherwise the height pins. Derivation rounds to
    // nearest — the even floor below is the only lossy step.
    let (w, h) = if src_w * box_h >= src_h * box_w {
        (box_w, (box_w * src_h + src_w / 2) / src_w)
    } else {
        ((box_h * src_w + src_h / 2) / src_h, box_h)
    };
    (even_floor(w as u32), even_floor(h as u32))
}

/// The centered placement of a `src`-aspect image on an `out_w`×`out_h`
/// surface: `Some((left, top, width, height))` when bars are needed (the
/// mid-broadcast window-resize case — the encoder's dimensions are fixed,
/// so a changed source aspect letterboxes instead of stretching), `None`
/// when the source fills the surface. Offsets are even for NV12 chroma
/// alignment.
pub fn letterbox(src_w: u32, src_h: u32, out_w: u32, out_h: u32) -> Option<(u32, u32, u32, u32)> {
    let (w, h) = fit_within(src_w, src_h, out_w, out_h);
    if w >= out_w && h >= out_h {
        return None;
    }
    Some((
        even_floor_zero((out_w - w) / 2),
        even_floor_zero((out_h - h) / 2),
        w,
        h,
    ))
}

fn even_floor(v: u32) -> u32 {
    (v & !1).max(2)
}

fn even_floor_zero(v: u32) -> u32 {
    v & !1
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn matching_aspect_fills_the_box_exactly() {
        assert_eq!(fit_within(2560, 1440, 1920, 1080), (1920, 1080));
        assert_eq!(fit_within(1920, 1080, 1920, 1080), (1920, 1080));
        // And upscaling small sources still pins one side to the box.
        assert_eq!(fit_within(640, 360, 1920, 1080), (1920, 1080));
    }

    #[test]
    fn narrower_sources_pin_height_and_shrink_width() {
        // 4:3 into a 16:9 box: height matches, width shrinks.
        assert_eq!(fit_within(1600, 1200, 1920, 1080), (1440, 1080));
        // Portrait window into the same box.
        assert_eq!(fit_within(1080, 1920, 1920, 1080), (608, 1080));
    }

    #[test]
    fn wider_sources_pin_width_and_shrink_height() {
        // 21:9 ultrawide into a 16:9 box: width matches, height shrinks.
        assert_eq!(fit_within(3440, 1440, 1920, 1080), (1920, 804));
    }

    #[test]
    fn results_are_even_for_nv12() {
        let (w, h) = fit_within(1366, 768, 1920, 1080);
        assert_eq!((w % 2, h % 2), (0, 0));
        assert!(w == 1920 || h == 1080, "one side must pin to the box");
        let (w, h) = fit_within(1279, 721, 1920, 1080);
        assert_eq!((w % 2, h % 2), (0, 0));
    }

    #[test]
    fn degenerate_inputs_fall_back_to_the_box() {
        assert_eq!(fit_within(0, 0, 1920, 1080), (1920, 1080));
        assert_eq!(fit_within(100, 0, 1280, 720), (1280, 720));
    }

    #[test]
    fn letterbox_is_none_when_the_source_fills_the_surface() {
        assert_eq!(letterbox(2560, 1440, 1920, 1080), None);
        assert_eq!(letterbox(1920, 1080, 1920, 1080), None);
    }

    #[test]
    fn letterbox_centers_with_even_offsets() {
        // 4:3 content into a 16:9 encode surface: pillarbox.
        assert_eq!(
            letterbox(1600, 1200, 1920, 1080),
            Some((240, 0, 1440, 1080))
        );
        // Ultrawide content into 16:9: letterbox, offset floored to even.
        assert_eq!(letterbox(3440, 1440, 1920, 1080), Some((0, 138, 1920, 804)));
    }
}
