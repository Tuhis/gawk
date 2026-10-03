//! Single instance on Windows (R66 OD1, docs/68 D8): the named mutex
//! `Local\fi.ioio.gawk.broadcast` decides who is primary, and the primary
//! listens on `\\.\pipe\fi.ioio.gawk.broadcast.<user SID>`, a pipe only the
//! current user can open and no remote client can reach. One line in
//! ([`gawk_ui::instance::Request`]'s format), `ok` back, per connection.
//!
//! The framing is portable and tested on any host; the endpoint itself is
//! Windows code, checked on a real machine in LH6 (docs/38 D18: CI has no
//! Windows runner).

use gawk_ui::instance::{MAX_MESSAGE, OK, Request};
use std::io::{self, Read, Write};

/// Reads one `\n`-terminated line (the `\n` dropped), refusing anything
/// longer than [`MAX_MESSAGE`] before it is all read. A line cut short by
/// the other end closing is an error.
pub fn read_line(r: &mut impl Read) -> io::Result<Vec<u8>> {
    let mut line = Vec::new();
    let mut byte = [0u8; 1];
    loop {
        if r.read(&mut byte)? == 0 {
            return Err(io::ErrorKind::UnexpectedEof.into());
        }
        if byte[0] == b'\n' {
            return Ok(line);
        }
        if line.len() == MAX_MESSAGE {
            return Err(io::Error::new(io::ErrorKind::InvalidData, "line too long"));
        }
        line.push(byte[0]);
    }
}

/// The primary's side of one connection: the request, answered `ok`, or
/// `None` (and no answer) for anything unreadable.
pub fn serve_one(stream: &mut (impl Read + Write)) -> Option<Request> {
    let line = read_line(stream).ok()?;
    let request = Request::decode(&line)?;
    stream.write_all(format!("{OK}\n").as_bytes()).ok()?;
    stream.flush().ok()?;
    Some(request)
}

/// The second launch's side: the request out, and `ok` back.
pub fn send_one(stream: &mut (impl Read + Write), request: &Request) -> io::Result<()> {
    stream.write_all(format!("{}\n", request.encode()).as_bytes())?;
    stream.flush()?;
    let answer = read_line(stream)?;
    if answer.strip_suffix(b"\r").unwrap_or(&answer) == OK.as_bytes() {
        Ok(())
    } else {
        Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "the running instance did not accept the request",
        ))
    }
}

#[cfg(windows)]
pub use win::Pipe;

#[cfg(windows)]
mod win {
    use super::{send_one, serve_one};
    use gawk_ui::instance::{Endpoint, Incoming, Request};
    use std::io;
    use std::os::windows::io::{AsRawHandle, FromRawHandle};
    use std::sync::mpsc;
    use std::time::Duration;
    use windows::Win32::Foundation::{
        CloseHandle, ERROR_ALREADY_EXISTS, ERROR_PIPE_CONNECTED, GetLastError, HANDLE, HLOCAL,
        INVALID_HANDLE_VALUE, LocalFree,
    };
    use windows::Win32::Security::Authorization::{
        ConvertSidToStringSidW, ConvertStringSecurityDescriptorToSecurityDescriptorW,
        SDDL_REVISION_1,
    };
    use windows::Win32::Security::{
        GetTokenInformation, PSECURITY_DESCRIPTOR, SECURITY_ATTRIBUTES, TOKEN_QUERY, TOKEN_USER,
        TokenUser,
    };
    use windows::Win32::Storage::FileSystem::{
        FILE_FLAG_FIRST_PIPE_INSTANCE, FILE_FLAGS_AND_ATTRIBUTES, PIPE_ACCESS_DUPLEX,
    };
    use windows::Win32::System::Pipes::{
        ConnectNamedPipe, CreateNamedPipeW, GetNamedPipeServerProcessId, PIPE_READMODE_BYTE,
        PIPE_REJECT_REMOTE_CLIENTS, PIPE_TYPE_BYTE, PIPE_UNLIMITED_INSTANCES, PIPE_WAIT,
    };
    use windows::Win32::System::Threading::{CreateMutexW, GetCurrentProcess, OpenProcessToken};
    use windows::Win32::UI::WindowsAndMessaging::AllowSetForegroundWindow;
    use windows::core::{HSTRING, PWSTR, w};

