// R22 device probe (docs/27): a throwaway-but-precise capability harness for the
// one question the stats overlay cannot answer — WHY the muxed AAC audio track
// dies on its first append on iOS, leaving native fullscreen silent.
//
// It drives the PRODUCTION units (Fmp4Muxer, AacTranscoder, MsePresenter,
// probeMseAudio) so every verdict transfers straight to the viewer. Nothing here
// is imported by the app bundle.
//
// Sections, in the order a failure would first show up:
//   A  environment + the R16 device gate
//   B  MediaSource.isTypeSupported matrix (MMS and classic, separately)
//   C  what this device's AAC encoder ACTUALLY produces (the AudioSpecificConfig)
//   D  a RAW append of the production init segments, per step, with the error
//      detail the presenter swallows — plus the same test with the codec string
//      derived from the encoder's own ASC (the leading hypothesis)
//   E  the production MsePresenter path end to end
//   F  an audible native-fullscreen test (the ground truth; needs a tap)

import { MsePresenter, probeMseAudio, probeMsePresentation, getMediaSourceCtor,
  type MediaSourceLike, type SourceBufferLike } from '../features/viewer/msePresentation';
import { AAC_CODEC, AacTranscoder, type TranscodeInput } from '../transport/audio-transcode';
import { Fmp4Muxer, aacMime, buildAudioInitSegment, opusMime } from '../transport/fmp4-muxer';
import { FIXTURE_FRAMES, FIXTURE_FRAME_INTERVAL_US } from '../transport/h264-fixture';

const SAMPLE_RATE = 48_000;
const OPUS_FRAME_SAMPLES = 960; // 20 ms @ 48 kHz — the R15 lane's packet
const VIDEO_CODEC = 'avc1.42E01F'; // the fixture's

const results: Record<string, unknown> = {};
const summary: Array<{ text: string; ok: boolean | null }> = [];

const $ = (id: string) => document.getElementById(id)!;
const setStatus = (s: string) => { $('status').textContent = s; };
const render = () => {
  ($('out') as HTMLTextAreaElement).value = JSON.stringify(results, null, 1);
  $('summary').innerHTML = '';
  for (const s of summary) {
    const d = document.createElement('div');
    d.textContent = s.text;
    if (s.ok === true) d.className = 'ok';
    if (s.ok === false) d.className = 'bad';
    $('summary').appendChild(d);
  }
};
const note = (text: string, ok: boolean | null = null) => { summary.push({ text, ok }); render(); };

const hex = (b: Uint8Array | null | undefined, max = 4096): string =>
  b ? Array.from(b.subarray(0, max), (x) => x.toString(16).padStart(2, '0')).join('') : '';
const errStr = (e: unknown): string =>
  e instanceof Error ? `${e.name}: ${e.message}` : String(e);

// ---------------------------------------------------------------------------
// A — environment

function probeEnvironment(): void {
  const g = globalThis as Record<string, unknown>;
  const v = document.createElement('video');
  results.env = {
    userAgent: navigator.userAgent,
    // R16 Decision 1's gate: its ABSENCE is what routes a device onto the whole
    // R22 path. If this is true here, the device is not iPhone-shaped.
    elementFullscreenAvailable: typeof document.documentElement.requestFullscreen === 'function',
    webkitEnterFullscreen: typeof (v as unknown as Record<string, unknown>).webkitEnterFullscreen === 'function',
    ManagedMediaSource: typeof g.ManagedMediaSource,
    MediaSource: typeof g.MediaSource,
    mediaSourceCtorChosen: getMediaSourceCtor()
      ? (typeof g.ManagedMediaSource === 'function' ? 'ManagedMediaSource' : 'MediaSource')
      : null,
    AudioEncoder: typeof g.AudioEncoder,
    AudioDecoder: typeof g.AudioDecoder,
    AudioData: typeof g.AudioData,
    VideoDecoder: typeof g.VideoDecoder,
    VideoEncoder: typeof g.VideoEncoder,
    audioTracksOnElement: 'audioTracks' in v,
    webkitAudioDecodedByteCount: 'webkitAudioDecodedByteCount' in v,
    disableRemotePlayback: 'disableRemotePlayback' in v,
    // iOS ignores writes to .volume entirely; worth confirming per-device
    // because ViewerScreen mirrors the viewer's slider onto the element.
    volumeSettable: (() => { try { v.volume = 0.5; return v.volume === 0.5; } catch { return false; } })(),
    devicePixelRatio: window.devicePixelRatio,
    hardwareConcurrency: navigator.hardwareConcurrency ?? null,
  };
}

