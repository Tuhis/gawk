//! The Ready page's source preview (docs/65): a capture-only stream of the
//! chosen source, sampled at 1 Hz, run by the platform while the shell asks
//! for one (D4). [`PreviewSlot`] is the lifecycle every platform shares —
//! start, restart on a new choice, stop, give up on a failed one (D6) — so
//! the platforms supply only "start a capture of this".

use crate::shell::Thumb;

/// What the source card should show after a tick.
#[derive(Debug, PartialEq, Eq)]
pub enum PreviewFrame {
    /// No preview: the card shows the source's icon.
    Hidden,
    /// Keep the picture already shown.
    Keep,
    /// A new picture.
    New(Thumb),
}

/// A running capture-only preview. Dropping it stops the capture.
pub trait PreviewSource {
    /// The newest picture since the last call, if any.
    fn take(&self) -> Option<Thumb>;
}

/// One platform's preview, keyed by what is chosen (`K`): a capture target
/// on Windows, a pick counter where the system picker owns the choice.
pub struct PreviewSlot<K> {
    running: Option<(K, Box<dyn PreviewSource>)>,
    /// The key whose preview could not start: not retried until the key
    /// changes or the preview is unwanted in between (D6).
    failed: Option<K>,
    /// A picture has been shown for the running preview.
    shown: bool,
}

impl<K> Default for PreviewSlot<K> {
    fn default() -> Self {
        Self {
            running: None,
            failed: None,
            shown: false,
        }
    }
}

impl<K: PartialEq + std::fmt::Debug> PreviewSlot<K> {
    /// One tick. `want` is the chosen source while the shell wants a
    /// preview, `None` otherwise; `start` opens a capture of it.
    pub fn update(
        &mut self,
        want: Option<K>,
        start: impl FnOnce(&K) -> Result<Box<dyn PreviewSource>, String>,
    ) -> PreviewFrame {
        let Some(key) = want else {
            self.stop();
            self.failed = None;
            return PreviewFrame::Hidden;
        };
        if self.running.as_ref().is_some_and(|(k, _)| *k != key) {
            self.stop();
        }
        if self.running.is_none() {
            if self.failed.as_ref() == Some(&key) {
                return PreviewFrame::Hidden;
            }
            match start(&key) {
                Ok(source) => {
                    log::debug!("preview started for {key:?}");
                    self.failed = None;
                    self.running = Some((key, source));
                }
                Err(e) => {
                    log::info!("no preview for {key:?}: {e}");
                    self.failed = Some(key);
                    return PreviewFrame::Hidden;
                }
            }
        }
        let (_, source) = self.running.as_ref().expect("running just ensured");
        match source.take() {
            Some(thumb) => {
                self.shown = true;
                PreviewFrame::New(thumb)
            }
            None if self.shown => PreviewFrame::Keep,
            None => PreviewFrame::Hidden,
        }
    }

    /// Stops the running preview, if any (D5: before a broadcast captures,
    /// and before a grant it reads is let go).
    pub fn stop(&mut self) {
        if let Some((key, _)) = self.running.take() {
            log::debug!("preview stopped for {key:?}");
        }
        self.shown = false;
    }

    pub fn is_running(&self) -> bool {
        self.running.is_some()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::cell::{Cell, RefCell};
    use std::rc::Rc;

    /// A preview whose pictures the test hands it, counting its drop.
    struct Fake {
        frames: Rc<RefCell<Vec<Thumb>>>,
        dropped: Rc<Cell<u32>>,
    }

    impl PreviewSource for Fake {
        fn take(&self) -> Option<Thumb> {
            self.frames.borrow_mut().pop()
        }
    }

    impl Drop for Fake {
        fn drop(&mut self) {
            self.dropped.set(self.dropped.get() + 1);
        }
    }

    #[derive(Default)]
    struct Rig {
        frames: Rc<RefCell<Vec<Thumb>>>,
        dropped: Rc<Cell<u32>>,
        started: Vec<u32>,
    }

    impl Rig {
        fn tick(&mut self, slot: &mut PreviewSlot<u32>, want: Option<u32>) -> PreviewFrame {
            let (frames, dropped) = (self.frames.clone(), self.dropped.clone());
            let started = &mut self.started;
            slot.update(want, |k| {
                started.push(*k);
                Ok(Box::new(Fake { frames, dropped }))
            })
        }

        fn push(&self, w: u32) {
            self.frames
                .borrow_mut()
                .push((w, 1, vec![0; 4 * w as usize]));
        }
    }

    #[test]
    fn hidden_until_the_first_picture_then_new_then_keep() {
        let (mut rig, mut slot) = (Rig::default(), PreviewSlot::default());
        assert_eq!(rig.tick(&mut slot, Some(1)), PreviewFrame::Hidden);
        assert_eq!(rig.started, [1], "starts on the first wanted tick");
        rig.push(2);
        assert!(matches!(
            rig.tick(&mut slot, Some(1)),
            PreviewFrame::New((2, 1, _))
        ));
        assert_eq!(rig.tick(&mut slot, Some(1)), PreviewFrame::Keep);
        assert_eq!(rig.started, [1], "one capture per choice");
    }

    #[test]
    fn a_new_choice_restarts_and_hides_the_old_picture() {
        let (mut rig, mut slot) = (Rig::default(), PreviewSlot::default());
        rig.tick(&mut slot, Some(1));
        rig.push(2);
        rig.tick(&mut slot, Some(1));
        assert_eq!(rig.tick(&mut slot, Some(7)), PreviewFrame::Hidden);
        assert_eq!(rig.started, [1, 7]);
        assert_eq!(rig.dropped.get(), 1, "the old capture stopped");
    }

    #[test]
    fn unwanted_stops_and_wanted_again_restarts() {
        let (mut rig, mut slot) = (Rig::default(), PreviewSlot::default());
        rig.tick(&mut slot, Some(1));
        assert_eq!(rig.tick(&mut slot, None), PreviewFrame::Hidden);
        assert_eq!(rig.dropped.get(), 1);
        assert!(!slot.is_running());
        rig.tick(&mut slot, Some(1));
        assert_eq!(rig.started, [1, 1]);
    }

    #[test]
    fn stop_drops_the_capture_and_forgets_the_picture() {
        let (mut rig, mut slot) = (Rig::default(), PreviewSlot::default());
        rig.tick(&mut slot, Some(1));
        rig.push(2);
        rig.tick(&mut slot, Some(1));
        slot.stop();
        assert_eq!(rig.dropped.get(), 1);
        // Restarted: Hidden again until its own first picture.
        assert_eq!(rig.tick(&mut slot, Some(1)), PreviewFrame::Hidden);
    }

    #[test]
    fn a_failed_choice_is_not_retried_until_it_changes_or_was_unwanted() {
        let mut slot = PreviewSlot::<u32>::default();
        let attempts = Cell::new(0);
        let fail = |slot: &mut PreviewSlot<u32>, want| {
            slot.update(want, |_| {
                attempts.set(attempts.get() + 1);
                Err("no capture".into())
            })
        };
        assert_eq!(fail(&mut slot, Some(1)), PreviewFrame::Hidden);
        assert_eq!(fail(&mut slot, Some(1)), PreviewFrame::Hidden);
        assert_eq!(fail(&mut slot, Some(1)), PreviewFrame::Hidden);
        fail(&mut slot, Some(2));
        fail(&mut slot, None);
        fail(&mut slot, Some(2));
        assert_eq!(
            attempts.get(),
            3,
            "once for 1, once for 2, once after unwanted"
        );
    }
}
