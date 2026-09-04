import { useCallback, useEffect, useRef, useState } from 'react';
import styles from './broadcaster.module.css';
import { LadderPicker } from '../stream/LadderPicker';
import { EncoderSettingsPanel } from '../stream/EncoderSettingsPanel';
import { useCodecMatrices, useSupportMatrix } from '../stream/useSupportMatrix';
import { Button } from '../../ui/Button';
import { GlassPanel } from '../../ui/GlassPanel';
import { IconButton } from '../../ui/IconButton';
import { Toast } from '../../ui/Toast';
import {
  CloseIcon,
  CopyIcon,
  EyeIcon,
  GearIcon,
  LeaveIcon,
  PeopleIcon,
  PlayIcon,
  ScreenIcon,
  StatsIcon,
  StopIcon,
} from '../../ui/Icons';
import { BroadcasterStatsOverlay } from './BroadcasterStatsOverlay';
import { BroadcastStartError, type BroadcastSessionLike, type BroadcastStats } from '../../transport/broadcaster';
import { readVisibility } from '../../lib/visibility';
import { createBroadcastSession } from './workerBroadcastSession';
import type { EncoderConfigured } from '../../media/encoder';
import type { ResolutionSelection } from '../../media/ladder';
import { DEFAULT_CAPTURE_CONFIG, type CaptureConfig } from '../../media/types';
import { encoderSettingsFromStore, useBroadcastSettingsStore } from '../../state/broadcastSettingsStore';
import { resolvedUrlIsDefault, useTransportStore } from '../../state/transportStore';
import { allowCustomRelays, requiresPublishSecret } from '../../config';
import { ServerIndicator } from '../servers/ServerIndicator';
import { ServerPickerPanel } from '../servers/ServerPickerPanel';
import { acceptCurrentTerms, hasAcceptedCurrentTerms } from '../terms/acceptance';
import { DiagnosticsBuffer } from '../../lib/diagnostics';
import { useTelemetryCollector } from '../../lib/useTelemetry';
import type { TelemetryHelloMessage } from '../../transport/wire';
import { buildViewLink } from '../../lib/shareLink';
import { STATS_HOTKEY } from '../../lib/hotkeys';
import { useHotkey } from '../../lib/useHotkey';
import { useWakeLock } from '../../lib/useWakeLock';
import { fmt, fmtWatching } from '../../lib/format';
import { HOME } from '../../routing';
import { log } from '../../lib/logger';
// R42 RM5 (docs/44 §4.8): the Room panel and the in-page room view.
import { RoomView } from '../room/RoomScreen';
import type { RoomTarget } from '../../transport/room-session';
import { parseGrant, readGrant, type RoomGrant } from '../room/grantHandoff';
import { takeRoomReturn } from '../room/roomReturn';
import { loadNickname } from '../room/roomPrefs';
import { isValidRoomCode, parseRoomLink } from '../../lib/roomCode';
import { MAX_ROOM_LABEL_LEN } from '../../transport/wire';
// R24 (docs/30): browser-aware capture & audio guidance — words + dismissible
// reactive notes, gated on the real audio capability (never UA sniffing) and
// never on the start path.
import {
  AUDIO_SETTINGS,
  AUDIO_TIP,
  audioGuidanceForBrowser,
  audioLaneSupported,
  audioReactiveNote,
  captureSurfaceNote,
  dismissHint,
  HINT_AUDIO_MISSING_KEY,
  HINT_WINDOW_SHARE_KEY,
  isHintDismissed,
  WHOLE_SCREEN_TIP,
} from './captureGuidance';

type Status = 'idle' | 'connecting' | 'broadcasting' | 'reconnecting' | 'stopping' | 'error';

// R15 (docs/20): system audio is unconditional on the production broadcaster
// since 2026-07-23 — the experimental toggle is gone. capture.ts owns the
// degradation (a browser that can't start a source gets a video-only grant,
// reported as audioState 'unavailable'), so there is nothing to decide here.
// The frozen `#/debug/*` surfaces keep plain DEFAULT_CAPTURE_CONFIG — audio
// absent — and stay byte-identical.
const BROADCASTER_CAPTURE_CONFIG: CaptureConfig = { ...DEFAULT_CAPTURE_CONFIG, audio: true };

function selectionLabel(selection: ResolutionSelection): string {
  if (selection === 'auto') return 'auto';
  return selection === 'native' ? 'native' : `${selection}p`;
}

