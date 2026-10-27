// R80 NS0 — what each browser engine does with the RNNoise module
// (docs/81 §12). Self-contained, imports nothing from gawk-app. Driven by
// run-browser.mjs; every probe returns plain JSON.
//
//   caps         wasm SIMD, AudioWorklet, MSTP on Window
//   cpuMain      ms per 480-sample frame on the main thread, per variant
//   worklet      a compiled WebAssembly.Module handed to an AudioWorklet
//                (processorOptions, then the port), instantiated there,
//                benchmarked there and run in real time for a few seconds
//   rates        AudioContext at 44.1 / 48 kHz / default + the microphone
//   constraints  noiseSuppression off and back on with applyConstraints
//   record       (Chromium) the fake microphone, with the browser's noise
//                suppression on or off (auto gain on, as in production), as
//                f32 for offline analysis; optionally switching suppression
//                with applyConstraints part-way through
//   recordPair   (Chromium) two tracks on the device at once, NS off and on
//   recordReopen (Chromium) stop() a track and open one with other processing

const FRAME = 480;

function instantiate(module) {
  const e = new WebAssembly.Instance(module, {}).exports;
  e._initialize();
  const st = e.rnnoise_create(0);
  const inP = e.malloc(FRAME * 4), outP = e.malloc(FRAME * 4);
  const heap = new Float32Array(e.memory.buffer);
  return (input, output) => {
    for (let i = 0; i < FRAME; i++) heap[(inP >> 2) + i] = input[i] * 32768;
    const p = e.rnnoise_process_frame(st, outP, inP);
    for (let i = 0; i < FRAME; i++) output[i] = heap[(outP >> 2) + i] / 32768;
    return p;
  };
}

function testSignal(frames) {
  // 1 kHz tone bursts in white noise: deterministic, speech-band, nonzero VAD.
  const x = new Float32Array(frames * FRAME);
  let seed = 1;
  for (let i = 0; i < x.length; i++) {
    seed = (seed * 1103515245 + 12345) >>> 0;
    const tone = (Math.floor(i / 9600) % 2) * 0.1 * Math.sin((2 * Math.PI * 1000 * i) / 48000);
    x[i] = tone + 0.01 * (seed / 2 ** 31 - 1);
  }
  return x;
}

async function fetchModule(variant) {
  const t0 = performance.now();
  const bytes = await (await fetch(`wasm/${variant}.wasm`)).arrayBuffer();
  const t1 = performance.now();
  const module = await WebAssembly.compile(bytes);
  return { module, bytes: bytes.byteLength, fetchMs: t1 - t0, compileMs: performance.now() - t1 };
}

const SIMD_PROBE = new Uint8Array([0, 97, 115, 109, 1, 0, 0, 0, 1, 5, 1, 96, 0, 1, 123, 3, 2, 1, 0, 10, 10, 1, 8, 0, 65, 0, 253, 15, 253, 98, 11]);

async function caps() {
  return {
    userAgent: navigator.userAgent,
    wasmSimd: WebAssembly.validate(SIMD_PROBE),
    audioWorklet: typeof AudioWorkletNode === 'function',
    mstpOnWindow: typeof globalThis.MediaStreamTrackProcessor === 'function',
    supportedConstraints: Object.keys(navigator.mediaDevices?.getSupportedConstraints?.() ?? {}).sort(),
    defaultContextRate: await (async () => { const c = new AudioContext(); const r = c.sampleRate; await c.close(); return r; })(),
  };
}

