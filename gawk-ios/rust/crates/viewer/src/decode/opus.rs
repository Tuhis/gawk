//! Audio decode (docs/67 D16): `AudioFrame` Opus packets to interleaved
//! `f32` PCM through libopus, which Swift wraps as LPCM `CMSampleBuffer`s
//! with the packet's PTS for the `AVSampleBufferAudioRenderer` (D15).
//!
//! It is the SPA's audio lane (`gawk-app/src/transport/audio-decode.ts`) in
//! Rust: the decoder is built from the `AudioConfig` (0x08) the relay
//! join-primes and the broadcaster re-sends at 1 Hz, **deduplicated by
//! content**, because rebuilding it every second would throw away the
//! decoder's state for nothing; a config that really changes rebuilds it.
//! Packets that arrive before any config are dropped, as the SPA drops them:
//! the wait is bounded by that 1 Hz cadence. The config's sample rate and
//! channel count are honoured as WebCodecs' `AudioDecoder.configure` honours
//! them. Every broadcaster sends 48 kHz stereo (docs/38 D8, D11), so in
//! practice the output is D16's 48 kHz stereo.
//!
//! libopus is the vendored static build `gawk-audio` already encodes with
//! (the `opus` crate), so the app carries one copy. Decode stays in Rust,
//! rather than AudioToolbox's Opus decoder, so CI tests it on the host.

use std::fmt;

use gawk_wire::AudioConfig;

/// The one codec the wire carries for audio (docs/28 Decision 3), spelled as
/// WebCodecs spells it.
pub const OPUS_CODEC: &str = "opus";

/// Sample rates libopus decodes to. WebCodecs refuses others for Opus too.
const OPUS_SAMPLE_RATES: [u32; 5] = [8_000, 12_000, 16_000, 24_000, 48_000];

/// The longest Opus packet is 120 ms; at 48 kHz that is 5760 frames per
/// channel, which sizes the scratch buffer once.
const MAX_FRAMES_PER_PACKET: usize = 5_760;

/// Why audio could not be configured or decoded. The SPA's lane goes
/// absent on either; the caller decides whether to retry on the next config.
#[derive(Debug)]
pub enum AudioDecodeError {
    /// The config names a codec other than Opus.
    UnsupportedCodec(String),
    /// A sample rate libopus can't decode to, or more than two channels
    /// (the wire never carries a channel mapping, docs/28).
    UnsupportedFormat { sample_rate: u32, channels: u8 },
    /// An empty packet. The wire has none (`parse_audio_frame` refuses
    /// one), and libopus would read it as "conceal a loss", which is not
    /// what a caller handing it a packet means.
    EmptyPacket,
    /// libopus refused a packet or the decoder.
    Opus(::opus::Error),
}

impl fmt::Display for AudioDecodeError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::UnsupportedCodec(c) => write!(f, "audio: unsupported codec {c:?}, want opus"),
            Self::UnsupportedFormat {
                sample_rate,
                channels,
            } => write!(
                f,
                "audio: unsupported format {sample_rate} Hz × {channels} channels"
            ),
            Self::EmptyPacket => write!(f, "audio: empty packet"),
            Self::Opus(e) => write!(f, "audio: opus: {e}"),
        }
    }
}

impl std::error::Error for AudioDecodeError {}

impl From<::opus::Error> for AudioDecodeError {
    fn from(e: ::opus::Error) -> Self {
        Self::Opus(e)
    }
}

/// One decoded packet: interleaved `f32` samples (L R L R … for stereo).
#[derive(Debug, Clone, PartialEq)]
pub struct Pcm {
    pub sample_rate: u32,
    pub channels: u8,
    pub samples: Vec<f32>,
}

impl Pcm {
    /// Samples per channel: what a `CMSampleBuffer`'s sample count is.
    pub fn frames(&self) -> usize {
        self.samples.len() / usize::from(self.channels)
    }
}

