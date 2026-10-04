//! The broadcast rung (docs/67 D8, *provisional on IO0*): what size, rate
//! and bitrate a capture is encoded at.

use gawk_capture::fit::fit_within;

/// D8's two presets. Cellular is chosen automatically on an expensive
/// network path at start (D19), never switched mid-broadcast.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Quality {
    /// 1920 on the long edge, 60 fps, 8 Mbps peak.
    Standard,
    /// 1280 on the long edge, 30 fps, 3 Mbps peak.
    Cellular,
}

/// The encoder's settings for one orientation.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Rung {
    pub width: u32,
    pub height: u32,
    pub fps: u32,
    pub peak_bitrate_bps: u32,
}

impl Quality {
    pub fn long_edge(self) -> u32 {
        match self {
            Quality::Standard => 1920,
            Quality::Cellular => 1280,
        }
    }

    pub fn fps(self) -> u32 {
        match self {
            Quality::Standard => 60,
            Quality::Cellular => 30,
        }
    }

    /// Below the desktop's 12 Mbps: cellular uplinks are the common case.
    pub fn peak_bitrate_bps(self) -> u32 {
        match self {
            Quality::Standard => 8_000_000,
            Quality::Cellular => 3_000_000,
        }
    }

    /// The rung for an upright frame of `w` × `h`: fitted into a square
    /// box of the long edge (aspect kept, even dimensions) and never
    /// upscaled, since `fit_within` alone would enlarge a small source.
    pub fn rung(self, w: u32, h: u32) -> Rung {
        let edge = self.long_edge();
        let (width, height) = if w.max(h) <= edge {
            (w & !1, h & !1)
        } else {
            fit_within(w, h, edge, edge)
        };
        Rung {
            width,
            height,
            fps: self.fps(),
            peak_bitrate_bps: self.peak_bitrate_bps(),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_pro_max_panel_streams_at_884_by_1920_portrait_and_1920_by_884_landscape() {
        let p = Quality::Standard.rung(1320, 2868);
        assert_eq!((p.width, p.height, p.fps), (884, 1920, 60));
        let l = Quality::Standard.rung(2868, 1320);
        assert_eq!((l.width, l.height), (1920, 884));
        assert_eq!(p.peak_bitrate_bps, 8_000_000);
    }

    #[test]
    fn cellular_is_1280_on_the_long_edge_at_30_fps() {
        let r = Quality::Cellular.rung(1320, 2868);
        assert_eq!((r.width, r.height, r.fps), (588, 1280, 30));
        assert_eq!(r.peak_bitrate_bps, 3_000_000);
    }

    #[test]
    fn never_upscales_and_keeps_dimensions_even() {
        let r = Quality::Standard.rung(641, 481);
        assert_eq!((r.width, r.height), (640, 480));
    }
}