async function cpuMain({ variants, frames = 3000 }) {
  const x = testSignal(frames);
  const out = new Float32Array(FRAME);
  const res = {};
  for (const v of variants) {
    try {
      const m = await fetchModule(v);
      const t0 = performance.now();
      const proc = instantiate(m.module);
      const instMs = performance.now() - t0;
      for (let f = 0; f < 200; f++) proc(x.subarray(f * FRAME, (f + 1) * FRAME), out); // warm-up
      const reps = [];
      for (let r = 0; r < 3; r++) {
        const t = performance.now();
        for (let f = 0; f < frames; f++) proc(x.subarray(f * FRAME, (f + 1) * FRAME), out);
        reps.push((performance.now() - t) / frames);
      }
      res[v] = { msPerFrame: Math.min(...reps), reps, bytes: m.bytes, fetchMs: m.fetchMs, compileMs: m.compileMs, instantiateMs: instMs };
    } catch (e) {
      res[v] = { error: String(e) };
    }
  }
  return res;
}

// The worklet: instantiates whatever module it is given, benchmarks 1000
// frames with Date.now() (the one clock every worklet scope has), then
// denoises its input in real time, rebuffering 128-frame quanta into 480.
const PROCESSOR = `
const FRAME = 480;
class NsProbe extends AudioWorkletProcessor {
  constructor(opts) {
    super();
    this.frames = 0; this.quanta = 0; this.maxProbe = 0; this.prob = 0;
    this.buf = new Float32Array(FRAME); this.fill = 0; this.out = new Float32Array(FRAME);
    const start = (module, via) => {
      try {
        const t0 = Date.now();
        const e = new WebAssembly.Instance(module, {}).exports;
        e._initialize();
        const st = e.rnnoise_create(0);
        const inP = e.malloc(FRAME * 4), outP = e.malloc(FRAME * 4);
        const heap = new Float32Array(e.memory.buffer);
        this.proc = (input, output) => {
          for (let i = 0; i < FRAME; i++) heap[(inP >> 2) + i] = input[i] * 32768;
          const p = e.rnnoise_process_frame(st, outP, inP);
          for (let i = 0; i < FRAME; i++) output[i] = heap[(outP >> 2) + i] / 32768;
          return p;
        };
        const instMs = Date.now() - t0;
        const x = new Float32Array(FRAME); const y = new Float32Array(FRAME);
        let seed = 1;
        const t1 = Date.now();
        for (let f = 0; f < 1000; f++) {
          for (let i = 0; i < FRAME; i++) { seed = (seed * 1103515245 + 12345) >>> 0; x[i] = 0.01 * (seed / 2 ** 31 - 1); }
          this.proc(x, y);
        }
        this.port.postMessage({ kind: 'ready', via, instMs, benchMsPerFrame: (Date.now() - t1) / 1000, sampleRate });
      } catch (err) {
        this.port.postMessage({ kind: 'error', via, error: String(err) });
      }
    };
    const m = opts.processorOptions && opts.processorOptions.module;
    if (m) start(m, 'processorOptions');
    else this.port.postMessage({ kind: 'noOptionsModule', type: typeof m });
    this.port.onmessage = (ev) => {
      if (ev.data && ev.data.module) start(ev.data.module, 'port');
      if (ev.data === 'stats') this.port.postMessage({ kind: 'stats', frames: this.frames, quanta: this.quanta, prob: this.prob, renderFrame: currentFrame, workletMs: Date.now() });
    };
  }
  process(inputs, outputs) {
    this.quanta++;
    const ch = inputs[0] && inputs[0][0];
    if (!ch || !this.proc) return true;
    let off = 0;
    while (off < ch.length) {
      const take = Math.min(FRAME - this.fill, ch.length - off);
      this.buf.set(ch.subarray(off, off + take), this.fill);
      this.fill += take; off += take;
      if (this.fill === FRAME) { this.prob = this.proc(this.buf, this.out); this.frames++; this.fill = 0; }
    }
    return true;
  }
}
registerProcessor('ns-probe', NsProbe);
`;