/// The `AudioConfig` fields the decoder was built from, owned, so the next
/// config can be compared by content.
#[derive(Debug, Clone, PartialEq, Eq)]
struct ConfigKey {
    codec: String,
    sample_rate: u32,
    channels: u8,
    description: Vec<u8>,
}

struct Configured {
    key: ConfigKey,
    decoder: ::opus::Decoder,
}

/// The audio lane's decoder. Build one per session; feed it every
/// `AudioConfig` and `AudioFrame` payload in arrival order.
#[derive(Default)]
pub struct AudioDecoder {
    current: Option<Configured>,
    scratch: Vec<f32>,
}

impl fmt::Debug for AudioDecoder {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("AudioDecoder")
            .field("config", &self.current.as_ref().map(|c| &c.key))
            .finish()
    }
}

impl AudioDecoder {
    pub fn new() -> Self {
        Self::default()
    }

    /// Applies an `AudioConfig`. Returns whether the decoder was (re)built:
    /// `false` for the 1 Hz repeat of the config already in force.
    ///
    /// An unusable config leaves the lane unconfigured, so packets are
    /// dropped until a usable one arrives; the error says why.
    ///
    /// The description is part of the dedup key but otherwise unused: it
    /// would be an OpusHead, and every broadcaster sends it empty
    /// (docs/38 D8), since a stereo stream with no channel mapping needs
    /// none.
    pub fn configure(&mut self, config: &AudioConfig<'_>) -> Result<bool, AudioDecodeError> {
        if self.current.as_ref().is_some_and(|c| c.key.matches(config)) {
            return Ok(false);
        }
        // Whatever happens next, the old decoder no longer describes the
        // stream.
        self.current = None;
        if config.codec != OPUS_CODEC {
            return Err(AudioDecodeError::UnsupportedCodec(config.codec.to_owned()));
        }
        let channels = match config.channels {
            1 => ::opus::Channels::Mono,
            2 => ::opus::Channels::Stereo,
            _ => return Err(unsupported(config)),
        };
        if !OPUS_SAMPLE_RATES.contains(&config.sample_rate) {
            return Err(unsupported(config));
        }
        let decoder = ::opus::Decoder::new(config.sample_rate, channels)?;
        self.current = Some(Configured {
            key: ConfigKey {
                codec: config.codec.to_owned(),
                sample_rate: config.sample_rate,
                channels: config.channels,
                description: config.description.to_vec(),
            },
            decoder,
        });
        Ok(true)
    }

    /// The sample rate and channel count in force, if configured.
    pub fn format(&self) -> Option<(u32, u8)> {
        self.current
            .as_ref()
            .map(|c| (c.key.sample_rate, c.key.channels))
    }

    /// Decodes one Opus packet (an `AudioFrame` payload). `Ok(None)` before
    /// any config: the packet is dropped, as the SPA drops it.
    pub fn decode(&mut self, packet: &[u8]) -> Result<Option<Pcm>, AudioDecodeError> {
        let Some(c) = self.current.as_mut() else {
            return Ok(None);
        };
        if packet.is_empty() {
            return Err(AudioDecodeError::EmptyPacket);
        }
        let channels = usize::from(c.key.channels);
        // Room for 120 ms at 48 kHz, the longest packet at the highest
        // rate, so no packet is refused for want of space.
        self.scratch.resize(MAX_FRAMES_PER_PACKET * channels, 0.0);
        let frames = c.decoder.decode_float(packet, &mut self.scratch, false)?;
        Ok(Some(Pcm {
            sample_rate: c.key.sample_rate,
            channels: c.key.channels,
            samples: self.scratch[..frames * channels].to_vec(),
        }))
    }
}

fn unsupported(config: &AudioConfig<'_>) -> AudioDecodeError {
    AudioDecodeError::UnsupportedFormat {
        sample_rate: config.sample_rate,
        channels: config.channels,
    }
}

