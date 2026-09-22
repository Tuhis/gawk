//! Windows toasts with R14 Decision 17's urgency mapping (docs/38 D12):
//! normal for went-live / ended / first-viewer; critical (urgent scenario,
//! so it can surface over a fullscreen game — Focus Assist behavior is V-8)
//! for failed-to-start / broadcast error / ended-unexpectedly. Unpackaged
//! apps need an explicit AppUserModelID before the notifier works.

use windows::Data::Xml::Dom::XmlDocument;
use windows::UI::Notifications::{ToastNotification, ToastNotificationManager};
use windows::core::HSTRING;

const AUMID: &str = "gawk.broadcast";

#[derive(Clone, Copy, PartialEq, Eq)]
pub enum Urgency {
    Normal,
    Critical,
}

/// Must run once before the first toast (and before window creation is
/// tidiest). Failure is cosmetic: toasts just won't show; the in-window
/// heartbeat/status stays the fallback truth (D12).
pub fn init() {
    unsafe {
        let _ = windows::Win32::UI::Shell::SetCurrentProcessExplicitAppUserModelID(&HSTRING::from(
            AUMID,
        ));
    }
}

/// Fire-and-forget; never fails the caller.
pub fn show(summary: &str, body: &str, urgency: Urgency) {
    let scenario = match urgency {
        Urgency::Normal => "",
        Urgency::Critical => r#" scenario="urgent""#,
    };
    let xml = format!(
        r#"<toast{scenario}><visual><binding template="ToastGeneric"><text>{}</text><text>{}</text></binding></visual></toast>"#,
        xml_escape(summary),
        xml_escape(body),
    );
    let _ = (|| -> windows::core::Result<()> {
        let doc = XmlDocument::new()?;
        doc.LoadXml(&HSTRING::from(xml))?;
        let toast = ToastNotification::CreateToastNotification(&doc)?;
        let notifier = ToastNotificationManager::CreateToastNotifierWithId(&HSTRING::from(AUMID))?;
        notifier.Show(&toast)
    })();
}

fn xml_escape(s: &str) -> String {
    s.replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
        .replace('"', "&quot;")
}