async function worklet({ variant, via, seconds = 4, contextRate }) {
  const res = { variant, via };
  const ctx = new AudioContext(contextRate ? { sampleRate: contextRate } : {});
  try {
    await ctx.resume();
    res.contextRate = ctx.sampleRate;
    const url = URL.createObjectURL(new Blob([PROCESSOR], { type: 'application/javascript' }));
    await ctx.audioWorklet.addModule(url);
    const { module } = await fetchModule(variant);
    const msgs = [];
    let node;
    const ready = new Promise((resolve) => {
      const opts = { numberOfInputs: 1, numberOfOutputs: 1, outputChannelCount: [1] };
      if (via === 'processorOptions') opts.processorOptions = { module };
      try {
        node = new AudioWorkletNode(ctx, 'ns-probe', opts);
      } catch (e) {
        resolve({ kind: 'error', via, error: `construct: ${e}` });
        return;
      }
      node.port.onmessage = (ev) => {
        msgs.push(ev.data);
        if (ev.data.kind === 'ready' || ev.data.kind === 'error') resolve(ev.data);
      };
      if (via === 'port') {
        try { node.port.postMessage({ module }); } catch (e) { resolve({ kind: 'error', via, error: `postMessage: ${e}` }); }
      }
      setTimeout(() => resolve({ kind: 'timeout', msgs }), 5000);
    });
    const r = await ready;
    Object.assign(res, r);
    if (r.kind !== 'ready') return res;
    // Real time: a noise source through the node into a muted gain.
    const len = ctx.sampleRate * 2;
    const buf = ctx.createBuffer(1, len, ctx.sampleRate);
    const d = buf.getChannelData(0);
    for (let i = 0; i < len; i++) d[i] = 0.02 * (Math.random() * 2 - 1);
    const src = ctx.createBufferSource();
    src.buffer = buf; src.loop = true;
    const sink = ctx.createGain(); sink.gain.value = 0;
    src.connect(node); node.connect(sink); sink.connect(ctx.destination);
    const stats = () => new Promise((resolve) => {
      const prev = node.port.onmessage;
      node.port.onmessage = (ev) => { if (ev.data.kind === 'stats') { node.port.onmessage = prev; resolve(ev.data); } };
      node.port.postMessage('stats');
    });
    src.start();
    // Real time, judged inside the worklet: how far the render clock
    // (currentFrame) advanced against the worklet's own Date.now() over a
    // long window, after a second of start-up. Main-thread wall time around a
    // port round trip skews short windows (it read WebKit above real time).
    await new Promise((r2) => setTimeout(r2, 1000));
    const s0 = await stats();
    await new Promise((r2) => setTimeout(r2, seconds * 1000));
    const s1 = await stats();
    const ms = s1.workletMs - s0.workletMs;
    res.realtime = {
      workletWindowMs: ms,
      renderRateHz: ((s1.renderFrame - s0.renderFrame) / ms) * 1000,
      contextRateHz: ctx.sampleRate,
      // rendered audio ÷ elapsed time: 1.0 keeps real time, < 1 falls behind
      renderRatio: ((s1.renderFrame - s0.renderFrame) / ms) * 1000 / ctx.sampleRate,
      framesPerSecond: ((s1.frames - s0.frames) / ms) * 1000,
      expectedFramesPerSecond: ctx.sampleRate / FRAME,
    };
    src.stop();
    return res;
  } catch (e) {
    res.error = String(e);
    return res;
  } finally {
    await ctx.close().catch(() => {});
  }
}

async function rates() {
  const out = [];
  const stream = await navigator.mediaDevices.getUserMedia({ audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true, channelCount: 1 } });
  const track = stream.getAudioTracks()[0];
  const settings = track.getSettings();
  for (const rate of [44100, 48000, null]) {
    const r = { requested: rate };
    let ctx;
    try {
      ctx = new AudioContext(rate ? { sampleRate: rate } : {});
      r.contextRate = ctx.sampleRate;
      const src = ctx.createMediaStreamSource(stream);
      r.mediaStreamSource = 'ok';
      // Is anything flowing? An analyser's peak over 500 ms.
      const an = ctx.createAnalyser();
      src.connect(an);
      await ctx.resume();
      await new Promise((r2) => setTimeout(r2, 500));
      const a = new Float32Array(an.fftSize);
      an.getFloatTimeDomainData(a);
      r.peak = a.reduce((m, v) => Math.max(m, Math.abs(v)), 0);
    } catch (e) {
      r.mediaStreamSource = `${e.name}: ${e.message}`;
    } finally {
      await ctx?.close().catch(() => {});
    }
    out.push(r);
  }
  track.stop();
  return { trackSampleRate: settings.sampleRate ?? null, contexts: out };
}

