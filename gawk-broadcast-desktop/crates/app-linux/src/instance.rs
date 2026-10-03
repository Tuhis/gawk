//! Single instance on Linux (R66, docs/68 D8): the session D-Bus name
//! `fi.ioio.gawk.broadcast`, requested with `DO_NOT_QUEUE`, is the lock.
//! Its owner serves `/fi/ioio/gawk/broadcast` with the interface
//! `fi.ioio.gawk.Broadcast1`: `Raise(s token)` and `OpenUrl(s url,
//! s token)`. `token` is the second launch's `XDG_ACTIVATION_TOKEN`, so the
//! primary can say who asked to be raised.
//!
//! zbus's blocking API on its default async-io executor — never its `tokio`
//! feature (docs/gotchas.md, docs/58 F-2). The object is registered before
//! the name is requested, so a second launch that finds the name always
//! finds the object behind it.

use gawk_ui::instance::{Endpoint, Incoming, MAX_MESSAGE, Request};
use std::io;
use std::sync::mpsc;
use std::time::Duration;
use zbus::blocking::Connection;
use zbus::fdo::{RequestNameFlags, RequestNameReply};

pub const BUS_NAME: &str = "fi.ioio.gawk.broadcast";
pub const OBJECT_PATH: &str = "/fi/ioio/gawk/broadcast";
pub const INTERFACE: &str = "fi.ioio.gawk.Broadcast1";

/// A primary that doesn't answer within this is treated as no primary.
const CALL_TIMEOUT: Duration = Duration::from_secs(5);

/// The D-Bus [`Endpoint`].
pub struct DBus {
    /// The bus to use: `None` is the session bus.
    address: Option<String>,
    /// What this launch passes on as its activation token.
    token: String,
    conn: Option<Connection>,
    /// Filled by the object while this process is the primary, drained to
    /// the shell's inbox once it serves.
    received: Option<mpsc::Receiver<Incoming>>,
}

impl DBus {
    /// The session bus, with this launch's `XDG_ACTIVATION_TOKEN`.
    pub fn session() -> DBus {
        DBus::new(
            None,
            std::env::var("XDG_ACTIVATION_TOKEN").unwrap_or_default(),
        )
    }

    fn new(address: Option<String>, token: String) -> DBus {
        DBus {
            address,
            token,
            conn: None,
            received: None,
        }
    }

    fn connection(&mut self) -> io::Result<&Connection> {
        if self.conn.is_none() {
            let builder = match &self.address {
                Some(a) => zbus::blocking::connection::Builder::address(a.as_str()),
                None => zbus::blocking::connection::Builder::session(),
            }
            .map_err(to_io)?;
            self.conn = Some(
                builder
                    .method_timeout(CALL_TIMEOUT)
                    .build()
                    .map_err(to_io)?,
            );
        }
        Ok(self.conn.as_ref().expect("connected above"))
    }
}

fn to_io(e: zbus::Error) -> io::Error {
    io::Error::other(e)
}

/// The served object: every call becomes an [`Incoming`].
struct Broadcast1 {
    tx: mpsc::Sender<Incoming>,
}

fn activation(token: String) -> Option<String> {
    (!token.is_empty()).then_some(token)
}

#[zbus::interface(name = "fi.ioio.gawk.Broadcast1")]
impl Broadcast1 {
    fn raise(&self, token: String) {
        let _ = self.tx.send(Incoming {
            request: Request::Raise,
            activation: activation(token),
        });
    }

    /// An empty `url` is a launch that named more than one link (D7).
    fn open_url(&self, url: String, token: String) -> zbus::fdo::Result<()> {
        if url.len() > MAX_MESSAGE || token.len() > MAX_MESSAGE {
            return Err(zbus::fdo::Error::InvalidArgs("too long".into()));
        }
        let link = if url.is_empty() { Err(()) } else { Ok(url) };
        let _ = self.tx.send(Incoming {
            request: Request::Open(link),
            activation: activation(token),
        });
        Ok(())
    }
}

impl Endpoint for DBus {
    fn claim(&mut self) -> io::Result<bool> {
        let (tx, rx) = mpsc::channel();
        let conn = self.connection()?.clone();
        // A claim that loses leaves the object on a connection nobody calls;
        // a retry replaces it.
        conn.object_server()
            .at(OBJECT_PATH, Broadcast1 { tx })
            .map_err(to_io)?;
        // zbus reports DBUS_REQUEST_NAME_REPLY_EXISTS as `NameTaken`.
        let owned =
            match conn.request_name_with_flags(BUS_NAME, RequestNameFlags::DoNotQueue.into()) {
                Ok(RequestNameReply::PrimaryOwner | RequestNameReply::AlreadyOwner) => true,
                Ok(RequestNameReply::Exists | RequestNameReply::InQueue)
                | Err(zbus::Error::NameTaken) => false,
                Err(e) => return Err(to_io(e)),
            };
        if owned {
            self.received = Some(rx);
        } else {
            let _ = conn.object_server().remove::<Broadcast1, _>(OBJECT_PATH);
        }
        Ok(owned)
    }

    fn send(&mut self, request: &Request) -> io::Result<()> {
        let token = self.token.clone();
        let conn = self.connection()?;
        let reply = match request {
            Request::Raise => conn.call_method(
                Some(BUS_NAME),
                OBJECT_PATH,
                Some(INTERFACE),
                "Raise",
                &(token.as_str(),),
            ),
            Request::Open(link) => {
                let url = link.as_deref().unwrap_or("");
                if url.len() > MAX_MESSAGE {
                    return Err(io::Error::new(io::ErrorKind::InvalidInput, "link too long"));
                }
                conn.call_method(
                    Some(BUS_NAME),
                    OBJECT_PATH,
                    Some(INTERFACE),
                    "OpenUrl",
                    &(url, token.as_str()),
                )
            }
        };
        reply.map(|_| ()).map_err(to_io)
    }

