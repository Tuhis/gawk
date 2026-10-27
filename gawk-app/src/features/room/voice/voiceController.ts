// The room voice client's state machine (R79, docs/80 D5, D17–D19, §11.1
// C4). DOM-free: every effect goes through VoiceControllerDeps, so the whole
// lifecycle — ticket, connect, frames, the close codes — is unit-tested with
// fakes.
//
//   join ─▶ (capture, unless listen-only) ─▶ VoiceJoin on the room session
//        ─▶ VoiceTicket ─▶ VoiceSession to the bridge ─▶ connected
//
// The microphone is opened BEFORE the ticket is asked for: a ticket lives
// 30 s (D7) and a permission prompt can sit longer than that.
//
// How a voice session ends decides what happens next:
//   - 4015 CloseCodeVoiceRemoved: terminal — "You were removed from voice",
//     no automatic rejoin (D17).
//   - 4016 CloseCodeVoiceUnavailable: "Voice is unavailable", with the reason
//     when the browser got one, and a Try again (D18).
//   - 4002 (a bridge drain, D19, or the room session ending, C4) and a lost
//     connection: rejoin with a fresh ticket, after a short backoff and only
//     once the room session is up again (a reconnected room session is a new
//     participant, so the old ticket means nothing).
// Every failure releases what was acquired: mic track, encoder, decoders,
// worklets, the WebTransport session.

import { log } from '../../../lib/logger';
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
  VOICE_STATE_FLAG_SERVER_DEAF,
  VOICE_STATE_FLAG_SERVER_MUTE,
  VOICE_STATE_FLAG_SUPPRESSED,
  type VoiceFrameHeader,
  type VoiceStateMessage,
} from '../../../transport/wire';
import { TalkGate, rmsToDbfs } from './talkGate';
import {
  DEFAULT_LEVEL_THRESHOLD_DB,
  DEFAULT_NOISE_MODE,
  DEFAULT_SPEECH_THRESHOLD,
  type NoiseMode,
  type TalkMode,
} from './voicePrefs';
import {
  VoiceError,
  type VoiceCloseInfo,
  type VoiceFailurePhase,
  type VoiceSessionCallbacks,
  type VoiceSessionLike,
  type VoiceSessionOptions,
} from './voiceSession';
import type { CaptureContext, NoiseStatus, VoiceCaptureCallbacks, VoiceCaptureLike } from './voiceCapture';
import type { VoicePlaybackCallbacks, VoicePlaybackLike } from './voicePlayback';

export type VoicePhase =
  | 'idle'
  | 'joining'
  | 'connected'
  | 'rejoining'
  // 4015: terminal for voice.
  | 'removed'
  // 4016, or the relay's Unavailable answer to VoiceJoin: Try again.
  | 'unavailable'
  // A typed failure (error.phase).
  | 'failed';

export interface VoiceViewState {
  phase: VoicePhase;
  listenOnly: boolean;
  error: { phase: VoiceFailurePhase; message: string } | null;
  unavailableReason: string | null;
  selfMuted: boolean;
  selfDeafened: boolean;
  talkMode: TalkMode;
  pttHeld: boolean;
  // The own speaking indicator: this browser is transmitting.
  talking: boolean;
  // VoiceState: the bridged Mumble connection is up and in the channel.
  mumbleConnected: boolean;
  ownSession: number | null;
  ownName: string;
  serverMuted: boolean;
  serverDeafened: boolean;
  suppressed: boolean;
  // Mumble sessions heard speaking (speaking.ts).
  speaking: number[];
  // R80 (docs/81 D15, D17): the noise suppression chosen, and what the
  // running capture does with it (null without one).
  noiseMode: NoiseMode;
  noise: NoiseStatus | null;
  speechThreshold: number;
  levelThresholdDb: number;
  // Hear yourself (D16): this session only, never saved.
  hearSelf: boolean;
}

// The Audio section's live meter (docs/81 D15), read on animation frames
// rather than published per block.
export interface VoiceMeter {
  // RNNoise's speech probability for the last block, or null when it was
  // not denoised.
  prob: number | null;
  levelDb: number;
  // The gate sent the block.
  open: boolean;
  // The level floor the block had to meet while RNNoise ran, dBFS, else
  // null (docs/82 D7: the meter greys a block under it).
  floorDb: number | null;
}