// ---------------------------------------------------------------------------
// B — isTypeSupported matrix

const MIMES = [
  `video/mp4; codecs="${VIDEO_CODEC}"`,
  'video/mp4; codecs="avc1.4D4034"', // the codec in the failing session
  opusMime(),
  aacMime('mp4a.40.2'), // AAC-LC — what the code hardcodes
  aacMime('mp4a.40.5'), // HE-AAC (SBR)
  aacMime('mp4a.40.29'), // HE-AACv2 (PS)
  aacMime('mp4a.40.34'), // MP3-in-MP4, for contrast
  'audio/mp4; codecs="mp4a.67"',
  'audio/mp4', // bare
];

function probeTypes(): void {
  const g = globalThis as Record<string, unknown>;
  const test = (Ctor: unknown): Record<string, boolean | string> | null => {
    if (typeof Ctor !== 'function') return null;
    const out: Record<string, boolean | string> = {};
    for (const m of MIMES) {
      try {
        out[m] = (Ctor as unknown as { isTypeSupported(m: string): boolean }).isTypeSupported(m);
      } catch (e) {
        out[m] = `threw ${errStr(e)}`;
      }
    }
    return out;
  };
  results.isTypeSupported = {
    ManagedMediaSource: test(g.ManagedMediaSource),
    MediaSource: test(g.MediaSource),
  };
  // The production verdict, from the production function.
  results.productionProbe = {
    video: probeMsePresentation('avc1.4D4034'),
    audio: probeMseAudio('opus', 2),
  };
}

// ---------------------------------------------------------------------------
// C — what the AAC encoder really produces

// AudioSpecificConfig (ISO 14496-3 §1.6.2.1) — the bytes that go inside `esds`
// and, on the hypothesis under test, disagree with the hardcoded mp4a.40.2.
const ASC_RATES = [96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050,
  16000, 12000, 11025, 8000, 7350, null, null, null];

function decodeAsc(b: Uint8Array): Record<string, unknown> {
  let bit = 0;
  const read = (n: number): number => {
    let v = 0;
    for (let i = 0; i < n; i++) {
      const byte = b[bit >> 3] ?? 0;
      v = (v << 1) | ((byte >> (7 - (bit & 7))) & 1);
      bit++;
    }
    return v;
  };
  try {
    let aot = read(5);
    if (aot === 31) aot = 32 + read(6);
    const freqIdx = read(4);
    const sampleRate = freqIdx === 15 ? read(24) : ASC_RATES[freqIdx];
    const channelConfig = read(4);
    const out: Record<string, unknown> = {
      audioObjectType: aot,
      samplingFrequencyIndex: freqIdx,
      sampleRate,
      channelConfiguration: channelConfig,
      // This is the string the SourceBuffer/init segment SHOULD declare.
      impliedCodecString: `mp4a.40.${aot}`,
      matchesHardcoded: `mp4a.40.${aot}` === AAC_CODEC,
    };
    if (aot === 5 || aot === 29) {
      const extIdx = read(4);
      out.extensionSampleRate = extIdx === 15 ? read(24) : ASC_RATES[extIdx];
      let inner = read(5);
      if (inner === 31) inner = 32 + read(6);
      out.innerAudioObjectType = inner;
    }
    return out;
  } catch (e) {
    return { error: errStr(e) };
  }
}

function pcmFor(timestampUs: number): TranscodeInput {
  const channels = [new Float32Array(OPUS_FRAME_SAMPLES), new Float32Array(OPUS_FRAME_SAMPLES)];
  for (let s = 0; s < OPUS_FRAME_SAMPLES; s++) {
    const t = timestampUs / 1_000_000 + s / SAMPLE_RATE;
    const v = Math.sin(2 * Math.PI * 440 * t) * 0.25;
    channels[0][s] = v;
    channels[1][s] = v;
  }
  return { timestampUs, sampleRate: SAMPLE_RATE, channels, frameCount: OPUS_FRAME_SAMPLES };
}

interface AacResult {
  description: Uint8Array | null;
  firstChunk: Uint8Array | null;
  outputs: number;
  stats: unknown;
}

