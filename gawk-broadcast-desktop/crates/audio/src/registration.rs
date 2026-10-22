//! A callback registration that is undone before its callback is released.
//!
//! `IMMDeviceEnumerator::RegisterEndpointNotificationCallback` keeps a raw
//! pointer to the client: it takes no reference of its own, so releasing the
//! last reference while still registered leaves the audio service calling
//! freed memory on the next device event — an access violation inside
//! `mmdevapi.dll` (2026-10-22, field). Portable so the ordering is tested on
//! any host; `wasapi.rs` supplies the COM half.

/// Undoes a registration, given the callback it registered.
type Unregister<T> = Box<dyn FnOnce(&T)>;

/// Owns a registered callback object and the means to unregister it.
/// Dropping it unregisters first, then releases the object.
pub struct Registration<T> {
    callback: Option<T>,
    unregister: Option<Unregister<T>>,
}

impl<T> Registration<T> {
    /// `callback` is already registered; `unregister` undoes that and must
    /// tolerate failing (there is nothing to do about it on teardown).
    pub fn new(callback: T, unregister: impl FnOnce(&T) + 'static) -> Self {
        Self {
            callback: Some(callback),
            unregister: Some(Box::new(unregister)),
        }
    }
}

impl<T> Drop for Registration<T> {
    fn drop(&mut self) {
        if let (Some(callback), Some(unregister)) = (&self.callback, self.unregister.take()) {
            unregister(callback);
        }
        // Only now may the last reference go.
        self.callback.take();
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{Arc, Mutex};

    /// A callback that records its own release.
    struct Callback(Arc<Mutex<Vec<&'static str>>>);

    impl Drop for Callback {
        fn drop(&mut self) {
            self.0.lock().unwrap().push("released");
        }
    }

    #[test]
    fn unregisters_before_releasing_the_callback() {
        let log = Arc::new(Mutex::new(Vec::new()));
        let reg = Registration::new(Callback(log.clone()), {
            let log = log.clone();
            move |_cb: &Callback| log.lock().unwrap().push("unregistered")
        });
        drop(reg);
        assert_eq!(*log.lock().unwrap(), ["unregistered", "released"]);
    }

    #[test]
    fn unregisters_on_an_error_exit_too() {
        let log = Arc::new(Mutex::new(Vec::new()));
        let run = || -> Result<(), String> {
            let _reg = Registration::new(Callback(log.clone()), {
                let log = log.clone();
                move |_cb: &Callback| log.lock().unwrap().push("unregistered")
            });
            Err("capture next-packet: device invalidated".into())
        };
        assert!(run().is_err());
        assert_eq!(*log.lock().unwrap(), ["unregistered", "released"]);
    }
}
