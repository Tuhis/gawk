//! The system content picker (docs/54 OD6, D4): `SCContentSharingPicker` is
//! how the user chooses what to share, so the app never enumerates
//! `SCShareableContent` and — the point of it — never needs the Screen
//! Recording grant on the happy path (G8, V-1).
//!
//! One of the three modules `unsafe` is confined to (D3). What leaves it is
//! a [`Picked`]: the framework's filter, opaque, plus the plain facts the
//! GUI and the stream need.

use crate::sck::Capture;
use crate::sck_policy::{CallbackGuard, ShareStyle};
use objc2::rc::Retained;
use objc2::runtime::{NSObject, NSObjectProtocol, ProtocolObject};
use objc2::{AnyThread, DefinedClass, Message, define_class, msg_send, sel};
use objc2_foundation::{NSArray, NSBundle, NSError};
use objc2_screen_capture_kit::{
    SCContentFilter, SCContentSharingPicker, SCContentSharingPickerConfiguration,
    SCContentSharingPickerMode, SCContentSharingPickerObserver, SCStream,
};
use std::sync::Arc;

/// What the user picked. Cloning retains the same filter.
#[derive(Clone)]
pub struct Picked {
    pub(crate) filter: Retained<SCContentFilter>,
    pub style: ShareStyle,
    /// "Safari — Apple", "Mail", "Display 3024×1964": what the Share card
    /// shows. Names need macOS 15.2's `included*` accessors; on older
    /// systems this is the generic "A window" / "An app" / "A display".
    pub summary: String,
    /// The picked content's size in pixels (`contentRect` ×
    /// `pointPixelScale`) — the source the rung box is fitted to (D9).
    pub width: u32,
    pub height: u32,
}

// SAFETY: SCContentFilter is an immutable value object once the picker has
// built it; ScreenCaptureKit itself hands it between its own queues, and
// this crate only ever passes it back to SCStream methods.
unsafe impl Send for Picked {}

impl std::fmt::Debug for Picked {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Picked")
            .field("style", &self.style)
            .field("summary", &self.summary)
            .field("width", &self.width)
            .field("height", &self.height)
            .finish_non_exhaustive()
    }
}

pub enum PickerEvent {
    Picked(Picked),
    Cancelled,
    /// The picker could not start, or returned a filter this app cannot
    /// use; the text is for the error card.
    Failed(String),
}

type OnEvent = dyn Fn(PickerEvent) + Send + Sync;

struct ObserverIvars {
    on_event: Arc<OnEvent>,
    guard: CallbackGuard,
}

define_class!(
    // SAFETY: NSObject has no subclassing requirements and this class does
    // not implement Drop.
    #[unsafe(super(NSObject))]
    #[name = "GawkContentSharingPickerObserver"]
    #[ivars = ObserverIvars]
    struct Observer;

    unsafe impl NSObjectProtocol for Observer {}

    unsafe impl SCContentSharingPickerObserver for Observer {
        #[unsafe(method(contentSharingPicker:didCancelForStream:))]
        fn did_cancel(&self, _picker: &SCContentSharingPicker, _stream: Option<&SCStream>) {
            let iv = self.ivars();
            iv.guard.run(|| (iv.on_event)(PickerEvent::Cancelled));
        }

        #[unsafe(method(contentSharingPicker:didUpdateWithFilter:forStream:))]
        fn did_update(
            &self,
            _picker: &SCContentSharingPicker,
            filter: &SCContentFilter,
            _stream: Option<&SCStream>,
        ) {
            let iv = self.ivars();
            iv.guard.run(|| (iv.on_event)(describe(filter)));
        }

        #[unsafe(method(contentSharingPickerStartDidFailWithError:))]
        fn start_did_fail(&self, error: &NSError) {
            let iv = self.ivars();
            let text = format!(
                "The system picker could not open: {}",
                error.localizedDescription()
            );
            iv.guard.run(|| (iv.on_event)(PickerEvent::Failed(text)));
        }
    }
);

impl Observer {
    fn new(on_event: Arc<OnEvent>) -> Retained<Self> {
        let fail = on_event.clone();
        let this = Self::alloc().set_ivars(ObserverIvars {
            on_event,
            guard: CallbackGuard::new(move |msg| fail(PickerEvent::Failed(msg))),
        });
        // SAFETY: NSObject's designated initializer on a freshly allocated
        // instance whose ivars are set.
        unsafe { msg_send![super(this), init] }
    }
}

/// The app's hold on the shared system picker. Create and use it on the
/// main thread — the picker is UI. Events arrive on whatever thread the
/// framework chooses; the callback must hop to the GUI thread itself.
///
/// The picker is *active* only while it is on screen or a capture is live:
/// an active picker puts the system's screen-sharing indicator in the menu
/// bar, which an app that is not sharing anything must not do. The shell
/// calls [`Picker::set_active`] with "a capture is running" whenever that
/// changes and after every picker result.
pub struct Picker {
    observer: Retained<Observer>,
}