async function constraints() {
  const pick = (s) => ({ noiseSuppression: s.noiseSuppression, echoCancellation: s.echoCancellation, autoGainControl: s.autoGainControl, voiceIsolation: s.voiceIsolation, sampleRate: s.sampleRate });
  const res = {};
  const stream = await navigator.mediaDevices.getUserMedia({ audio: { echoCancellation: true, noiseSuppression: false, autoGainControl: true, channelCount: 1 } });
  const track = stream.getAudioTracks()[0];
  res.openedWithNsOff = pick(track.getSettings());
  try {
    await track.applyConstraints({ echoCancellation: true, noiseSuppression: true, autoGainControl: true, channelCount: 1 });
    res.afterApplyNsOn = pick(track.getSettings());
    await track.applyConstraints({ echoCancellation: true, noiseSuppression: false, autoGainControl: true, channelCount: 1 });
    res.afterApplyNsOff = pick(track.getSettings());
  } catch (e) {
    res.applyError = `${e.name}: ${e.message}`;
  }
  track.stop();
  return res;
}

// Chromium: the fake microphone (a WAV the driver chose) through the
// browser's processing, captured with MSTP, returned as base64 f32.
// applyAtS: switch noiseSuppression to `applyTo` with applyConstraints that
// many seconds in, to see whether the audio (not getSettings()) changes.
async function record({ noiseSuppression, autoGainControl = true, seconds, applyAtS = null, applyTo = null }) {
  const stream = await navigator.mediaDevices.getUserMedia({ audio: { echoCancellation: true, noiseSuppression, autoGainControl, channelCount: 1 } });
  const track = stream.getAudioTracks()[0];
  const settings = track.getSettings();
  const reader = new MediaStreamTrackProcessor({ track }).readable.getReader();
  const chunks = [];
  let n = 0, rate = 0;
  const t0 = performance.now();
  const end = t0 + seconds * 1000;
  let applied = null;
  while (performance.now() < end) {
    if (applyAtS !== null && applied === null && performance.now() - t0 >= applyAtS * 1000) {
      applied = { atSample: n };
      try {
        await track.applyConstraints({ echoCancellation: true, noiseSuppression: applyTo, autoGainControl, channelCount: 1 });
        applied.settingsAfter = track.getSettings().noiseSuppression;
      } catch (e) {
        applied.error = `${e.name}: ${e.message}`;
      }
    }
    const { value } = await reader.read();
    if (!value) break;
    rate = value.sampleRate;
    const p = new Float32Array(value.numberOfFrames);
    value.copyTo(p, { planeIndex: 0, format: 'f32-planar' });
    chunks.push(p); n += p.length;
    value.close();
  }
  track.stop();
  const all = new Float32Array(n);
  let o = 0;
  for (const c of chunks) { all.set(c, o); o += c.length; }
  const u8 = new Uint8Array(all.buffer);
  let s = '';
  for (let i = 0; i < u8.length; i += 0x8000) s += String.fromCharCode(...u8.subarray(i, i + 0x8000));
  return { sampleRate: rate, settings: { noiseSuppression: settings.noiseSuppression, echoCancellation: settings.echoCancellation, autoGainControl: settings.autoGainControl }, samples: n, applied, f32: btoa(s) };
}

