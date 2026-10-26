// R79 MV0 — browser voice pipeline measurement harness (docs/80 §9 MV0 (a)).
//
// Self-contained: no gawk-app imports, so it measures the browser's own
// WebCodecs / WebAudio / WebTransport behaviour rather than our wrappers.
// The shape mirrors docs/80 D21/D22:
//
//   source ─► capture ─► AudioEncoder(opus) ─► loopback ─► AudioDecoder
//          ─► jitter-buffer stand-in (in the output worklet) ─► AudioWorklet out
//
//   source:    'gum'   getUserMedia (fake device under automation), EC/NS/AGC on
//              'synth' a looped tone-burst train generated in the same
//                      AudioContext (deterministic onsets on every browser)
//   capture:   'mstp'    MediaStreamTrackProcessor (Chromium; Safari has it in
//                        workers only, not probed here)
//              'worklet' an AudioWorklet node that copies 128-frame render
//                        quanta into 480-frame (10 ms) blocks and posts them;
//                        the main thread wraps each block in an AudioData
//                        (the Firefox path docs/80 D21 budgets)
//   transport: 'inpage'  chunk handed straight to the decoder
//              'wt'      each chunk sent as a WebTransport datagram to a gawk
//                        relay's /echo and decoded when it comes back
//
// Every clock reading is performance.now() on the main thread (wall), or the
// AudioContext frame counter inside a worklet (context). The page exposes
// window.mv0.run(opts) → a JSON-serialisable result; run.mjs drives it.

const CAPTURE_WORKLET = `
class Mv0Capture extends AudioWorkletProcessor {
  constructor(o) {
    super();
    const p = o.processorOptions;
    this.block = p.block;          // frames per posted block (480 = 10 ms @ 48k)
    this.emit = p.emit;            // false: onset tap only (MSTP runs)
    this.thr = p.threshold;
    this.buf = new Float32Array(this.block);
    this.n = 0;
    this.blockStart = 0;
    this.armed = true;
    this.quiet = 0;
    this.quanta = 0;
    this.quantumLens = {};
    this.emptyInputs = 0;
    this.stopped = false;
    this.port.onmessage = (e) => {
      if (e.data === 'stop') {
        this.stopped = true;
        this.port.postMessage({ t: 'stats', quanta: this.quanta, quantumLens: this.quantumLens,
          emptyInputs: this.emptyInputs, audioDataInScope: typeof AudioData !== 'undefined',
          sampleRate });
      }
    };
  }
  process(inputs) {
    if (this.stopped) return false;
    this.quanta++;
    // A quantum with no input (source not started yet, or a MediaStream
    // source that has not delivered) is counted and captured as silence, so
    // the block clock stays contiguous with the context's frame counter.
    const ch = inputs[0] && inputs[0][0] ? inputs[0][0] : (this.emptyInputs++, this.zeros || (this.zeros = new Float32Array(128)));
    this.quantumLens[ch.length] = (this.quantumLens[ch.length] || 0) + 1;
    for (let i = 0; i < ch.length; i++) {
      const f = currentFrame + i;
      const v = ch[i];
      const a = v < 0 ? -v : v;
      if (a > this.thr) {
        if (this.armed) { this.port.postMessage({ t: 'onset', frame: f }); this.armed = false; }
        this.quiet = 0;
      } else if (++this.quiet > 0.05 * sampleRate) {
        this.armed = true;
      }
      if (!this.emit) continue;
      if (this.n === 0) this.blockStart = f;
      this.buf[this.n++] = v;
      if (this.n === this.block) {
        // Posted from inside process(): the block's last sample is in this
        // quantum, so (currentFrame + 128) - (blockStart + block) is how long
        // the block waited for the quantum boundary.
        this.port.postMessage({ t: 'pcm', frame: this.blockStart, quantumEnd: currentFrame + ch.length,
          pcm: this.buf }, [this.buf.buffer]);
        this.buf = new Float32Array(this.block);
        this.n = 0;
      }
    }
    return true;
  }
}
registerProcessor('mv0-capture', Mv0Capture);

// Output: the jitter-buffer stand-in is a FIFO that holds playback until
// targetFrames are queued (prefill), then drains 128-frame quanta; on an
// underrun it goes back to prefill. It records the queued depth at every
// quantum and timestamps output onsets in context frames.
class Mv0Output extends AudioWorkletProcessor {
  constructor(o) {
    super();
    const p = o.processorOptions;
    this.target = p.targetFrames;
    this.thr = p.threshold;
    this.q = [];
    this.qFrames = 0;
    this.off = 0;
    this.playing = false;
    this.underruns = 0;
    this.depths = [];
    this.transits = []; // frames from a packet's first captured sample to its arrival here
    this.armed = true;
    this.quiet = 0;
    this.stopped = false;
    this.port.onmessage = (e) => {
      const m = e.data;
      if (m.t === 'pcm') {
        this.q.push(m.pcm); this.qFrames += m.pcm.length;
        // currentFrame here is the first frame of the next quantum this node
        // renders: the earliest the packet could start playing.
        if (m.captureFrame !== null && m.captureFrame !== undefined) this.transits.push(currentFrame - m.captureFrame);
      }
      else if (m.t === 'stop') {
        this.stopped = true;
        this.port.postMessage({ t: 'stats', underruns: this.underruns, depths: this.depths, transits: this.transits });
      }
    };
  }
  process(_inputs, outputs) {
    if (this.stopped) return false;
    const out = outputs[0][0];
    if (!this.playing && this.qFrames >= Math.max(this.target, 1)) this.playing = true;
    if (this.playing) {
      this.depths.push(this.qFrames);
      for (let i = 0; i < out.length; i++) {
        if (this.q.length === 0) { this.underruns++; this.playing = false; break; }
        const head = this.q[0];
        const v = head[this.off++];
        this.qFrames--;
        if (this.off >= head.length) { this.q.shift(); this.off = 0; }
        out[i] = v;
        const a = v < 0 ? -v : v;
        if (a > this.thr) {
          if (this.armed) { this.port.postMessage({ t: 'onset', frame: currentFrame + i }); this.armed = false; }
          this.quiet = 0;
        } else if (++this.quiet > 0.05 * sampleRate) {
          this.armed = true;
        }
      }
    }
    return true;
  }
}
registerProcessor('mv0-output', Mv0Output);
`;