async function probeAac(): Promise<AacResult> {
  const g = globalThis as Record<string, unknown>;
  // The declarative answer first — and the NORMALIZED config it hands back,
  // which is where a runtime admits it will encode something else.
  let configSupport: unknown = 'no AudioEncoder';
  if (typeof (g.AudioEncoder as { isConfigSupported?: unknown })?.isConfigSupported === 'function') {
    const cfg = { codec: AAC_CODEC, sampleRate: SAMPLE_RATE, numberOfChannels: 2, bitrate: 128_000 };
    try {
      const r = await (g.AudioEncoder as { isConfigSupported(c: unknown): Promise<unknown> })
        .isConfigSupported(cfg);
      configSupport = JSON.parse(JSON.stringify(r));
    } catch (e) {
      configSupport = `threw ${errStr(e)}`;
    }
  }

  const out: AacResult = { description: null, firstChunk: null, outputs: 0, stats: null };
  const transcoder = new AacTranscoder((o) => {
    out.outputs++;
    if (o.description && !out.description) out.description = o.description;
    if (!out.firstChunk) out.firstChunk = o.data;
  });
  // WebCodecs AudioEncoder buffers: 1024-sample AAC frames out of 960-sample
  // input, and implementations hold several before the first output. Feed real
  // seconds of audio and poll, rather than guessing a sleep.
  const deadline = Date.now() + 6000;
  for (let i = 0; i < 150 && Date.now() < deadline; i++) {
    transcoder.push(pcmFor(i * 20_000));
    if (i % 10 === 9) {
      await new Promise((r) => setTimeout(r, 60));
      if (out.description && out.outputs > 3) break;
      if (transcoder.getStats().state !== 'active') break;
    }
  }
  while (Date.now() < deadline && !out.description) await new Promise((r) => setTimeout(r, 100));
  out.stats = transcoder.getStats();

  const asc = out.description ? decodeAsc(out.description) : null;
  results.aacEncoder = {
    isConfigSupported: configSupport,
    transcoderStats: out.stats,
    outputs: out.outputs,
    descriptionHex: hex(out.description),
    descriptionBytes: out.description?.length ?? 0,
    audioSpecificConfig: asc,
    firstChunkBytes: out.firstChunk?.length ?? 0,
    firstChunkHead: hex(out.firstChunk?.subarray(0, 12)),
    // An ADTS syncword here would mean the encoder framed the payload, which
    // must never be muxed into an mp4 sample.
    looksLikeAdts: out.firstChunk
      ? out.firstChunk[0] === 0xff && (out.firstChunk[1] & 0xf0) === 0xf0
      : null,
  };
  transcoder.close();
  return out;
}

// ---------------------------------------------------------------------------
// D — RAW append: exactly which append kills the audio track, and with what error

interface AppendOutcome {
  step: string;
  ok: boolean;
  detail: string | null;
  msReadyState: string | null;
  videoError: string | null;
}

function mediaErrorOf(v: HTMLVideoElement): string | null {
  const e = v.error;
  return e ? `code ${e.code}${e.message ? `: ${e.message}` : ''}` : null;
}

function appendAndWait(
  sb: SourceBufferLike,
  data: Uint8Array,
  step: string,
  ms: MediaSourceLike,
  video: HTMLVideoElement,
): Promise<AppendOutcome> {
  return new Promise((resolve) => {
    let done = false;
    const finish = (ok: boolean, detail: string | null) => {
      if (done) return;
      done = true;
      clearTimeout(timer);
      sb.removeEventListener('updateend', onEnd);
      sb.removeEventListener('error', onErr);
      // Let the error propagate to the element before reading it.
      setTimeout(() => resolve({
        step, ok, detail,
        msReadyState: ms.readyState,
        videoError: mediaErrorOf(video),
      }), 40);
    };
    const onEnd = () => finish(true, null);
    // Per MSE's append-error algorithm this fires BEFORE updateend, so it wins.
    const onErr = () => finish(false, 'SourceBuffer "error" event (append rejected)');
    const timer = setTimeout(() => finish(false, 'timed out after 4s'), 4000);
    sb.addEventListener('updateend', onEnd);
    sb.addEventListener('error', onErr);
    try {
      sb.appendBuffer(data as BufferSource);
    } catch (e) {
      finish(false, `appendBuffer threw ${errStr(e)}`);
    }
  });
}

