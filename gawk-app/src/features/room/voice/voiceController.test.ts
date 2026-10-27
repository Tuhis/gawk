// The voice client's lifecycle against fakes (docs/80 D5, D17–D19, §11.1
// C4): ticket → connect → hello flags → frames out and in → the close codes'
// three behaviours, and the teardown of everything on every failure path.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  CLOSE_CODE_SERVER_DRAINING,
  CLOSE_CODE_VOICE_REMOVED,
  CLOSE_CODE_VOICE_UNAVAILABLE,
  ROOM_REJECT_LIMIT,
  ROOM_REJECT_UNAVAILABLE,
  ROOM_REJECT_UNSUPPORTED,
  VOICE_CONTROL_FLAG_SELF_DEAF,
  VOICE_CONTROL_FLAG_SELF_MUTE,
  VOICE_HELLO_FLAG_LISTEN_ONLY,
  VOICE_HELLO_FLAG_SELF_DEAF,
  VOICE_HELLO_FLAG_SELF_MUTE,
  VOICE_STATE_FLAG_CONNECTED,
  VOICE_STATE_FLAG_SERVER_MUTE,
  VOICE_STATE_FLAG_SUPPRESSED,
  type VoiceFrameHeader,
} from '../../../transport/wire';
import {
  MAX_REJOIN_ATTEMPTS,
  REJOIN_DELAYS_MS,
  TICKET_TIMEOUT_MS,
  VoiceController,
  rejoinDelayMs,
  type RoomVoiceReadiness,
  type VoiceAudioPrefs,
  type VoiceViewState,
} from './voiceController';
import type { NoiseMode } from './voicePrefs';
import { VoiceError, type VoiceSessionCallbacks, type VoiceSessionOptions } from './voiceSession';
import type { VoiceCaptureCallbacks } from './voiceCapture';
import type { VoicePlaybackCallbacks } from './voicePlayback';
import { VAD_THRESHOLD_RMS } from './talkGate';

// When set, every new FakeSession's start() waits on it (a dial in flight).
let sessionHold: Promise<void> | null = null;

class FakeSession {
  opts: VoiceSessionOptions;
  cb: VoiceSessionCallbacks;
  frames: { h: VoiceFrameHeader; p: Uint8Array }[] = [];
  controls: number[] = [];
  closed = 0;
  startResult: Promise<void>;
  constructor(opts: VoiceSessionOptions, cb: VoiceSessionCallbacks, fail: Error | null) {
    this.opts = opts;
    this.cb = cb;
    this.startResult = fail ? Promise.reject(fail) : (sessionHold ?? Promise.resolve());
    this.startResult.catch(() => {});
  }
  // The real session has no control stream until start() resolves and
  // drops a VoiceControl sent before that (voiceSession.ts writeRecord).
  open = false;
  start() {
    return this.startResult.then(() => {
      this.open = true;
    });
  }
  sendFrame(h: VoiceFrameHeader, p: Uint8Array) {
    this.frames.push({ h, p });
  }
  sendControl(f: number) {
    if (this.open) this.controls.push(f);
  }
  close() {
    this.closed++;
  }
}

class FakeCapture {
  cb: VoiceCaptureCallbacks;
  noise: NoiseMode;
  stopped = 0;
  started = 0;
  modes: NoiseMode[] = [];
  constructor(cb: VoiceCaptureCallbacks, noise: NoiseMode) {
    this.cb = cb;
    this.noise = noise;
  }
  async setNoise(mode: NoiseMode) {
    this.modes.push(mode);
  }
  async start() {
    this.started++;
    if (captureFail) throw captureFail;
    if (captureHold) await captureHold;
  }
  stop() {
    this.stopped++;
  }
}