function serverHost(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

// The production broadcaster (docs/10 J5): preview-hero, controls float, and
// R3/R4 feedback as quiet badges. Reuses BroadcastPipeline and the reclaim→mint
// fallback verbatim; LadderPicker drops into the gear panel. Server URL / cert
// hash are developer-only (localhost); the publish secret is asked at start
// when the deploy requires one (config.requirePublishSecret).
export function BroadcasterScreen() {
  const pipelineRef = useRef<BroadcastSessionLike | null>(null);
  const videoRef = useRef<HTMLVideoElement | null>(null);

  const [status, setStatus] = useState<Status>('idle');
  const [sourceStream, setSourceStream] = useState<MediaStream | null>(null);
  const [stats, setStats] = useState<BroadcastStats | null>(null);
  const [encoderInfo, setEncoderInfo] = useState<EncoderConfigured | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [broadcastId, setBroadcastId] = useState<string | null>(null);
  const [reclaimFailedNote, setReclaimFailedNote] = useState<string | null>(null);
  const [resumeAttempt, setResumeAttempt] = useState<number | null>(null);
  // R17 W2: the relay-minted resume token, kept next to the broadcast ID (a
  // ref, not state — nothing renders it) so a manual restart can reclaim.
  const resumeTokenRef = useRef<string | null>(null);
  const [copied, setCopied] = useState(false);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [secretPrompt, setSecretPrompt] = useState(false);
  const [secretDraft, setSecretDraft] = useState('');
  // R37 (docs/40 §4.2 F3): set when a secret-less connect to a non-default
  // relay failed — the retry path out of an otherwise opaque CONNECT 401.
  const [secretPromptNote, setSecretPromptNote] = useState<string | null>(null);
  // R23 (docs/29): one-time terms acknowledgment, shown before the first
  // broadcast's transport connect (ahead of the secret prompt below).
  const [termsPrompt, setTermsPrompt] = useState(false);
  const [showStats, setShowStats] = useState(false);
  const [statsCopied, setStatsCopied] = useState(false);

  // R42 RM5 (docs/44 §4.8): the Room panel. `roomTarget` set ⇒ the page
  // renders the room view in place of the preview, with the publish session
  // untouched in pipelineRef — no hash change, the broadcast never stops.
  // A code stashed by a room's "start streaming here" pre-fills the join
  // field and opens the panel (roomReturn.ts).
  const [roomCodeDraft, setRoomCodeDraft] = useState(() => takeRoomReturn() ?? '');
  const [roomPanelOpen, setRoomPanelOpen] = useState(() => roomCodeDraft !== '');
  const [roomTarget, setRoomTarget] = useState<RoomTarget | null>(null);
  const [roomGrant, setRoomGrant] = useState<RoomGrant | null>(null);
  const [roomLabel, setRoomLabel] = useState(() => loadNickname() ?? '');
  const [roomLinkDraft, setRoomLinkDraft] = useState('');
  const [roomSecretDraft, setRoomSecretDraft] = useState('');
  const [roomNote, setRoomNote] = useState<string | null>(null);
  // Bumped on every publish auto-resume so the room view re-sends Attach.
  const [attachEpoch, setAttachEpoch] = useState(0);
  // Set by the own tile's Detach: the broadcast stays live and the
  // broadcaster stays in the room as a participant; no re-attach follows.
  const [ownDetached, setOwnDetached] = useState(false);

  // R24 (docs/30): audio capability answered once, by feature detection. Drives
  // the browser-aware copy and gates the runtime audio-missing note — Firefox
  // (unsupported) is never nagged about audio it cannot have.
  const [audioSupported] = useState(() => audioLaneSupported());
  const guidance = audioGuidanceForBrowser(audioSupported);
  // Reactive notes are an onboarding aid: dismissible, and the dismissal is
  // remembered so an experienced broadcaster is never nagged (decisions 3–4).
  const [audioHintDismissed, setAudioHintDismissed] = useState(() =>
    isHintDismissed(HINT_AUDIO_MISSING_KEY),
  );
  const [windowHintDismissed, setWindowHintDismissed] = useState(() =>
    isHintDismissed(HINT_WINDOW_SHARE_KEY),
  );
  // Controlled (not native <details>) so the reveal animates its real height in
  // both directions — the card's height is content-driven, so it reflows
  // smoothly with it instead of snapping.
  const [tipsOpen, setTipsOpen] = useState(false);

  // R9 M7: rolling stat samples backing "Copy diagnostics" + the sent bitrate.
  const diagRef = useRef(new DiagnosticsBuffer<BroadcastStats>());

  // R28 (docs/33 D13): the send-side collector. Same objects the overlay
  // renders and the diagnostics buffer already holds — this adds a pipe, not
  // a measurement.
  const telemetry = useTelemetryCollector<BroadcastStats>('broadcaster');

  const resolutionSelection = useBroadcastSettingsStore((s) => s.resolutionSelection);

  // R13 (docs/18 L4): probe matrices for picker + codec-pin annotations —
  // advisory only; the overlay's Encode mode row shows the runtime truth.
  // The per-codec set is the expensive one and only probes once the
  // settings panel is open (lazy — see useCodecMatrices).
  const supportMatrix = useSupportMatrix();
  const codecMatrices = useCodecMatrices(settingsOpen);

  // R37 (docs/40 §4.3, F1): the server picker replaced the dev-only inline
  // panel; the resolved server renders in the settings section for context.
  const [showServerPicker, setShowServerPicker] = useState(false);
  const resolvedServerUrl = useTransportStore((s) => s.serverUrl);

  useEffect(() => {
    const el = videoRef.current;
    if (!el) return;
    el.srcObject = sourceStream;
    if (sourceStream) void el.play().catch(() => { /* autoplay races on mount */ });
  }, [sourceStream]);

  const handleStart = useCallback(async () => {
    if (pipelineRef.current) return;
    const { serverUrl, certHashHex, publishSecret } = useTransportStore.getState();
    setError(null);
    setStats(null);
    setEncoderInfo(null);
    // Fresh sample window per broadcast: the pipeline's cumulative counters
    // restart at zero, and mixing sessions would poison the derived rates.
    diagRef.current = new DiagnosticsBuffer<BroadcastStats>();

    const makeCallbacks = (afterFailedReclaim: boolean) => ({
      onSourceStream: (s: MediaStream) => {
        setSourceStream(s);
        setStatus('broadcasting');
      },
      onEncoderConfigured: (info: EncoderConfigured) => setEncoderInfo(info),
      onCapturePathChosen: () => {},
      onStats: (s: BroadcastStats) => {
        // Tab visibility is merged on the main thread because `document` does
        // not exist in the broadcast worker. A backgrounded broadcaster is not
        // the same failure as a stalled one — capture itself is throttled when
        // the tab is hidden — and only the document can tell them apart.
        const next: BroadcastStats = { ...s, ...readVisibility() };
        diagRef.current.push(next);
        telemetry.sample(next);
        setStats(next);
      },
      onBroadcastId: (id: string) => {
        setBroadcastId(id);
        setReclaimFailedNote(
          afterFailedReclaim ? 'Reclaim failed (expired/taken); started a new broadcast.' : null,
        );
      },
      onResumeToken: (token: string) => {
        resumeTokenRef.current = token;
      },
      // R17 W2 auto-resume: session death mid-broadcast is no longer
      // terminal — amber "reconnecting" until the transport re-attaches.
      onReconnecting: (info: { attempt: number }) => {
        telemetry.event('reconnect', `attempt ${info.attempt}`);
        setResumeAttempt(info.attempt);
        setStatus('reconnecting');
      },
      onResumed: () => {
        telemetry.event('resumed');
        setResumeAttempt(null);
        setStatus('broadcasting');
        // R42: the relay's grace GC may have dropped the attachment while the
        // publisher was away; the room view re-sends Attach (idempotent).
        setAttachEpoch((e) => e + 1);
      },
      onError: (err: Error) => {
        telemetry.event('error', err.message);
        setError(err.message);
        setStatus('error');
      },
      onEnded: () => {
        // The broadcast is over — final flush now rather than making the
        // service wait out an idle timeout to finalize the session.
        telemetry.event('ended');
        telemetry.finish();
        setSourceStream(null);
        pipelineRef.current = null;
        setStatus((prev) => (prev === 'error' ? prev : 'idle'));
      },
      // R28: this session's telemetry identity (wire 0x0D). A transport
      // auto-resume delivers a new one, which begins a new telemetry session —
      // the relay session it describes really is a different one.
      onTelemetryHello: (hello: TelemetryHelloMessage) => {
        telemetry.begin(hello);
      },
      // R37 (docs/40 D15/D16): the relay-advertised ingest URL; the
      // disclosure flips only when this session is on a foreign relay.
      onTelemetryEndpoint: (url: string) => {
        telemetry.setAdvertisedUrl(url);
        // URL-keyed, not id-keyed (G3): a saved duplicate of the deployment's
        // own relay is not foreign.
        if (!resolvedUrlIsDefault()) {
          useTransportStore.getState().setForeignTelemetryActive(true);
        }
      },
    });

    const { resolutionSelection: res, framerateSelection } =
      useBroadcastSettingsStore.getState();
    let activeId = broadcastId;
    let triedReclaim = false;

    if (activeId) {
      triedReclaim = true;
      setStatus('connecting');
      const pipeline = await createBroadcastSession(
        BROADCASTER_CAPTURE_CONFIG,
        serverUrl,
        // R17 W2: the reclaim needs the resume token from the prior session.
        { certHashHex, publishSecret, resumeToken: resumeTokenRef.current ?? undefined },
        makeCallbacks(false),
        activeId,
      );
      pipeline.setLadder(res, framerateSelection);
      pipeline.setEncoderSettings(encoderSettingsFromStore());
      pipelineRef.current = pipeline;
      try {
        await pipeline.start();
        return;
      } catch (e) {
        pipelineRef.current = null;
        if (!(e instanceof BroadcastStartError) || e.phase !== 'connect') {
          const err = e instanceof Error ? e : new Error(String(e));
          log.error(err);
          // A start() rejection fires no onEnded (the rejection is our error
          // surface), and a capture-phase failure lands after onSourceStream
          // already flipped the stage to LIVE — reset it here or the screen
          // is stranded on a dead LIVE stage with the error card unreachable.
          setSourceStream(null);
          setError(err.message);
          setStatus('error');
          return;
        }
        log.warn('Reclaim failed, falling back to mint:', e);
        setBroadcastId(null);
        resumeTokenRef.current = null;
        activeId = null;
      }
    }

    setStatus('connecting');
    const pipeline = await createBroadcastSession(
      BROADCASTER_CAPTURE_CONFIG,
      serverUrl,
      { certHashHex, publishSecret },
      makeCallbacks(triedReclaim),
    );
    pipeline.setLadder(res, framerateSelection);
    pipeline.setEncoderSettings(encoderSettingsFromStore());
    pipelineRef.current = pipeline;
    try {
      await pipeline.start();
    } catch (e) {
      const err = e instanceof Error ? e : new Error(String(e));
      log.error(err);
      // Same stage reset as the reclaim catch above: no onEnded follows a
      // start() rejection.
      setSourceStream(null);
      pipelineRef.current = null;
      // R37 (docs/40 §4.2 F3): a connect-phase failure against a non-default
      // relay with no secret presented is offered as "may require a publish
      // secret" + retry — the relay answers a missing secret with a 401 the
      // browser surfaces opaquely, so without this the foreign-secured-relay
      // case dead-ends indistinguishable from "unreachable".
      const resolved = useTransportStore.getState();
      if (
        e instanceof BroadcastStartError &&
        e.phase === 'connect' &&
        resolved.resolvedSource !== 'default' &&
        publishSecret === ''
      ) {
        setStatus('idle');
        setSecretDraft('');
        setSecretPromptNote(
          'The server refused the connection. It may require a publish secret — enter one to retry, or cancel.',
        );
        setSecretPrompt(true);
        return;
      }
      setError(err.message);
      setStatus('error');
    }
  }, [broadcastId, telemetry]);

  const handleStop = useCallback(async () => {
    if (!pipelineRef.current) return;
    setStatus('stopping');
    await pipelineRef.current.stop();
  }, []);

  // Past the terms gate: ask for a publish secret first when one is called
  // for. R37 (docs/40 §4.2 F3): the decision is per resolved server, not per
  // deployment — config.requirePublishSecret governs only the pinned default
  // (pre-filled with any stored value so returning broadcasters just
  // confirm); any other resolved server prompts exactly when its entry holds
  // no secret. Otherwise start straight away.
  const proceedStart = useCallback(() => {
    const { publishSecret, resolvedSource } = useTransportStore.getState();
    const needsPrompt =
      resolvedSource === 'default' ? requiresPublishSecret() : publishSecret === '';
    if (needsPrompt) {
      setSecretDraft(publishSecret);
      setSecretPromptNote(null);
      setSecretPrompt(true);
      return;
    }
    void handleStart();
  }, [handleStart]);

  // "Start a stream" entry point. R23 (docs/29 D5): the terms acknowledgment
  // gate comes first — before connect, before the secret prompt — so nothing
  // touches the transport until the broadcaster has accepted (once per terms
  // version). Viewers are never gated; only broadcasting carries the gate.
  const beginStart = useCallback(() => {
    if (!hasAcceptedCurrentTerms()) {
      setTermsPrompt(true);
      return;
    }
    proceedStart();
  }, [proceedStart]);

  const acceptTermsAndContinue = useCallback(() => {
    acceptCurrentTerms();
    setTermsPrompt(false);
    proceedStart();
  }, [proceedStart]);

  const submitSecret = useCallback(() => {
    // Per-resolved-server storage (F3): the setter writes to whatever entry
    // the store resolves to — the default's record, a saved entry, or
    // session-only memory for an unsaved link override.
    useTransportStore.getState().setPublishSecret(secretDraft.trim());
    setSecretPrompt(false);
    setSecretPromptNote(null);
    void handleStart();
  }, [secretDraft, handleStart]);

  const handleCopy = useCallback(() => {
    if (!broadcastId) return;
    const link = buildViewLink(broadcastId);
    void navigator.clipboard?.writeText(link).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1800);
    });
  }, [broadcastId]);

  useEffect(() => {
    return () => {
      void pipelineRef.current?.stop();
    };
  }, []);

  // Keep the display awake while live. Capturing a screen is not "playing
  // media" to the browser, so an idle broadcaster's OS dims and then sleeps
  // the display — and on macOS a slept display can stop delivering
  // getDisplayMedia frames, which takes the whole broadcast down rather than
  // just dimming one desk. Held through 'reconnecting' (R17 auto-resume keeps
  // capture and the encoder alive across it — the broadcast never stopped).
  // See lib/useWakeLock.ts.
  useWakeLock(status === 'broadcasting' || status === 'reconnecting');

  const copyDiagnostics = useCallback(() => {
    const { resolutionSelection: res, framerateSelection, hwPreference, bitrateOverride, codecOverride } =
      useBroadcastSettingsStore.getState();
    const json = diagRef.current.build({
      surface: 'broadcaster',
      broadcastId,
      encoder: encoderInfo,
      resolutionSelection: res,
      framerateSelection,
      hwPreference,
      bitrateOverride,
      codecOverride,
    });
    void navigator.clipboard?.writeText(json).then(() => {
      setStatsCopied(true);
      setTimeout(() => setStatsCopied(false), 1800);
    });
  }, [broadcastId, encoderInfo]);

  useHotkey(STATS_HOTKEY, () => setShowStats((s) => !s));

  // R42 RM5: the three ways into a room from the broadcast page.
  const canMint = broadcastId !== null && resumeTokenRef.current !== null;
  const enterRoom = useCallback((target: RoomTarget, grant: RoomGrant | null) => {
    setRoomGrant(grant);
    setOwnDetached(false);
    setRoomNote(null);
    setRoomPanelOpen(false);
    setRoomTarget(target);
  }, []);
  const newRoom = useCallback(() => {
    const token = resumeTokenRef.current;
    if (!broadcastId || !token) {
      setRoomNote('Start a stream first — a room is made from a running broadcast.');
      return;
    }
    enterRoom({ kind: 'mint', broadcastId, resumeTokenHex: token, label: roomLabel.trim() }, null);
  }, [broadcastId, roomLabel, enterRoom]);
  const joinRoomByCode = useCallback(() => {
    const code = roomCodeDraft.trim();
    if (!isValidRoomCode(code)) {
      setRoomNote('That doesn’t look like a room code.');
      return;
    }
    // A grant a native launch stashed for this code (grantHandoff.ts) still
    // applies; a typed attach secret wins.
    const secret = roomSecretDraft.trim();
    enterRoom({ kind: 'join', code }, secret !== '' ? { kind: 'attach', secret } : readGrant(code));
  }, [roomCodeDraft, roomSecretDraft, enterRoom]);
  const joinRoomByLink = useCallback(() => {
    const parsed = parseRoomLink(roomLinkDraft);
    if (!parsed) {
      setRoomNote('That doesn’t look like a room link.');
      return;
    }
    const secret = roomSecretDraft.trim();
    const grant: RoomGrant | null =
      secret !== '' ? { kind: 'attach', secret } : parsed.grant ? parseGrant(parsed.grant) : readGrant(parsed.code);
    enterRoom({ kind: 'join', code: parsed.code }, grant);
  }, [roomLinkDraft, roomSecretDraft, enterRoom]);
  // "Source" on the own tile: stop and reclaim, which re-opens the share
  // picker; the resume token keeps the ID, so the attachment goes away and
  // comes back rather than being replaced.
  const changeSource = useCallback(async () => {
    if (!pipelineRef.current) return;
    await handleStop();
    beginStart();
  }, [handleStop, beginStart]);

  const running =
    status === 'connecting' ||
    status === 'broadcasting' ||
    status === 'reconnecting' ||
    status === 'stopping';
  const onStage = sourceStream != null;

  const settingsPanel = settingsOpen && (
    <>
      <div className={styles.scrim} onClick={() => setSettingsOpen(false)} />
      <GlassPanel className={styles.settings}>
        <div className={styles.settingsHead}>
          <span>Settings</span>
          <Button variant="ghost" onClick={() => setSettingsOpen(false)}>
            Done
          </Button>
        </div>

        <section className={styles.group}>
          <h3 className={styles.groupTitle}>Stream quality</h3>
          <LadderPicker
            matrix={supportMatrix}
            onChange={(res, fps) => pipelineRef.current?.setLadder(res, fps)}
          />
        </section>

        <section className={styles.group}>
          <h3 className={styles.groupTitle}>Advanced</h3>
          <EncoderSettingsPanel
            codecMatrices={codecMatrices}
            onChange={(settings) => pipelineRef.current?.setEncoderSettings(settings)}
          />
        </section>

        {/* R24 (docs/30 CG4): a read-only echo of the browser-aware audio
            status — there is no audio *setting* to offer (R15 graduated to
            always-on), so this is information, not a control. */}
        <section className={styles.group}>
          <h3 className={styles.groupTitle}>Audio</h3>
          <p className={styles.settingsAudioNote}>{AUDIO_SETTINGS[guidance]}</p>
        </section>

        {/* R37 (docs/40 §4.3): the server picker replaced the dev-only inline
            fields (F1) — a production surface gated by allowCustomRelays. */}
        {allowCustomRelays() && (
          <section className={styles.group}>
            <h3 className={styles.groupTitle}>Server</h3>
            <p className={styles.settingsAudioNote}>
              Broadcasting to {serverHost(resolvedServerUrl)}
            </p>
            <Button
              variant="secondary"
              disabled={running}
              onClick={() => setShowServerPicker(true)}
            >
              Change server…
            </Button>
          </section>
        )}

        {/* R23 (docs/29): terms reachable from the broadcaster's settings. */}
        <div className={styles.settingsFoot}>
          <a href="#/terms">Terms of use</a>
        </div>
      </GlassPanel>
    </>
  );

  // R42 RM5 (docs/44 §4.8, §4.9 "ways in"): the Room panel — the server
  // picker's idiom (a topbar IconButton opening a glass sheet). New room
  // needs a live broadcast with its resume token; join by code / link work
  // from either stage and attach the broadcast once it is live.
  const roomPanel = roomPanelOpen && (
    <>
      <div className={styles.scrim} onClick={() => setRoomPanelOpen(false)} />
      <GlassPanel className={styles.settings} role="dialog" aria-label="Room">
        <div className={styles.settingsHead}>
          <span>Room</span>
          <Button variant="ghost" onClick={() => setRoomPanelOpen(false)}>
            Done
          </Button>
        </div>

        <section className={styles.group}>
          <h3 className={styles.groupTitle}>Your tile</h3>
          <input
            className={styles.modalInput}
            value={roomLabel}
            maxLength={MAX_ROOM_LABEL_LEN}
            onChange={(e) => setRoomLabel(e.target.value)}
            placeholder="label (shown on your tile)"
            aria-label="Tile label"
            spellCheck={false}
          />
        </section>

        <section className={styles.group}>
          <h3 className={styles.groupTitle}>New room</h3>
          <p className={styles.settingsAudioNote}>
            {canMint
              ? 'Make a room from this broadcast. Others join with the room code or link.'
              : 'Start a stream first — a room is made from a running broadcast.'}
          </p>
          <Button onClick={newRoom} disabled={!canMint}>
            <PeopleIcon /> New room
          </Button>
        </section>

        <section className={styles.group}>
          <h3 className={styles.groupTitle}>Join a room</h3>
          <form
            className={styles.modalForm}
            onSubmit={(e) => {
              e.preventDefault();
              joinRoomByCode();
            }}
          >
            <input
              className={styles.modalInput}
              value={roomCodeDraft}
              onChange={(e) => setRoomCodeDraft(e.target.value)}
              placeholder="room code"
              aria-label="Room code"
              autoCapitalize="characters"
              spellCheck={false}
            />
            <Button type="submit" variant="secondary" disabled={roomCodeDraft.trim() === ''}>
              Join by code
            </Button>
          </form>
          <form
            className={styles.modalForm}
            onSubmit={(e) => {
              e.preventDefault();
              joinRoomByLink();
            }}
          >
            <input
              className={styles.modalInput}
              value={roomLinkDraft}
              onChange={(e) => setRoomLinkDraft(e.target.value)}
              placeholder="room link"
              aria-label="Room link"
              spellCheck={false}
            />
            <input
              type="password"
              className={styles.modalInput}
              value={roomSecretDraft}
              onChange={(e) => setRoomSecretDraft(e.target.value)}
              placeholder="attach secret (static rooms only)"
              aria-label="Attach secret"
              autoComplete="off"
            />
            <Button type="submit" variant="secondary" disabled={roomLinkDraft.trim() === ''}>
              Use a room link
            </Button>
          </form>
          {roomNote && <p className={styles.note}>{roomNote}</p>}
          <p className={styles.settingsAudioNote}>
            Your broadcast is attached when it is live; the room sees your tile, everyone else keeps their own code.
          </p>
        </section>
      </GlassPanel>
    </>
  );

  // R42 RM5: the room view, rendered in place of the preview while attached.
  // The preview <video> stays mounted (hidden) so its srcObject survives the
  // hop back. Own-tile controls: Stop, Source, Quality, Stats (+ Detach,
  // added by the room view).
  if (roomTarget) {
    const ownControls = (
      <>
        <IconButton label="Stop broadcast" className={styles.stopBtn} onClick={handleStop} disabled={!running}>
          <StopIcon />
        </IconButton>
        <IconButton label="Change source" onClick={() => void changeSource()} disabled={status !== 'broadcasting'}>
          <ScreenIcon />
        </IconButton>
        <IconButton label="Quality" onClick={() => setSettingsOpen((o) => !o)}>
          <GearIcon />
        </IconButton>
        <IconButton label={showStats ? 'Hide stats' : 'Show stats'} onClick={() => setShowStats((s) => !s)}>
          <StatsIcon />
        </IconButton>
      </>
    );
    const own =
      broadcastId && resumeTokenRef.current && !ownDetached
        ? {
            broadcastId,
            resumeTokenHex: resumeTokenRef.current,
            label: roomLabel.trim(),
            attachEpoch,
            preview: sourceStream,
            controls: ownControls,
            onDetach: () => setOwnDetached(true),
          }
        : null;
    return (
      <div className={styles.root}>
        <video ref={videoRef} className={styles.preview} muted playsInline hidden />
        <RoomView target={roomTarget} grant={roomGrant} own={own} onLeave={() => setRoomTarget(null)} />
        {showStats && (
          <BroadcasterStatsOverlay
            stats={stats}
            audioSupported={audioSupported}
            encoderInfo={encoderInfo}
            bitrateBps={(() => {
              const bytesRate = diagRef.current.rate((s) => s.bytesSent);
              return bytesRate == null ? null : bytesRate * 8;
            })()}
            onClose={() => setShowStats(false)}
            onCopy={copyDiagnostics}
            copied={statsCopied}
          />
        )}
        {settingsPanel}
        {showServerPicker && <ServerPickerPanel onClose={() => setShowServerPicker(false)} />}
      </div>
    );
  }

  // R24 (docs/30 CG3): reactive live notes, read from signals that already
  // exist — stats.audioState (from the pipeline) and the preview stream's
  // capture surface (UI-local, fully optional-chained so a teardown race or a
  // trackless test fake yields undefined → no hint, never a throw). Each fires
  // only where it is actionable and not previously dismissed.
  const displaySurface = sourceStream?.getVideoTracks?.()[0]?.getSettings?.().displaySurface;
  const audioNote = audioHintDismissed
    ? null
    : audioReactiveNote(stats?.audioState ?? 'off', audioSupported);
  const windowNote = windowHintDismissed ? null : captureSurfaceNote(displaySurface);

  // ── Live stage ──
  if (onStage) {
    return (
      <div className={styles.root}>
        <video ref={videoRef} className={styles.preview} muted playsInline />

        <div className={styles.topbar}>
          <div className={styles.left}>
            <span className={styles.liveBadge}>
              <span className={styles.liveDot} aria-hidden="true" />
              LIVE
            </span>
            {broadcastId && (
              <span className={styles.code}>
                <code>{broadcastId}</code>
                <IconButton label={copied ? 'Copied' : 'Copy join link'} onClick={handleCopy}>
                  <CopyIcon />
                </IconButton>
              </span>
            )}
            {status === 'reconnecting' && (
              <span className={`${styles.badge} ${styles.warnBadge}`}>
                Reconnecting{resumeAttempt != null ? ` (attempt ${resumeAttempt})` : ''}…
              </span>
            )}
            {/* R18 (docs/23 Decision 7): the live audience figure, in the
                topbar slot docs/10 reserved for it. */}
            {stats?.viewerCount != null && (
              <span className={`${styles.badge} ${styles.watchingBadge}`}>
                <EyeIcon /> {fmtWatching(stats.viewerCount)}
              </span>
            )}
            {renderAutoBadge(stats)}
            {stats?.encoderPressure && (
              <span className={`${styles.badge} ${styles.warnBadge}`}>
                Can’t keep up at {selectionLabel(resolutionSelection)}
              </span>
            )}
          </div>
          <div className={styles.right}>
            {encoderInfo && (
              <span className={styles.sending}>
                {encoderInfo.width}×{encoderInfo.height} @ {fmt(encoderInfo.framerate, 0)}
              </span>
            )}
            <IconButton label={showStats ? 'Hide stats' : 'Show stats'} onClick={() => setShowStats((s) => !s)}>
              <StatsIcon />
            </IconButton>
            <IconButton label="Settings" onClick={() => setSettingsOpen((o) => !o)}>
              <GearIcon />
            </IconButton>
            {/* R42 RM5: the Room panel, in the server-picker idiom. */}
            <IconButton label="Room" onClick={() => setRoomPanelOpen((o) => !o)}>
              <PeopleIcon />
            </IconButton>
            <IconButton label="Stop broadcast" className={styles.stopBtn} onClick={handleStop} disabled={status === 'connecting'}>
              <StopIcon />
            </IconButton>
          </div>
        </div>

        {(audioNote || windowNote) && (
          <div className={styles.hints}>
            {audioNote && (
              <div className={styles.hint} role="status">
                <span>{audioNote.text}</span>
                <IconButton
                  label="Dismiss audio note"
                  className={styles.hintDismiss}
                  onClick={() => {
                    dismissHint(HINT_AUDIO_MISSING_KEY);
                    setAudioHintDismissed(true);
                  }}
                >
                  <CloseIcon />
                </IconButton>
              </div>
            )}
            {windowNote && (
              <div className={styles.hint} role="status">
                <span>{windowNote.text}</span>
                <IconButton
                  label="Dismiss window note"
                  className={styles.hintDismiss}
                  onClick={() => {
                    dismissHint(HINT_WINDOW_SHARE_KEY);
                    setWindowHintDismissed(true);
                  }}
                >
                  <CloseIcon />
                </IconButton>
              </div>
            )}
          </div>
        )}

        {showStats && (
          <BroadcasterStatsOverlay
            stats={stats}
            audioSupported={audioSupported}
            encoderInfo={encoderInfo}
            bitrateBps={(() => {
              const bytesRate = diagRef.current.rate((s) => s.bytesSent);
              return bytesRate == null ? null : bytesRate * 8;
            })()}
            onClose={() => setShowStats(false)}
            onCopy={copyDiagnostics}
            copied={statsCopied}
          />
        )}

        {copied && <Toast className={styles.toast}>Join link copied</Toast>}
        {settingsPanel}
        {roomPanel}
        {showServerPicker && <ServerPickerPanel onClose={() => setShowServerPicker(false)} />}
        <ServerIndicator />
      </div>
    );
  }

  // ── Pre-start ──
  return (
    <div className={styles.root}>
      <div className={styles.bg} aria-hidden="true" />
      <div className={styles.prestart}>
        <GlassPanel className={styles.card}>
          <div className={styles.brand}>gawk</div>
          <p className={styles.kicker}>broadcast</p>

          {status === 'error' ? (
            <>
              <h1 className={styles.title}>Couldn’t start</h1>
              <p className={styles.errorText}>{error}</p>
              <Button onClick={beginStart}>Try again</Button>
            </>
          ) : status === 'connecting' || status === 'stopping' ? (
            <>
              <div className={styles.spinner} aria-hidden="true" />
              <p className={styles.cardText}>
                {status === 'connecting' ? 'Starting…' : 'Stopping…'}
              </p>
            </>
          ) : (
            <>
              <h1 className={styles.title}>Start a stream</h1>
              <p className={styles.cardText}>Share a screen or window; you’ll get a code to hand out.</p>
              <Button className={styles.startBtn} onClick={beginStart}>
                <PlayIcon /> Start a stream
              </Button>
              {/* R24 (docs/30 CG2): a collapsed-by-default disclosure —
                  controlled (not native <details>) so the reveal animates its
                  real height, letting the card grow smoothly with it. Toggling
                  it touches nothing on the start path. Experienced eyes skip
                  the one quiet line; a first-timer opens the browser-aware
                  tips. */}
              <div className={styles.tips}>
                <button
                  type="button"
                  className={styles.tipsSummary}
                  aria-expanded={tipsOpen}
                  aria-controls="sharing-tips"
                  onClick={() => setTipsOpen((o) => !o)}
                >
                  Sharing tips
                </button>
                <div
                  id="sharing-tips"
                  className={`${styles.tipsBody} ${tipsOpen ? styles.tipsBodyOpen : ''}`}
                >
                  {/* overflow-hidden inner: the grid row animates 0fr↔1fr and
                      clips the content during the transition. */}
                  <div className={styles.tipsInner}>
                    <p className={styles.tipText}>{WHOLE_SCREEN_TIP}</p>
                    <p className={styles.tipText}>{AUDIO_TIP[guidance]}</p>
                  </div>
                </div>
              </div>
            </>
          )}

          {reclaimFailedNote && <p className={styles.note}>{reclaimFailedNote}</p>}

          <div className={styles.cardFoot}>
            <Button variant="ghost" onClick={() => setSettingsOpen((o) => !o)}>
              <GearIcon /> Settings
            </Button>
            <Button variant="ghost" onClick={() => setRoomPanelOpen((o) => !o)}>
              <PeopleIcon /> Room
            </Button>
            <Button variant="ghost" onClick={() => (window.location.hash = HOME)}>
              <LeaveIcon /> Home
            </Button>
          </div>
        </GlassPanel>
      </div>
      {settingsPanel}
      {roomPanel}

      {termsPrompt && (
        <>
          <div className={styles.scrim} onClick={() => setTermsPrompt(false)} />
          <div className={styles.modalCenter}>
            <GlassPanel className={styles.modal} role="dialog" aria-label="Terms of use">
              <h2 className={styles.modalTitle}>Before you broadcast</h2>
              <p className={styles.cardText}>
                You are about to publish your screen to anyone holding your join code. You are
                responsible for the content you stream, which must be lawful where you are. The
                service is provided as is, with no warranty.
              </p>
              <p className={styles.cardText}>
                By continuing you agree to the{' '}
                <a href="#/terms" target="_blank" rel="noopener noreferrer">
                  Terms of use
                </a>
                .
              </p>
              <div className={styles.modalActions}>
                <Button variant="secondary" onClick={() => setTermsPrompt(false)}>
                  Cancel
                </Button>
                <Button onClick={acceptTermsAndContinue}>Agree &amp; continue</Button>
              </div>
            </GlassPanel>
          </div>
        </>
      )}

      {showServerPicker && <ServerPickerPanel onClose={() => setShowServerPicker(false)} />}
      {/* R37 (docs/40 §4.3 F2): rendered before capture is granted or a
          secret entered — the crafted-link warning lives on this screen. */}
      <ServerIndicator />

      {secretPrompt && (
        <>
          <div className={styles.scrim} onClick={() => setSecretPrompt(false)} />
          <div className={styles.modalCenter}>
            <GlassPanel className={styles.modal} role="dialog" aria-label="Publish secret">
              <h2 className={styles.modalTitle}>Publish secret</h2>
              <p className={styles.cardText}>
                {secretPromptNote ?? 'This relay requires a secret to broadcast.'}
              </p>
              <form
                className={styles.modalForm}
                onSubmit={(e) => {
                  e.preventDefault();
                  submitSecret();
                }}
              >
                <input
                  type="password"
                  className={styles.modalInput}
                  value={secretDraft}
                  onChange={(e) => setSecretDraft(e.target.value)}
                  placeholder="shared secret"
                  autoComplete="current-password"
                  spellCheck={false}
                  // eslint-disable-next-line jsx-a11y/no-autofocus -- the field is the modal's sole purpose
                  autoFocus
                />
                <div className={styles.modalActions}>
                  <Button type="button" variant="secondary" onClick={() => setSecretPrompt(false)}>
                    Cancel
                  </Button>
                  <Button type="submit">Start broadcasting</Button>
                </div>
              </form>
            </GlassPanel>
          </div>
        </>
      )}
    </div>
  );
}

function renderAutoBadge(stats: BroadcastStats | null) {
  if (!stats || stats.autoRung == null) return null;
  if (stats.autoAtFloor) {
    return (
      <span className={`${styles.badge} ${styles.warnBadge}`}>
        Can’t keep up even at {selectionLabel(stats.autoRung)}
      </span>
    );
  }
  if (stats.autoRung !== 'native') {
    return (
      <span className={`${styles.badge} ${styles.autoBadge}`}>Auto · {selectionLabel(stats.autoRung)}</span>
    );
  }
  return null;
}
