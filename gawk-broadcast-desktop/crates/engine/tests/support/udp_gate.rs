//! A UDP forwarder in front of a relay that loses traffic for a while, then
//! forwards both ways — the first seconds of a freshly booted CI Simulator,
//! whose early packets never arrive (R65 IO5's flake). Test-only, included
//! with `#[path]` like `relay.rs`.
#![allow(dead_code)]

use std::collections::HashMap;
use std::net::{SocketAddr, UdpSocket};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

/// Which direction the gate loses while it is closed.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Lose {
    /// Client → relay: the relay never hears the client.
    Up,
    /// Relay → client: the relay answers, the client never hears it.
    Down,
}

pub struct UdpGate {
    /// Where clients dial instead of the relay.
    pub addr: SocketAddr,
    clients: Arc<Mutex<Vec<SocketAddr>>>,
}

impl UdpGate {
    /// Forwards to `relay`, losing `lose`'s direction for the first
    /// `closed_for` after it starts.
    pub fn start(relay: SocketAddr, lose: Lose, closed_for: Duration) -> UdpGate {
        let front = UdpSocket::bind("127.0.0.1:0").unwrap();
        let addr = front.local_addr().unwrap();
        let start = Instant::now();
        let closed = move || start.elapsed() < closed_for;
        let clients = Arc::new(Mutex::new(Vec::new()));
        let seen = clients.clone();
        std::thread::spawn(move || {
            // One relay-side socket per client source address, so the relay
            // tells clients apart exactly as it would without the gate.
            let mut legs: HashMap<SocketAddr, UdpSocket> = HashMap::new();
            let mut buf = [0u8; 65536];
            while let Ok((n, from)) = front.recv_from(&mut buf) {
                let leg = legs.entry(from).or_insert_with(|| {
                    seen.lock().unwrap().push(from);
                    let up = UdpSocket::bind("127.0.0.1:0").unwrap();
                    up.connect(relay).unwrap();
                    let (back, front) = (up.try_clone().unwrap(), front.try_clone().unwrap());
                    std::thread::spawn(move || {
                        let mut buf = [0u8; 65536];
                        while let Ok(n) = back.recv(&mut buf) {
                            if !(lose == Lose::Down && closed()) {
                                let _ = front.send_to(&buf[..n], from);
                            }
                        }
                    });
                    up
                });
                if !(lose == Lose::Up && closed()) {
                    let _ = leg.send(&buf[..n]);
                }
            }
        });
        UdpGate { addr, clients }
    }

    /// The distinct client source addresses seen so far: a dial on a fresh
    /// endpoint comes from a fresh port.
    pub fn clients(&self) -> usize {
        self.clients.lock().unwrap().len()
    }

    /// The gate as a relay URL.
    pub fn url(&self) -> String {
        format!("https://{}", self.addr)
    }
}