class FakePlayback {
  cb: VoicePlaybackCallbacks;
  frames: VoiceFrameHeader[] = [];
  self: [Pick<VoiceFrameHeader, 'end' | 'seq'>, number][] = [];
  selfStops = 0;
  volumes: [number, number][] = [];
  disposed = 0;
  constructor(cb: VoicePlaybackCallbacks) {
    this.cb = cb;
  }
  handleFrame(h: VoiceFrameHeader) {
    this.frames.push(h);
  }
  handleSelfFrame(h: Pick<VoiceFrameHeader, 'end' | 'seq'>, p: Uint8Array) {
    this.self.push([h, p.length]);
  }
  stopSelf() {
    this.selfStops++;
  }
  setSpeakerVolume(s: number, v: number) {
    this.volumes.push([s, v]);
  }
  dispose() {
    this.disposed++;
  }
}

let readiness: RoomVoiceReadiness;
let roomListeners: (() => void)[];
let joins: number;
let sendOk: boolean;
let sessions: FakeSession[];
let captures: FakeCapture[];
let playbacks: FakePlayback[];
let sessionFail: Error | null;
let captureFail: Error | null;
let captureHold: Promise<void> | null;
let busMuted: boolean[];
let clock: number;
let states: VoiceViewState[];
let savedLevels: number[];

function make(mode: 'vad' | 'ptt' = 'vad', audio?: Partial<VoiceAudioPrefs>) {
  return new VoiceController(
    {
      sendVoiceJoin: () => {
        joins++;
        return sendOk;
      },
      roomReadiness: () => readiness,
      subscribeRoom: (fn) => {
        roomListeners.push(fn);
        return () => {
          roomListeners = roomListeners.filter((f) => f !== fn);
        };
      },
      webTransportInit: () => ({ serverCertificateHashes: [{ algorithm: 'sha-256', value: new Uint8Array(32) }] }),
      createSession: (opts, cb) => {
        const s = new FakeSession(opts, cb, sessionFail);
        sessions.push(s);
        return s;
      },
      createCapture: (cb, noise) => {
        const c = new FakeCapture(cb, noise);
        captures.push(c);
        return c;
      },
      captureContext: () => null,
      createPlayback: (cb) => {
        const p = new FakePlayback(cb);
        playbacks.push(p);
        return p;
      },
      setVoiceMuted: (m) => busMuted.push(m),
      now: () => clock,
      onChange: (s) => states.push(s),
      saveSpeechLevel: (db) => savedLevels.push(db),
    },
    mode,
    audio,
  );
}

const roomChanged = () => roomListeners.forEach((f) => f());
const flush = () => vi.advanceTimersByTimeAsync(0);
const TICKET = new Uint8Array([7, 7, 7]);
const LOUD = VAD_THRESHOLD_RMS * 5;

async function connected(c: VoiceController, listenOnly = false) {
  void c.join(listenOnly);
  await flush();
  c.onTicket('https://bridge.test/voice', TICKET);
  await flush();
  const s = sessions[sessions.length - 1];
  s.cb.onState({ flags: VOICE_STATE_FLAG_CONNECTED, session: 42, name: 'tuhis [web]' });
  return s;
}

beforeEach(() => {
  vi.useFakeTimers();
  readiness = 'ready';
  roomListeners = [];
  joins = 0;
  sendOk = true;
  sessions = [];
  captures = [];
  playbacks = [];
  sessionFail = null;
  sessionHold = null;
  captureFail = null;
  captureHold = null;
  busMuted = [];
  clock = 0;
  states = [];
  savedLevels = [];
});
afterEach(() => vi.useRealTimers());