    fn serve(self: Box<Self>, inbox: mpsc::Sender<Incoming>) -> io::Result<()> {
        let mut this = *self;
        let rx = this
            .received
            .take()
            .ok_or_else(|| io::Error::other("serve before a successful claim"))?;
        let conn = this.conn.take();
        std::thread::Builder::new()
            .name("instance".into())
            .spawn(move || {
                // Holding the connection holds the name; zbus's own
                // executor thread dispatches the calls into `rx`.
                let _conn = conn;
                for incoming in rx {
                    if inbox.send(incoming).is_err() {
                        break; // the shell is gone
                    }
                }
            })?;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use gawk_ui::instance::{Startup, startup};
    use std::process::{Child, Command, Stdio};

    /// A private `dbus-daemon` for one test: its address is passed to the
    /// endpoints, never put in the process environment the tests share.
    struct Bus {
        child: Child,
        address: String,
        _dir: std::path::PathBuf,
    }

    impl Drop for Bus {
        fn drop(&mut self) {
            let _ = self.child.kill();
            let _ = self.child.wait();
            let _ = std::fs::remove_dir_all(&self._dir);
        }
    }

    fn bus(tag: &str) -> Option<Bus> {
        let dir = std::env::temp_dir().join(format!("gawk-dbus-{tag}-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let address = format!("unix:path={}", dir.join("bus").display());
        let child = Command::new("dbus-daemon")
            .args(["--session", "--nofork", "--nopidfile"])
            .arg(format!("--address={address}"))
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn();
        let child = match child {
            Ok(c) => c,
            Err(e) => {
                assert!(
                    std::env::var_os("GAWK_REQUIRE_DBUS").is_none(),
                    "GAWK_REQUIRE_DBUS is set but dbus-daemon could not start: {e}"
                );
                eprintln!("skipping: no dbus-daemon ({e})");
                return None;
            }
        };
        // Wait for the socket.
        for _ in 0..100 {
            if dir.join("bus").exists() {
                break;
            }
            std::thread::sleep(Duration::from_millis(20));
        }
        Some(Bus {
            child,
            address,
            _dir: dir,
        })
    }

    fn endpoint(bus: &Bus, token: &str) -> DBus {
        DBus::new(Some(bus.address.clone()), token.into())
    }

    fn primary(bus: &Bus) -> mpsc::Receiver<Incoming> {
        let mut p = endpoint(bus, "");
        assert_eq!(startup(&mut p, &Request::Raise, false), Startup::Primary);
        let (tx, rx) = mpsc::channel();
        Box::new(p).serve(tx).unwrap();
        rx
    }

    fn recv(rx: &mpsc::Receiver<Incoming>) -> Incoming {
        rx.recv_timeout(Duration::from_secs(5))
            .expect("the primary's inbox got the request")
    }

    // LH3: a second launch's link reaches the running instance's inbox,
    // and the second launch is told to exit.
    #[test]
    fn a_second_launch_hands_its_link_to_the_primary() {
        let Some(bus) = bus("open") else { return };
        let rx = primary(&bus);

        let link = "gawk://broadcast?room=lan-party&nick=J%C3%BCrgen";
        let mut s = endpoint(&bus, "act-123");
        let req = Request::Open(Ok(link.into()));
        assert_eq!(startup(&mut s, &req, false), Startup::HandedOff);
        assert_eq!(
            recv(&rx),
            Incoming {
                request: req,
                activation: Some("act-123".into()),
            }
        );

        let mut s = endpoint(&bus, "");
        assert_eq!(startup(&mut s, &Request::Raise, false), Startup::HandedOff);
        assert_eq!(
            recv(&rx),
            Incoming {
                request: Request::Raise,
                activation: None,
            }
        );

        // Two links on one launch arrive as the rejected link.
        let mut s = endpoint(&bus, "");
        assert_eq!(
            startup(&mut s, &Request::Open(Err(())), false),
            Startup::HandedOff
        );
        assert_eq!(recv(&rx).request, Request::Open(Err(())));
    }

    #[test]
    fn the_name_has_one_owner_and_caps_hold() {
        let Some(bus) = bus("owner") else { return };
        let _rx = primary(&bus);
        let mut other = endpoint(&bus, "");
        assert!(!other.claim().unwrap(), "the name is taken");

        let huge = format!("gawk://broadcast?nick={}", "x".repeat(MAX_MESSAGE));
        assert!(other.send(&Request::Open(Ok(huge.clone()))).is_err());
        // The primary refuses one too, whoever sent it.
        let conn = other.connection().unwrap();
        assert!(
            conn.call_method(
                Some(BUS_NAME),
                OBJECT_PATH,
                Some(INTERFACE),
                "OpenUrl",
                &(huge.as_str(), ""),
            )
            .is_err()
        );
    }

    #[test]
    fn no_bus_is_an_error_not_a_panic() {
        let mut ep = DBus::new(
            Some("unix:path=/nonexistent/gawk-bus".into()),
            String::new(),
        );
        assert!(ep.send(&Request::Raise).is_err());
        assert!(ep.claim().is_err());
        assert!(matches!(
            startup(&mut ep, &Request::Raise, false),
            Startup::Alone(_)
        ));
    }
}
