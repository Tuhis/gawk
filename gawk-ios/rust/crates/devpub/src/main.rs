//! Publishes one of the viewer's recorded fixtures (docs/67 IO5), looping,
//! through the shared engine: what G7 needs in the Simulator, where the
//! only other local source, `gawk-pubsim`, sends H.264 alone.
//!
//!   cargo run -p gawk-devpub -- --codec vp9 --url https://127.0.0.1:4433 --insecure
//!
//! Prints `GAWK_DEVPUB_ID=<ID>` once the relay announces the broadcast.
//!
//! A room fixture (docs/70 K14): `--room-new` mints a room from the
//! broadcast, `--room <code>` joins one and attaches to it, and `--nick
//! <name>` names the participant and its tile. `GAWK_DEVPUB_ROOM=<code>` is
//! printed once the room is known (minted, or attached), and a minted
//! room's creator token as `GAWK_DEVPUB_CREATOR=<hex>`, for a test to join
//! it as its creator through a `?rt=c:<hex>` link:
//!
//!   cargo run -p gawk-devpub -- --insecure --room-new --nick Ann
//!   cargo run -p gawk-devpub -- --insecure --room QX7P2K --nick Ben
//!
//! `--quit-after <seconds>` exits without ending the broadcast, as a
//! broadcaster that drops off does: viewers and rooms see it away until the
//! relay's grace ends (docs/70 D19's away tile).
//!
//! A fixture stays up until it is killed. The engine resumes a lost session
//! for five minutes and a lost room for as long, and then gives up; a CI
//! runner can stall for longer than that, and a fixture that gave up failed
//! every test after it (2026-10-13). So an unanswered dial is tried again, a
//! session the engine ended is started again reclaiming its code, and a room
//! it lost is joined again, unless the room's creator removed it. Only a
//! refusal (a wrong secret, a reclaim the relay won't take) ends the process.
//!
//! The fixtures are `crates/viewer/tests/fixtures/` (their README says how
//! they were made): 320×240, 30 frames, keyframes at 0 and 15.

use gawk_engine::clock::{Clock, MonotonicClock};
use gawk_engine::media::AccessUnit;
use gawk_engine::session::{EngineEvent, Session, SessionConfig};
use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;
use tokio::sync::mpsc::UnboundedReceiver;
use tokio::time::MissedTickBehavior;

const FPS: u64 = 30;

/// How long after an unanswered dial, an ended session or a lost room the
/// next try starts. The engine has already backed off inside each.
const RETRY: Duration = Duration::from_secs(2);

fn fixtures() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../viewer/tests/fixtures")
}

/// IVF frames (VP8/VP9): 32-byte file header, then 12-byte frame headers.
fn ivf_frames(b: &[u8]) -> Vec<Vec<u8>> {
    assert_eq!(&b[..4], b"DKIF", "not an IVF file");
    let mut off = usize::from(u16::from_le_bytes([b[6], b[7]]));
    let mut out = Vec::new();
    while off + 12 <= b.len() {
        let size = u32::from_le_bytes(b[off..off + 4].try_into().unwrap()) as usize;
        off += 12;
        out.push(b[off..off + size].to_vec());
        off += size;
    }
    out
}

/// An Annex-B stream's access units, split at each access unit delimiter.
fn h264_access_units(b: &[u8]) -> Vec<Vec<u8>> {
    let mut units: Vec<Vec<&[u8]>> = Vec::new();
    for nal in gawk_encode::h264::annex_b_nals(b) {
        if nal.first().is_some_and(|h| h & 0x1f == 9) || units.is_empty() {
            units.push(Vec::new());
        }
        units.last_mut().unwrap().push(nal);
    }
    units
        .into_iter()
        .map(gawk_encode::h264::annex_b_from_nals)
        .collect()
}

/// A VP8 keyframe has bit 0 of its first byte clear; a VP9 one has
/// `frame_type` 0 in its uncompressed header (profile 0, no show-existing).
fn vpx_keyframe(codec: &str, frame: &[u8]) -> bool {
    match codec {
        "vp8" => frame.first().is_some_and(|b| b & 0x01 == 0),
        _ => frame.first().is_some_and(|b| (b >> 2) & 0x01 == 0),
    }
}

