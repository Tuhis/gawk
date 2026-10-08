//! The decode layer on recorded streams (docs/67 §9 IO4: "Rust tests on
//! recorded H.264, VP9 and VP8 streams"). The video fixtures are committed
//! under `tests/fixtures/` (see its README for how they were made); the Opus
//! fixture is the Go publisher simulator's, read from where it lives in
//! the repo rather than copied.

use std::path::{Path, PathBuf};

use gawk_viewer::decode::h264::{BitstreamFormat, H264Stream};
use gawk_viewer::decode::opus::AudioDecoder;
use gawk_viewer::decode::vpx::{I420Frame, VpxCodec, VpxDecoder};
use gawk_wire::AudioConfig;

fn fixture(name: &str) -> Vec<u8> {
    let path = Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("tests/fixtures")
        .join(name);
    std::fs::read(&path).unwrap_or_else(|e| panic!("{}: {e}", path.display()))
}

fn repo_root() -> PathBuf {
    // crates/viewer → rust → gawk-ios → the repo.
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../..")
}

// --- IVF ---------------------------------------------------------------------

/// The fixtures' container: a 32-byte file header (`DKIF`, version 0,
/// header size, FourCC, width, height, timebase, frame count), then per
/// frame a 12-byte header (size, 64-bit PTS) and the frame. Little-endian
/// throughout. Only what the tests need is read.
struct Ivf {
    fourcc: [u8; 4],
    width: u16,
    height: u16,
    frames: Vec<Vec<u8>>,
}

fn read_ivf(b: &[u8]) -> Ivf {
    assert_eq!(&b[0..4], b"DKIF", "IVF signature");
    let header_len = usize::from(u16::from_le_bytes([b[6], b[7]]));
    assert_eq!(header_len, 32);
    let mut frames = Vec::new();
    let mut off = header_len;
    while off < b.len() {
        assert!(off + 12 <= b.len(), "truncated IVF frame header at {off}");
        let size = u32::from_le_bytes(b[off..off + 4].try_into().unwrap()) as usize;
        off += 12;
        assert!(
            off + size <= b.len(),
            "IVF frame at {off} overruns the file"
        );
        frames.push(b[off..off + size].to_vec());
        off += size;
    }
    Ivf {
        fourcc: b[8..12].try_into().unwrap(),
        width: u16::from_le_bytes([b[12], b[13]]),
        height: u16::from_le_bytes([b[14], b[15]]),
        frames,
    }
}

/// Frames that are keyframes, by the VP8/VP9 uncompressed header.
fn is_keyframe(codec: VpxCodec, frame: &[u8]) -> bool {
    match codec {
        // RFC 6386 §9.1: bit 0 of the frame tag is 0 for a key frame.
        VpxCodec::Vp8 => frame[0] & 0x01 == 0,
        // VP9 §6.2 (profile 0): frame_marker(2) profile(2) show_existing(1)
        // frame_type(1), with frame_type 0 for a key frame.
        VpxCodec::Vp9 => frame[0] & 0x04 == 0,
    }
}

// --- VP8 / VP9 ---------------------------------------------------------------

fn assert_i420(f: &I420Frame, width: usize, height: usize) {
    assert_eq!((f.width, f.height), (width, height));
    assert_eq!((f.y.width, f.y.rows), (width, height));
    for c in [&f.u, &f.v] {
        assert_eq!((c.width, c.rows), (width.div_ceil(2), height.div_ceil(2)));
    }
    for p in [&f.y, &f.u, &f.v] {
        assert!(
            p.stride >= p.width,
            "stride {} < width {}",
            p.stride,
            p.width
        );
        assert_eq!(p.data.len(), p.stride * p.rows);
    }
}

/// Decodes a whole clip, asserting one shown frame of the right size per
/// compressed frame. Returns the luma mean of the last frame.
fn decode_clip(d: &mut VpxDecoder, clip: &Ivf) -> f64 {
    let (w, h) = (usize::from(clip.width), usize::from(clip.height));
    let mut last = None;
    for (i, frame) in clip.frames.iter().enumerate() {
        let out = d.decode(frame).unwrap_or_else(|e| panic!("frame {i}: {e}"));
        assert_eq!(out.len(), 1, "frame {i}: one shown frame per frame");
        assert_i420(&out[0], w, h);
        last = out.into_iter().next();
    }
    let f = last.expect("clip has frames");
    let sum: u64 = (0..f.y.rows)
        .flat_map(|r| &f.y.data[r * f.y.stride..r * f.y.stride + f.y.width])
        .map(|&b| u64::from(b))
        .sum();
    sum as f64 / (f.width * f.height) as f64
}