const SR = 48000;
// One tone burst every 257.29 ms (12350 frames at 48 kHz): deliberately not a
// multiple of 480 or 960, so onsets sweep every phase of a 10/20 ms packet.
const BURST_PERIOD_S = 12350 / 48000;
const BURST_LEN_S = 0.02;    // 20 ms of 1 kHz
const ONSET_THRESHOLD = 0.05;

function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms));
}

function quantiles(xs) {
  const v = xs.filter((x) => Number.isFinite(x)).sort((a, b) => a - b);
  if (v.length === 0) return { n: 0 };
  const q = (p) => v[Math.min(v.length - 1, Math.max(0, Math.ceil(p * v.length) - 1))];
  return { n: v.length, min: v[0], median: q(0.5), p95: q(0.95), max: v[v.length - 1],
    mean: v.reduce((a, b) => a + b, 0) / v.length };
}

function histogram(xs) {
  const h = {};
  for (const x of xs) h[x] = (h[x] || 0) + 1;
  return h;
}

// Opus TOC byte (RFC 6716 §3.1) → packet duration in µs and the mode. This is
// the authoritative "did 10 ms really give 10 ms packets" check: it reads the
// bitstream, not the chunk's duration field.
export function opusPacketInfo(bytes) {
  if (bytes.length === 0) return { durationUs: 0, mode: 'empty', frames: 0 };
  const toc = bytes[0];
  const config = toc >> 3;
  const code = toc & 3;
  let frameUs;
  let mode;
  if (config < 12) { mode = 'silk'; frameUs = [10000, 20000, 40000, 60000][config % 4]; }
  else if (config < 16) { mode = 'hybrid'; frameUs = config % 2 ? 20000 : 10000; }
  else { mode = 'celt'; frameUs = [2500, 5000, 10000, 20000][config % 4]; }
  let frames = code === 0 ? 1 : code === 3 ? (bytes.length > 1 ? bytes[1] & 0x3f : 0) : 2;
  const bw = config < 12 ? ['NB', 'MB', 'WB'][Math.floor(config / 4)]
    : config < 16 ? (config < 14 ? 'SWB' : 'FB')
    : ['NB', 'WB', 'SWB', 'FB'][Math.floor((config - 16) / 4)];
  return { durationUs: frameUs * frames, mode, frames, bandwidth: bw, config };
}

function makeBurstBuffer(ctx) {
  const len = Math.round(BURST_PERIOD_S * ctx.sampleRate);
  const buf = ctx.createBuffer(1, len, ctx.sampleRate);
  const d = buf.getChannelData(0);
  const burst = Math.round(BURST_LEN_S * ctx.sampleRate);
  for (let i = 0; i < burst; i++) d[i] = 0.5 * Math.sin((2 * Math.PI * 1000 * i) / ctx.sampleRate);
  return buf;
}

// Pairs every output onset with the latest input onset before it.
function pairOnsets(inFrames, outFrames, sampleRate) {
  const lat = [];
  for (const o of outFrames) {
    let best = null;
    for (const i of inFrames) if (i <= o && (best === null || i > best)) best = i;
    if (best !== null) {
      const ms = ((o - best) / sampleRate) * 1000;
      if (ms < BURST_PERIOD_S * 1000) lat.push(ms);
    }
  }
  return lat;
}