/// The command line.
#[derive(Debug, Clone, PartialEq, Eq)]
struct Args {
    codec: String,
    url: String,
    secret: String,
    insecure: bool,
    /// A room to join and attach to (docs/70 K14), empty for none.
    room: String,
    /// Mint a new room from this broadcast.
    room_new: bool,
    /// The room nickname and tile label; empty lets the relay assign one.
    nick: String,
    /// Exit after this many seconds without a clean stop.
    quit_after: Option<u64>,
}

fn parse_args(args: impl IntoIterator<Item = String>) -> Result<Args, String> {
    let mut parsed = Args {
        codec: "h264".into(),
        url: "https://127.0.0.1:4433".into(),
        secret: String::new(),
        insecure: false,
        room: String::new(),
        room_new: false,
        nick: String::new(),
        quit_after: None,
    };
    let mut args = args.into_iter();
    while let Some(flag) = args.next() {
        let mut value = |usage: &str| args.next().ok_or_else(|| format!("usage: {usage}"));
        match flag.as_str() {
            "--codec" => parsed.codec = value("--codec vp8|vp9|h264")?,
            "--url" => parsed.url = value("--url <relay>")?,
            "--secret" => parsed.secret = value("--secret <publish secret>")?,
            "--insecure" => parsed.insecure = true,
            "--room" => parsed.room = value("--room <code>")?,
            "--room-new" => parsed.room_new = true,
            "--nick" => parsed.nick = value("--nick <name>")?,
            "--quit-after" => {
                let v = value("--quit-after <seconds>")?;
                parsed.quit_after = Some(
                    v.parse()
                        .map_err(|_| format!("--quit-after {v}: not a number of seconds"))?,
                );
            }
            other => return Err(format!("unknown argument {other}")),
        }
    }
    if parsed.room_new && !parsed.room.is_empty() {
        return Err("--room and --room-new: join a room or mint one, not both".into());
    }
    Ok(parsed)
}

/// What the fixture has learned that its next session needs: the code and
/// token to reclaim, and the room to join again.
#[derive(Debug, Default, Clone, PartialEq, Eq)]
struct Held {
    broadcast_id: String,
    token_hex: String,
    /// The room: the one joined, or once minted, the minted one.
    room: String,
    /// The minted room's creator token, to join it again as its creator.
    creator_token_hex: String,
    /// The room's code has been printed.
    room_said: bool,
    /// The room's creator took this broadcast out; it never goes back in.
    removed: bool,
    /// The relay refused to give the code back (its HTTP status).
    refused: Option<u16>,
}

/// What the fixture does about one engine event.
#[derive(Debug, PartialEq, Eq)]
enum Step {
    Nothing,
    /// Lines for stdout: the variables the scripts wait for.
    Say(Vec<String>),
    /// Join the room again.
    Rejoin,
    /// The session is over.
    Over,
}

impl Held {
    fn new(args: &Args) -> Self {
        Self {
            room: args.room.clone(),
            ..Self::default()
        }
    }

    /// The next session's config: the first mints (or reclaims nothing),
    /// every later one reclaims the held code into the held room.
    fn config(&self, args: &Args) -> SessionConfig {
        let room = if self.removed {
            String::new()
        } else {
            self.room.clone()
        };
        SessionConfig {
            relay_url: args.url.clone(),
            broadcast_id: self.broadcast_id.clone(),
            resume_token_hex: self.token_hex.clone(),
            publish_secret: args.secret.clone(),
            origin: gawk_engine::defaults::origin().to_owned(),
            insecure: args.insecure,
            room_new: args.room_new && self.room.is_empty(),
            room_code: room,
            nickname: args.nick.clone(),
            ..SessionConfig::default()
        }
    }