export interface VoiceAudioPrefs {
  noiseMode: NoiseMode;
  speechThreshold: number;
  levelThresholdDb: number;
  // docs/82 D6: the level learned in an earlier session, for the gain's
  // first blocks.
  speechLevelDb: number | null;
}

export const INITIAL_VOICE_STATE: VoiceViewState = {
  phase: 'idle',
  listenOnly: false,
  error: null,
  unavailableReason: null,
  selfMuted: false,
  selfDeafened: false,
  talkMode: 'vad',
  pttHeld: false,
  talking: false,
  mumbleConnected: false,
  ownSession: null,
  ownName: '',
  serverMuted: false,
  serverDeafened: false,
  suppressed: false,
  speaking: [],
  noiseMode: DEFAULT_NOISE_MODE,
  noise: null,
  speechThreshold: DEFAULT_SPEECH_THRESHOLD,
  levelThresholdDb: DEFAULT_LEVEL_THRESHOLD_DB,
  hearSelf: false,
};

// Whether the room session can carry a VoiceJoin right now: 'ready' (joined,
// the room advertises voice), 'waiting' (reconnecting), or 'gone' (the room
// ended, failed, or stopped advertising voice — voice ends with it).
export type RoomVoiceReadiness = 'ready' | 'waiting' | 'gone';

export interface VoiceControllerDeps {
  sendVoiceJoin: () => boolean;
  roomReadiness: () => RoomVoiceReadiness;
  subscribeRoom: (onChange: () => void) => () => void;
  webTransportInit: () => WebTransportOptions;
  createSession: (opts: VoiceSessionOptions, cb: VoiceSessionCallbacks) => VoiceSessionLike;
  createCapture: (cb: VoiceCaptureCallbacks, noise: NoiseMode) => VoiceCaptureLike;
  captureContext: () => CaptureContext | null;
  createPlayback: (cb: VoicePlaybackCallbacks) => VoicePlaybackLike;
  // Deafen mutes the voice bus (roomAudio.ts).
  setVoiceMuted: (muted: boolean) => void;
  now: () => number;
  onChange: (state: VoiceViewState) => void;
  // docs/82 D6: the learned level outlives the session (voicePrefs).
  saveSpeechLevel?: (db: number) => void;
}

// No ticket this long after VoiceJoin: the relay is not answering.
export const TICKET_TIMEOUT_MS = 10_000;
// Rejoin backoff. A 4002 drain rejoins at once the first time (the
// replacement pod is behind the same LoadBalancer); everything else walks the
// ladder. The budget resets once Mumble says the user is connected.
export const REJOIN_DELAYS_MS = [500, 1000, 2000, 4000, 8000];
export const MAX_REJOIN_ATTEMPTS = 8;

export const REMOVED_MESSAGE = 'You were removed from voice.';
export const LOST_MESSAGE = 'Lost the voice connection.';
const TICKET_TIMEOUT_MESSAGE = 'The server didn’t answer the voice request.';

export function rejoinDelayMs(attempt: number, closeCode: number | null): number {
  if (attempt === 1 && closeCode === CLOSE_CODE_SERVER_DRAINING) return 0;
  return REJOIN_DELAYS_MS[Math.min(attempt - 1, REJOIN_DELAYS_MS.length - 1)];
}

function rejectionMessage(reason: number, message: string): string {
  const detail = message.trim();
  switch (reason) {
    case ROOM_REJECT_UNSUPPORTED:
      return 'This room has no voice.';
    case ROOM_REJECT_LIMIT:
      return 'Voice is full in this room.';
    default:
      return detail !== '' ? `The server refused voice (${detail}).` : 'The server refused voice.';
  }
}