// One raw run: both SourceBuffers created up front (docs/27 finding 5), video
// init, then the audio init built by the production muxer, then media.
async function rawAppendRun(
  label: string,
  audioCodecString: string,
  description: Uint8Array,
): Promise<void> {
  const Ctor = getMediaSourceCtor();
  const rec: Record<string, unknown> = { label, audioCodecString };
  results.rawAppend = { ...(results.rawAppend as object ?? {}), [label]: rec };
  if (!Ctor) { rec.error = 'no MediaSource ctor'; return; }

  const video = document.createElement('video');
  video.playsInline = true;
  video.muted = true;
  (video as HTMLVideoElement & { disableRemotePlayback?: boolean }).disableRemotePlayback = true;
  video.style.cssText = 'position:fixed;left:-9999px;width:2px;height:2px';
  document.body.appendChild(video);

  const ms = new Ctor();
  let objectUrl: string | null = null;
  try {
    (video as unknown as { srcObject: unknown }).srcObject = ms;
  } catch {
    objectUrl = URL.createObjectURL(ms as unknown as MediaSource);
    video.src = objectUrl;
  }
  // MMS only opens once the element wants data.
  video.load();
  void video.play().catch(() => {});

  const opened = await new Promise<boolean>((resolve) => {
    if (ms.readyState === 'open') return resolve(true);
    const t = setTimeout(() => resolve(ms.readyState === 'open'), 4000);
    ms.addEventListener('sourceopen', () => { clearTimeout(t); resolve(true); });
  });
  rec.sourceOpened = opened;
  rec.streaming = (ms as { streaming?: boolean }).streaming ?? null;
  if (!opened) { rec.error = `MediaSource never opened (readyState ${ms.readyState})`; return; }

  try { ms.duration = Infinity; } catch (e) { rec.durationError = errStr(e); }
  rec.liveDuration = ms.duration === Infinity;

  const videoMime = `video/mp4; codecs="${VIDEO_CODEC}"`;
  const audioMime = aacMime(audioCodecString);
  rec.videoMime = videoMime;
  rec.audioMime = audioMime;

  let vsb: SourceBufferLike, asb: SourceBufferLike | null = null;
  try {
    vsb = ms.addSourceBuffer(videoMime);
  } catch (e) { rec.error = `video addSourceBuffer: ${errStr(e)}`; return; }
  try {
    asb = ms.addSourceBuffer(audioMime);
    rec.audioSourceBufferCreated = true;
  } catch (e) {
    rec.audioSourceBufferCreated = false;
    rec.audioAddSourceBufferError = errStr(e);
  }

  const steps: AppendOutcome[] = [];
  const muxer = new Fmp4Muxer();

  // Video init + a couple of real frames, so the element has something to hold.
  const vsegs = muxer.push({
    keyframe: FIXTURE_FRAMES[0].keyframe,
    timestampUs: 0n,
    data: FIXTURE_FRAMES[0].data,
    config: { codec: VIDEO_CODEC, extradata: new Uint8Array(0) },
  });
  for (const s of vsegs) {
    if (s.kind === 'init') steps.push(await appendAndWait(vsb, s.data, 'video init', ms, video));
  }

  // The audio init the production muxer builds for THIS device's ASC.
  const audioInit = buildAudioInitSegment(
    { codec: audioCodecString, sampleRate: SAMPLE_RATE, channels: 2, description },
    'aac',
  );
  rec.audioInitBytes = audioInit.length;
  rec.audioInitHex = hex(audioInit);
  if (asb) steps.push(await appendAndWait(asb, audioInit, 'audio init', ms, video));

  // A few audio media segments, via the production muxer path.
  if (asb && steps[steps.length - 1]?.ok) {
    const m2 = new Fmp4Muxer();
    // Anchor its output timeline with one video frame, as production does.
    m2.push({ keyframe: true, timestampUs: 0n, data: FIXTURE_FRAMES[0].data,
      config: { codec: VIDEO_CODEC, extradata: new Uint8Array(0) } });
    m2.setAudioConfig({ codec: audioCodecString, sampleRate: SAMPLE_RATE, channels: 2, description });
    const pendingAudio: Uint8Array[] = [];
    const t2 = new AacTranscoder((o) => {
      for (const seg of m2.pushAudio({ timestampUs: BigInt(Math.round(o.timestampUs)), data: o.data })) {
        pendingAudio.push(seg.data);
      }
    });
    for (let i = 0; i < 6; i++) t2.push(pcmFor(i * 20_000));
    await new Promise((r) => setTimeout(r, 500));
    t2.close();
    rec.audioMediaSegmentsBuilt = pendingAudio.length;
    for (let i = 0; i < Math.min(3, pendingAudio.length); i++) {
      steps.push(await appendAndWait(asb, pendingAudio[i], `audio media ${i}`, ms, video));
      if (!steps[steps.length - 1].ok) break;
    }
  }

  rec.steps = steps;
  rec.finalVideoError = mediaErrorOf(video);
  rec.finalReadyState = video.readyState;
  rec.audioTracks = (video as HTMLVideoElement & { audioTracks?: { length: number } }).audioTracks?.length ?? null;

  try { (video as unknown as { srcObject: unknown }).srcObject = null; } catch { /* n/a */ }
  if (objectUrl) URL.revokeObjectURL(objectUrl);
  video.remove();
}