    /// How long a second launch waits for the primary to answer before
    /// treating it as not there.
    const ANSWER_TIMEOUT: Duration = Duration::from_secs(3);
    /// ERROR_PIPE_BUSY: every instance of the pipe is serving someone.
    const PIPE_BUSY: i32 = 231;

    /// The Windows [`Endpoint`].
    pub struct Pipe {
        /// The current user's SID, as a string.
        sid: String,
        /// The mutex, held for the life of the process once claimed.
        mutex: Option<HANDLE>,
    }

    // SAFETY: a mutex handle is a process-wide kernel handle, usable from
    // any thread.
    unsafe impl Send for Pipe {}

    impl Pipe {
        /// The endpoint for this user, or why there is none.
        pub fn new() -> io::Result<Pipe> {
            Ok(Pipe {
                sid: user_sid()?,
                mutex: None,
            })
        }

        fn name(&self) -> String {
            format!(r"\\.\pipe\fi.ioio.gawk.broadcast.{}", self.sid)
        }
    }

    impl Endpoint for Pipe {
        fn claim(&mut self) -> io::Result<bool> {
            if self.mutex.is_some() {
                return Ok(true);
            }
            // SAFETY: a named mutex with default security; the name is a
            // static NUL-terminated string.
            let handle = unsafe { CreateMutexW(None, false, w!(r"Local\fi.ioio.gawk.broadcast")) }
                .map_err(io::Error::other)?;
            // SAFETY: read straight after the call that set it.
            if unsafe { GetLastError() } == ERROR_ALREADY_EXISTS {
                // Let go of it, so a later retry (an update relaunch) can
                // find it gone once the other process exits.
                // SAFETY: `handle` was just opened and is not used again.
                let _ = unsafe { CloseHandle(handle) };
                return Ok(false);
            }
            self.mutex = Some(handle);
            Ok(true)
        }

        fn send(&mut self, request: &Request) -> io::Result<()> {
            let name = self.name();
            let mut pipe = match open(&name) {
                Err(e) if e.raw_os_error() == Some(PIPE_BUSY) => {
                    std::thread::sleep(Duration::from_millis(200));
                    open(&name)?
                }
                r => r?,
            };
            // The second launch holds the foreground right (the user just
            // launched it); pass it on, or the primary's SetForegroundWindow
            // only flashes its taskbar button (D8).
            let mut pid = 0u32;
            // SAFETY: `pipe` is an open client end of a named pipe.
            if unsafe { GetNamedPipeServerProcessId(HANDLE(pipe.as_raw_handle()), &mut pid) }
                .is_ok()
            {
                // SAFETY: plain call on a process id.
                let _ = unsafe { AllowSetForegroundWindow(pid) };
            }
            // A primary that accepts the connection but never answers must
            // not hang this launch: the exchange runs on a thread of its own.
            let request = request.clone();
            let (tx, rx) = mpsc::channel();
            std::thread::spawn(move || {
                let _ = tx.send(send_one(&mut pipe, &request));
            });
            rx.recv_timeout(ANSWER_TIMEOUT)
                .unwrap_or_else(|_| Err(io::ErrorKind::TimedOut.into()))
        }