// Chromium: two tracks open on the same device at once, the second with
// other processing (as D11's re-open has them, briefly). Does each get its
// own processing, and does opening the second change the first? Both are
// read for the whole run and returned as base64 f32.
async function recordPair({ first, second, seconds }) {
  const open = (o) => navigator.mediaDevices.getUserMedia({ audio: { echoCancellation: true, channelCount: 1, ...o } });
  const a = (await open(first)).getAudioTracks()[0];
  const b = (await open(second)).getAudioTracks()[0];
  const read = async (track) => {
    const reader = new MediaStreamTrackProcessor({ track }).readable.getReader();
    const chunks = [];
    let n = 0;
    const end = performance.now() + seconds * 1000;
    while (performance.now() < end) {
      const { value } = await reader.read();
      if (!value) break;
      const p = new Float32Array(value.numberOfFrames);
      value.copyTo(p, { planeIndex: 0, format: 'f32-planar' });
      chunks.push(p); n += p.length;
      value.close();
    }
    track.stop();
    const all = new Float32Array(n);
    let o = 0;
    for (const c of chunks) { all.set(c, o); o += c.length; }
    const u8 = new Uint8Array(all.buffer);
    let s = '';
    for (let i = 0; i < u8.length; i += 0x8000) s += String.fromCharCode(...u8.subarray(i, i + 0x8000));
    return { samples: n, f32: btoa(s) };
  };
  const sa = a.getSettings(), sb = b.getSettings();
  const [ra, rb] = await Promise.all([read(a), read(b)]);
  return {
    first: { ...ra, settings: { noiseSuppression: sa.noiseSuppression, autoGainControl: sa.autoGainControl } },
    second: { ...rb, settings: { noiseSuppression: sb.noiseSuppression, autoGainControl: sb.autoGainControl } },
  };
}

// Chromium: D11's re-open in one page. Read a track opened with `first`
// for firstSeconds, stop() it, call getUserMedia with `second` at once,
// and return the new track's audio: does it get the new processing?
async function recordReopen({ first, second, firstSeconds, seconds }) {
  const open = (o) => navigator.mediaDevices.getUserMedia({ audio: { echoCancellation: true, channelCount: 1, ...o } });
  const a = (await open(first)).getAudioTracks()[0];
  const ra = a.getSettings();
  const reader = new MediaStreamTrackProcessor({ track: a }).readable.getReader();
  const end = performance.now() + firstSeconds * 1000;
  let n = 0;
  while (performance.now() < end) {
    const { value } = await reader.read();
    if (!value) break;
    n += value.numberOfFrames;
    value.close();
  }
  reader.releaseLock();
  a.stop();
  const t0 = performance.now();
  const b = (await open(second)).getAudioTracks()[0];
  const reopenMs = performance.now() - t0;
  const sb = b.getSettings();
  const rb = await (async () => {
    const r = new MediaStreamTrackProcessor({ track: b }).readable.getReader();
    const chunks = [];
    let m = 0;
    const stop = performance.now() + seconds * 1000;
    while (performance.now() < stop) {
      const { value } = await r.read();
      if (!value) break;
      const p = new Float32Array(value.numberOfFrames);
      value.copyTo(p, { planeIndex: 0, format: 'f32-planar' });
      chunks.push(p); m += p.length;
      value.close();
    }
    b.stop();
    const all = new Float32Array(m);
    let o = 0;
    for (const c of chunks) { all.set(c, o); o += c.length; }
    const u8 = new Uint8Array(all.buffer);
    let s = '';
    for (let i = 0; i < u8.length; i += 0x8000) s += String.fromCharCode(...u8.subarray(i, i + 0x8000));
    return { samples: m, f32: btoa(s) };
  })();
  return {
    first: { samples: n, settings: { noiseSuppression: ra.noiseSuppression, autoGainControl: ra.autoGainControl } },
    second: { ...rb, settings: { noiseSuppression: sb.noiseSuppression, autoGainControl: sb.autoGainControl } },
    reopenMs,
  };
}

window.ns0 = { caps, cpuMain, worklet, rates, constraints, record, recordPair, recordReopen };
window.ns0Ready = true;