// ---------------------------------------------------------------------------
// E + F — the production presenter path, left on screen for the audible test

let livePresenter: MsePresenter | null = null;

async function productionRun(audioCodecString: string): Promise<void> {
  const video = $('stage') as HTMLVideoElement;
  video.playsInline = true;
  // Muted for the AUTOMATED phase only: by the time this runs the "Run probe"
  // gesture is long spent, and iOS blocks unmuted autoplay — an unmuted element
  // here would just sit paused and report zero frames. The audible test unmutes
  // inside the fullscreen tap, which is a real gesture. Muted playback still
  // decodes, so audioTracks/webkitAudioDecodedByteCount stay meaningful.
  video.muted = true;
  (video as HTMLVideoElement & { disableRemotePlayback?: boolean }).disableRemotePlayback = true;

  let framesPresented = 0;
  const rvfc = video as HTMLVideoElement & { requestVideoFrameCallback?: (cb: () => void) => number };
  const onFrame = () => { framesPresented++; rvfc.requestVideoFrameCallback?.(onFrame); };
  rvfc.requestVideoFrameCallback?.(onFrame);

  const muxer = new Fmp4Muxer();
  const presenter = new MsePresenter();
  livePresenter = presenter;
  presenter.attach(video);
  presenter.setExpectedAudioMime(aacMime(audioCodecString));

  const transcoder = new AacTranscoder((o) => {
    if (o.description) {
      for (const seg of muxer.setAudioConfig({
        codec: audioCodecString, sampleRate: SAMPLE_RATE, channels: 2, description: o.description,
      })) presenter.pushSegment(seg);
    }
    for (const seg of muxer.pushAudio({
      timestampUs: BigInt(Math.round(o.timestampUs)), data: o.data,
    })) presenter.pushSegment(seg);
  });
  // Prime the transcoder before the tier is armed, as production does.
  transcoder.push(pcmFor(0));
  await new Promise((r) => setTimeout(r, 300));

  // The fixture looped, with audio interleaved on the same clock. It must keep
  // running FOREVER, not for a fixed few seconds: the first device pass fed ~4 s,
  // which had played out by the time the fullscreen tap came — so the native
  // player showed a frozen last frame and had nothing left to sound. A live
  // stream is what this is standing in for, so keep it live.
  let frameIndex = 0;
  let audioUs = 0;
  const feedOneSecond = () => {
    const until = frameIndex + 30;
    for (; frameIndex < until; frameIndex++) {
      const f = FIXTURE_FRAMES[frameIndex % FIXTURE_FRAMES.length];
      const tsUs = Math.round(frameIndex * FIXTURE_FRAME_INTERVAL_US);
      for (const seg of muxer.push({
        keyframe: f.keyframe,
        timestampUs: BigInt(tsUs),
        data: f.data,
        config: f.keyframe ? { codec: VIDEO_CODEC, extradata: new Uint8Array(0) } : null,
      })) presenter.pushSegment(seg);
      while (audioUs <= tsUs + FIXTURE_FRAME_INTERVAL_US) {
        transcoder.push(pcmFor(audioUs));
        audioUs += 20_000;
      }
    }
  };
  // Prime ~3 s, then keep a second of lead topped up in real time.
  for (let i = 0; i < 3; i++) { feedOneSecond(); await new Promise((r) => setTimeout(r, 40)); }
  setInterval(feedOneSecond, 1000);
  await new Promise((r) => setTimeout(r, 600));

  await Promise.race([video.play().catch(() => {}), new Promise((r) => setTimeout(r, 2000))]);
  await new Promise((r) => setTimeout(r, 1500));

  const stats = presenter.getStats();
  results.production = {
    presenter: stats,
    framesPresented,
    currentTime: video.currentTime,
    paused: video.paused,
    readyState: video.readyState,
    videoWidth: video.videoWidth,
    videoHeight: video.videoHeight,
    videoError: mediaErrorOf(video),
    buffered: (() => {
      try {
        const b = video.buffered;
        return b.length ? { ranges: b.length, start: b.start(0), end: b.end(b.length - 1) } : { ranges: 0 };
      } catch (e) { return errStr(e); }
    })(),
    // The two element-level facts the viewer's diagnostics do not carry.
    audioTracks: (video as HTMLVideoElement & { audioTracks?: { length: number } }).audioTracks?.length ?? null,
    webkitAudioDecodedByteCount:
      (video as HTMLVideoElement & { webkitAudioDecodedByteCount?: number }).webkitAudioDecodedByteCount ?? null,
    webkitVideoDecodedByteCount:
      (video as HTMLVideoElement & { webkitVideoDecodedByteCount?: number }).webkitVideoDecodedByteCount ?? null,
    muted: video.muted,
    volume: video.volume,
  };

  note(`Production path: audio SourceBuffer ${stats.audioTrack ? 'ALIVE' : 'DROPPED'}, `
    + `${stats.audioSegmentsAppended} audio appends, ${stats.appendErrors} append errors`,
    stats.audioTrack && stats.audioSegmentsAppended > 2);
  note(`Element: ${framesPresented} frames presented, audioTracks=`
    + `${(results.production as Record<string, unknown>).audioTracks}, `
    + `audioDecodedBytes=${(results.production as Record<string, unknown>).webkitAudioDecodedByteCount}`,
    framesPresented > 0);
}