describe('VoiceController join', () => {
  it('opens the mic BEFORE asking for a ticket, then connects with the ticket and the pins', async () => {
    let release!: () => void;
    captureHold = new Promise<void>((r) => (release = r));
    const c = make();
    void c.join(false);
    await flush();
    expect(c.getState().phase).toBe('joining');
    expect(captures).toHaveLength(1);
    expect(joins).toBe(0); // the prompt may outlast a 30 s ticket
    release();
    await flush();
    expect(joins).toBe(1);
    c.onTicket('https://bridge.test/voice', TICKET);
    await flush();
    expect(sessions[0].opts.url).toBe('https://bridge.test/voice');
    expect(sessions[0].opts.ticket).toEqual(TICKET);
    expect(sessions[0].opts.helloFlags).toBe(0);
    expect(sessions[0].opts.init.serverCertificateHashes).toHaveLength(1);
    expect(c.getState().phase).toBe('connected');
    expect(playbacks).toHaveLength(1);
    c.dispose();
  });

  it('listen-only never opens the microphone and says so in the hello', async () => {
    const c = make();
    const s = await connected(c, true);
    expect(captures).toHaveLength(0);
    expect(s.opts.helloFlags).toBe(VOICE_HELLO_FLAG_LISTEN_ONLY);
    expect(c.getState()).toMatchObject({ phase: 'connected', listenOnly: true, mumbleConnected: true, ownSession: 42 });
    c.dispose();
  });

  it('joins muted / deafened as the hello says', async () => {
    const c = make();
    c.setSelfMuted(true);
    let s = await connected(c);
    expect(s.opts.helloFlags).toBe(VOICE_HELLO_FLAG_SELF_MUTE);
    c.setSelfDeafened(true);
    c.leave();
    c.setSelfDeafened(true);
    // A join starts undeafened (deafen is per call), but keeps the mute.
    s = await connected(c);
    expect(s.opts.helloFlags).toBe(VOICE_HELLO_FLAG_SELF_MUTE);
    c.leave();
    c.setSelfMuted(false);
    void c.join(true);
    await flush();
    c.setSelfDeafened(true);
    c.onTicket('https://b/voice', TICKET);
    await flush();
    expect(sessions[sessions.length - 1].opts.helloFlags).toBe(
      VOICE_HELLO_FLAG_LISTEN_ONLY | VOICE_HELLO_FLAG_SELF_MUTE | VOICE_HELLO_FLAG_SELF_DEAF,
    );
    c.dispose();
  });

  it('a second join while in voice is a no-op; a ticket nobody asked for is ignored', async () => {
    const c = make();
    c.onTicket('https://b/voice', TICKET);
    expect(sessions).toHaveLength(0);
    await connected(c);
    void c.join(false);
    await flush();
    expect(captures).toHaveLength(1);
    c.onTicket('https://b/voice', TICKET);
    await flush();
    expect(sessions).toHaveLength(1);
    c.dispose();
  });
});

