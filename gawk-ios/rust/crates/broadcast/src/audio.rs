//! App audio into the shared lane's contract (docs/67 D11): interleaved
//! stereo `f32` at 48 kHz, whatever the capture delivered.
//!
//! The format is read from every buffer's ASBD, never assumed: the
//! requested 48 kHz stereo is a request, and ReplayKit's app audio was seen
//! as 44.1 kHz and big-endian 16-bit (V-3). The shared `pcm` shim refuses
//! both on purpose (on the desktop they're surprises worth an error), so
//! iOS converts here first: byte order, sample type, planar or interleaved,
//! mono or stereo, then a fixed-ratio polyphase resampler.

/// `kAudioFormatLinearPCM` ('lpcm').
const FORMAT_LINEAR_PCM: u32 = 0x6C70_636D;
const FLAG_IS_FLOAT: u32 = 1 << 0;
const FLAG_IS_BIG_ENDIAN: u32 = 1 << 1;
const FLAG_IS_SIGNED_INTEGER: u32 = 1 << 2;
const FLAG_IS_NON_INTERLEAVED: u32 = 1 << 5;

/// The lane's rate (R25).
pub const OUT_RATE: u32 = 48_000;

/// One buffer's ASBD fields, as Swift reads them.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Asbd {
    pub format_id: u32,
    pub format_flags: u32,
    pub sample_rate: f64,
    pub channels: u32,
    pub bits_per_channel: u32,
}

/// `frames` frames from `buffers` (one per channel when planar) as
/// interleaved stereo `f32`, appended to `out`. Mono is duplicated.
pub fn to_stereo_f32(
    asbd: &Asbd,
    buffers: &[&[u8]],
    frames: usize,
    out: &mut Vec<f32>,
) -> Result<(), String> {
    if asbd.format_id != FORMAT_LINEAR_PCM {
        return Err(format!("audio is not linear PCM ({:#x})", asbd.format_id));
    }
    let channels = asbd.channels as usize;
    if !(1..=2).contains(&channels) {
        return Err(format!("{channels}-channel audio"));
    }
    let float = asbd.format_flags & FLAG_IS_FLOAT != 0;
    let big = asbd.format_flags & FLAG_IS_BIG_ENDIAN != 0;
    let bytes = match (float, asbd.bits_per_channel) {
        (true, 32) => 4,
        (false, 16) if asbd.format_flags & FLAG_IS_SIGNED_INTEGER != 0 => 2,
        (_, bits) => return Err(format!("{bits}-bit PCM")),
    };
    let planar = asbd.format_flags & FLAG_IS_NON_INTERLEAVED != 0;
    let need = if planar { channels } else { 1 };
    let per_buffer = frames * bytes * if planar { 1 } else { channels };
    if buffers.len() < need || buffers[..need].iter().any(|b| b.len() < per_buffer) {
        return Err("audio buffers shorter than their frame count".into());
    }
    let sample = |buf: &[u8], i: usize| -> f32 {
        let b = &buf[i * bytes..i * bytes + bytes];
        if bytes == 4 {
            let a = [b[0], b[1], b[2], b[3]];
            if big {
                f32::from_be_bytes(a)
            } else {
                f32::from_le_bytes(a)
            }
        } else {
            let a = [b[0], b[1]];
            let v = if big {
                i16::from_be_bytes(a)
            } else {
                i16::from_le_bytes(a)
            };
            f32::from(v) / 32768.0
        }
    };
    out.reserve(frames * 2);
    for f in 0..frames {
        let (l, r) = match (planar, channels) {
            (true, 1) | (false, 1) => {
                let v = sample(buffers[0], f);
                (v, v)
            }
            (true, _) => (sample(buffers[0], f), sample(buffers[1], f)),
            (false, _) => (sample(buffers[0], 2 * f), sample(buffers[0], 2 * f + 1)),
        };
        out.push(l);
        out.push(r);
    }
    Ok(())
}

/// Taps per polyphase branch: a windowed-sinc low-pass, good enough for a
/// game's audio at 128 kbps Opus.
const TAPS: usize = 16;