export class VoiceController {
  private readonly deps: VoiceControllerDeps;
  private state: VoiceViewState;
  private readonly gate: TalkGate;
  // Bumped by every join and every leave: async work from an older one
  // (a capture start, a dial, a timer) checks it and stands down.
  private gen = 0;
  private session: VoiceSessionLike | null = null;
  private capture: VoiceCaptureLike | null = null;
  private playback: VoicePlaybackLike | null = null;
  private awaitingTicket = false;
  private waitingForRoom = false;
  private ticketTimer: ReturnType<typeof setTimeout> | null = null;
  private rejoinTimer: ReturnType<typeof setTimeout> | null = null;
  private attempts = 0;
  private everConnected = false;
  // Outgoing VoiceFrame seq (10 ms units), on a clock anchored at the first
  // packet, so a pause between spurts advances it as a Mumble client's does.
  private seq = 0;
  private seqEpochMs: number | null = null;
  private spurtStarting = true;
  private lastMeter: VoiceMeter = { prob: null, levelDb: -100, open: false, floorDb: null };
  private readonly unsubscribe: () => void;
  private disposed = false;

  constructor(deps: VoiceControllerDeps, talkMode: TalkMode, audio?: Partial<VoiceAudioPrefs>) {
    this.deps = deps;
    this.state = {
      ...INITIAL_VOICE_STATE,
      talkMode,
      noiseMode: audio?.noiseMode ?? DEFAULT_NOISE_MODE,
      speechThreshold: audio?.speechThreshold ?? DEFAULT_SPEECH_THRESHOLD,
      levelThresholdDb: audio?.levelThresholdDb ?? DEFAULT_LEVEL_THRESHOLD_DB,
    };
    this.gate = new TalkGate(talkMode, undefined, undefined, audio?.speechLevelDb ?? null);
    this.gate.setSpeechThreshold(this.state.speechThreshold);
    this.gate.setLevelThresholdDb(this.state.levelThresholdDb);
    this.gate.setBlocked(true);
    this.unsubscribe = deps.subscribeRoom(() => this.onRoomChange());
  }

  getState(): VoiceViewState {
    return this.state;
  }

  // The last block's probability, level and gate decision (docs/81 D15).
  meter(): VoiceMeter {
    return this.lastMeter;
  }

  // The settings that outlive a voice session: what join() and leave() keep.
  private kept(): Partial<VoiceViewState> {
    const s = this.state;
    return {
      talkMode: s.talkMode,
      selfMuted: s.selfMuted,
      noiseMode: s.noiseMode,
      speechThreshold: s.speechThreshold,
      levelThresholdDb: s.levelThresholdDb,
    };
  }

  // ---- User actions ----------------------------------------------------------

  // From a click: Join voice / Join listen-only / Try again.
  async join(listenOnly: boolean): Promise<void> {
    if (this.disposed) return;
    if (this.state.phase === 'joining' || this.state.phase === 'connected' || this.state.phase === 'rejoining') return;
    this.teardown();
    const gen = ++this.gen;
    this.deps.setVoiceMuted(false);
    this.attempts = 0;
    this.everConnected = false;
    this.update({
      ...INITIAL_VOICE_STATE,
      ...this.kept(),
      phase: 'joining',
      listenOnly,
    });
    if (!listenOnly) {
      const capture = this.deps.createCapture(this.captureCallbacks(gen), this.state.noiseMode);
      this.capture = capture;
      try {
        await capture.start(this.deps.captureContext);
      } catch (e) {
        if (gen !== this.gen) return;
        this.fail(e instanceof VoiceError ? e : new VoiceError('capture', String(e)));
        return;
      }
      if (gen !== this.gen) {
        capture.stop();
        return;
      }
    }
    this.requestTicket(gen);
  }

  retry(): void {
    void this.join(this.state.listenOnly);
  }

  leave(): void {
    this.teardown();
    this.gen++;
    this.deps.setVoiceMuted(false);
    this.update({ ...INITIAL_VOICE_STATE, ...this.kept() });
  }

  dispose(): void {
    if (this.disposed) return;
    this.leave();
    this.disposed = true;
    this.unsubscribe();
  }

  setSelfMuted(muted: boolean): void {
    this.update({ selfMuted: muted });
    this.sendControl();
  }

  setSelfDeafened(deafened: boolean): void {
    this.update({ selfDeafened: deafened });
    this.deps.setVoiceMuted(deafened);
    this.sendControl();
  }

  setTalkMode(mode: TalkMode): void {
    this.gate.setMode(mode);
    this.update({ talkMode: mode, pttHeld: false });
  }

  setPttHeld(held: boolean): void {
    if (held === this.state.pttHeld) return;
    this.gate.setPttHeld(held);
    this.update({ pttHeld: held });
  }