describe('VoiceController failures release everything', () => {
  it('capture failure: typed capture phase, no ticket asked', async () => {
    captureFail = new VoiceError('capture', 'Microphone access was refused.');
    const c = make();
    await c.join(false);
    expect(c.getState()).toMatchObject({ phase: 'failed', error: { phase: 'capture', message: 'Microphone access was refused.' } });
    expect(joins).toBe(0);
    c.dispose();
  });

  it('a VoiceJoin rejection: typed ticket phase, mic released', async () => {
    const c = make();
    void c.join(false);
    await flush();
    c.onTicketRejected(ROOM_REJECT_LIMIT, '');
    expect(c.getState()).toMatchObject({ phase: 'failed', error: { phase: 'ticket' } });
    expect(c.getState().error?.message).toMatch(/full/);
    expect(captures[0].stopped).toBe(1);
    c.onTicketRejected(ROOM_REJECT_LIMIT, ''); // not awaiting any more
    void c.join(false);
    await flush();
    c.onTicketRejected(ROOM_REJECT_UNSUPPORTED, '');
    expect(c.getState().error?.message).toMatch(/no voice/);
    void c.join(false);
    await flush();
    c.onTicketRejected(99, 'nope');
    expect(c.getState().error?.message).toMatch(/nope/);
    c.dispose();
  });

  it('an Unavailable rejection is the unavailable state, with its reason and Try again', async () => {
    const c = make();
    void c.join(false);
    await flush();
    c.onTicketRejected(ROOM_REJECT_UNAVAILABLE, 'mumble is down');
    expect(c.getState()).toMatchObject({ phase: 'unavailable', unavailableReason: 'mumble is down' });
    expect(captures[0].stopped).toBe(1);
    c.retry();
    await flush();
    expect(captures).toHaveLength(2);
    expect(joins).toBe(2);
    c.dispose();
  });

  it('no ticket in time: typed ticket phase, mic released', async () => {
    const c = make();
    void c.join(false);
    await flush();
    await vi.advanceTimersByTimeAsync(TICKET_TIMEOUT_MS);
    expect(c.getState()).toMatchObject({ phase: 'failed', error: { phase: 'ticket' } });
    expect(captures[0].stopped).toBe(1);
    c.dispose();
  });

  it('a first connect failure: typed connect phase, mic released, the session never kept', async () => {
    sessionFail = new VoiceError('connect', 'handshake refused');
    const c = make();
    void c.join(false);
    await flush();
    c.onTicket('https://b/voice', TICKET);
    await flush();
    expect(c.getState()).toMatchObject({ phase: 'failed', error: { phase: 'connect', message: 'handshake refused' } });
    expect(captures[0].stopped).toBe(1);
    expect(playbacks).toHaveLength(0);
    c.dispose();
  });

  it('a capture that dies mid-call fails voice and releases the session and playback', async () => {
    const c = make();
    const s = await connected(c);
    captures[0].cb.onError(new VoiceError('capture', 'The microphone stopped.'));
    expect(c.getState()).toMatchObject({ phase: 'failed', error: { phase: 'capture' } });
    expect(s.closed).toBe(1);
    expect(playbacks[0].disposed).toBe(1);
    c.dispose();
  });

  it('leave and dispose release mic, session and playback, and un-deafen the bus', async () => {
    const c = make();
    const s = await connected(c);
    c.setSelfDeafened(true);
    c.leave();
    expect(s.closed).toBe(1);
    expect(captures[0].stopped).toBe(1);
    expect(playbacks[0].disposed).toBe(1);
    expect(busMuted[busMuted.length - 1]).toBe(false);
    expect(c.getState().phase).toBe('idle');
    c.dispose();
    c.dispose();
    expect(roomListeners).toHaveLength(0);
    void c.join(false);
    expect(captures).toHaveLength(1);
  });

  it('leave while the mic prompt is open stops the capture once it resolves', async () => {
    let release!: () => void;
    captureHold = new Promise<void>((r) => (release = r));
    const c = make();
    void c.join(false);
    await flush();
    c.leave();
    release();
    await flush();
    expect(captures[0].stopped).toBeGreaterThanOrEqual(1);
    expect(joins).toBe(0);
    c.dispose();
  });
});

// Review finding (PR #595): the hello's flags are read before the dial, and a
// VoiceControl sent while the dial is in flight has no stream to go on, so a
// mute or deafen clicked while connecting never reached Mumble.
describe('VoiceController controls during the dial', () => {
  it('a mute and deafen toggled while connecting reach the bridge once connected', async () => {
    let open!: () => void;
    sessionHold = new Promise<void>((r) => (open = r));
    const c = make();
    void c.join(false);
    await flush();
    c.onTicket('https://bridge.test/voice', TICKET);
    await flush();
    const s = sessions[0];
    expect(s.opts.helloFlags).toBe(0);
    c.setSelfMuted(true);
    c.setSelfDeafened(true);
    open();
    await flush();
    expect(c.getState().phase).toBe('connected');
    expect(s.controls.at(-1)).toBe(VOICE_CONTROL_FLAG_SELF_MUTE | VOICE_CONTROL_FLAG_SELF_DEAF);
    c.dispose();
  });
});