// ---------------------------------------------------------------------------

async function run(): Promise<void> {
  ($('run') as HTMLButtonElement).disabled = true;
  summary.length = 0;
  results.capturedAt = new Date().toISOString();

  setStatus('A — environment…');
  probeEnvironment();
  const env = results.env as Record<string, unknown>;
  note(`Gate: elementFullscreen=${env.elementFullscreenAvailable} `
    + `(R22 runs only when false), MMS=${env.ManagedMediaSource}`, null);
  render();

  setStatus('B — isTypeSupported matrix…');
  probeTypes();
  const prodAudio = (results.productionProbe as { audio: { supported: boolean; codec: string | null; reason: string } }).audio;
  note(`probeMseAudio → ${prodAudio.supported ? `tier "${prodAudio.codec}"` : 'unsupported'} (${prodAudio.reason})`,
    prodAudio.supported);
  render();

  setStatus('C — AAC encoder (this is the slow one)…');
  const aac = await probeAac();
  if (!aac.description) {
    note('AAC encoder produced NO AudioSpecificConfig — the muxer can never build an init segment here.', false);
    setStatus('Stopped: no AAC description. Copy the JSON.');
    ($('run') as HTMLButtonElement).disabled = false;
    render();
    return;
  }
  const asc = (results.aacEncoder as { audioSpecificConfig: Record<string, unknown> }).audioSpecificConfig;
  note(`Encoder ASC: AOT ${asc.audioObjectType} @ ${asc.sampleRate} Hz, `
    + `${asc.channelConfiguration} ch → implies "${asc.impliedCodecString}"`
    + (asc.matchesHardcoded ? ' (matches mp4a.40.2)' : ` — MISMATCHES the hardcoded ${AAC_CODEC}`),
    Boolean(asc.matchesHardcoded));
  render();

  setStatus('D — raw append of the production init segments…');
  await rawAppendRun('hardcoded_mp4a.40.2', AAC_CODEC, aac.description);
  const impl = String(asc.impliedCodecString);
  if (impl !== AAC_CODEC) await rawAppendRun('asc_derived', impl, aac.description);

  const runs = results.rawAppend as Record<string, { steps?: AppendOutcome[] }>;
  for (const [label, rec] of Object.entries(runs)) {
    const bad = rec.steps?.find((s) => !s.ok);
    note(bad
      ? `Raw [${label}]: FAILED at "${bad.step}" — ${bad.detail}; videoError=${bad.videoError}`
      : `Raw [${label}]: all appends accepted`,
      !bad);
  }
  render();

  setStatus('E — production MsePresenter path…');
  // Pick the codec string whose raw run actually got an audio init ACCEPTED.
  // "No failing steps" is not the same thing and must not be read as success:
  // on the first device pass the ASC-derived run failed at addSourceBuffer, so
  // it recorded no audio-init step at all — and the production run then went out
  // with a codec string iOS had already refused.
  const acceptedInit = (r?: { steps?: AppendOutcome[] }) =>
    Boolean(r?.steps?.some((s) => s.step === 'audio init' && s.ok));
  const chosen = acceptedInit(runs.asc_derived) && impl !== AAC_CODEC ? impl : AAC_CODEC;
  results.productionUsedCodec = chosen;
  results.productionCodecRationale = acceptedInit(runs[`hardcoded_${AAC_CODEC}`])
    ? `${AAC_CODEC} init accepted in the raw test`
    : acceptedInit(runs.asc_derived)
      ? `${AAC_CODEC} init rejected; using the ASC-derived ${impl}`
      : 'no audio init was accepted by any codec string — expect a video-only presentation';
  await productionRun(chosen);

  setStatus('Done. Now tap “Enter native fullscreen” and listen for a 440 Hz tone, '
    + 'then copy the JSON.');
  ($('fs') as HTMLButtonElement).disabled = false;
  $('heard').style.display = 'block';
  ($('run') as HTMLButtonElement).disabled = false;
  render();
}