  setSpeakerVolume(session: number, volume: number): void {
    this.playback?.setSpeakerVolume(session, volume);
  }

  // ---- Audio settings (R80, docs/81 D15) ------------------------------------

  // Applies to the running capture (a switch, a re-open, or the next join
  // when it would prompt) and to every later join.
  setNoiseMode(mode: NoiseMode): void {
    this.update({ noiseMode: mode });
    void this.capture?.setNoise(mode);
  }

  setSpeechThreshold(on: number): void {
    this.gate.setSpeechThreshold(on);
    this.update({ speechThreshold: on });
  }

  setLevelThresholdDb(db: number): void {
    this.gate.setLevelThresholdDb(db);
    this.update({ levelThresholdDb: db });
  }

  // Reset to defaults (docs/82 D6): the learned level goes too.
  forgetSpeechLevel(): void {
    this.gate.forgetLevel();
  }

  // Hear yourself (D16): your own packets, after the gate, through the same
  // decoder, jitter buffer and voice bus as everyone else's.
  setHearSelf(on: boolean): void {
    if (on === this.state.hearSelf) return;
    if (!on) this.playback?.stopSelf();
    this.update({ hearSelf: on });
  }

  // ---- Room session events ---------------------------------------------------

  onTicket(url: string, ticket: Uint8Array): void {
    if (!this.awaitingTicket || this.disposed) return;
    this.awaitingTicket = false;
    this.clearTicketTimer();
    void this.connect(this.gen, url, ticket);
  }

  // A CommandRejected for VoiceJoin.
  onTicketRejected(reason: number, message: string): void {
    if (!this.awaitingTicket) return;
    this.awaitingTicket = false;
    this.clearTicketTimer();
    if (reason === ROOM_REJECT_UNAVAILABLE) {
      if (this.state.phase === 'rejoining') {
        // A draining bridge pod takes the room's observer with it, so the
        // relay says Unavailable until it has redialled the observer onto
        // another pod: a moment in the rejoin, not the end of voice. The
        // rejoin budget still bounds it.
        this.scheduleRejoin(null);
        return;
      }
      this.unavailable(message.trim() || null);
      return;
    }
    this.fail(new VoiceError('ticket', rejectionMessage(reason, message)));
  }

  private onRoomChange(): void {
    if (this.disposed) return;
    const phase = this.state.phase;
    const active = phase === 'joining' || phase === 'connected' || phase === 'rejoining';
    const readiness = this.deps.roomReadiness();
    if (active && readiness === 'gone') {
      log.info('voice: the room is gone or no longer has voice; leaving voice');
      this.leave();
      return;
    }
    if (this.waitingForRoom && readiness === 'ready') {
      this.waitingForRoom = false;
      this.requestTicket(this.gen);
    }
  }

  // ---- Ticket and connect ----------------------------------------------------

  private requestTicket(gen: number): void {
    if (gen !== this.gen || this.disposed) return;
    const readiness = this.deps.roomReadiness();
    if (readiness === 'gone') {
      this.leave();
      return;
    }
    if (readiness === 'waiting' || !this.deps.sendVoiceJoin()) {
      // Asked again from onRoomChange once the room session is back.
      this.waitingForRoom = true;
      return;
    }
    this.awaitingTicket = true;
    this.clearTicketTimer();
    this.ticketTimer = setTimeout(() => {
      this.ticketTimer = null;
      if (gen !== this.gen || !this.awaitingTicket) return;
      this.awaitingTicket = false;
      if (this.everConnected) this.scheduleRejoin(null);
      else this.fail(new VoiceError('ticket', TICKET_TIMEOUT_MESSAGE));
    }, TICKET_TIMEOUT_MS);
  }