        fn serve(self: Box<Self>, inbox: mpsc::Sender<Incoming>) -> io::Result<()> {
            let name = HSTRING::from(self.name());
            let sddl = HSTRING::from(format!("D:P(A;;GA;;;{})", self.sid));
            let mut sd = PSECURITY_DESCRIPTOR::default();
            // SAFETY: `sd` receives a LocalAlloc'd descriptor, kept for the
            // life of the process (every pipe instance is created with it).
            unsafe {
                ConvertStringSecurityDescriptorToSecurityDescriptorW(
                    &sddl,
                    SDDL_REVISION_1,
                    &mut sd,
                    None,
                )
            }
            .map_err(io::Error::other)?;
            // The first instance is created here, so a failure is the
            // caller's to report; later ones on the serving thread. Both
            // cross to it as addresses: the descriptor is never freed and
            // the handle is a process-wide kernel handle.
            let first = create(&name, sd.0, true)?.0 as usize;
            let sd = sd.0 as usize;
            std::thread::Builder::new()
                .name("gawk-instance".into())
                .spawn(move || {
                    let mut next = Some(HANDLE(first as *mut core::ffi::c_void));
                    loop {
                        let pipe = match next.take() {
                            Some(p) => p,
                            None => match create(&name, sd as *mut core::ffi::c_void, false) {
                                Ok(p) => p,
                                Err(e) => {
                                    log::warn!("single instance: pipe gone ({e})");
                                    return;
                                }
                            },
                        };
                        // SAFETY: `pipe` is a server end created above.
                        let connected = match unsafe { ConnectNamedPipe(pipe, None) } {
                            Ok(()) => true,
                            Err(e) => e.code() == ERROR_PIPE_CONNECTED.to_hresult(),
                        };
                        // SAFETY: ownership of the handle moves to the File,
                        // which closes it.
                        let mut file = unsafe { std::fs::File::from_raw_handle(pipe.0) };
                        if !connected {
                            continue;
                        }
                        let inbox = inbox.clone();
                        // One thread per connection: a client that never
                        // writes holds only its own instance.
                        std::thread::spawn(move || {
                            if let Some(request) = serve_one(&mut file) {
                                let _ = inbox.send(Incoming {
                                    request,
                                    activation: None,
                                });
                            } else {
                                log::info!("single instance: dropped an unreadable request");
                            }
                        });
                    }
                })?;
            Ok(())
        }
    }

    fn open(name: &str) -> io::Result<std::fs::File> {
        std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .open(name)
    }

    fn create(name: &HSTRING, sd: *mut core::ffi::c_void, first: bool) -> io::Result<HANDLE> {
        let sa = SECURITY_ATTRIBUTES {
            nLength: size_of::<SECURITY_ATTRIBUTES>() as u32,
            lpSecurityDescriptor: sd,
            bInheritHandle: false.into(),
        };
        let mode = if first {
            PIPE_ACCESS_DUPLEX | FILE_FLAG_FIRST_PIPE_INSTANCE
        } else {
            PIPE_ACCESS_DUPLEX | FILE_FLAGS_AND_ATTRIBUTES(0)
        };
        // SAFETY: `sa` and the descriptor it points at outlive the call.
        let h = unsafe {
            CreateNamedPipeW(
                name,
                mode,
                PIPE_TYPE_BYTE | PIPE_READMODE_BYTE | PIPE_WAIT | PIPE_REJECT_REMOTE_CLIENTS,
                PIPE_UNLIMITED_INSTANCES,
                4096,
                4096,
                0,
                Some(&sa),
            )
        };
        if h == INVALID_HANDLE_VALUE || h.is_invalid() {
            return Err(io::Error::last_os_error());
        }
        Ok(h)
    }