    fn on(&mut self, event: EngineEvent) -> Step {
        match event {
            EngineEvent::Announce { broadcast_id } => {
                let first = self.broadcast_id.is_empty();
                self.broadcast_id = broadcast_id;
                if first {
                    Step::Say(vec![format!("GAWK_DEVPUB_ID={}", self.broadcast_id)])
                } else {
                    Step::Nothing
                }
            }
            EngineEvent::ResumeToken { token_hex } => {
                self.token_hex = token_hex;
                Step::Nothing
            }
            // The room's code once it is known: minted, or joined with this
            // broadcast attached.
            EngineEvent::RoomCreated {
                code,
                creator_token_hex,
            } if !self.room_said => {
                self.room_said = true;
                self.room = code;
                self.creator_token_hex = creator_token_hex;
                Step::Say(vec![
                    format!("GAWK_DEVPUB_ROOM={}", self.room),
                    format!("GAWK_DEVPUB_CREATOR={}", self.creator_token_hex),
                ])
            }
            EngineEvent::RoomAttached if !self.room_said && !self.room.is_empty() => {
                self.room_said = true;
                Step::Say(vec![format!("GAWK_DEVPUB_ROOM={}", self.room)])
            }
            EngineEvent::RoomDetached { reason, by_creator } => {
                eprintln!("room: detached ({reason})");
                self.removed |= by_creator;
                Step::Nothing
            }
            EngineEvent::RoomEnded { reason } => {
                eprintln!("room: {reason}");
                if self.removed || self.room.is_empty() {
                    Step::Nothing
                } else {
                    Step::Rejoin
                }
            }
            EngineEvent::RoomRejected { reason, message } => {
                eprintln!("room refused a command: {reason} ({message})");
                Step::Nothing
            }
            EngineEvent::RoomReconnecting { attempt: 1 } => {
                eprintln!("room: reconnecting");
                Step::Nothing
            }
            EngineEvent::Resuming { attempt: 1 } => {
                eprintln!("resuming the broadcast");
                Step::Nothing
            }
            EngineEvent::Resumed => {
                eprintln!("resumed");
                Step::Nothing
            }
            EngineEvent::ReclaimRefused { status } => {
                self.refused = Some(status);
                Step::Nothing
            }
            EngineEvent::Ended { error } => {
                eprintln!(
                    "the session ended: {}",
                    error.as_deref().unwrap_or("stopped")
                );
                Step::Over
            }
            _ => Step::Nothing,
        }
    }
}

/// Starts a session, dialing again while the relay doesn't answer: on a
/// stalled runner it can go a whole dial without answering. A refusal is
/// final.
async fn publish(
    cfg: &SessionConfig,
    clock: &Arc<dyn Clock>,
) -> (Arc<Session>, UnboundedReceiver<EngineEvent>) {
    loop {
        match Session::start(cfg.clone(), clock.clone()).await {
            Ok(started) => return started,
            Err(e) if e.status == 0 => {
                eprintln!("could not publish to {}: {e}; trying again", cfg.relay_url);
                tokio::time::sleep(RETRY).await;
            }
            Err(e) => panic!("could not publish to {}: {e}", cfg.relay_url),
        }
    }
}