  private async connect(gen: number, url: string, ticket: Uint8Array): Promise<void> {
    const s = this.state;
    const deaf = s.selfDeafened;
    const helloFlags =
      (s.listenOnly ? VOICE_HELLO_FLAG_LISTEN_ONLY : 0) |
      (s.selfMuted || deaf ? VOICE_HELLO_FLAG_SELF_MUTE : 0) |
      (deaf ? VOICE_HELLO_FLAG_SELF_DEAF : 0);
    const session = this.deps.createSession(
      { url, ticket, helloFlags, init: this.deps.webTransportInit() },
      {
        onState: (st) => {
          if (gen === this.gen && this.session === session) this.onVoiceState(st);
        },
        onFrame: (h, p) => {
          if (gen === this.gen && this.session === session) this.onFrame(h, p);
        },
        onClosed: (info) => {
          if (gen === this.gen && this.session === session) this.onClosed(info);
        },
      },
    );
    this.session = session;
    try {
      await session.start();
    } catch (e) {
      if (gen !== this.gen || this.session !== session) return;
      this.session = null;
      const err = e instanceof VoiceError ? e : new VoiceError('connect', String(e));
      log.info(`voice connect failed: ${err.message}`);
      if (this.everConnected) this.scheduleRejoin(null);
      else this.fail(err);
      return;
    }
    if (gen !== this.gen || this.session !== session) {
      session.close();
      return;
    }
    this.everConnected = true;
    this.playback = this.deps.createPlayback({
      onSpeaking: (sessions) => {
        if (gen === this.gen) this.update({ speaking: sessions });
      },
    });
    this.spurtStarting = true;
    this.update({ phase: 'connected' });
    // The hello carried the flags as they were before the dial; a mute or
    // deafen clicked while it was in flight had no stream to go on. Send the
    // current state now (a repeat of the hello when nothing changed).
    this.sendControl();
  }

  // ---- Voice session events ----------------------------------------------------

  private onVoiceState(st: VoiceStateMessage): void {
    const connected = (st.flags & VOICE_STATE_FLAG_CONNECTED) !== 0;
    // Mumble has us: a later drop starts the rejoin ladder from the bottom.
    if (connected) this.attempts = 0;
    this.update({
      mumbleConnected: connected,
      ownSession: st.session === 0 ? null : st.session,
      ownName: st.name,
      serverMuted: (st.flags & VOICE_STATE_FLAG_SERVER_MUTE) !== 0,
      serverDeafened: (st.flags & VOICE_STATE_FLAG_SERVER_DEAF) !== 0,
      suppressed: (st.flags & VOICE_STATE_FLAG_SUPPRESSED) !== 0,
    });
  }

  private onFrame(h: VoiceFrameHeader, payload: Uint8Array): void {
    this.playback?.handleFrame(h, payload);
  }

  private onClosed(info: VoiceCloseInfo): void {
    this.session = null;
    this.stopPlayback();
    this.update({ mumbleConnected: false, ownSession: null, speaking: [], talking: false });
    if (info.code === CLOSE_CODE_VOICE_REMOVED) {
      log.info('voice: removed (4015)');
      this.teardown();
      this.gen++;
      this.update({ phase: 'removed' });
      return;
    }
    if (info.code === CLOSE_CODE_VOICE_UNAVAILABLE) {
      log.info(`voice: unavailable (4016) ${info.reason}`);
      this.unavailable(info.reason.trim() || null);
      return;
    }
    log.info(`voice session ended (code ${info.code ?? 'none'}: ${info.reason}); rejoining`);
    this.scheduleRejoin(info.code);
  }

  private scheduleRejoin(closeCode: number | null): void {
    this.attempts += 1;
    if (this.attempts > MAX_REJOIN_ATTEMPTS) {
      this.fail(new VoiceError('connect', LOST_MESSAGE));
      return;
    }
    const gen = this.gen;
    this.update({ phase: 'rejoining' });
    const delay = rejoinDelayMs(this.attempts, closeCode);
    this.clearRejoinTimer();
    this.rejoinTimer = setTimeout(() => {
      this.rejoinTimer = null;
      this.requestTicket(gen);
    }, delay);
  }

  // ---- Capture -------------------------------------------------------------------