impl Picker {
    pub fn new(on_event: impl Fn(PickerEvent) + Send + Sync + 'static) -> Self {
        let observer = Observer::new(Arc::new(on_event));
        // SAFETY: plain property setters on the shared picker and a fresh
        // configuration object, from the main thread.
        unsafe {
            let picker = SCContentSharingPicker::sharedPicker();
            let config = SCContentSharingPickerConfiguration::new();
            // docs/54 D4: one window, one app or one display — the three
            // shapes D6's audio scoping is defined for.
            config.setAllowedPickerModes(
                SCContentSharingPickerMode::SingleWindow
                    | SCContentSharingPickerMode::SingleApplication
                    | SCContentSharingPickerMode::SingleDisplay,
            );
            // Re-pick while live without stopping (D4).
            config.setAllowsChangingSelectedContent(true);
            // Never offer our own window. A bare dev binary has no bundle
            // identifier; the bundle (D14) does.
            if let Some(own) = NSBundle::mainBundle().bundleIdentifier() {
                config.setExcludedBundleIDs(&NSArray::from_retained_slice(&[own]));
            }
            picker.setDefaultConfiguration(&config);
            picker.addObserver(ProtocolObject::from_ref(&*observer));
        }
        Self { observer }
    }

    /// Opens the picker to choose content for a new stream.
    pub fn present(&self) {
        self.set_active(true);
        // SAFETY: presenting the shared, now-active picker from the main
        // thread.
        unsafe { SCContentSharingPicker::sharedPicker().present() }
    }

    /// Opens the picker to change a live stream's content (D4's "Change…"
    /// while live). The result arrives through the same event callback.
    pub fn present_for(&self, capture: &Capture) {
        self.set_active(true);
        // SAFETY: as `present`; the stream is alive for the call.
        unsafe { SCContentSharingPicker::sharedPicker().presentPickerForStream(capture.stream()) }
    }

    /// Whether the shared picker is active — see the type's docs.
    pub fn set_active(&self, active: bool) {
        // SAFETY: a property setter on the shared picker, main thread.
        unsafe { SCContentSharingPicker::sharedPicker().setActive(active) }
    }
}

impl Drop for Picker {
    fn drop(&mut self) {
        // SAFETY: undoing what `new` did, on the thread that owns the Picker.
        unsafe {
            let picker = SCContentSharingPicker::sharedPicker();
            picker.removeObserver(ProtocolObject::from_ref(&*self.observer));
            picker.setActive(false);
        }
    }
}

/// Turns the picker's filter into a [`PickerEvent`].
fn describe(filter: &SCContentFilter) -> PickerEvent {
    // SAFETY: read-only property getters on a filter the framework just
    // handed us; the macOS 15.2 accessors are only sent when the object
    // answers to them.
    unsafe {
        let Some(style) = ShareStyle::from_raw(filter.style().0 as i64) else {
            return PickerEvent::Failed(
                "The picker returned content this app cannot share — pick one window, \
                 one app or one display."
                    .into(),
            );
        };
        let rect = filter.contentRect();
        let scale = f64::from(filter.pointPixelScale()).max(1.0);
        let px = |v: f64| (v * scale).round().max(0.0) as u32;
        PickerEvent::Picked(Picked {
            filter: filter.retain(),
            style,
            summary: summary(filter, style),
            width: px(rect.size.width),
            height: px(rect.size.height),
        })
    }
}

unsafe fn summary(filter: &SCContentFilter, style: ShareStyle) -> String {
    let answers = |s| filter.respondsToSelector(s);
    unsafe {
        match style {
            ShareStyle::Window if answers(sel!(includedWindows)) => {
                if let Some(w) = filter.includedWindows().firstObject() {
                    let title = w.title().map(|t| t.to_string()).unwrap_or_default();
                    let app = w
                        .owningApplication()
                        .map(|a| a.applicationName().to_string())
                        .unwrap_or_default();
                    return join_nonempty(&title, &app, "A window");
                }
                "A window".into()
            }
            ShareStyle::Application if answers(sel!(includedApplications)) => filter
                .includedApplications()
                .firstObject()
                .map(|a| a.applicationName().to_string())
                .filter(|n| !n.is_empty())
                .unwrap_or_else(|| "An app".into()),
            ShareStyle::Display if answers(sel!(includedDisplays)) => filter
                .includedDisplays()
                .firstObject()
                .map(|d| format!("Display {}×{}", d.width(), d.height()))
                .unwrap_or_else(|| "A display".into()),
            ShareStyle::Window => "A window".into(),
            ShareStyle::Application => "An app".into(),
            ShareStyle::Display => "A display".into(),
        }
    }
}

fn join_nonempty(title: &str, app: &str, fallback: &str) -> String {
    match (title.trim(), app.trim()) {
        ("", "") => fallback.into(),
        (t, "") => t.into(),
        ("", a) => a.into(),
        (t, a) => format!("{t} — {a}"),
    }
}