/// A fixed-ratio polyphase resampler for interleaved stereo `f32`. The
/// filter bank is built once for a rate; the hot path allocates nothing
/// beyond what `out` already holds.
#[derive(Debug, Clone)]
pub struct Resampler {
    in_rate: u32,
    /// Interpolation factor L and decimation factor M (out/in = L/M).
    up: usize,
    down: usize,
    /// `up` branches of [`TAPS`] coefficients each.
    bank: Vec<f32>,
    /// The last [`TAPS`] input frames (stereo pairs), oldest first.
    history: Vec<[f32; 2]>,
    /// Position in the upsampled stream, modulo `up`, of the next output.
    phase: usize,
}

fn gcd(a: usize, b: usize) -> usize {
    if b == 0 { a } else { gcd(b, a % b) }
}

impl Resampler {
    pub fn new(in_rate: u32) -> Result<Self, String> {
        if in_rate == 0 || in_rate > 384_000 {
            return Err(format!("audio at {in_rate} Hz"));
        }
        let g = gcd(OUT_RATE as usize, in_rate as usize);
        let (up, down) = (OUT_RATE as usize / g, in_rate as usize / g);
        // The cutoff is the lower Nyquist of the two rates, in units of the
        // upsampled rate.
        let cutoff = 0.5 / up.max(down) as f64 * 0.95;
        let len = TAPS * up;
        let center = (len - 1) as f64 / 2.0;
        let mut proto = vec![0f64; len];
        for (i, c) in proto.iter_mut().enumerate() {
            let x = i as f64 - center;
            let sinc = if x == 0.0 {
                2.0 * cutoff
            } else {
                (2.0 * std::f64::consts::PI * cutoff * x).sin() / (std::f64::consts::PI * x)
            };
            // Blackman window.
            let n = i as f64 / (len - 1) as f64;
            let w = 0.42 - 0.5 * (2.0 * std::f64::consts::PI * n).cos()
                + 0.08 * (4.0 * std::f64::consts::PI * n).cos();
            *c = sinc * w * up as f64;
        }
        // Branch p holds taps p, p+up, p+2up, …, reversed so branch·history
        // is a forward dot product over oldest→newest.
        let mut bank = vec![0f32; len];
        for p in 0..up {
            for t in 0..TAPS {
                bank[p * TAPS + (TAPS - 1 - t)] = proto[p + t * up] as f32;
            }
        }
        Ok(Self {
            in_rate,
            up,
            down,
            bank,
            history: vec![[0.0; 2]; TAPS],
            phase: 0,
        })
    }

    pub fn in_rate(&self) -> u32 {
        self.in_rate
    }

