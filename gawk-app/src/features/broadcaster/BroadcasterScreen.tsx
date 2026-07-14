import { useCallback, useEffect, useRef, useState } from 'react';
import styles from './broadcaster.module.css';
import { LadderPicker } from '../stream/LadderPicker';
import { Button } from '../../ui/Button';
import { GlassPanel } from '../../ui/GlassPanel';
import { IconButton } from '../../ui/IconButton';
import { CopyIcon, GearIcon, LeaveIcon, PlayIcon, StatsIcon, StopIcon } from '../../ui/Icons';
import { BroadcasterStatsOverlay } from './BroadcasterStatsOverlay';
import { BroadcastStartError, type BroadcastSessionLike, type BroadcastStats } from '../../transport/broadcaster';
import { createBroadcastSession } from './workerBroadcastSession';
import type { EncoderConfigured } from '../../media/encoder';
import type { ResolutionSelection } from '../../media/ladder';
import { DEFAULT_CAPTURE_CONFIG } from '../../media/types';
import { useBroadcastSettingsStore } from '../../state/broadcastSettingsStore';
import { useTransportStore } from '../../state/transportStore';
import { isDevEnvironment, requiresPublishSecret } from '../../config';
import { DiagnosticsBuffer } from '../../lib/diagnostics';
import { STATS_HOTKEY } from '../../lib/hotkeys';
import { useHotkey } from '../../lib/useHotkey';
import { fmt } from '../../lib/format';
import { HOME } from '../../routing';
import { log } from '../../lib/logger';

type Status = 'idle' | 'connecting' | 'broadcasting' | 'stopping' | 'error';

