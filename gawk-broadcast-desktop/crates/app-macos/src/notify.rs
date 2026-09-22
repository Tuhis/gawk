//! Notifications through `UNUserNotificationCenter` (docs/54 D11), with the
//! docs/38 D12 urgency mapping: normal → a banner, critical → a banner with
//! the default sound. Every notification is also a debug-log line.
//!
//! The notification center needs a bundle identifier — in a bare dev binary
//! it throws an Objective-C exception — which is one more reason the product
//! is a bundle (D14). Outside one, notifications go to the debug log only.
//! Focus modes are V-8's to measure: nothing here asks for time-sensitive
//! delivery (that needs an entitlement this app does not request, D13).

use objc2::rc::Retained;
use objc2::runtime::{NSObject, NSObjectProtocol, ProtocolObject};
use objc2::{AnyThread, define_class, msg_send};
use objc2_foundation::{NSBundle, NSError, NSString};
use objc2_user_notifications::{
    UNAuthorizationOptions, UNMutableNotificationContent, UNNotification,
    UNNotificationInterruptionLevel, UNNotificationPresentationOptions, UNNotificationRequest,
    UNNotificationSound, UNUserNotificationCenter, UNUserNotificationCenterDelegate,
};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};

static ENABLED: AtomicBool = AtomicBool::new(false);
static SEQ: AtomicU64 = AtomicU64::new(0);

define_class!(
    // SAFETY: NSObject has no subclassing requirements and this class does
    // not implement Drop.
    #[unsafe(super(NSObject))]
    #[name = "GawkNotificationDelegate"]
    struct Delegate;

    unsafe impl NSObjectProtocol for Delegate {}

    unsafe impl UNUserNotificationCenterDelegate for Delegate {
        /// Show banners even while the app is frontmost, as the Windows
        /// toasts do — "broadcast ended unexpectedly" matters most exactly
        /// when the user is looking at this window.
        #[unsafe(method(userNotificationCenter:willPresentNotification:withCompletionHandler:))]
        fn will_present(
            &self,
            _center: &UNUserNotificationCenter,
            _notification: &UNNotification,
            completion: &block2::DynBlock<dyn Fn(UNNotificationPresentationOptions)>,
        ) {
            completion.call((UNNotificationPresentationOptions::Banner
                | UNNotificationPresentationOptions::List
                | UNNotificationPresentationOptions::Sound,));
        }
    }
);

/// At launch, on the main thread: installs the delegate and asks for
/// permission (the system prompts once, on the first run).
pub fn init() {
    if NSBundle::mainBundle().bundleIdentifier().is_none() {
        log::info!("not running from the app bundle: notifications go to the debug log only");
        return;
    }
    let center = UNUserNotificationCenter::currentNotificationCenter();
    // SAFETY: NSObject's designated initializer on a fresh instance.
    let delegate: Retained<Delegate> =
        unsafe { msg_send![super(Delegate::alloc().set_ivars(())), init] };
    center.setDelegate(Some(ProtocolObject::from_ref(&*delegate)));
    // The center holds its delegate weakly; this one lives as long as the
    // process does.
    std::mem::forget(delegate);
    let answered = block2::RcBlock::new(|granted: objc2::runtime::Bool, err: *mut NSError| {
        // SAFETY: a non-null error is a live NSError for the handler.
        match unsafe { err.as_ref() } {
            Some(e) => log::warn!("notification permission: {}", e.localizedDescription()),
            None => log::info!("notification permission granted: {}", granted.as_bool()),
        }
    });
    center.requestAuthorizationWithOptions_completionHandler(
        UNAuthorizationOptions::Alert | UNAuthorizationOptions::Sound,
        &answered,
    );
    ENABLED.store(true, Ordering::Release);
}

/// The shell's notify hook.
pub fn notify(summary: &str, body: &str, critical: bool) {
    if critical {
        log::warn!("notification: {summary}: {body}");
    } else {
        log::info!("notification: {summary}: {body}");
    }
    if !ENABLED.load(Ordering::Acquire) {
        return;
    }
    let content = UNMutableNotificationContent::new();
    content.setTitle(&NSString::from_str(summary));
    content.setBody(&NSString::from_str(body));
    content.setInterruptionLevel(UNNotificationInterruptionLevel::Active);
    if critical {
        content.setSound(Some(&UNNotificationSound::defaultSound()));
    }
    let id = NSString::from_str(&format!("gawk-{}", SEQ.fetch_add(1, Ordering::Relaxed)));
    let request = UNNotificationRequest::requestWithIdentifier_content_trigger(&id, &content, None);
    UNUserNotificationCenter::currentNotificationCenter()
        .addNotificationRequest_withCompletionHandler(&request, None);
}
