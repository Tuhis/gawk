//! The audio lane as a pure function of what arrives (R52 MB4, docs/54 D6):
//! PCM blocks in, Opus packets out, under R25's contract and D6's
//! subordination rule. No capture, no sender — the macOS pipeline feeds it
//! ScreenCaptureKit's buffers and sends what comes out — so every rule is a
//! host test:
//!
//! * the format is read from each block, never assumed ([`crate::pcm`]);
//! * the first packet is checked against the advertised config (R25
//!   Decision 10) and the config is advertised only then — audio that never
//!   produced a good packet leaves the wire byte-identical to a video-only
//!   broadcaster;
//! * any failure is sticky: the lane goes quiet and says why once, and the
//!   broadcast's video is none of its business.

use crate::framer::Framer;
use crate::level::LevelMeter;
use crate::opusenc::OpusEncoder;
use crate::pcm::{PcmFormat, to_stereo_interleaved};
use crate::{advertised_format, toc};
use gawk_engine::media::{AudioFormat, AudioPacket};

/// One block of PCM as the platform delivered it, with its ASBD fields.
pub struct Block<'a> {
    pub format_id: u32,
    pub format_flags: u32,
    pub sample_rate: f64,
    pub channels: u32,
    pub bits_per_channel: u32,
    pub frames: usize,
    pub buffers: &'a [&'a [u8]],
    /// Session-clock µs of the block's first sample.
    pub timestamp_us: u64,
}

/// What one fed block produced.
#[derive(Debug, Default)]
pub struct Output {
    /// Set once, with the first verified packet: advertise it, then send.
    pub advertise: Option<AudioFormat>,
    pub packets: Vec<AudioPacket>,
    /// Set once, when the lane fails: log it, show it, carry on without audio.
    pub failed: Option<String>,
}

pub struct Lane {
    opus: OpusEncoder,
    framer: Framer,
    format: AudioFormat,
    level: LevelMeter,
    advertised: bool,
    failed: bool,
}

impl Lane {
    /// `source` names the capture path in diagnostics ("sck-app",
    /// "sck-system"). Fails only when libopus will not open.
    pub fn new(source: &str) -> Result<Self, String> {
        Ok(Self {
            opus: OpusEncoder::new()?,
            framer: Framer::new(),
            format: advertised_format(source),
            level: LevelMeter::default(),
            advertised: false,
            failed: false,
        })
    }

    pub fn failed(&self) -> bool {
        self.failed
    }

    pub fn level(&self) -> &LevelMeter {
        &self.level
    }

    /// Feeds one block (or the reason the platform could not read one).
    pub fn feed(&mut self, block: Result<Block<'_>, String>) -> Output {
        let mut out = Output::default();
        if self.failed {
            return out;
        }
        if let Err(why) = self.feed_inner(block, &mut out) {
            self.failed = true;
            out.failed = Some(why);
        }
        out
    }

    fn feed_inner(
        &mut self,
        block: Result<Block<'_>, String>,
        out: &mut Output,
    ) -> Result<(), String> {
        let b = block?;
        let fmt = PcmFormat::from_asbd(
            b.format_id,
            b.format_flags,
            b.sample_rate,
            b.channels,
            b.bits_per_channel,
        )?;
        let pcm = to_stereo_interleaved(fmt, b.buffers, b.frames)?;
        for frame in self.framer.push(&pcm, b.timestamp_us) {
            self.level.observe(&frame.interleaved, frame.timestamp_us);
            let data = self.opus.encode(&frame.interleaved)?;
            if !self.advertised {
                toc::verify_against_config(&data, &self.format)
                    .map_err(|e| format!("config verification failed: {e}"))?;
                self.advertised = true;
                out.advertise = Some(self.format.clone());
            }
            out.packets.push(AudioPacket {
                data,
                timestamp_us: frame.timestamp_us,
            });
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::pcm::FORMAT_LINEAR_PCM;

    // Float32, packed, non-interleaved: what ScreenCaptureKit reports.
    const SCK_FLAGS: u32 = 1 | (1 << 3) | (1 << 5);

    fn plane(frames: usize, v: f32) -> Vec<u8> {
        (0..frames).flat_map(|_| v.to_le_bytes()).collect()
    }

    fn feed(lane: &mut Lane, frames: usize, ts: u64, rate: f64) -> Output {
        let (l, r) = (plane(frames, 0.25), plane(frames, -0.25));
        let bufs: [&[u8]; 2] = [&l, &r];
        lane.feed(Ok(Block {
            format_id: FORMAT_LINEAR_PCM,
            format_flags: SCK_FLAGS,
            sample_rate: rate,
            channels: 2,
            bits_per_channel: 32,
            frames,
            buffers: &bufs,
            timestamp_us: ts,
        }))
    }

    #[test]
    fn sck_blocks_become_20ms_opus_packets_advertised_once() {
        let mut lane = Lane::new("sck-app").unwrap();
        // 1024-frame blocks (SCK's usual size): packets appear as 960-frame
        // (20 ms) frames complete; the config is advertised with the first.
        let out = feed(&mut lane, 1024, 1_000_000, 48_000.0);
        assert_eq!(out.packets.len(), 1);
        let fmt = out.advertise.expect("advertised with the first packet");
        assert_eq!((fmt.sample_rate, fmt.channels), (48_000, 2));
        assert_eq!(fmt.source, "sck-app");
        assert_eq!(out.packets[0].timestamp_us, 1_000_000);
        let out = feed(&mut lane, 1024, 1_021_333, 48_000.0);
        assert_eq!(out.packets.len(), 1);
        assert!(out.advertise.is_none(), "advertised exactly once");
        assert_eq!(
            out.packets[0].timestamp_us, 1_020_000,
            "stamped by sample count"
        );
        assert!(!lane.failed());
    }

    #[test]
    fn a_probe_failure_leaves_the_wire_video_only() {
        let mut lane = Lane::new("sck-app").unwrap();
        // The first block is at a rate the lane refuses: nothing advertised,
        // nothing sent — byte-identical to a video-only broadcaster.
        let out = feed(&mut lane, 1024, 0, 44_100.0);
        assert!(out.advertise.is_none() && out.packets.is_empty());
        assert!(out.failed.unwrap().contains("44100"));
        // Sticky: later good blocks produce nothing, and say nothing more.
        let out = feed(&mut lane, 2048, 30_000, 48_000.0);
        assert!(out.advertise.is_none() && out.packets.is_empty() && out.failed.is_none());
        assert!(lane.failed());
    }

    #[test]
    fn a_live_failure_stops_audio_once_and_quietly() {
        let mut lane = Lane::new("sck-system").unwrap();
        assert_eq!(feed(&mut lane, 1024, 0, 48_000.0).packets.len(), 1);
        // The platform could not read a buffer mid-broadcast.
        let out = lane.feed(Err("could not read the audio buffers (-12731)".into()));
        assert_eq!(
            out.failed.as_deref(),
            Some("could not read the audio buffers (-12731)")
        );
        assert!(feed(&mut lane, 4096, 50_000, 48_000.0).packets.is_empty());
    }

    #[test]
    fn the_level_meter_hears_the_frames() {
        let mut lane = Lane::new("sck-app").unwrap();
        feed(&mut lane, 4800, 0, 48_000.0);
        assert!(lane.level().level() > 0.0);
    }
}