#[test]
fn vp8_clip_decodes_every_frame_at_its_size() {
    let clip = read_ivf(&fixture("vp8-320x240.ivf"));
    assert_eq!(&clip.fourcc, b"VP80");
    assert_eq!(clip.frames.len(), 30);
    let keys: Vec<usize> = (0..30)
        .filter(|&i| is_keyframe(VpxCodec::Vp8, &clip.frames[i]))
        .collect();
    assert_eq!(keys, [0, 15], "keyframe every 15 frames");

    let mut d = VpxDecoder::new(VpxCodec::from_codec_string("vp8").unwrap()).unwrap();
    let luma = decode_clip(&mut d, &clip);
    // testsrc2 is a busy test card, never a flat black or white frame.
    assert!((40.0..215.0).contains(&luma), "luma mean {luma}");
}

#[test]
fn vp9_clip_decodes_every_frame_at_its_size() {
    let clip = read_ivf(&fixture("vp9-320x240.ivf"));
    assert_eq!(&clip.fourcc, b"VP90");
    assert_eq!(clip.frames.len(), 30);
    let keys: Vec<usize> = (0..30)
        .filter(|&i| is_keyframe(VpxCodec::Vp9, &clip.frames[i]))
        .collect();
    assert_eq!(keys, [0, 15], "keyframe every 15 frames");

    let mut d = VpxDecoder::new(VpxCodec::from_codec_string("vp09.00.10.08").unwrap()).unwrap();
    let luma = decode_clip(&mut d, &clip);
    assert!((40.0..215.0).contains(&luma), "luma mean {luma}");
}

/// D9/D13: a rotation arrives as a new `DecoderConfig`, and the viewer
/// answers it with a fresh decoder. Landscape, then portrait through a
/// rebuilt decoder, then a codec switch (a VP8 broadcaster reclaiming the
/// ID), with the frames reporting the size they decoded at each step.
#[test]
fn a_decoder_config_change_resets_the_decoder() {
    let landscape = read_ivf(&fixture("vp9-320x240.ivf"));
    let portrait = read_ivf(&fixture("vp9-240x320.ivf"));
    assert_eq!((portrait.width, portrait.height), (240, 320));
    let vp8 = read_ivf(&fixture("vp8-320x240.ivf"));

    let mut d = VpxDecoder::new(VpxCodec::Vp9).unwrap();
    // Stop mid-GOP, as a rotation does.
    for frame in &landscape.frames[..20] {
        assert_i420(&d.decode(frame).unwrap()[0], 320, 240);
    }
    d = VpxDecoder::new(VpxCodec::Vp9).unwrap();
    decode_clip(&mut d, &portrait);
    d = VpxDecoder::new(VpxCodec::Vp8).unwrap();
    assert_eq!(d.codec(), VpxCodec::Vp8);
    decode_clip(&mut d, &vp8);
}

/// Without a config change, libvpx itself follows a VP9 keyframe at a new
/// size, and the frame says so: the size is read off the frame, never
/// carried over from what came before (CLAUDE.md: trust the frame).
#[test]
fn the_decoded_frame_reports_its_own_size() {
    let landscape = read_ivf(&fixture("vp9-320x240.ivf"));
    let portrait = read_ivf(&fixture("vp9-240x320.ivf"));
    let mut d = VpxDecoder::new(VpxCodec::Vp9).unwrap();
    assert_i420(&d.decode(&landscape.frames[0]).unwrap()[0], 320, 240);
    assert_i420(&d.decode(&portrait.frames[0]).unwrap()[0], 240, 320);
}

// --- Opus --------------------------------------------------------------------

/// The Go fixture's framing (gawk-server/internal/pubsim/fixture/audio.go): a
/// big-endian u16 length before each packet.
fn split_audio(b: &[u8]) -> Vec<&[u8]> {
    let mut out = Vec::new();
    let mut off = 0;
    while off < b.len() {
        let n = usize::from(u16::from_be_bytes([b[off], b[off + 1]]));
        off += 2;
        assert!(n > 0 && off + n <= b.len(), "bad framing at {}", off - 2);
        out.push(&b[off..off + n]);
        off += n;
    }
    out
}

/// Zero crossings of one channel of interleaved stereo.
fn zero_crossings(samples: &[f32], channel: usize) -> usize {
    let ch: Vec<f32> = samples.iter().skip(channel).step_by(2).copied().collect();
    ch.windows(2)
        .filter(|w| (w[0] < 0.0) != (w[1] < 0.0))
        .count()
}