async function probeSupport(frameDurationUs) {
  const out = {};
  const base = { codec: 'opus', sampleRate: SR, numberOfChannels: 1, bitrate: 40000 };
  const variants = {
    withFrameDuration: { ...base, opus: { frameDuration: frameDurationUs } },
    withFrameDurationVoip: { ...base, opus: { frameDuration: frameDurationUs, application: 'voip' } },
    withFrameDurationLowdelay: { ...base, opus: { frameDuration: frameDurationUs, application: 'lowdelay' } },
    plain: base,
  };
  for (const [k, cfg] of Object.entries(variants)) {
    try {
      const r = await AudioEncoder.isConfigSupported(cfg);
      out[k] = { supported: r.supported, echoedOpus: r.config && r.config.opus ? { ...r.config.opus } : null };
    } catch (e) {
      out[k] = { supported: false, error: String(e) };
    }
  }
  return out;
}

export async function connectEcho(url, hashHex) {
  const hash = new Uint8Array(hashHex.match(/../g).map((h) => parseInt(h, 16)));
  const wt = new WebTransport(url, { serverCertificateHashes: [{ algorithm: 'sha-256', value: hash }] });
  await wt.ready;
  return wt;
}

async function run(opts) {
  const {
    source = 'synth',         // 'gum' | 'synth'
    capture = 'worklet',      // 'mstp' | 'worklet'
    frameDurationUs = 10000,  // opus.frameDuration
    application = null,       // null (UA default) | 'voip'
    seconds = 6,
    jitterTargetMs = 20,
    transport = 'inpage',     // 'inpage' | 'wt'
    echoUrl = null,
    echoHash = null,
  } = opts;
  const result = { opts, ua: navigator.userAgent, errors: [], notes: [] };
  result.env = {
    AudioEncoder: typeof AudioEncoder !== 'undefined',
    AudioDecoder: typeof AudioDecoder !== 'undefined',
    MediaStreamTrackProcessor: typeof MediaStreamTrackProcessor !== 'undefined',
    AudioWorkletNode: typeof AudioWorkletNode !== 'undefined',
    WebTransport: typeof WebTransport !== 'undefined',
  };
  if (!result.env.AudioEncoder || !result.env.AudioDecoder) {
    result.errors.push('WebCodecs audio missing');
    return result;
  }
  result.support = await probeSupport(frameDurationUs);
  if (transport === 'wt' && !result.env.WebTransport) {
    result.errors.push('no WebTransport');
    return result;
  }
  if (capture === 'mstp' && !result.env.MediaStreamTrackProcessor) {
    result.errors.push('no MediaStreamTrackProcessor on Window');
    return result;
  }

  let ctx;
  try {
    ctx = new AudioContext({ sampleRate: SR, latencyHint: 'interactive' });
  } catch (e) {
    result.notes.push(`AudioContext({sampleRate:48000}) threw ${e}; using the default rate`);
    ctx = new AudioContext({ latencyHint: 'interactive' });
  }
  const tCtx = performance.now();
  if (ctx.state !== 'running') {
    await Promise.race([ctx.resume().catch(() => {}), sleep(10000)]);
  }
  result.contextStartMs = performance.now() - tCtx;
  if (ctx.state !== 'running') {
    result.errors.push(`AudioContext never reached 'running' (state ${ctx.state}): no audio output device to clock it`);
    result.audioContext = { sampleRate: ctx.sampleRate, state: ctx.state };
    ctx.close().catch(() => {});
    return result;
  }
  result.audioContext = {
    sampleRate: ctx.sampleRate,
    state: ctx.state,
    baseLatency: ctx.baseLatency ?? null,
    outputLatency: ctx.outputLatency ?? null,
  };
  const blobUrl = URL.createObjectURL(new Blob([CAPTURE_WORKLET], { type: 'text/javascript' }));
  await ctx.audioWorklet.addModule(blobUrl);

  // --- source -------------------------------------------------------------
  let srcNode;
  let track = null;
  let stream = null;
  if (source === 'gum') {
    const t0 = performance.now();
    stream = await navigator.mediaDevices.getUserMedia({
      audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true, channelCount: 1 },
    });
    result.gumMs = performance.now() - t0;
    track = stream.getAudioTracks()[0];
    result.track = { label: track.label, settings: track.getSettings() };
    srcNode = ctx.createMediaStreamSource(stream);
  } else {
    const bs = ctx.createBufferSource();
    bs.buffer = makeBurstBuffer(ctx);
    bs.loop = true;
    srcNode = bs;
    if (capture === 'mstp') {
      const dest = ctx.createMediaStreamDestination();
      dest.channelCount = 1;
      bs.connect(dest);
      stream = dest.stream;
      track = stream.getAudioTracks()[0];
      result.track = { label: track.label, settings: track.getSettings() };
    }
  }

  // --- output -------------------------------------------------------------
  const outNode = new AudioWorkletNode(ctx, 'mv0-output', {
    numberOfInputs: 0, numberOfOutputs: 1, outputChannelCount: [1],
    processorOptions: { targetFrames: Math.round((jitterTargetMs / 1000) * ctx.sampleRate), threshold: ONSET_THRESHOLD },
  });
  const mute = ctx.createGain();
  mute.gain.value = 0; // headless has no device anyway; headed runs stay silent
  outNode.connect(mute).connect(ctx.destination);
  const outOnsets = [];
  let outStats = null;
  outNode.port.onmessage = (e) => {
    if (e.data.t === 'onset') outOnsets.push(e.data.frame);
    else if (e.data.t === 'stats') outStats = e.data;
  };

  // --- capture tap / worklet ---------------------------------------------
  const capNode = new AudioWorkletNode(ctx, 'mv0-capture', {
    numberOfInputs: 1, numberOfOutputs: 0, channelCount: 1, channelCountMode: 'explicit',
    processorOptions: { block: Math.round(0.01 * ctx.sampleRate), emit: capture === 'worklet', threshold: ONSET_THRESHOLD },
  });
  srcNode.connect(capNode);
  const inOnsets = [];
  let capStats = null;

  // --- encoder / decoder --------------------------------------------------
  const inputs = [];      // { tsUs, endUs, wall }
  const chunks = [];      // { tsUs, durField, bytes, info, wall, latMs }
  const decoded = [];     // { tsUs, frames, wall, latMs }
  const decodeCalls = new Map(); // tsUs → wall at decode()
  const decodeCallLog = [];        // [tsUs, wall]
  const decodedRawTs = [];
  const blockArrivals = []; // worklet path: { wall, frame, waitFrames }
  const mstpFrames = [];    // mstp path: { wall, frames, sampleRate, channels, format, tsUs }
  let decoderConfigured = false;
  let decoderDescription = null;

  const decoder = new AudioDecoder({
    output: (ad) => {
      const wall = performance.now();
      // Matched by order: the decoder emits one AudioData per packet, in
      // decode order, but its timestamps need not echo the input's (Chromium
      // shifts them by the Opus pre-skip).
      const call = decodeCallLog[decoded.length];
      const callAt = call ? call[1] : undefined;
      decodedRawTs.push(ad.timestamp);
      decoded.push({ tsUs: ad.timestamp, frames: ad.numberOfFrames, sampleRate: ad.sampleRate,
        format: ad.format, wall, latMs: callAt === undefined ? null : wall - callAt });
      const pcm = new Float32Array(ad.numberOfFrames);
      try {
        ad.copyTo(pcm, { planeIndex: 0, format: 'f32-planar' });
      } catch (e) {
        result.errors.push(`decoded copyTo f32-planar: ${e}`);
      }
      const ad0frames = ad.numberOfFrames;
      ad.close();
      outNode.port.postMessage({ t: 'pcm', pcm, captureFrame: decodedCaptureFrame(ad0frames) }, [pcm.buffer]);
      decodedCum += ad0frames;
    },
    error: (e) => result.errors.push(`decoder: ${e}`),
  });

  function configureDecoder() {
    const cfg = { codec: 'opus', sampleRate: SR, numberOfChannels: 1 };
    if (decoderDescription) cfg.description = decoderDescription;
    decoder.configure(cfg);
    decoderConfigured = true;
  }

  function decodeChunk(tsUs, bytes) {
    if (!decoderConfigured) configureDecoder();
    const now = performance.now();
    decodeCalls.set(tsUs, now);
    decodeCallLog.push([tsUs, now]);
    decoder.decode(new EncodedAudioChunk({ type: 'key', timestamp: tsUs, data: bytes }));
  }

  // Transport
  let wt = null;
  let dgWriter = null;
  const wtRtt = [];
  if (transport === 'wt') {
    const t0 = performance.now();
    wt = await connectEcho(echoUrl, echoHash);
    result.wtConnectMs = performance.now() - t0;
    dgWriter = wt.datagrams.writable.getWriter();
    const reader = wt.datagrams.readable.getReader();
    (async () => {
      for (;;) {
        const { value, done } = await reader.read();
        if (done) return;
        // 8-byte timestamp header (float64 µs), then Opus bytes.
        const dv = new DataView(value.buffer, value.byteOffset, value.byteLength);
        const tsUs = dv.getFloat64(0);
        const c = chunks.find((x) => x.tsUs === tsUs);
        if (c) wtRtt.push(performance.now() - c.wall);
        decodeChunk(tsUs, value.subarray(8));
      }
    })().catch(() => {});
  }

  let outSamples = 0;
  let decodedCum = 0;
  let preSkipTrimmed = null;
  let inPtr = 0;
  let inSamples = 0;
  const encoder = new AudioEncoder({
    output: (chunk, meta) => {
      const wall = performance.now();
      const bytes = new Uint8Array(chunk.byteLength);
      chunk.copyTo(bytes);
      const info = opusPacketInfo(bytes);
      const durUs = info.durationUs;
      const endUs = chunk.timestamp + durUs;
      // The input that completed this packet: first input whose span covers
      // the packet's last sample.
      // Matched by sample count, not timestamp: packet k ends at the
      // cumulative input sample sum(durations[0..k]); the input that
      // completed it is the first whose cumulative end reaches that. (Chromium
      // stamps outputs from its own running counter, so timestamps drift
      // from the inputs' after any input discontinuity.)
      outSamples += (durUs / 1e6) * SR;
      let lat = null;
      while (inPtr < inputs.length && inputs[inPtr].cumEnd < outSamples - 0.5) inPtr++;
      if (inPtr < inputs.length) lat = wall - inputs[inPtr].wall;
      if (meta && meta.decoderConfig && meta.decoderConfig.description && !decoderDescription) {
        const d = meta.decoderConfig.description;
        decoderDescription = d instanceof ArrayBuffer ? new Uint8Array(d.slice(0)) : new Uint8Array(d.buffer.slice(d.byteOffset, d.byteOffset + d.byteLength));
      }
      chunks.push({ tsUs: chunk.timestamp, durField: chunk.duration ?? null, bytes: bytes.length, info, wall, latMs: lat });
      if (transport === 'wt') {
        const dg = new Uint8Array(8 + bytes.length);
        new DataView(dg.buffer).setFloat64(0, chunk.timestamp);
        dg.set(bytes, 8);
        dgWriter.write(dg).catch((e) => result.errors.push(`dg write: ${e}`));
      } else {
        decodeChunk(chunk.timestamp, bytes);
      }
    },
    error: (e) => result.errors.push(`encoder: ${e}`),
  });
  const encCfg = { codec: 'opus', sampleRate: SR, numberOfChannels: 1, bitrate: 40000,
    opus: { frameDuration: frameDurationUs, ...(application ? { application } : {}) } };
  try {
    encoder.configure(encCfg);
  } catch (e) {
    result.errors.push(`encoder.configure threw: ${e}`);
    return finish();
  }

  // Context frame at which the first sample of the decoded output that
  // starts at cumulative decoded sample `decodedCum` was captured (worklet
  // capture only: MSTP AudioData has no relation to the context clock).
  // Matched in the decoded-sample domain, not by packet index: Opus has
  // 312 samples (6.5 ms) of encoder lookahead, which the decoder removes as
  // pre-skip from its first output. When it does (first output shorter than
  // the packet), decoded sample k is input sample k; when it does not, the
  // decoded stream lags the input by the pre-skip.
  function decodedCaptureFrame(nFrames) {
    if (capture !== 'worklet') return null;
    if (preSkipTrimmed === null) {
      const pkt = chunks.length ? (chunks[0].info.durationUs / 1e6) * SR : nFrames;
      preSkipTrimmed = nFrames < pkt;
      result.decoderPreSkip = { firstOutputFrames: nFrames, packetFrames: pkt, trimmed: preSkipTrimmed, assumedPreSkip: 312 };
    }
    const start = preSkipTrimmed ? decodedCum : decodedCum - 312;
    if (start < 0) return null;
    for (const inp of inputs) {
      if (inp.cumEnd > start) return inp.frame === undefined ? null : inp.frame + (start - (inp.cumEnd - inp.frames));
    }
    return null;
  }

  function encodeAudioData(ad, frame) {
    const tsUs = ad.timestamp;
    const endUs = tsUs + (ad.numberOfFrames / ad.sampleRate) * 1e6;
    inSamples += ad.numberOfFrames * (SR / ad.sampleRate);
    inputs.push({ tsUs, endUs, cumEnd: inSamples, frames: ad.numberOfFrames, frame, wall: performance.now() });
    try {
      encoder.encode(ad);
    } catch (e) {
      result.errors.push(`encode threw: ${e}`);
    }
    ad.close();
  }

  capNode.port.onmessage = (e) => {
    const m = e.data;
    if (m.t === 'onset') inOnsets.push(m.frame);
    else if (m.t === 'stats') capStats = m;
    else if (m.t === 'pcm') {
      const wall = performance.now();
      blockArrivals.push({ wall, frame: m.frame, waitFrames: m.quantumEnd - (m.frame + m.pcm.length) });
      let ad;
      try {
        ad = new AudioData({ format: 'f32-planar', sampleRate: ctx.sampleRate, numberOfFrames: m.pcm.length,
          numberOfChannels: 1, timestamp: Math.round((m.frame / ctx.sampleRate) * 1e6), data: m.pcm });
      } catch (err) {
        if (!result.errors.some((x) => x.startsWith('AudioData from worklet PCM'))) result.errors.push(`AudioData from worklet PCM: ${err}`);
        return;
      }
      encodeAudioData(ad, m.frame);
    }
  };

  // MSTP pump
  let mstpStop = false;
  let mstpDone = Promise.resolve();
  if (capture === 'mstp') {
    // eslint-disable-next-line no-undef
    const proc = new MediaStreamTrackProcessor({ track });
    const reader = proc.readable.getReader();
    mstpDone = (async () => {
      while (!mstpStop) {
        const { value: ad, done } = await reader.read();
        if (done) break;
        const wall = performance.now();
        mstpFrames.push({ wall, frames: ad.numberOfFrames, sampleRate: ad.sampleRate,
          channels: ad.numberOfChannels, format: ad.format, tsUs: ad.timestamp });
        if (ad.numberOfChannels !== 1 || ad.sampleRate !== SR) {
          // Downmix to channel 0 (the Opus config is mono). Resampling is not
          // attempted: a rate mismatch is recorded and the frame dropped.
          if (ad.sampleRate !== SR) { ad.close(); continue; }
          const pcm = new Float32Array(ad.numberOfFrames);
          ad.copyTo(pcm, { planeIndex: 0, format: 'f32-planar' });
          const mono = new AudioData({ format: 'f32-planar', sampleRate: ad.sampleRate, numberOfFrames: ad.numberOfFrames,
            numberOfChannels: 1, timestamp: ad.timestamp, data: pcm });
          ad.close();
          encodeAudioData(mono);
        } else {
          encodeAudioData(ad);
        }
      }
      reader.releaseLock();
    })().catch((e) => result.errors.push(`mstp: ${e}`));
  }

  if (source === 'synth') srcNode.start();
  const runStartWall = performance.now();
  await sleep(seconds * 1000);
  mstpStop = true;
  return finish();

  async function finish() {
    result.runWallMs = performance.now() - runStartWall;
    try { if (srcNode.stop) srcNode.stop(); } catch { /* not started */ }
    if (stream) stream.getTracks().forEach((t) => t.stop());
    try { await encoder.flush(); } catch (e) { /* closed */ }
    try { if (decoderConfigured) await decoder.flush(); } catch (e) { /* closed */ }
    await sleep(200);
    capNode.port.postMessage('stop');
    outNode.port.postMessage({ t: 'stop' });
    for (let i = 0; i < 50 && (!capStats || !outStats); i++) await sleep(20);
    result.audioContext.outputLatencyAtEnd = ctx.outputLatency ?? null;
    result.audioContext.baseLatencyAtEnd = ctx.baseLatency ?? null;
    await mstpDone;
    const r3 = (x) => (x === null || x === undefined ? null : Math.round(x * 1000) / 1000);
    result.raw = {
      inputs: inputs.map((i) => [i.tsUs, i.endUs, r3(i.wall)]),
      chunks: chunks.map((c) => [c.tsUs, c.durField, c.bytes, c.info.durationUs, c.info.mode, r3(c.wall), r3(c.latMs)]),
      decodeCalls: decodeCallLog.map(([t, w]) => [t, r3(w)]),
      decoded: decoded.map((d) => [d.tsUs, d.frames, r3(d.wall), r3(d.latMs)]),
      legend: { inputs: '[tsUs, endUs, wallMs]', chunks: '[tsUs, durationField, bytes, tocDurationUs, mode, wallMs, encLatMs]',
        decodeCalls: '[tsUs, wallMs]', decoded: '[tsUs, frames, wallMs, decLatMs]', onsets: 'AudioContext frame numbers' },
      inOnsets, outOnsets,
      transitsFrames: outStats ? outStats.transits : null,
    };
    if (wt) { try { wt.close(); } catch { /* closed */ } }
    await ctx.close().catch(() => {});

    const steady = chunks.slice(5); // skip encoder start-up
    const packetDur = steady.map((c) => c.info.durationUs);
    result.encoder = {
      config: encCfg,
      packets: chunks.length,
      packetDurationUsHist: histogram(packetDur),
      durationFieldUsHist: histogram(steady.map((c) => c.durField)),
      modeHist: histogram(steady.map((c) => `${c.info.mode}/${c.info.bandwidth}`)),
      tsDeltaUs: quantiles(steady.slice(1).map((c, i) => c.tsUs - steady[i].tsUs)),
      bytes: quantiles(steady.map((c) => c.bytes)),
      latencyMs: quantiles(steady.map((c) => c.latMs)),
      firstPacketLatencyMs: chunks.length ? chunks[0].latMs : null,
      outputIntervalMs: quantiles(steady.slice(1).map((c, i) => c.wall - steady[i].wall)),
      hadDescription: decoderDescription !== null,
    };
    const dSteady = decoded.slice(5);
    result.decoder = {
      outputs: decoded.length,
      framesHist: histogram(dSteady.map((d) => d.frames)),
      formatHist: histogram(dSteady.map((d) => d.format)),
      latencyMs: quantiles(dSteady.map((d) => d.latMs)),
      decodeCalls: decodeCallLog.length,
      firstOutputTsShiftUs: decoded.length && decodeCallLog.length ? decoded[0].tsUs - decodeCallLog[0][0] : null,
      secondOutputTsShiftUs: decoded.length > 1 && decodeCallLog.length > 1 ? decoded[1].tsUs - decodeCallLog[1][0] : null,
    };
    if (capture === 'worklet') {
      result.workletCapture = {
        blocks: blockArrivals.length,
        blockFrames: Math.round(0.01 * ctx.sampleRate),
        arrivalIntervalMs: quantiles(blockArrivals.slice(1).map((b, i) => b.wall - blockArrivals[i].wall)),
        quantumAlignmentWaitMs: quantiles(blockArrivals.map((b) => (b.waitFrames / ctx.sampleRate) * 1000)),
        contiguous: blockArrivals.every((b, i) => i === 0 || b.frame - blockArrivals[i - 1].frame === Math.round(0.01 * ctx.sampleRate)),
      };
    } else {
      result.mstp = {
        frames: mstpFrames.length,
        framesPerAudioDataHist: histogram(mstpFrames.map((f) => f.frames)),
        sampleRateHist: histogram(mstpFrames.map((f) => f.sampleRate)),
        channelsHist: histogram(mstpFrames.map((f) => f.channels)),
        formatHist: histogram(mstpFrames.map((f) => f.format)),
        arrivalIntervalMs: quantiles(mstpFrames.slice(1).map((f, i) => f.wall - mstpFrames[i].wall)),
      };
    }
    result.captureWorkletStats = capStats;
    result.output = outStats ? {
      underruns: outStats.underruns,
      depthMs: quantiles(outStats.depths.slice(50).map((d) => (d / ctx.sampleRate) * 1000)),
      jitterTargetMs,
      // Capture-node → playout-ready transit per packet (worklet capture):
      // the smallest fixed playout delay that would play that packet on
      // time. Includes packetisation (the frame duration), main-thread hops,
      // encode, loopback, decode and the output quantum boundary.
      transitMs: quantiles(outStats.transits.slice(5).map((f) => (f / ctx.sampleRate) * 1000)),
      transitP99Ms: (() => { const v = outStats.transits.slice(5).map((f) => (f / ctx.sampleRate) * 1000).sort((a, b) => a - b); return v.length ? v[Math.ceil(0.99 * v.length) - 1] : null; })(),
    } : null;
    result.onsets = { input: inOnsets.length, output: outOnsets.length };
    // In-graph latency: onset as seen by the capture-side worklet → onset as
    // rendered by the output worklet, both in context frames. Excludes the
    // device input and output latency (none exists headless).
    result.inGraphLatencyMs = quantiles(pairOnsets(inOnsets, outOnsets, ctx.sampleRate).slice(2));
    if (transport === 'wt') result.wtRttMs = quantiles(wtRtt.slice(5));
    return result;
  }
}