    /// Resamples interleaved stereo `input`, appending to `out`.
    pub fn process(&mut self, input: &[f32], out: &mut Vec<f32>) {
        if self.up == self.down {
            out.extend_from_slice(input);
            return;
        }
        for frame in input.chunks_exact(2) {
            self.history.rotate_left(1);
            self.history[TAPS - 1] = [frame[0], frame[1]];
            // Every input frame advances the upsampled clock by `up`; emit
            // an output each time it passes a multiple of `down`.
            while self.phase < self.up {
                let branch = &self.bank[self.phase * TAPS..(self.phase + 1) * TAPS];
                let (mut l, mut r) = (0f32, 0f32);
                for (c, h) in branch.iter().zip(&self.history) {
                    l += c * h[0];
                    r += c * h[1];
                }
                out.push(l);
                out.push(r);
                self.phase += self.down;
            }
            self.phase -= self.up;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn asbd(flags: u32, rate: f64, channels: u32, bits: u32) -> Asbd {
        Asbd {
            format_id: FORMAT_LINEAR_PCM,
            format_flags: flags,
            sample_rate: rate,
            channels,
            bits_per_channel: bits,
        }
    }

    #[test]
    fn converts_big_endian_int16_interleaved_stereo() {
        let a = asbd(FLAG_IS_SIGNED_INTEGER | FLAG_IS_BIG_ENDIAN, 44_100.0, 2, 16);
        let pcm: Vec<u8> = [16384i16, -16384, 0, 32767]
            .iter()
            .flat_map(|s| s.to_be_bytes())
            .collect();
        let mut out = Vec::new();
        to_stereo_f32(&a, &[&pcm], 2, &mut out).unwrap();
        assert_eq!(out, [0.5, -0.5, 0.0, 32767.0 / 32768.0]);
    }

    #[test]
    fn converts_planar_float_and_duplicates_mono() {
        let planar = asbd(FLAG_IS_FLOAT | FLAG_IS_NON_INTERLEAVED, 48_000.0, 2, 32);
        let l: Vec<u8> = [0.25f32, 0.5]
            .iter()
            .flat_map(|s| s.to_le_bytes())
            .collect();
        let r: Vec<u8> = [-0.25f32, -0.5]
            .iter()
            .flat_map(|s| s.to_le_bytes())
            .collect();
        let mut out = Vec::new();
        to_stereo_f32(&planar, &[&l, &r], 2, &mut out).unwrap();
        assert_eq!(out, [0.25, -0.25, 0.5, -0.5]);
        let mono = asbd(FLAG_IS_FLOAT, 48_000.0, 1, 32);
        out.clear();
        to_stereo_f32(&mono, &[&l], 2, &mut out).unwrap();
        assert_eq!(out, [0.25, 0.25, 0.5, 0.5]);
    }

    #[test]
    fn refuses_what_it_cannot_convert() {
        let mut out = Vec::new();
        let bad = Asbd {
            format_id: 0x6161_6320, // 'aac '
            ..asbd(FLAG_IS_FLOAT, 48_000.0, 2, 32)
        };
        assert!(to_stereo_f32(&bad, &[&[0; 16]], 2, &mut out).is_err());
        assert!(
            to_stereo_f32(
                &asbd(FLAG_IS_FLOAT, 48_000.0, 6, 32),
                &[&[0; 96]],
                2,
                &mut out
            )
            .is_err()
        );
        assert!(
            to_stereo_f32(
                &asbd(FLAG_IS_FLOAT, 48_000.0, 2, 32),
                &[&[0; 8]],
                2,
                &mut out
            )
            .is_err()
        );
    }

    /// A tone's frequency from its zero crossings over the settled middle.
    fn frequency(samples: &[f32], rate: f64) -> f64 {
        let left: Vec<f32> = samples.chunks_exact(2).map(|f| f[0]).collect();
        let mid = &left[left.len() / 4..left.len() * 3 / 4];
        let crossings = mid.windows(2).filter(|w| w[0] < 0.0 && w[1] >= 0.0).count();
        crossings as f64 * rate / mid.len() as f64
    }

    #[test]
    fn resamples_44100_to_48000_keeping_pitch_and_length() {
        let mut rs = Resampler::new(44_100).unwrap();
        let input: Vec<f32> = (0..44_100)
            .flat_map(|i| {
                let v = (2.0 * std::f32::consts::PI * 440.0 * i as f32 / 44_100.0).sin() * 0.5;
                [v, v]
            })
            .collect();
        let mut out = Vec::new();
        // In uneven pieces, as capture delivers them.
        for piece in input.chunks(2 * 1031) {
            rs.process(piece, &mut out);
        }
        let frames = out.len() / 2;
        assert!((47_990..=48_010).contains(&frames), "{frames} frames");
        let f = frequency(&out, 48_000.0);
        assert!((f - 440.0).abs() < 3.0, "pitch {f} Hz");
        let peak = out[out.len() / 2..]
            .iter()
            .fold(0f32, |m, v| m.max(v.abs()));
        assert!((0.45..0.55).contains(&peak), "gain {peak}");
    }

    #[test]
    fn passes_48000_through_untouched() {
        let mut rs = Resampler::new(48_000).unwrap();
        let mut out = Vec::new();
        rs.process(&[0.1, 0.2, 0.3, 0.4], &mut out);
        assert_eq!(out, [0.1, 0.2, 0.3, 0.4]);
    }
}