describe('VoiceController close codes', () => {
  it('4015: terminal "removed", everything released, no rejoin', async () => {
    const c = make();
    const s = await connected(c);
    s.cb.onClosed({ code: CLOSE_CODE_VOICE_REMOVED, reason: '' });
    expect(c.getState().phase).toBe('removed');
    expect(captures[0].stopped).toBe(1);
    expect(playbacks[0].disposed).toBe(1);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(joins).toBe(1);
    c.dispose();
  });

  it('4016: unavailable with the reason, everything released, Try again rejoins', async () => {
    const c = make();
    const s = await connected(c);
    s.cb.onClosed({ code: CLOSE_CODE_VOICE_UNAVAILABLE, reason: 'murmur autobanned the bridge' });
    expect(c.getState()).toMatchObject({ phase: 'unavailable', unavailableReason: 'murmur autobanned the bridge' });
    expect(captures[0].stopped).toBe(1);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(joins).toBe(1);
    c.retry();
    await flush();
    expect(joins).toBe(2);
    c.dispose();
  });

  it('4016 without a reason (Chrome never reads one) is still unavailable', async () => {
    const c = make();
    const s = await connected(c);
    s.cb.onClosed({ code: CLOSE_CODE_VOICE_UNAVAILABLE, reason: '  ' });
    expect(c.getState()).toMatchObject({ phase: 'unavailable', unavailableReason: null });
    c.dispose();
  });

  it('4002: rejoins at once with a fresh ticket, keeping the mic', async () => {
    const c = make();
    const s = await connected(c);
    s.cb.onClosed({ code: CLOSE_CODE_SERVER_DRAINING, reason: 'drain' });
    expect(c.getState().phase).toBe('rejoining');
    expect(playbacks[0].disposed).toBe(1);
    expect(captures[0].stopped).toBe(0);
    await flush();
    expect(joins).toBe(2);
    c.onTicket('https://b2/voice', TICKET);
    await flush();
    expect(sessions).toHaveLength(2);
    expect(c.getState().phase).toBe('connected');
    c.dispose();
  });

  it('a lost connection rejoins after the backoff, only once the room session is back', async () => {
    const c = make();
    const s = await connected(c);
    readiness = 'waiting'; // the room session is reconnecting too (C4)
    s.cb.onClosed({ code: null, reason: 'connection lost' });
    await vi.advanceTimersByTimeAsync(REJOIN_DELAYS_MS[0]);
    expect(joins).toBe(1);
    readiness = 'ready';
    roomChanged();
    expect(joins).toBe(2);
    c.dispose();
  });

  it('a rejoin whose VoiceJoin cannot be sent waits for the room', async () => {
    const c = make();
    const s = await connected(c);
    sendOk = false;
    s.cb.onClosed({ code: CLOSE_CODE_SERVER_DRAINING, reason: '' });
    await flush();
    expect(joins).toBe(2);
    sendOk = true;
    roomChanged();
    expect(joins).toBe(3);
    c.dispose();
  });

  it('gives up after MAX_REJOIN_ATTEMPTS failed rejoins, releasing the mic', async () => {
    const c = make();
    const s = await connected(c);
    sessionFail = new VoiceError('connect', 'unreachable');
    s.cb.onClosed({ code: null, reason: 'lost' });
    for (let i = 0; i < MAX_REJOIN_ATTEMPTS + 1; i++) {
      await vi.advanceTimersByTimeAsync(10_000);
      c.onTicket('https://b/voice', TICKET);
      await flush();
    }
    expect(c.getState()).toMatchObject({ phase: 'failed', error: { phase: 'connect' } });
    expect(captures[0].stopped).toBe(1);
    c.dispose();
  });

  // Found by the MV7 e2e: a draining bridge pod takes the room's observer
  // with it, so the relay answers the immediate rejoin with Unavailable until
  // the observer is redialled onto another pod. That is a moment in a rejoin,
  // not the end of voice.
  it('an Unavailable answer during a rejoin is another rejoin, not the unavailable state', async () => {
    const c = make();
    const s = await connected(c);
    s.cb.onClosed({ code: CLOSE_CODE_SERVER_DRAINING, reason: '' });
    await flush();
    expect(joins).toBe(2);
    c.onTicketRejected(ROOM_REJECT_UNAVAILABLE, 'the voice channel is not reachable');
    expect(c.getState().phase).toBe('rejoining');
    expect(captures[0].stopped).toBe(0);
    await vi.advanceTimersByTimeAsync(REJOIN_DELAYS_MS[1]);
    expect(joins).toBe(3);
    c.onTicket('https://b2/voice', TICKET);
    await flush();
    expect(c.getState().phase).toBe('connected');
    c.dispose();
  });

  it('a ticket that never comes during a rejoin is another rejoin, not a failure', async () => {
    const c = make();
    const s = await connected(c);
    s.cb.onClosed({ code: CLOSE_CODE_SERVER_DRAINING, reason: '' });
    await flush();
    await vi.advanceTimersByTimeAsync(TICKET_TIMEOUT_MS);
    expect(c.getState().phase).toBe('rejoining');
    c.dispose();
  });

  it('the room ending or losing voice ends voice', async () => {
    const c = make();
    await connected(c);
    readiness = 'gone';
    roomChanged();
    expect(c.getState().phase).toBe('idle');
    expect(captures[0].stopped).toBe(1);
    c.dispose();
  });

  it('a join with the room already gone goes nowhere', async () => {
    readiness = 'gone';
    const c = make();
    await c.join(true);
    expect(c.getState().phase).toBe('idle');
    expect(joins).toBe(0);
    c.dispose();
  });

  it('a late close from a replaced session is ignored', async () => {
    const c = make();
    const s = await connected(c);
    c.leave();
    s.cb.onClosed({ code: CLOSE_CODE_VOICE_REMOVED, reason: '' });
    s.cb.onState({ flags: VOICE_STATE_FLAG_CONNECTED, session: 1, name: '' });
    expect(c.getState().phase).toBe('idle');
    c.dispose();
  });
});