function selectionLabel(selection: ResolutionSelection): string {
  if (selection === 'auto') return 'auto';
  return selection === 'native' ? 'native' : `${selection}p`;
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
  const [copied, setCopied] = useState(false);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [secretPrompt, setSecretPrompt] = useState(false);
  const [secretDraft, setSecretDraft] = useState('');
  const [showStats, setShowStats] = useState(false);
  const [statsCopied, setStatsCopied] = useState(false);

  // R9 M7: rolling stat samples backing "Copy diagnostics" + the sent bitrate.
  const diagRef = useRef(new DiagnosticsBuffer<BroadcastStats>());

  const resolutionSelection = useBroadcastSettingsStore((s) => s.resolutionSelection);

  // Developer-only settings (localhost). Wired straight to the transport store.
  const showDevSettings = isDevEnvironment();
  const serverUrl = useTransportStore((s) => s.serverUrl);
  const certHashHex = useTransportStore((s) => s.certHashHex);
  const setServerUrl = useTransportStore((s) => s.setServerUrl);
  const setCertHashHex = useTransportStore((s) => s.setCertHashHex);

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
        diagRef.current.push(s);
        setStats(s);
      },
      onBroadcastId: (id: string) => {
        setBroadcastId(id);
        setReclaimFailedNote(
          afterFailedReclaim ? 'Reclaim failed (expired/taken); started a new broadcast.' : null,
        );
      },
      onError: (err: Error) => {
        setError(err.message);
        setStatus('error');
      },
      onEnded: () => {
        setSourceStream(null);
        pipelineRef.current = null;
        setStatus((prev) => (prev === 'error' ? prev : 'idle'));
      },
    });

    const { resolutionSelection: res, framerateRung } = useBroadcastSettingsStore.getState();
    let activeId = broadcastId;
    let triedReclaim = false;

    if (activeId) {
      triedReclaim = true;
      setStatus('connecting');
      const pipeline = await createBroadcastSession(
        { ...DEFAULT_CAPTURE_CONFIG },
        serverUrl,
        { certHashHex, publishSecret },
        makeCallbacks(false),
        activeId,
      );
      pipeline.setLadder(res, framerateRung);
      pipelineRef.current = pipeline;
      try {
        await pipeline.start();
        return;
      } catch (e) {
        pipelineRef.current = null;
        if (!(e instanceof BroadcastStartError) || e.phase !== 'connect') {
          const err = e instanceof Error ? e : new Error(String(e));
          log.error(err);
          setError(err.message);
          setStatus('error');
          return;
        }
        log.warn('Reclaim failed, falling back to mint:', e);
        setBroadcastId(null);
        activeId = null;
      }
    }

    setStatus('connecting');
    const pipeline = await createBroadcastSession(
      { ...DEFAULT_CAPTURE_CONFIG },
      serverUrl,
      { certHashHex, publishSecret },
      makeCallbacks(triedReclaim),
    );
    pipeline.setLadder(res, framerateRung);
    pipelineRef.current = pipeline;
    try {
      await pipeline.start();
    } catch (e) {
      const err = e instanceof Error ? e : new Error(String(e));
      log.error(err);
      setError(err.message);
      setStatus('error');
      pipelineRef.current = null;
    }
  }, [broadcastId]);

  const handleStop = useCallback(async () => {
    if (!pipelineRef.current) return;
    setStatus('stopping');
    await pipelineRef.current.stop();
  }, []);

  // "Start a stream" entry point: when the deploy requires a publish secret,
  // ask for it first (pre-filled with any stored value so returning
  // broadcasters just confirm). Otherwise start straight away.
  const beginStart = useCallback(() => {
    if (requiresPublishSecret()) {
      setSecretDraft(useTransportStore.getState().publishSecret);
      setSecretPrompt(true);
      return;
    }
    void handleStart();
  }, [handleStart]);

  const submitSecret = useCallback(() => {
    useTransportStore.getState().setPublishSecret(secretDraft.trim());
    setSecretPrompt(false);
    void handleStart();
  }, [secretDraft, handleStart]);

  const handleCopy = useCallback(() => {
    if (!broadcastId) return;
    const link = `${window.location.origin}${window.location.pathname}#/view/${broadcastId}`;
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

  const copyDiagnostics = useCallback(() => {
    const { resolutionSelection: res, framerateRung } = useBroadcastSettingsStore.getState();
    const json = diagRef.current.build({
      surface: 'broadcaster',
      broadcastId,
      encoder: encoderInfo,
      resolutionSelection: res,
      framerateRung,
    });
    void navigator.clipboard?.writeText(json).then(() => {
      setStatsCopied(true);
      setTimeout(() => setStatsCopied(false), 1800);
    });
  }, [broadcastId, encoderInfo]);

  useHotkey(STATS_HOTKEY, () => setShowStats((s) => !s));

  const running = status === 'connecting' || status === 'broadcasting' || status === 'stopping';
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
          <LadderPicker onChange={(res, fps) => pipelineRef.current?.setLadder(res, fps)} />
        </section>

        {showDevSettings && (
          <section className={styles.group}>
            <h3 className={styles.groupTitle}>Development settings</h3>
            {/* Stacked, full-width — no horizontal scroll in the side panel. */}
            <label className={styles.field}>
              <span>Server URL</span>
              <input
                value={serverUrl}
                onChange={(e) => setServerUrl(e.target.value)}
                disabled={running}
                placeholder="https://localhost:4433"
                spellCheck={false}
              />
            </label>
            <label className={styles.field}>
              <span>Dev cert hash (hex; empty for a real cert)</span>
              <input
                value={certHashHex}
                onChange={(e) => setCertHashHex(e.target.value)}
                disabled={running}
                placeholder="cert_hash_hex from the server startup log"
                spellCheck={false}
              />
            </label>
          </section>
        )}
      </GlassPanel>
    </>
  );

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
            <IconButton label="Stop broadcast" className={styles.stopBtn} onClick={handleStop} disabled={status === 'connecting'}>
              <StopIcon />
            </IconButton>
          </div>
        </div>

        {showStats && (
          <BroadcasterStatsOverlay
            stats={stats}
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

        {copied && <div className={styles.toast}>Join link copied</div>}
        {settingsPanel}
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
            </>
          )}

          {reclaimFailedNote && <p className={styles.note}>{reclaimFailedNote}</p>}

          <div className={styles.cardFoot}>
            <Button variant="ghost" onClick={() => setSettingsOpen((o) => !o)}>
              <GearIcon /> Settings
            </Button>
            <Button variant="ghost" onClick={() => (window.location.hash = HOME)}>
              <LeaveIcon /> Home
            </Button>
          </div>
        </GlassPanel>
      </div>
      {settingsPanel}

      {secretPrompt && (
        <>
          <div className={styles.scrim} onClick={() => setSecretPrompt(false)} />
          <div className={styles.modalCenter}>
            <GlassPanel className={styles.modal} role="dialog" aria-label="Publish secret">
              <h2 className={styles.modalTitle}>Publish secret</h2>
              <p className={styles.cardText}>This relay requires a secret to broadcast.</p>
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
