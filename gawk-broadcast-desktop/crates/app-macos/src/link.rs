//! `gawk://` links on macOS (R66, docs/68 D10). `Info.plist` declares the
//! scheme (`CFBundleURLTypes`) and LaunchServices registers it when the
//! bundle is first launched or copied. A link arrives as an Apple Event
//! (`kInternetEventClass`/`kAEGetURL`), not in argv, on a cold start and
//! while running alike; a second launch of the bundle goes to the running
//! process too, so macOS needs no single-instance endpoint of its own (D8).
//!
//! winit 0.30 owns the `NSApplicationDelegate` and doesn't forward
//! `application:openURLs:`, and AppKit installs its own `GURL` handler
//! during `finishLaunching`, replacing one installed earlier. So the handler
//! goes in from `NSApplicationWillFinishLaunchingNotification` — the point
//! Apple documents for Apple Event handlers — through an observer
//! registered before the shell starts the event loop.
//!
//! The selectors are sent with `msg_send!` and plain `u32` four-char codes:
//! objc2-foundation types `setEventHandler:…` and `paramDescriptorForKeyword:`
//! behind its `objc2-core-services` feature, a whole framework crate for
//! two integer typedefs.

use gawk_ui::instance::{Incoming, Request};
use objc2::rc::Retained;
use objc2::runtime::{NSObject, NSObjectProtocol};
use objc2::{AnyThread, define_class, msg_send, sel};
use objc2_app_kit::NSApplicationWillFinishLaunchingNotification;
use objc2_foundation::{
    NSAppleEventDescriptor, NSAppleEventManager, NSBundle, NSNotification, NSNotificationCenter,
};
use std::sync::OnceLock;
use std::sync::mpsc;

/// A four-char code as Carbon spells it: `'GURL'` is `0x4755524C`.
const fn four_cc(code: &[u8; 4]) -> u32 {
    u32::from_be_bytes(*code)
}

/// `kInternetEventClass` and `kAEGetURL` — both `'GURL'`.
const INTERNET_EVENT_CLASS: u32 = four_cc(b"GURL");
const GET_URL: u32 = four_cc(b"GURL");
/// `keyDirectObject`: the event parameter that holds the URL.
const DIRECT_OBJECT: u32 = four_cc(b"----");

/// Where links go: the shell's inbox, for the life of the process.
static INBOX: OnceLock<mpsc::Sender<Incoming>> = OnceLock::new();

define_class!(
    // SAFETY: NSObject has no subclassing requirements and this class does
    // not implement Drop.
    #[unsafe(super(NSObject))]
    #[name = "GawkLinkHandler"]
    struct Handler;

    unsafe impl NSObjectProtocol for Handler {}

    impl Handler {
        /// `NSApplicationWillFinishLaunchingNotification`: install the
        /// `GURL` handler now, after AppKit's own and before the launch's
        /// events are dispatched.
        #[unsafe(method(willFinishLaunching:))]
        fn will_finish_launching(&self, _notification: &NSNotification) {
            let manager = NSAppleEventManager::sharedAppleEventManager();
            // SAFETY: `self` lives for the process (it is never released,
            // see `init`) and implements the selector with the signature
            // NSAppleEventManager calls: two NSAppleEventDescriptors.
            let _: () = unsafe {
                msg_send![
                    &*manager,
                    setEventHandler: self,
                    andSelector: sel!(handleGetURLEvent:withReplyEvent:),
                    forEventClass: INTERNET_EVENT_CLASS,
                    andEventID: GET_URL
                ]
            };
            log::info!("gawk:// link handler installed");
        }

        /// One `gawk://` link, cold or warm. Logged by kind only: a link may
        /// hold a raw broadcast ID (docs/68 D13).
        #[unsafe(method(handleGetURLEvent:withReplyEvent:))]
        fn handle_get_url(&self, event: &NSAppleEventDescriptor, _reply: &NSAppleEventDescriptor) {
            log::info!("link event received");
            // SAFETY: `paramDescriptorForKeyword:` takes an AEKeyword
            // (FourCharCode, a UInt32) and returns a descriptor or nil.
            let param: Option<Retained<NSAppleEventDescriptor>> =
                unsafe { msg_send![event, paramDescriptorForKeyword: DIRECT_OBJECT] };
            let Some(url) = param.and_then(|d| d.stringValue()) else {
                log::warn!("a link event without a URL; ignored");
                return;
            };
            let Some(inbox) = INBOX.get() else {
                return;
            };
            let _ = inbox.send(Incoming {
                request: Request::Open(Ok(url.to_string())),
                activation: None,
            });
        }
    }
);

/// At launch, on the main thread and before the event loop starts: routes
/// `gawk://` links to `inbox`. False — and nothing installed — for a dev
/// binary outside the app bundle, which LaunchServices never sends links
/// to, as [`crate::notify::init`] does.
pub fn init(inbox: mpsc::Sender<Incoming>) -> bool {
    if NSBundle::mainBundle().bundleIdentifier().is_none() {
        log::info!("not running from the app bundle: gawk:// links are not handled");
        return false;
    }
    if INBOX.set(inbox).is_err() {
        return true;
    }
    // SAFETY: NSObject's designated initializer on a fresh instance.
    let handler: Retained<Handler> =
        unsafe { msg_send![super(Handler::alloc().set_ivars(())), init] };
    // SAFETY: `handler` implements `willFinishLaunching:` taking one
    // NSNotification, and lives for the process (below), so the center's
    // unretained reference never dangles.
    unsafe {
        NSNotificationCenter::defaultCenter().addObserver_selector_name_object(
            &handler,
            sel!(willFinishLaunching:),
            Some(NSApplicationWillFinishLaunchingNotification),
            None,
        );
    }
    // The notification center and the Apple Event manager both hold the
    // handler unretained; it lives as long as the process does.
    std::mem::forget(handler);
    true
}
