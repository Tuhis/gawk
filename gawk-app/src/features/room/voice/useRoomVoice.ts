// The React glue of room voice (R79): one VoiceController per room view,
// created on mount and disposed with it, publishing into useVoiceStore. It
// acts only on explicit events — the Join / Leave / mute clicks, the
// push-to-talk key's own keydown/keyup — never on a re-render (CODE-REVIEW's
// effect rule); the room session's commands and the mixer are read through
// refs.

import { useEffect, useMemo, useRef, type RefObject } from 'react';
import { webTransportInit } from '../../../transport/connection';
import { selectCertHashList, useTransportStore } from '../../../state/transportStore';
import { roomHasVoice, useRoomStore } from '../../../state/roomStore';
import type { RoomAudioMixer } from '../roomAudio';
import type { RoomCommands, RoomVoiceEvents } from '../useRoomSession';
import { VoiceController, type RoomVoiceReadiness } from './voiceController';
import { VoiceCapture } from './voiceCapture';
import { VoicePlayback, realPlaybackDeps } from './voicePlayback';
import { VoiceSession } from './voiceSession';
import { useVoiceStore } from './voiceStore';
import { loadSpeakerVolume, savePttKey, saveSpeakerVolume, saveTalkMode, validPttKey, type TalkMode } from './voicePrefs';

export interface VoiceActions {
  join: (listenOnly: boolean) => void;
  leave: () => void;
  retry: () => void;
  setMuted: (muted: boolean) => void;
  setDeafened: (deafened: boolean) => void;
  setTalkMode: (mode: TalkMode) => void;
  setPttHeld: (held: boolean) => void;
  setPttKey: (code: string) => void;
  // `volume` is what persists per name (D22); `muted` is this session's
  // per-person mute, applied to the gain only and never saved.
  setSpeakerVolume: (session: number, mumbleName: string, volume: number, muted: boolean) => void;
}

export function roomVoiceReadiness(): RoomVoiceReadiness {
  const { status, snapshot } = useRoomStore.getState();
  if (status === 'reconnecting' || status === 'connecting') return 'waiting';
  if (status === 'joined') return roomHasVoice(snapshot) ? 'ready' : 'gone';
  return 'gone';
}

function speakerName(session: number): string | null {
  return useRoomStore.getState().voiceRoster?.find((e) => e.session === session)?.name ?? null;
}

// Typing in a field is not talking.
function isEditable(target: EventTarget | null): boolean {
  const el = target as HTMLElement | null;
  if (!el || typeof el.tagName !== 'string') return false;
  return el.isContentEditable || el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.tagName === 'SELECT';
}

export function useRoomVoice({
  mixerRef,
  commandsRef,
}: {
  mixerRef: RefObject<RoomAudioMixer | null>;
  commandsRef: RefObject<RoomCommands | null>;
}): { actions: VoiceActions; events: RoomVoiceEvents } {
  const ctrlRef = useRef<VoiceController | null>(null);

  useEffect(() => {
    const store = useVoiceStore.getState();
    store.reset();
    const ctrl = new VoiceController(
      {
        sendVoiceJoin: () => commandsRef.current?.voiceJoin() ?? false,
        roomReadiness: roomVoiceReadiness,
        subscribeRoom: (fn) => useRoomStore.subscribe(fn),
        // The same pins every relay session dials with: in the dev stack the
        // bridge serves the relay's certificate.
        webTransportInit: () => webTransportInit({ certHashHex: selectCertHashList(useTransportStore.getState()) }),
        createSession: (opts, cb) => new VoiceSession(opts, cb),
        createCapture: (cb) => new VoiceCapture(cb),
        captureContext: () => {
          const context = mixerRef.current?.context() ?? null;
          return context ? { context } : null;
        },
        createPlayback: (cb) =>
          new VoicePlayback(realPlaybackDeps(() => mixerRef.current?.voiceOutput() ?? null), cb, (session) => {
            const name = speakerName(session);
            return name === null ? 1 : loadSpeakerVolume(name);
          }),
        setVoiceMuted: (muted) => mixerRef.current?.setVoiceMuted(muted),
        now: () => performance.now(),
        onChange: (st) => useVoiceStore.getState().set(st),
      },
      store.talkMode,
    );
    ctrlRef.current = ctrl;
    return () => {
      if (ctrlRef.current === ctrl) ctrlRef.current = null;
      ctrl.dispose();
      useVoiceStore.getState().reset();
    };
    // Mount only: the refs carry everything that changes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Push-to-talk on the keyboard, while the tab has focus. A listener, not an
  // action: it acts only on the key's own events.
  const phase = useVoiceStore((s) => s.phase);
  const talkMode = useVoiceStore((s) => s.talkMode);
  const pttKey = useVoiceStore((s) => s.pttKey);
  const listenOnly = useVoiceStore((s) => s.listenOnly);
  const pttLive = phase === 'connected' && talkMode === 'ptt' && !listenOnly;
  useEffect(() => {
    if (!pttLive) return;
    const down = (e: KeyboardEvent) => {
      if (e.code !== pttKey || isEditable(e.target)) return;
      e.preventDefault();
      if (!e.repeat) ctrlRef.current?.setPttHeld(true);
    };
    const up = (e: KeyboardEvent) => {
      if (e.code === pttKey) ctrlRef.current?.setPttHeld(false);
    };
    // A key released while the tab was in the background never reaches us.
    const release = () => ctrlRef.current?.setPttHeld(false);
    const hidden = () => {
      if (document.visibilityState === 'hidden') release();
    };
    window.addEventListener('keydown', down);
    window.addEventListener('keyup', up);
    window.addEventListener('blur', release);
    document.addEventListener('visibilitychange', hidden);
    return () => {
      window.removeEventListener('keydown', down);
      window.removeEventListener('keyup', up);
      window.removeEventListener('blur', release);
      document.removeEventListener('visibilitychange', hidden);
      release();
    };
  }, [pttLive, pttKey]);

  const actions = useMemo<VoiceActions>(
    () => ({
      join: (listenOnly) => {
        // From the click: the shared context may be waiting for a gesture.
        void mixerRef.current?.resume();
        void ctrlRef.current?.join(listenOnly);
      },
      leave: () => ctrlRef.current?.leave(),
      retry: () => {
        void mixerRef.current?.resume();
        ctrlRef.current?.retry();
      },
      setMuted: (m) => ctrlRef.current?.setSelfMuted(m),
      setDeafened: (d) => ctrlRef.current?.setSelfDeafened(d),
      setTalkMode: (mode) => {
        saveTalkMode(mode);
        ctrlRef.current?.setTalkMode(mode);
      },
      setPttHeld: (held) => ctrlRef.current?.setPttHeld(held),
      setPttKey: (code) => {
        if (!validPttKey(code)) return;
        savePttKey(code);
        useVoiceStore.getState().set({ pttKey: code });
      },
      setSpeakerVolume: (session, name, volume, muted) => {
        saveSpeakerVolume(name, volume);
        ctrlRef.current?.setSpeakerVolume(session, muted ? 0 : volume);
      },
    }),
    [mixerRef],
  );

  const events = useMemo<RoomVoiceEvents>(
    () => ({
      onTicket: (url, ticket) => ctrlRef.current?.onTicket(url, ticket),
      onRejected: (reason, message) => ctrlRef.current?.onTicketRejected(reason, message),
    }),
    [],
  );

  return { actions, events };
}