impl ConfigKey {
    /// Content equality with a parsed config: the SPA's dedup key.
    fn matches(&self, c: &AudioConfig<'_>) -> bool {
        self.codec == c.codec
            && self.sample_rate == c.sample_rate
            && self.channels == c.channels
            && self.description == c.description
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const STEREO_48K: AudioConfig<'static> = AudioConfig {
        codec: "opus",
        sample_rate: 48_000,
        channels: 2,
        description: &[],
    };

    /// A 20 ms CELT fullband stereo packet of digital silence: TOC 0xFC
    /// (config 31, stereo, one frame) and an empty frame body, which
    /// libopus decodes as silence (RFC 6716 §3.2.1: a 0-byte frame is a
    /// dropped frame).
    const SILENT_STEREO_20MS: [u8; 1] = [0xFC];

    #[test]
    fn drops_packets_before_any_config() {
        let mut d = AudioDecoder::new();
        assert!(d.decode(&SILENT_STEREO_20MS).unwrap().is_none());
        assert_eq!(d.format(), None);
    }

    #[test]
    fn the_1hz_repeat_does_not_rebuild_the_decoder() {
        let mut d = AudioDecoder::new();
        assert!(d.configure(&STEREO_48K).unwrap());
        assert!(!d.configure(&STEREO_48K).unwrap());
        let changed = AudioConfig {
            description: &[1],
            ..STEREO_48K
        };
        assert!(d.configure(&changed).unwrap(), "a real change rebuilds");
    }

    #[test]
    fn decodes_to_the_configured_rate_and_channels() {
        let mut d = AudioDecoder::new();
        d.configure(&STEREO_48K).unwrap();
        let pcm = d.decode(&SILENT_STEREO_20MS).unwrap().unwrap();
        assert_eq!((pcm.sample_rate, pcm.channels), (48_000, 2));
        assert_eq!(pcm.frames(), 960, "20 ms at 48 kHz");
        assert_eq!(pcm.samples.len(), 1_920);

        // The config, not the packet, decides the output, as WebCodecs'.
        let mono_24k = AudioConfig {
            sample_rate: 24_000,
            channels: 1,
            ..STEREO_48K
        };
        assert!(d.configure(&mono_24k).unwrap());
        assert_eq!(d.format(), Some((24_000, 1)));
        let pcm = d.decode(&SILENT_STEREO_20MS).unwrap().unwrap();
        assert_eq!((pcm.sample_rate, pcm.channels), (24_000, 1));
        assert_eq!(pcm.frames(), 480);
    }

    #[test]
    fn refuses_what_libopus_cannot_decode_and_drops_until_fixed() {
        let mut d = AudioDecoder::new();
        d.configure(&STEREO_48K).unwrap();

        let aac = AudioConfig {
            codec: "mp4a.40.2",
            ..STEREO_48K
        };
        assert!(matches!(
            d.configure(&aac),
            Err(AudioDecodeError::UnsupportedCodec(c)) if c == "mp4a.40.2"
        ));
        // The bad config replaced the good one: nothing decodes on a lane
        // whose stream is no longer Opus.
        assert_eq!(d.format(), None);
        assert!(d.decode(&SILENT_STEREO_20MS).unwrap().is_none());

        for (sample_rate, channels) in [(44_100, 2), (48_000, 3)] {
            let bad = AudioConfig {
                sample_rate,
                channels,
                ..STEREO_48K
            };
            assert!(matches!(
                d.configure(&bad),
                Err(AudioDecodeError::UnsupportedFormat { .. })
            ));
        }

        assert!(d.configure(&STEREO_48K).unwrap());
        assert!(d.decode(&SILENT_STEREO_20MS).unwrap().is_some());
    }

    #[test]
    fn a_malformed_packet_is_an_error_not_a_panic() {
        let mut d = AudioDecoder::new();
        d.configure(&STEREO_48K).unwrap();
        // TOC code 3 (an arbitrary frame count) with no count byte.
        assert!(matches!(d.decode(&[0xFF]), Err(AudioDecodeError::Opus(_))));
        assert!(matches!(d.decode(&[]), Err(AudioDecodeError::EmptyPacket)));
    }
}