// docs/80 §7 / MV0 (b): an echo-cancellation probe for real devices. It plays
// the tone-burst train through an AudioWorklet (the WebAudio path the voice
// playback uses, not a WebRTC/<audio> sink) while capturing the mic with
// echoCancellation on or off, and reports the mic level inside the bursts vs
// between them. Headless runs only prove the code path executes.
async function aec({ echoCancellation = true, seconds = 10, outputGain = 0.5 } = {}) {
  const ctx = new AudioContext({ latencyHint: 'interactive' });
  if (ctx.state !== 'running') await ctx.resume();
  const blobUrl = URL.createObjectURL(new Blob([CAPTURE_WORKLET], { type: 'text/javascript' }));
  await ctx.audioWorklet.addModule(blobUrl);
  const stream = await navigator.mediaDevices.getUserMedia({
    audio: { echoCancellation, noiseSuppression: false, autoGainControl: false, channelCount: 1 },
  });
  const track = stream.getAudioTracks()[0];
  const out = new AudioWorkletNode(ctx, 'mv0-output', { numberOfInputs: 0, numberOfOutputs: 1, outputChannelCount: [1],
    processorOptions: { targetFrames: 0, threshold: 2 } });
  const g = ctx.createGain();
  g.gain.value = outputGain;
  out.connect(g).connect(ctx.destination);
  // Feed the playback worklet a burst train by hand (as decoded PCM would be).
  const period = Math.round(BURST_PERIOD_S * 2 * ctx.sampleRate); // 500 ms: 100 ms on, 400 off
  const on = Math.round(0.1 * ctx.sampleRate);
  const total = Math.round(seconds * ctx.sampleRate);
  const pcm = new Float32Array(total);
  for (let i = 0; i < total; i++) {
    const p = i % period;
    // A voice-band chirp-ish burst: 300 Hz → 3 kHz across the 100 ms.
    if (p < on) pcm[i] = 0.6 * Math.sin(2 * Math.PI * (300 + (2700 * p) / on) * (p / ctx.sampleRate));
  }
  const playStartFrame = Math.round(ctx.currentTime * ctx.sampleRate) + Math.round(0.1 * ctx.sampleRate);
  out.port.postMessage({ t: 'pcm', pcm }, [pcm.buffer]);
  // Mic level per 10 ms block, in context frames.
  const analyser = ctx.createMediaStreamSource(stream);
  const recNode = new AudioWorkletNode(ctx, 'mv0-capture', { numberOfInputs: 1, numberOfOutputs: 0,
    processorOptions: { block: Math.round(0.01 * ctx.sampleRate), emit: true, threshold: 2 } });
  analyser.connect(recNode);
  const blocks = [];
  recNode.port.onmessage = (e) => {
    if (e.data.t !== 'pcm') return;
    let s = 0;
    for (const v of e.data.pcm) s += v * v;
    blocks.push({ frame: e.data.frame, rms: Math.sqrt(s / e.data.pcm.length) });
  };
  await sleep(seconds * 1000 + 300);
  stream.getTracks().forEach((t) => t.stop());
  await ctx.close();
  // Classify blocks as in-burst / between-burst by playback phase, allowing
  // up to 150 ms for output+input device latency by skipping the edges.
  const inBurst = [];
  const gap = [];
  for (const b of blocks) {
    const p = (((b.frame - playStartFrame) % period) + period) % period;
    const ms = (p / ctx.sampleRate) * 1000;
    if (ms >= 20 && ms < 100) inBurst.push(b.rms);
    else if (ms >= 300 && ms < 480) gap.push(b.rms);
  }
  const med = (xs) => quantiles(xs).median ?? null;
  const db = (x) => (x > 0 ? 20 * Math.log10(x) : -Infinity);
  const r = {
    echoCancellation,
    trackSettings: track.getSettings(),
    sampleRate: ctx.sampleRate,
    baseLatency: ctx.baseLatency ?? null,
    outputLatency: ctx.outputLatency ?? null,
    micRmsInBurstMedian: med(inBurst),
    micRmsBetweenBurstsMedian: med(gap),
  };
  r.echoOverFloorDb = db(r.micRmsInBurstMedian) - db(r.micRmsBetweenBurstsMedian);
  return r;
}