describe('VoiceController frames and controls', () => {
  it('sends a spurt with seq in 10 ms units, then a terminator with the end flag', async () => {
    const c = make();
    const s = await connected(c);
    const cap = captures[0].cb;
    clock = 1000;
    expect(cap.shouldSend(LOUD, null, clock)).toBe(true);
    expect(c.getState().talking).toBe(true);
    cap.onPacket(new Uint8Array([1]), 1);
    cap.onPacket(new Uint8Array([2]), 1);
    cap.onPacket(new Uint8Array([3]), 2); // a 20 ms frame
    cap.onSpurtEnd();
    expect(s.frames.map((f) => [f.h.seq, f.h.end, f.p.length])).toEqual([
      [0, false, 1],
      [1, false, 1],
      [2, false, 1],
      [4, true, 0],
    ]);
    expect(s.frames.every((f) => f.h.speaker === 0 && f.h.target === 0)).toBe(true);
    // The next spurt's seq jumps by the pause, as a Mumble client's does.
    clock = 3000;
    cap.onPacket(new Uint8Array([4]), 1);
    expect(s.frames[4].h.seq).toBe(200);
    // A terminator with nothing sent since is not repeated.
    cap.onSpurtEnd();
    cap.onSpurtEnd();
    expect(s.frames.filter((f) => f.h.end)).toHaveLength(2);
    c.dispose();
  });

  it('sends nothing while muted, deafened, server-muted or suppressed, and tells Mumble', async () => {
    const c = make();
    const s = await connected(c);
    const cap = captures[0].cb;
    c.setSelfMuted(true);
    expect(cap.shouldSend(LOUD, null, 0)).toBe(false);
    expect(s.controls[s.controls.length - 1]).toBe(VOICE_CONTROL_FLAG_SELF_MUTE);
    c.setSelfMuted(false);
    expect(s.controls[s.controls.length - 1]).toBe(0);
    c.setSelfDeafened(true);
    expect(busMuted[busMuted.length - 1]).toBe(true);
    expect(s.controls[s.controls.length - 1]).toBe(VOICE_CONTROL_FLAG_SELF_MUTE | VOICE_CONTROL_FLAG_SELF_DEAF);
    expect(cap.shouldSend(LOUD, null, 10)).toBe(false);
    c.setSelfDeafened(false);
    s.cb.onState({ flags: VOICE_STATE_FLAG_CONNECTED | VOICE_STATE_FLAG_SERVER_MUTE, session: 42, name: 'x' });
    expect(c.getState().serverMuted).toBe(true);
    expect(cap.shouldSend(LOUD, null, 20)).toBe(false);
    s.cb.onState({ flags: VOICE_STATE_FLAG_CONNECTED | VOICE_STATE_FLAG_SUPPRESSED, session: 42, name: 'x' });
    expect(cap.shouldSend(LOUD, null, 30)).toBe(false);
    s.cb.onState({ flags: VOICE_STATE_FLAG_CONNECTED, session: 42, name: 'x' });
    expect(cap.shouldSend(LOUD, null, 40)).toBe(true);
    c.dispose();
  });

  it('push-to-talk: talks exactly while held', async () => {
    const c = make('ptt');
    await connected(c);
    const cap = captures[0].cb;
    expect(cap.shouldSend(LOUD, null, 0)).toBe(false);
    c.setPttHeld(true);
    c.setPttHeld(true);
    expect(c.getState().pttHeld).toBe(true);
    expect(cap.shouldSend(0, null, 10)).toBe(true);
    c.setPttHeld(false);
    expect(cap.shouldSend(LOUD, null, 20)).toBe(false);
    c.setTalkMode('vad');
    expect(cap.shouldSend(LOUD, null, 30)).toBe(true);
    c.dispose();
  });

  it('nothing is sent before connecting, and packets from a stale capture are dropped', async () => {
    const c = make();
    void c.join(false);
    await flush();
    const cap = captures[0].cb;
    expect(cap.shouldSend(LOUD, null, 0)).toBe(false);
    cap.onPacket(new Uint8Array([1]), 1);
    c.leave();
    expect(cap.shouldSend(LOUD, null, 0)).toBe(false);
    cap.onPacket(new Uint8Array([1]), 1);
    cap.onSpurtEnd();
    cap.onError(new VoiceError('capture', 'late'));
    expect(c.getState().phase).toBe('idle');
    c.dispose();
  });

  it('received frames go to playback, the speaking set into the state, volumes through', async () => {
    const c = make();
    const s = await connected(c);
    s.cb.onFrame({ end: false, target: 0, speaker: 7, seq: 1 }, new Uint8Array([1]));
    expect(playbacks[0].frames).toHaveLength(1);
    playbacks[0].cb.onSpeaking([7]);
    expect(c.getState().speaking).toEqual([7]);
    c.setSpeakerVolume(7, 0.5);
    expect(playbacks[0].volumes).toEqual([[7, 0.5]]);
    c.dispose();
    expect(states[states.length - 1].speaking).toEqual([]);
  });

  it('VoiceState CONNECTED resets the rejoin budget', async () => {
    const c = make();
    let s = await connected(c);
    for (let i = 0; i < MAX_REJOIN_ATTEMPTS - 1; i++) {
      s.cb.onClosed({ code: null, reason: 'lost' });
      await vi.advanceTimersByTimeAsync(10_000);
      c.onTicket('https://b/voice', TICKET);
      await flush();
      s = sessions[sessions.length - 1];
      s.cb.onState({ flags: VOICE_STATE_FLAG_CONNECTED, session: 42, name: '' });
    }
    s.cb.onClosed({ code: null, reason: 'lost' });
    expect(c.getState().phase).toBe('rejoining');
    c.dispose();
  });
});