// F — the audible ground truth. Mirrors useFullscreen tier 2 exactly:
// seek to live, play, webkitEnterFullscreen — all inside the gesture.
function enterFullscreen(): void {
  const video = $('stage') as HTMLVideoElement & { webkitEnterFullscreen?: () => void };
  const fsRec: Record<string, unknown> = {
    readyStateBefore: video.readyState,
    mutedBefore: video.muted,
    hasWebkitEnterFullscreen: typeof video.webkitEnterFullscreen === 'function',
  };
  try {
    const b = video.buffered;
    if (b.length) {
      const end = b.end(b.length - 1);
      if (end - video.currentTime > 0.5) video.currentTime = Math.max(b.start(b.length - 1), end - 0.1);
    }
    video.muted = false;
    if (video.paused) void video.play()?.catch?.(() => {});
    video.webkitEnterFullscreen?.();
    fsRec.entered = true;
  } catch (e) {
    fsRec.entered = false;
    fsRec.error = errStr(e);
  }
  setTimeout(() => {
    fsRec.audioTracksAfter =
      (video as HTMLVideoElement & { audioTracks?: { length: number } }).audioTracks?.length ?? null;
    fsRec.audioDecodedBytesAfter =
      (video as HTMLVideoElement & { webkitAudioDecodedByteCount?: number }).webkitAudioDecodedByteCount ?? null;
    fsRec.currentTimeAfter = video.currentTime;
    fsRec.pausedAfter = video.paused;
    fsRec.presenterAfter = livePresenter?.getStats() ?? null;
    results.fullscreen = fsRec;
    render();
  }, 3000);
  results.fullscreen = fsRec;
  render();
}

// A one-shot device test must never end with an empty textarea: whatever blew
// up, the sections that already ran are the point.
async function runGuarded(): Promise<void> {
  try {
    await run();
  } catch (e) {
    results.fatalError = errStr(e);
    note(`Probe threw: ${errStr(e)} — copy the JSON anyway, earlier sections are valid.`, false);
    setStatus(`Threw: ${errStr(e)}. Copy the JSON.`);
    ($('run') as HTMLButtonElement).disabled = false;
    ($('fs') as HTMLButtonElement).disabled = false;
    render();
  }
}

$('run').addEventListener('click', () => { void runGuarded(); });
$('fs').addEventListener('click', enterFullscreen);
$('yes').addEventListener('click', () => {
  results.audibleInFullscreen = true;
  note('Reported: tone WAS audible in native fullscreen', true);
});
$('no').addEventListener('click', () => {
  results.audibleInFullscreen = false;
  note('Reported: native fullscreen was SILENT', false);
});
$('copy').addEventListener('click', () => {
  const ta = $('out') as HTMLTextAreaElement;
  void navigator.clipboard?.writeText(ta.value).then(
    () => setStatus('Copied to clipboard.'),
    () => { ta.select(); setStatus('Select-all + copy manually.'); },
  );
});

probeEnvironment();
render();
setStatus('Ready. Tap “Run probe”.');