// Offline alignment probe: where does an impulse fed at input sample S come
// out of encode→decode, and with which packet? Settles how much Opus
// lookahead (312 samples = 6.5 ms at 48 kHz) the browser's encoder/decoder
// pair adds and whether the decoder trims the pre-skip. Independent of any
// clock: it is all sample indices.
async function alignment({ frameDurationUs = 10000, impulseAt = 4800 + 123, blocks = 40, application = null } = {}) {
  const block = 480;
  const packets = [];
  const enc = new AudioEncoder({
    output: (c) => { const b = new Uint8Array(c.byteLength); c.copyTo(b); packets.push({ ts: c.timestamp, b, inputsSoFar: fed }); },
    error: (e) => { throw e; },
  });
  enc.configure({ codec: 'opus', sampleRate: SR, numberOfChannels: 1, bitrate: 40000, opus: { frameDuration: frameDurationUs, ...(application ? { application } : {}) } });
  let fed = 0;
  for (let k = 0; k < blocks; k++) {
    const pcm = new Float32Array(block);
    for (let i = 0; i < block; i++) {
      const n = k * block + i;
      // A 1 ms 4 kHz burst: an impulse alone is mangled by a 40 kbit/s codec.
      if (n >= impulseAt && n < impulseAt + 48) pcm[i] = 0.8 * Math.sin((2 * Math.PI * 4000 * (n - impulseAt)) / SR);
    }
    enc.encode(new AudioData({ format: 'f32-planar', sampleRate: SR, numberOfFrames: block, numberOfChannels: 1, timestamp: Math.round((k * block / SR) * 1e6), data: pcm }));
    fed = k + 1;
    // Not flush(): Chromium's flush() pads and emits the encoder's lookahead
    // as an extra packet every time (measured: 80 packets for 40 blocks).
    // A pause lets an immediately-ready packet come out before the next
    // block, so inputsSoFar is exact unless the encoder holds a packet back.
    await sleep(8);
  }
  await enc.flush();
  enc.close();
  const outs = [];
  const dec = new AudioDecoder({
    output: (ad) => { const pcm = new Float32Array(ad.numberOfFrames); ad.copyTo(pcm, { planeIndex: 0, format: 'f32-planar' }); outs.push(pcm); ad.close(); },
    error: (e) => { throw e; },
  });
  dec.configure({ codec: 'opus', sampleRate: SR, numberOfChannels: 1 });
  for (const p of packets) dec.decode(new EncodedAudioChunk({ type: 'key', timestamp: p.ts, data: p.b }));
  await dec.flush();
  dec.close();
  const all = new Float32Array(outs.reduce((a, o) => a + o.length, 0));
  let off = 0;
  const outStart = [];
  for (const o of outs) { outStart.push(off); all.set(o, off); off += o.length; }
  let peakAt = -1;
  for (let i = 0; i < all.length; i++) if (Math.abs(all[i]) > 0.2) { peakAt = i; break; }
  let outIdx = -1;
  for (let j = 0; j < outs.length; j++) if (peakAt >= outStart[j] && peakAt < outStart[j] + outs[j].length) outIdx = j;
  const pkt = outIdx >= 0 ? packets[outIdx] : null;
  return {
    frameDurationUs, application, impulseAt, inputBlockOfImpulse: Math.floor(impulseAt / block),
    decodedOnsetAt: peakAt, decodedMinusInputSamples: peakAt - impulseAt,
    firstOutputFrames: outs.length ? outs[0].length : null, packets: packets.length, outputs: outs.length,
    // How many 480-sample input blocks had been handed to encode() when the
    // packet carrying the impulse came out, vs the minimum possible (the
    // block holding the impulse's end, rounded up to a whole packet).
    blocksFedWhenImpulsePacketEmitted: pkt ? pkt.inputsSoFar : null,
    minBlocksPossible: Math.ceil((impulseAt + 48) / (frameDurationUs / 1e6 * SR)) * (frameDurationUs / 1e6 * SR) / block,
  };
}

window.mv0 = { run, aec, alignment, opusPacketInfo };
window.mv0Ready = true;