describe('rejoinDelayMs', () => {
  it('rejoins a drain at once the first time and walks the ladder otherwise', () => {
    expect(rejoinDelayMs(1, CLOSE_CODE_SERVER_DRAINING)).toBe(0);
    expect(rejoinDelayMs(2, CLOSE_CODE_SERVER_DRAINING)).toBe(REJOIN_DELAYS_MS[1]);
    expect(rejoinDelayMs(1, null)).toBe(REJOIN_DELAYS_MS[0]);
    expect(rejoinDelayMs(50, null)).toBe(REJOIN_DELAYS_MS[REJOIN_DELAYS_MS.length - 1]);
  });
});

describe('VoiceController audio settings (R80, docs/81 D9, D10, D15–D17)', () => {
  it('starts the capture in the saved noise mode, and a mid-call change reaches it', async () => {
    const c = make('vad', { noiseMode: 'off' });
    await connected(c);
    expect(captures[0].noise).toBe('off');
    c.setNoiseMode('browser');
    expect(captures[0].modes).toEqual(['browser']);
    expect(c.getState().noiseMode).toBe('browser');
    // What the capture reports is published as is.
    const status = { requested: 'browser', running: 'off', loading: false, fallback: null, pending: true, offAvailable: true } as const;
    captures[0].cb.onNoise(status);
    expect(c.getState().noise).toEqual(status);
    // The next join starts in the new mode; leaving drops the live status.
    c.leave();
    expect(c.getState().noise).toBeNull();
    expect(c.getState().noiseMode).toBe('browser');
    await connected(c);
    expect(captures[1].noise).toBe('browser');
    c.dispose();
  });

  it('listen-only builds no capture, so it never downloads RNNoise or opens the microphone', async () => {
    const c = make();
    await connected(c, true);
    expect(captures).toHaveLength(0);
    c.setNoiseMode('browser');
    c.dispose();
  });

  it('the probability drives the gate with the user’s threshold; the level gate without one', async () => {
    const c = make('vad', { speechThreshold: 0.6, levelThresholdDb: -20 });
    await connected(c);
    const cap = captures[0].cb;
    expect(cap.shouldSend(0.05, 0.65, 0)).toBe(true);
    expect(c.meter()).toEqual({ prob: 0.65, levelDb: expect.closeTo(-26, 0), open: true, floorDb: -50 });
    c.setSpeechThreshold(0.9);
    expect(c.getState().speechThreshold).toBe(0.9);
    expect(cap.shouldSend(0.05, 0.65, 1000)).toBe(false);
    // Without RNNoise: the level threshold, -20 dBFS (0.1 RMS).
    expect(cap.shouldSend(0.05, null, 2000)).toBe(false);
    c.setLevelThresholdDb(-30);
    expect(c.getState().levelThresholdDb).toBe(-30);
    expect(cap.shouldSend(0.05, null, 3000)).toBe(true);
    expect(c.meter().prob).toBeNull();
    expect(c.meter().floorDb).toBeNull();
    c.leave();
    expect(c.meter()).toEqual({ prob: null, levelDb: -100, open: false, floorDb: null });
    c.dispose();
  });

  it('Hear yourself plays your own packets and terminators back, and stops when turned off', async () => {
    const c = make();
    await connected(c);
    const cap = captures[0].cb;
    cap.shouldSend(LOUD, null, 0);
    cap.onPacket(new Uint8Array([1]), 1);
    expect(playbacks[0].self).toEqual([]);
    c.setHearSelf(true);
    c.setHearSelf(true);
    cap.onPacket(new Uint8Array([2, 2]), 1);
    cap.onSpurtEnd();
    expect(playbacks[0].self).toEqual([
      [{ end: false, seq: 1 }, 2],
      [{ end: true, seq: 2 }, 0],
    ]);
    c.setHearSelf(false);
    expect(playbacks[0].selfStops).toBe(1);
    // Never kept past the session.
    c.setHearSelf(true);
    c.leave();
    expect(c.getState().hearSelf).toBe(false);
    c.dispose();
  });
});