    /// The current user's SID as a string (`S-1-5-21-…`).
    fn user_sid() -> io::Result<String> {
        let mut token = HANDLE::default();
        // SAFETY: the pseudo-handle of this process; `token` is closed below.
        unsafe { OpenProcessToken(GetCurrentProcess(), TOKEN_QUERY, &mut token) }
            .map_err(io::Error::other)?;
        let mut len = 0u32;
        // SAFETY: a size query; it fails with the size it needs.
        let _ = unsafe { GetTokenInformation(token, TokenUser, None, 0, &mut len) };
        let mut buf = vec![0u64; (len as usize).div_ceil(8).max(1)];
        // SAFETY: `buf` is 8-aligned and at least `len` bytes.
        let got = unsafe {
            GetTokenInformation(
                token,
                TokenUser,
                Some(buf.as_mut_ptr().cast()),
                len,
                &mut len,
            )
        };
        // SAFETY: opened above, not used again.
        let _ = unsafe { CloseHandle(token) };
        got.map_err(io::Error::other)?;
        // SAFETY: the call filled `buf` with a TOKEN_USER.
        let user = unsafe { &*buf.as_ptr().cast::<TOKEN_USER>() };
        let mut s = PWSTR::null();
        // SAFETY: the SID points into `buf`, alive here; `s` is
        // LocalAlloc'd and freed below.
        unsafe { ConvertSidToStringSidW(user.User.Sid, &mut s) }.map_err(io::Error::other)?;
        // SAFETY: `s` is a NUL-terminated wide string from the call above.
        let sid = unsafe { s.to_string() }.map_err(io::Error::other);
        // SAFETY: as above.
        unsafe {
            let _ = LocalFree(Some(HLOCAL(s.0.cast())));
        }
        sid
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Cursor;

    /// One end of a connection: what the other side wrote, and what this
    /// side writes back.
    struct Duplex {
        incoming: Cursor<Vec<u8>>,
        outgoing: Vec<u8>,
    }

    impl Duplex {
        fn new(incoming: &[u8]) -> Self {
            Duplex {
                incoming: Cursor::new(incoming.to_vec()),
                outgoing: Vec::new(),
            }
        }
    }

    impl Read for Duplex {
        fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
            self.incoming.read(buf)
        }
    }

    impl Write for Duplex {
        fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
            self.outgoing.write(buf)
        }
        fn flush(&mut self) -> io::Result<()> {
            Ok(())
        }
    }

    #[test]
    fn a_request_crosses_the_pipe_and_is_answered() {
        let request = Request::Open(Ok("gawk://broadcast?room=abc&nick=J%C3%BCrgen".into()));
        // The second launch writes its line and expects `ok`.
        let mut client = Duplex::new(b"ok\n");
        send_one(&mut client, &request).unwrap();
        // That line, served by the primary.
        let mut server = Duplex::new(&client.outgoing);
        assert_eq!(serve_one(&mut server), Some(request));
        assert_eq!(server.outgoing, b"ok\n");
    }

    #[test]
    fn a_bad_or_oversized_line_gets_no_answer() {
        for bad in [
            b"quit\n".to_vec(),
            b"raise".to_vec(), // cut short: no newline
            format!("open {}\n", "x".repeat(MAX_MESSAGE)).into_bytes(),
        ] {
            let mut server = Duplex::new(&bad);
            assert_eq!(serve_one(&mut server), None);
            assert!(server.outgoing.is_empty());
        }
    }

    #[test]
    fn the_line_cap_is_exact() {
        let fits = format!("{}\n", "x".repeat(MAX_MESSAGE));
        assert_eq!(
            read_line(&mut Cursor::new(fits.into_bytes()))
                .unwrap()
                .len(),
            MAX_MESSAGE
        );
        let over = format!("{}\n", "x".repeat(MAX_MESSAGE + 1));
        assert!(read_line(&mut Cursor::new(over.into_bytes())).is_err());
    }

    #[test]
    fn anything_but_ok_is_a_refusal() {
        let mut client = Duplex::new(b"no\n");
        assert!(send_one(&mut client, &Request::Raise).is_err());
        let mut client = Duplex::new(b"ok\r\n");
        assert!(send_one(&mut client, &Request::Raise).is_ok());
        let mut client = Duplex::new(b"");
        assert!(send_one(&mut client, &Request::Raise).is_err());
    }
}
