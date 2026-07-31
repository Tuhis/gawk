//! DPAPI credential wrapping (docs/38 D14): `publishSecret` and
//! `lastResumeToken` are stored as `dpapi:<base64>` under per-user scope —
//! zero prompts, and a copied config file leaks nothing on another machine.
//! Plaintext values are accepted on read (hand-editing stays possible) and
//! re-encrypted on the next save.

use crate::base64util;
use gawk_engine::config::Credentials;
use windows::Win32::Foundation::LocalFree;
use windows::Win32::Security::Cryptography::{
    CRYPT_INTEGER_BLOB, CRYPTPROTECT_UI_FORBIDDEN, CryptProtectData, CryptUnprotectData,
};

const PREFIX: &str = "dpapi:";

pub struct Dpapi;

impl Credentials for Dpapi {
    fn wrap(&self, value: &str) -> String {
        if value.is_empty() {
            return String::new();
        }
        match protect(value.as_bytes()) {
            Some(cipher) => format!("{PREFIX}{}", base64util::encode(&cipher)),
            // Refusing to save beats saving in the clear silently? No —
            // the Linux file is plaintext-with-0600; a DPAPI hiccup
            // degrading to the Linux posture (with a stderr note) keeps
            // the app usable.
            None => {
                eprintln!("DPAPI protect failed; storing the value unwrapped");
                value.to_owned()
            }
        }
    }

    fn unwrap(&self, stored: &str) -> String {
        let Some(b64) = stored.strip_prefix(PREFIX) else {
            return stored.to_owned(); // plaintext accepted
        };
        base64util::decode(b64)
            .and_then(|cipher| unprotect(&cipher))
            .and_then(|plain| String::from_utf8(plain).ok())
            .unwrap_or_else(|| {
                // Wrong user/machine or corrupt: an empty credential, not
                // a crash — the user re-enters the secret.
                eprintln!("could not decrypt a stored credential; ignoring it");
                String::new()
            })
    }
}

fn protect(data: &[u8]) -> Option<Vec<u8>> {
    dpapi_call(data, |blob_in, blob_out| unsafe {
        CryptProtectData(
            blob_in,
            None,
            None,
            None,
            None,
            CRYPTPROTECT_UI_FORBIDDEN,
            blob_out,
        )
    })
}

fn unprotect(data: &[u8]) -> Option<Vec<u8>> {
    dpapi_call(data, |blob_in, blob_out| unsafe {
        CryptUnprotectData(
            blob_in,
            None,
            None,
            None,
            None,
            CRYPTPROTECT_UI_FORBIDDEN,
            blob_out,
        )
    })
}

fn dpapi_call(
    data: &[u8],
    f: impl Fn(*const CRYPT_INTEGER_BLOB, *mut CRYPT_INTEGER_BLOB) -> windows::core::Result<()>,
) -> Option<Vec<u8>> {
    let blob_in = CRYPT_INTEGER_BLOB {
        cbData: data.len() as u32,
        pbData: data.as_ptr() as *mut u8,
    };
    let mut blob_out = CRYPT_INTEGER_BLOB::default();
    f(&blob_in, &mut blob_out).ok()?;
    let out =
        unsafe { std::slice::from_raw_parts(blob_out.pbData, blob_out.cbData as usize).to_vec() };
    unsafe {
        let _ = LocalFree(Some(windows::Win32::Foundation::HLOCAL(
            blob_out.pbData as *mut _,
        )));
    }
    Some(out)
}