  private captureCallbacks(gen: number): VoiceCaptureCallbacks {
    return {
      shouldSend: (rms, prob, nowMs) => {
        if (gen !== this.gen) return false;
        const send = this.gate.update(rms, prob, nowMs);
        this.lastMeter = { prob, levelDb: rmsToDbfs(rms), open: send, floorDb: this.gate.floorDb() };
        const talking = send && this.state.phase === 'connected';
        if (talking !== this.state.talking) this.update({ talking });
        return send;
      },
      onPacket: (payload, units) => {
        const session = this.session;
        if (gen !== this.gen || !session || this.state.phase !== 'connected') return;
        const now = this.deps.now();
        this.seqEpochMs ??= now;
        if (this.spurtStarting) {
          this.spurtStarting = false;
          this.seq = Math.max(this.seq, Math.floor((now - this.seqEpochMs) / 10)) >>> 0;
        }
        session.sendFrame({ end: false, target: 0, speaker: 0, seq: this.seq }, payload);
        if (this.state.hearSelf) this.playback?.handleSelfFrame({ end: false, seq: this.seq }, payload);
        this.seq = (this.seq + units) >>> 0;
      },
      onSpurtEnd: () => {
        if (gen !== this.gen) return;
        const wasSending = !this.spurtStarting;
        this.spurtStarting = true;
        if (!wasSending) return;
        this.session?.sendFrame({ end: true, target: 0, speaker: 0, seq: this.seq }, new Uint8Array(0));
        if (this.state.hearSelf) this.playback?.handleSelfFrame({ end: true, seq: this.seq }, new Uint8Array(0));
      },
      onNoise: (noise) => {
        if (gen === this.gen) this.update({ noise });
      },
      gainDb: () => this.gate.gainDb(),
      onError: (err) => {
        if (gen !== this.gen) return;
        this.capture = null;
        this.fail(err);
      },
    };
  }

  private sendControl(): void {
    const s = this.state;
    const flags =
      (s.selfMuted || s.selfDeafened || s.listenOnly ? VOICE_CONTROL_FLAG_SELF_MUTE : 0) |
      (s.selfDeafened ? VOICE_CONTROL_FLAG_SELF_DEAF : 0);
    this.session?.sendControl(flags);
  }

  // ---- Endings -------------------------------------------------------------------

  private unavailable(reason: string | null): void {
    this.teardown();
    this.gen++;
    this.update({ phase: 'unavailable', unavailableReason: reason });
  }

  private fail(err: VoiceError): void {
    log.warn(`voice failed (${err.phase}): ${err.message}`);
    this.teardown();
    this.gen++;
    this.update({ phase: 'failed', error: { phase: err.phase, message: err.message } });
  }

  // The one teardown: every path out of voice releases everything through it.
  private teardown(): void {
    this.awaitingTicket = false;
    this.waitingForRoom = false;
    this.clearTicketTimer();
    this.clearRejoinTimer();
    const session = this.session;
    this.session = null;
    session?.close();
    const capture = this.capture;
    this.capture = null;
    capture?.stop();
    this.stopPlayback();
    // The gain's level, not the tracker's: a long listen before leaving
    // empties the tracker but not what the gain was using (review round 1).
    const level = this.gate.levelToStore();
    if (capture && level !== null) {
      this.deps.saveSpeechLevel?.(level);
      this.gate.markStored();
    }
    this.seq = 0;
    this.seqEpochMs = null;
    this.spurtStarting = true;
    this.gate.setPttHeld(false);
    this.gate.setBlocked(true);
    this.lastMeter = { prob: null, levelDb: -100, open: false, floorDb: null };
    if (this.state.talking || this.state.speaking.length > 0 || this.state.pttHeld || this.state.noise) {
      this.update({ talking: false, speaking: [], pttHeld: false, noise: null });
    }
  }

  private stopPlayback(): void {
    const p = this.playback;
    this.playback = null;
    p?.dispose();
  }

  private clearTicketTimer(): void {
    if (this.ticketTimer !== null) clearTimeout(this.ticketTimer);
    this.ticketTimer = null;
  }

  private clearRejoinTimer(): void {
    if (this.rejoinTimer !== null) clearTimeout(this.rejoinTimer);
    this.rejoinTimer = null;
  }

  // Every state change also re-decides the gate: nothing is sent unless
  // connected, unmuted, undeafened and allowed to speak.
  private update(patch: Partial<VoiceViewState>): void {
    let next = { ...this.state, ...patch };
    const blocked =
      next.phase !== 'connected' ||
      next.listenOnly ||
      next.selfMuted ||
      next.selfDeafened ||
      next.serverMuted ||
      next.serverDeafened ||
      next.suppressed;
    this.gate.setBlocked(blocked);
    if (blocked && next.talking) next = { ...next, talking: false };
    this.state = next;
    this.deps.onChange(next);
  }
}