#[test]
fn opus_fixture_decodes_to_48k_stereo_in_its_two_tones() {
    let path = repo_root().join("gawk-server/internal/pubsim/fixture/sample-audio.opus");
    let bytes = std::fs::read(&path).unwrap_or_else(|e| panic!("{}: {e}", path.display()));
    let packets = split_audio(&bytes);
    assert_eq!(packets.len(), 101, "~2 s of 20 ms packets");

    // The production AudioConfig (docs/38 D8), as it arrives on the wire.
    let mut dgram = Vec::new();
    gawk_wire::append_audio_config(
        &mut dgram,
        &AudioConfig {
            codec: "opus",
            sample_rate: 48_000,
            channels: 2,
            description: &[],
        },
    )
    .unwrap();
    let config = gawk_wire::parse_audio_config(&dgram).unwrap();

    let mut d = AudioDecoder::new();
    assert!(d.configure(&config).unwrap());
    let mut pcm = Vec::new();
    for (i, p) in packets.iter().enumerate() {
        let out = d
            .decode(p)
            .unwrap_or_else(|e| panic!("packet {i}: {e}"))
            .expect("configured");
        assert_eq!((out.sample_rate, out.channels), (48_000, 2));
        assert_eq!(out.frames(), 960, "packet {i}: 20 ms");
        pcm.extend(out.samples);
    }

    // The content (fixture README): 440 Hz and 880 Hz alternating every
    // 250 ms, in opposite phase on the two channels. Over 60–220 ms (clear
    // of the codec's start-up and of the first switch) the left channel is
    // at 440 Hz and the right at 880 Hz: about 141 and 282 zero crossings.
    // That proves the channels are decoded, not swapped or folded to mono.
    let window = &pcm[2 * 2_880..2 * 10_560];
    let (left, right) = (zero_crossings(window, 0), zero_crossings(window, 1));
    assert!((120..=160).contains(&left), "left crossings {left}");
    assert!((250..=310).contains(&right), "right crossings {right}");
    let peak = window.iter().fold(0f32, |m, s| m.max(s.abs()));
    // lavfi's sine has amplitude 1/8, and the recipe scales it by 0.3.
    assert!((0.03..=0.045).contains(&peak), "peak {peak}, want ≈ 0.0375");
}

// --- H.264 -------------------------------------------------------------------

/// Splits a raw Annex-B stream into access units at its AUDs (the fixture
/// is encoded with one before every access unit).
fn access_units(stream: &[u8]) -> Vec<&[u8]> {
    let mut starts = Vec::new();
    for i in 0..stream.len().saturating_sub(4) {
        if stream[i..i + 4] == [0, 0, 0, 1] && stream[i + 4] & 0x1f == 9 {
            starts.push(i);
        }
    }
    starts.push(stream.len());
    starts.windows(2).map(|w| &stream[w[0]..w[1]]).collect()
}

/// Reads an AVCC sample's NAL types back.
fn nal_types(sample: &[u8]) -> Vec<u8> {
    gawk_viewer::decode::h264::split_avcc(sample, 4)
        .unwrap()
        .iter()
        .map(|n| n[0] & 0x1f)
        .collect()
}

#[test]
fn h264_annex_b_clip_becomes_avcc_samples() {
    let stream = fixture("h264-320x240.h264");
    let aus = access_units(&stream);
    assert_eq!(aus.len(), 30);

    // The native broadcasters' DecoderConfig: the codec from the SPS, empty
    // extradata.
    let codec = gawk_encode::h264::parse_codec_string(aus[0]).unwrap();
    assert_eq!(codec, "avc1.42C00D");
    let mut s = H264Stream::new(&codec, &[]).unwrap();
    assert_eq!(s.format(), BitstreamFormat::AnnexB);

    let mut changes = Vec::new();
    let mut idrs = Vec::new();
    for (i, au) in aus.iter().enumerate() {
        let sample = s.sample(au).unwrap_or_else(|e| panic!("AU {i}: {e}"));
        if sample.format_changed {
            changes.push(i);
        }
        if gawk_encode::h264::has_idr(au) {
            assert!(
                gawk_encode::h264::has_sps_pps(au),
                "AU {i}: IDR without SPS/PPS"
            );
            idrs.push(i);
        }
        let types = nal_types(&sample.data);
        assert!(
            !types.iter().any(|t| matches!(t, 7..=9)),
            "AU {i}: AUD/SPS/PPS left in the sample: {types:?}"
        );
        let slice = if gawk_encode::h264::has_idr(au) { 5 } else { 1 };
        assert!(types.contains(&slice), "AU {i}: {types:?}");
    }
    // In-band parameter sets ride every IDR, but only the first changes the
    // format description.
    assert_eq!(idrs, [0, 15], "keyframe every 15 frames");
    assert_eq!(changes, [0]);

    // The avcC built from them round-trips into an AVCC stream that reads
    // back the same parameter sets and codec, and the samples converted
    // above pass through it untouched: the two shapes agree.
    let avcc = s.avcc().unwrap();
    let mut back = H264Stream::new(&codec, &avcc).unwrap();
    assert_eq!(back.format(), BitstreamFormat::Avcc { length_size: 4 });
    assert_eq!(back.parameter_sets(), s.parameter_sets());
    assert_eq!(back.codec(), codec);
    let first = H264Stream::new(&codec, &[])
        .unwrap()
        .sample(aus[0])
        .unwrap();
    assert_eq!(back.sample(&first.data).unwrap().data, first.data);
}