#[tokio::main(flavor = "current_thread")]
async fn main() {
    let args = parse_args(std::env::args().skip(1)).unwrap_or_else(|e| panic!("{e}"));
    let codec = args.codec.clone();
    let (codec_string, units, keyframes): (String, Vec<Vec<u8>>, Vec<bool>) = match codec.as_str() {
        "h264" => {
            let units =
                h264_access_units(&std::fs::read(fixtures().join("h264-320x240.h264")).unwrap());
            let codec = gawk_encode::h264::parse_codec_string(&units[0]).expect("an SPS up front");
            let keys = units
                .iter()
                .map(|u| gawk_encode::h264::has_idr(u))
                .collect();
            (codec, units, keys)
        }
        "vp8" | "vp9" => {
            let file = format!("{codec}-320x240.ivf");
            let units = ivf_frames(&std::fs::read(fixtures().join(file)).unwrap());
            let name = if codec == "vp8" {
                "vp8"
            } else {
                "vp09.00.10.08"
            };
            let keys = units.iter().map(|u| vpx_keyframe(&codec, u)).collect();
            (name.to_owned(), units, keys)
        }
        other => panic!("unknown codec {other}: vp8, vp9 or h264"),
    };
    eprintln!(
        "looping the {codec} fixture ({codec_string}, {} frames) at {FPS} fps",
        units.len()
    );
    if let Some(secs) = args.quit_after {
        tokio::spawn(async move {
            tokio::time::sleep(Duration::from_secs(secs)).await;
            eprintln!("quitting after {secs} s without ending the broadcast");
            std::process::exit(0);
        });
    }
    let clock: Arc<dyn Clock> = Arc::new(MonotonicClock::new());
    let mut held = Held::new(&args);
    let mut tick = tokio::time::interval(Duration::from_micros(1_000_000 / FPS));
    // After a stall, carry on at 30 fps: the default burst sent every
    // missed frame at once, two keyframe streams for each second missed.
    tick.set_missed_tick_behavior(MissedTickBehavior::Skip);
    let start = tokio::time::Instant::now();
    loop {
        let (session, mut events) = publish(&held.config(&args), &clock).await;
        let sender = session.sender();
        sender.set_codec(&codec_string);
        // A new session starts on a keyframe, as does a resumed one: the
        // relay dropped its cached keyframe with the old session.
        let mut i = 0;
        let mut rejoin: Option<tokio::time::Instant> = None;
        loop {
            tokio::select! {
                _ = tick.tick() => {
                    sender
                        .send_video(AccessUnit {
                            data: units[i].clone(),
                            timestamp_us: start.elapsed().as_micros() as u64,
                            keyframe: keyframes[i],
                        })
                        .await;
                    i = (i + 1) % units.len();
                }
                event = events.recv() => {
                    let Some(event) = event else { break };
                    if event == EngineEvent::Resumed {
                        i = 0;
                    }
                    match held.on(event) {
                        Step::Nothing => {}
                        Step::Say(lines) => lines.iter().for_each(|l| println!("{l}")),
                        Step::Rejoin => rejoin = Some(tokio::time::Instant::now() + RETRY),
                        Step::Over => break,
                    }
                }
                _ = tokio::time::sleep_until(rejoin.unwrap_or_else(tokio::time::Instant::now)),
                    if rejoin.is_some() =>
                {
                    rejoin = None;
                    eprintln!("room: joining {} again", held.room);
                    session.room_join(&held.room, "", &held.creator_token_hex);
                }
            }
        }
        session.stop().await;
        if let Some(status) = held.refused {
            eprintln!(
                "the relay refused to give {} back (status {status})",
                held.broadcast_id
            );
            std::process::exit(1);
        }
        tokio::time::sleep(RETRY).await;
        eprintln!("publishing {} again", held.broadcast_id);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn parse(args: &[&str]) -> Result<Args, String> {
        parse_args(args.iter().map(|a| a.to_string()))
    }

    #[test]
    fn the_command_line_takes_the_room_fixture_flags() {
        let plain = parse(&[]).unwrap();
        assert_eq!(
            (plain.codec.as_str(), plain.url.as_str()),
            ("h264", "https://127.0.0.1:4433")
        );
        assert!(plain.room.is_empty() && !plain.room_new && plain.nick.is_empty());

        let a = parse(&[
            "--codec",
            "vp9",
            "--url",
            "https://relay:4433",
            "--secret",
            "s3",
            "--insecure",
            "--room",
            "QX7P2K",
            "--nick",
            "Ben",
        ])
        .unwrap();
        assert_eq!(
            a,
            Args {
                codec: "vp9".into(),
                url: "https://relay:4433".into(),
                secret: "s3".into(),
                insecure: true,
                room: "QX7P2K".into(),
                room_new: false,
                nick: "Ben".into(),
                quit_after: None,
            }
        );
        assert_eq!(parse(&["--quit-after", "20"]).unwrap().quit_after, Some(20));
        assert!(parse(&["--quit-after", "soon"]).is_err());
        assert!(parse(&["--room-new", "--nick", "Ann"]).unwrap().room_new);

        assert!(parse(&["--room"]).is_err(), "a flag without its value");
        assert!(parse(&["--nick"]).is_err());
        assert!(parse(&["--frobnicate"]).is_err());
        assert!(
            parse(&["--room", "QX7P2K", "--room-new"]).is_err(),
            "join or mint, not both"
        );
    }

    fn say(lines: &[&str]) -> Step {
        Step::Say(lines.iter().map(|l| l.to_string()).collect())
    }

    #[test]
    fn a_restart_reclaims_the_code_into_the_minted_room() {
        let args = parse(&["--room-new", "--nick", "P1", "--secret", "s3"]).unwrap();
        let mut held = Held::new(&args);
        let first = held.config(&args);
        assert!(first.room_new, "the first session mints");
        assert!(first.broadcast_id.is_empty() && first.room_code.is_empty());

        let announce = || EngineEvent::Announce {
            broadcast_id: "ABC234".into(),
        };
        assert_eq!(held.on(announce()), say(&["GAWK_DEVPUB_ID=ABC234"]));
        let token = EngineEvent::ResumeToken {
            token_hex: "00ff".into(),
        };
        assert_eq!(held.on(token), Step::Nothing);
        let created = EngineEvent::RoomCreated {
            code: "QX7P2K".into(),
            creator_token_hex: "c0de".into(),
        };
        assert_eq!(
            held.on(created),
            say(&["GAWK_DEVPUB_ROOM=QX7P2K", "GAWK_DEVPUB_CREATOR=c0de"])
        );
        assert_eq!(held.on(EngineEvent::RoomAttached), Step::Nothing);

        // The engine gave up resuming: the next session reclaims the code
        // into the room it minted, and mints nothing.
        let ended = EngineEvent::Ended {
            error: Some("gave up".into()),
        };
        assert_eq!(held.on(ended), Step::Over);
        let next = held.config(&args);
        assert_eq!(
            (
                next.broadcast_id.as_str(),
                next.resume_token_hex.as_str(),
                next.room_code.as_str(),
                next.room_new,
                next.nickname.as_str(),
                next.publish_secret.as_str(),
            ),
            ("ABC234", "00ff", "QX7P2K", false, "P1", "s3")
        );
        assert_eq!(
            held.on(announce()),
            Step::Nothing,
            "the code is printed once"
        );
        assert_eq!(held.on(EngineEvent::RoomAttached), Step::Nothing);
        assert_eq!(held.refused, None);
    }

    #[test]
    fn a_lost_room_is_joined_again_until_its_creator_removes_the_stream() {
        let args = parse(&["--room", "QX7P2K", "--nick", "P2"]).unwrap();
        let mut held = Held::new(&args);
        assert_eq!(held.config(&args).room_code, "QX7P2K");
        assert_eq!(
            held.on(EngineEvent::RoomAttached),
            say(&["GAWK_DEVPUB_ROOM=QX7P2K"])
        );
        let lost = || EngineEvent::RoomEnded {
            reason: "lost the room and could not rejoin".into(),
        };
        assert_eq!(held.on(lost()), Step::Rejoin);

        let removed = EngineEvent::RoomDetached {
            reason: "removed".into(),
            by_creator: true,
        };
        assert_eq!(held.on(removed), Step::Nothing);
        assert_eq!(held.on(lost()), Step::Nothing);
        let next = held.config(&args);
        assert!(
            next.room_code.is_empty() && !next.room_new,
            "a restart stays out of it"
        );
    }

    #[test]
    fn a_stream_in_no_room_has_none_to_rejoin() {
        let args = parse(&[]).unwrap();
        let mut held = Held::new(&args);
        let ended = EngineEvent::RoomEnded {
            reason: "the room ended".into(),
        };
        assert_eq!(held.on(ended), Step::Nothing);
        let cfg = held.config(&args);
        assert!(cfg.room_code.is_empty() && !cfg.room_new);
    }

    #[test]
    fn a_refused_reclaim_ends_the_fixture() {
        let args = parse(&[]).unwrap();
        let mut held = Held::new(&args);
        assert_eq!(
            held.on(EngineEvent::ReclaimRefused { status: 404 }),
            Step::Nothing
        );
        let ended = EngineEvent::Ended {
            error: Some("refused".into()),
        };
        assert_eq!(held.on(ended), Step::Over);
        assert_eq!(held.refused, Some(404));
    }

    #[test]
    fn splits_every_fixture_into_30_frames_with_keyframes_at_0_and_15() {
        let h264 = h264_access_units(&std::fs::read(fixtures().join("h264-320x240.h264")).unwrap());
        assert_eq!(h264.len(), 30);
        let keys: Vec<usize> = (0..30)
            .filter(|&i| gawk_encode::h264::has_idr(&h264[i]))
            .collect();
        assert_eq!(keys, [0, 15]);
        for codec in ["vp8", "vp9"] {
            let frames = ivf_frames(
                &std::fs::read(fixtures().join(format!("{codec}-320x240.ivf"))).unwrap(),
            );
            assert_eq!(frames.len(), 30, "{codec}");
            let keys: Vec<usize> = (0..30)
                .filter(|&i| vpx_keyframe(codec, &frames[i]))
                .collect();
            assert_eq!(keys, [0, 15], "{codec}");
        }
    }
}