describe('VoiceController learned level (R81, docs/82 D2–D7)', () => {
  const at = (db: number) => 10 ** (db / 20);

  it('the stored level sets the capture’s gain from the first block', async () => {
    const c = make('vad', { speechLevelDb: -35 });
    await connected(c);
    expect(captures[0].cb.gainDb?.()).toBe(15);
    c.dispose();
  });

  it('learns from the blocks it judges, raises the meter’s floor, and stores the level on leave', async () => {
    const c = make();
    await connected(c);
    const cap = captures[0].cb;
    for (let f = 0; f < 400; f++) cap.shouldSend(at(-24), 0.99, f * 10);
    expect(c.meter().floorDb).toBe(-36);
    expect(cap.gainDb?.()).toBe(4);
    // A confident hum 16 dB under the speaker is under the floor.
    expect(cap.shouldSend(at(-40), 0.99, 10_000)).toBe(false);
    c.leave();
    expect(savedLevels).toEqual([-24]);
    c.dispose();
  });

  it('forgetting the level drops the floor and the gain', async () => {
    const c = make('vad', { speechLevelDb: -35 });
    await connected(c);
    const cap = captures[0].cb;
    for (let f = 0; f < 400; f++) cap.shouldSend(at(-24), 0.99, f * 10);
    c.forgetSpeechLevel();
    expect(cap.gainDb?.()).toBe(0);
    cap.shouldSend(at(-24), 0.99, 5000);
    expect(c.meter().floorDb).toBe(-50);
    c.leave();
    expect(savedLevels).toEqual([]);
    c.dispose();
  });
});
