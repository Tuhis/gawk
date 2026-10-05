//! Rotation is a resolution change (docs/67 D9). The wire has no rotation
//! field and viewers trust the frame in hand, so frames go upright before
//! encode, and a new upright size means a new compression session inside
//! the same publish session.
//!
//! Whether iOS capture delivers frames in panel orientation with an
//! orientation attachment, or already upright, is V-7: the capture code
//! reports the rotation a frame needs (or `None` for face up/down and
//! unknown, which keep the last one), and this module decides when a change
//! takes effect.

/// Clockwise degrees a captured frame must turn to be upright.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Rotation {
    R0,
    R90,
    R180,
    R270,
}

impl Rotation {
    /// The upright size of a `w` × `h` capture under this rotation.
    pub fn upright(self, w: u32, h: u32) -> (u32, u32) {
        match self {
            Rotation::R0 | Rotation::R180 => (w, h),
            Rotation::R90 | Rotation::R270 => (h, w),
        }
    }
}

/// D9's debounce: an iPad turned back and forth must not flood keyframes.
pub const ROTATION_DEBOUNCE_US: u64 = 500_000;

/// Holds the rotation in force; a new one takes over only once every frame
/// for [`ROTATION_DEBOUNCE_US`] has asked for it.
#[derive(Debug, Clone)]
pub struct RotationDebounce {
    current: Rotation,
    pending: Option<(Rotation, u64)>,
}

impl RotationDebounce {
    pub fn new(initial: Rotation) -> Self {
        Self {
            current: initial,
            pending: None,
        }
    }

    pub fn current(&self) -> Rotation {
        self.current
    }

    /// One frame's wish at `ts_us` (`None`: face up/down or unknown, which
    /// keep the last rotation). Returns the rotation to apply to it.
    pub fn observe(&mut self, wanted: Option<Rotation>, ts_us: u64) -> Rotation {
        let Some(wanted) = wanted else {
            return self.current;
        };
        if wanted == self.current {
            self.pending = None;
            return self.current;
        }
        match self.pending {
            Some((p, since)) if p == wanted => {
                if ts_us.saturating_sub(since) >= ROTATION_DEBOUNCE_US {
                    self.current = wanted;
                    self.pending = None;
                }
            }
            _ => self.pending = Some((wanted, ts_us)),
        }
        self.current
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_quarter_turn_swaps_the_upright_size() {
        assert_eq!(Rotation::R90.upright(2868, 1320), (1320, 2868));
        assert_eq!(Rotation::R180.upright(2868, 1320), (2868, 1320));
    }

    #[test]
    fn a_new_orientation_takes_over_after_500ms_of_asking() {
        let mut d = RotationDebounce::new(Rotation::R0);
        assert_eq!(d.observe(Some(Rotation::R90), 0), Rotation::R0);
        assert_eq!(d.observe(Some(Rotation::R90), 499_999), Rotation::R0);
        assert_eq!(d.observe(Some(Rotation::R90), 500_000), Rotation::R90);
    }

    #[test]
    fn turning_back_before_the_debounce_changes_nothing() {
        let mut d = RotationDebounce::new(Rotation::R0);
        d.observe(Some(Rotation::R90), 0);
        assert_eq!(d.observe(Some(Rotation::R0), 300_000), Rotation::R0);
        // The earlier wish doesn't count towards a later one.
        assert_eq!(d.observe(Some(Rotation::R90), 400_000), Rotation::R0);
        assert_eq!(d.observe(Some(Rotation::R90), 800_000), Rotation::R0);
        assert_eq!(d.observe(Some(Rotation::R90), 900_000), Rotation::R90);
    }

    #[test]
    fn face_up_and_down_keep_the_last_orientation() {
        let mut d = RotationDebounce::new(Rotation::R270);
        assert_eq!(d.observe(None, 0), Rotation::R270);
        assert_eq!(d.observe(None, 10_000_000), Rotation::R270);
    }
}
