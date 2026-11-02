// R33 (docs/83 §5.4, OD10): the OBS page, `#/broadcast/obs`. Where an OBS
// broadcaster gets their stream key, OBS's settings and the gateway's live
// verdict, for the whole stream. It holds no session: OBS's own Start and
// Stop Streaming are the start and the stop (D8).
//
// States: U3 (this server takes no OBS), S1 (no key yet), and with a key the
// status poll's reading (status.ts describeObsStatus): S2 waiting, S3 live,
// S4 gawk ended the stream, S5 ended, or a refusal.
import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react';
import b from '../broadcaster/broadcaster.module.css';
import s from './obs.module.css';
import { Button } from '../../ui/Button';
import { GlassPanel } from '../../ui/GlassPanel';
import { IconButton } from '../../ui/IconButton';
import { CopyIcon, EyeIcon, LockIcon } from '../../ui/Icons';
import { getWhipStatusUrl, getWhipUrl, requiresPublishSecret, SOURCE_URL } from '../../config';
import { selectCertHashList, useTransportStore } from '../../state/transportStore';
import { acceptCurrentTerms, hasAcceptedCurrentTerms } from '../terms/acceptance';
import { PublishSecretModal, SECRET_MAYBE_REQUIRED_NOTE, TermsPromptModal } from '../broadcaster/PublishPrompts';
import { buildViewLink } from '../../lib/shareLink';
import { fmtWatching } from '../../lib/format';
import { hashWithPathKeepingQuery } from '../../routing';
import { AdvancedChip } from './AdvancedChip';
import { obsIngestAvailable } from './availability';
import { forgetObsKey, loadObsKey, maskObsKey, parseObsKey, saveObsKey, type ObsKey } from './key';
import { MintError, mintObsKey } from './mint';
import {
  clearWhipBackoff,
  describeObsStatus,
  fmtCountdown,
  fmtSeconds,
  MALFORMED_KEY_CAUSE,
  type Cause,
  type ObsView,
  type WhipStatus,
} from './status';
import { useWhipStatus } from './useWhipStatus';

export const OBS_GUIDE_URL = `${SOURCE_URL}/blob/main/docs/obs-setup.md`;

const COPIED_MS = 1800;

type Copied = 'server' | 'key' | 'link' | null;
type Confirm = 'forget' | 'reset' | null;

function backHref(): string {
  return hashWithPathKeepingQuery(window.location.hash, '/broadcast');
}

export function ObsPage() {
  const serverUrl = useTransportStore((st) => st.serverUrl);
  const available = obsIngestAvailable(serverUrl);
  return (
    <div className={s.root}>
      <div className={s.bg} aria-hidden="true" />
      <main className={s.page}>
        <GlassPanel className={s.card}>
          <header className={s.header}>
            <div className={b.brand}>gawk</div>
            <p className={b.kicker}>broadcast · OBS Studio</p>
            <AdvancedChip />
          </header>
          {available ? (
            <ObsKeyFlow key={serverUrl} serverUrl={serverUrl} />
          ) : (
            <>
              <h1 className={s.title}>This server doesn’t take streams from OBS</h1>
              <Footer keyActions={null} />
            </>
          )}
        </GlassPanel>
      </main>
    </div>
  );
}

function Footer({ keyActions }: { keyActions: ReactNode }) {
  return (
    <footer className={s.foot}>
      <a href={backHref()} className={s.footLink}>
        ← Back
      </a>
      {keyActions}
      <a href={OBS_GUIDE_URL} target="_blank" rel="noopener noreferrer" className={`${s.footLink} ${s.footEnd}`}>
        OBS setup guide ↗
      </a>
    </footer>
  );
}

function ObsKeyFlow({ serverUrl }: { serverUrl: string }) {
  const whipUrl = getWhipUrl();
  const statusUrl = getWhipStatusUrl();
  const [obsKey, setObsKey] = useState<ObsKey | null>(() => loadObsKey(serverUrl));
  const [revealed, setRevealed] = useState(false);
  const [copied, setCopied] = useState<Copied>(null);
  const [minting, setMinting] = useState(false);
  const [mintError, setMintError] = useState<string | null>(null);
  const [pasteOpen, setPasteOpen] = useState(false);
  const [pasteDraft, setPasteDraft] = useState('');
  const [pasteError, setPasteError] = useState<string | null>(null);
  const [confirm, setConfirm] = useState<Confirm>(null);
  const [termsPrompt, setTermsPrompt] = useState(false);
  const [secretPrompt, setSecretPrompt] = useState(false);
  const [secretDraft, setSecretDraft] = useState('');
  const [secretNote, setSecretNote] = useState<string | null>(null);
  const unmountedRef = useRef(false);
  const copiedTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    unmountedRef.current = false;
    return () => {
      unmountedRef.current = true;
      if (copiedTimer.current) clearTimeout(copiedTimer.current);
    };
  }, []);

  const { read, readAt, refresh } = useWhipStatus(statusUrl, obsKey?.key ?? null);

  const copy = useCallback((what: Exclude<Copied, null>, text: string) => {
    void navigator.clipboard?.writeText(text).then(() => {
      if (unmountedRef.current) return;
      setCopied(what);
      if (copiedTimer.current) clearTimeout(copiedTimer.current);
      copiedTimer.current = setTimeout(() => setCopied(null), COPIED_MS);
    });
  }, []);

  // The mint (D3). Success replaces the key (a Reset), failure keeps the
  // old one. A refused dial with no secret presented, or with one that may
  // be wrong, asks for it with the broadcaster's modal.
  const doMint = useCallback(async () => {
    const st = useTransportStore.getState();
    const publishSecret = st.publishSecret;
    setMinting(true);
    setMintError(null);
    try {
      const k = await mintObsKey(st.serverUrl, { publishSecret, certHashHex: selectCertHashList(st) });
      if (unmountedRef.current) return;
      saveObsKey(st.serverUrl, k);
      setObsKey(k);
      setRevealed(false);
      setPasteOpen(false);
    } catch (e) {
      if (unmountedRef.current) return;
      const message = e instanceof Error ? e.message : String(e);
      setMintError(`Couldn’t create a key: ${message}`);
      if (e instanceof MintError && e.phase === 'connect') {
        setSecretDraft(publishSecret);
        setSecretNote(SECRET_MAYBE_REQUIRED_NOTE);
        setSecretPrompt(true);
      }
    } finally {
      if (!unmountedRef.current) setMinting(false);
    }
  }, []);

  // As the broadcaster's start: the deployment's requirePublishSecret asks
  // first, pre-filled with the saved secret.
  const proceedMint = useCallback(() => {
    if (requiresPublishSecret()) {
      setSecretDraft(useTransportStore.getState().publishSecret);
      setSecretNote(null);
      setSecretPrompt(true);
      return;
    }
    void doMint();
  }, [doMint]);

  // A key is a licence to broadcast, so its mint carries the broadcaster's
  // one-time terms acknowledgment.
  const beginMint = useCallback(() => {
    if (minting) return;
    if (!hasAcceptedCurrentTerms()) {
      setTermsPrompt(true);
      return;
    }
    proceedMint();
  }, [minting, proceedMint]);

  const submitSecret = useCallback(() => {
    useTransportStore.getState().setPublishSecret(secretDraft.trim());
    setSecretPrompt(false);
    setSecretNote(null);
    void doMint();
  }, [secretDraft, doMint]);

  const savePasted = useCallback(() => {
    const k = parseObsKey(pasteDraft);
    if (k === null) {
      setPasteError('That isn’t a gawk stream key. It starts with gawk1_.');
      return;
    }
    saveObsKey(serverUrl, k);
    setObsKey(k);
    setPasteDraft('');
    setPasteError(null);
    setPasteOpen(false);
    setRevealed(false);
  }, [pasteDraft, serverUrl]);

  const forget = useCallback(() => {
    forgetObsKey(serverUrl);
    setObsKey(null);
    setRevealed(false);
    setConfirm(null);
  }, [serverUrl]);

  const retryNow = useCallback(async () => {
    if (!obsKey) return;
    await clearWhipBackoff(statusUrl, obsKey.key);
    if (!unmountedRef.current) refresh();
  }, [obsKey, statusUrl, refresh]);

  const anyPrompt = termsPrompt || secretPrompt || confirm !== null;
  // Fixed, so a dialog opened from a scrolled page sits in the viewport.
  const prompts = anyPrompt && (
    <div className={s.overlay}>
      {termsPrompt && (
        <TermsPromptModal
          onCancel={() => setTermsPrompt(false)}
          onAccept={() => {
            acceptCurrentTerms();
            setTermsPrompt(false);
            proceedMint();
          }}
        />
      )}
      {secretPrompt && (
        <PublishSecretModal
          note={secretNote}
          draft={secretDraft}
          onDraftChange={setSecretDraft}
          onSubmit={submitSecret}
          onCancel={() => setSecretPrompt(false)}
          submitLabel="Create stream key"
        />
      )}
      {confirm === 'forget' && (
        <ConfirmModal
          label="Forget key"
          title="Forget this key?"
          onCancel={() => setConfirm(null)}
          confirmButton={
            <Button variant="danger" onClick={forget}>
              Forget key
            </Button>
          }
        >
          <p className={s.confirmText}>
            OBS keeps streaming with it; this browser just stops showing its status. Copy it from OBS (Settings →
            Stream) to see it here again.
          </p>
        </ConfirmModal>
      )}
      {confirm === 'reset' && obsKey && (
        <ConfirmModal
          label="Reset key"
          title="Reset this key?"
          onCancel={() => setConfirm(null)}
          confirmButton={
            <Button
              variant="danger"
              onClick={() => {
                setConfirm(null);
                beginMint();
              }}
            >
              Reset key
            </Button>
          }
        >
          <p className={s.confirmText}>
            You get a new key and a new code. Paste the new key into OBS before you stream again, and share the new
            link.
          </p>
          <p className={s.confirmText}>
            The old key keeps working on {obsKey.broadcastId}; gawk can’t switch it off. If it leaked, ask whoever runs
            this server to ban {obsKey.broadcastId}.
          </p>
        </ConfirmModal>
      )}
    </div>
  );

  const mintErrorLine = mintError && (
    <p className={s.error} role="alert">
      {mintError}
    </p>
  );

  // ── S1: no key yet ──
  if (obsKey === null) {
    return (
      <>
        <div className={s.strip}>
          <span>New to streaming? The browser takes one click and needs no other app.</span>
          <a href={backHref()} className={s.link}>
            Start a stream in the browser
          </a>
        </div>
        <h1 className={s.title}>Stream from OBS Studio</h1>
        <p className={s.text}>
          For people who already stream with OBS: send your stream to gawk, and your scenes, overlays and OBS’s audio
          mixer come with you. Viewers join on a code, as with any gawk stream.
        </p>
        <section className={s.box} aria-labelledby="obs-needs">
          <h2 className={s.boxTitle} id="obs-needs">
            You need
          </h2>
          <ul className={s.needs}>
            <li>OBS Studio 30 or later (or another WHIP app, such as FFmpeg or GStreamer)</li>
            <li>An H.264 video encoder (NVENC, AMF, QuickSync or x264)</li>
            <li>A few minutes in OBS’s Settings</li>
          </ul>
        </section>
        <div className={s.actions}>
          <Button onClick={beginMint} disabled={minting}>
            {minting ? 'Creating…' : 'Create stream key'}
          </Button>
          {!pasteOpen && (
            <button type="button" className={s.linkBtn} onClick={() => setPasteOpen(true)}>
              Already have a key? Paste it
            </button>
          )}
        </div>
        {pasteOpen && (
          <form
            className={s.pasteForm}
            onSubmit={(e) => {
              e.preventDefault();
              savePasted();
            }}
          >
            <input
              type="password"
              className={s.input}
              aria-label="Stream key"
              placeholder="gawk1_…"
              value={pasteDraft}
              onChange={(e) => {
                setPasteDraft(e.target.value);
                setPasteError(null);
              }}
              autoComplete="off"
              spellCheck={false}
            />
            <Button type="submit" variant="secondary" disabled={pasteDraft.trim() === ''}>
              Save key
            </Button>
          </form>
        )}
        {pasteError && (
          <p className={s.error} role="alert">
            {pasteError}
          </p>
        )}
        {mintErrorLine}
        <Footer keyActions={null} />
        {prompts}
      </>
    );
  }

  // ── With a key: the poll's reading ──
  const code = obsKey.broadcastId;
  const viewLink = buildViewLink(code);
  const status = read?.kind === 'ok' ? read.status : null;
  const view: ObsView | { kind: 'malformed'; reason: Cause } =
    read?.kind === 'malformed-key'
      ? { kind: 'malformed', reason: MALFORMED_KEY_CAUSE }
      : status
        ? describeObsStatus(status, code)
        : describeObsStatus({ state: 'idle', warnings: [] }, code);

  const steps = (statusBox: ReactNode) => (
    <ol className={s.steps}>
      <li className={s.step}>
        <h3 className={s.stepTitle}>
          In OBS, open <span className={s.mono}>Settings → Stream</span>.
        </h3>
        <dl className={s.grid}>
          <dt>Service</dt>
          <dd>
            <span className={s.value}>WHIP</span>
          </dd>
          <dt>Server</dt>
          <dd>
            <span className={s.value} data-testid="obs-whip-url">
              {whipUrl}
            </span>
            <Button variant="secondary" className={s.small} onClick={() => copy('server', whipUrl)}>
              {copied === 'server' ? 'Copied' : 'Copy'}
            </Button>
          </dd>
          <dt>Bearer Token</dt>
          <dd>
            <span className={s.value} data-testid="obs-key">
              {revealed ? obsKey.key : maskObsKey(obsKey)}
            </span>
            <Button
              variant="secondary"
              className={s.small}
              aria-pressed={revealed}
              onClick={() => setRevealed((r) => !r)}
            >
              {revealed ? 'Hide' : 'Show'}
            </Button>
            <Button
              variant="secondary"
              className={s.small}
              aria-label="Copy key"
              onClick={() => copy('key', obsKey.key)}
            >
              {copied === 'key' ? 'Copied' : 'Copy'}
            </Button>
          </dd>
          <dt>Simulcast</dt>
          <dd>
            <span className={s.value}>Off</span>
            <span className={s.hint}>if your OBS shows it</span>
          </dd>
        </dl>
        <p className={s.lockNote}>
          <LockIcon />
          <span>The token works like a password: anyone who has it can stream on your code. Keep it off screen.</span>
        </p>
      </li>
      <li className={s.step}>
        <h3 className={s.stepTitle}>
          Then <span className={s.mono}>Settings → Output</span>.
        </h3>
        <dl className={s.grid}>
          <dt>Output Mode</dt>
          <dd>
            <span className={s.value}>Advanced</span>
            <span className={s.hint}>Keyframe Interval isn’t in Simple</span>
          </dd>
          <dt>Video Encoder</dt>
          <dd>
            <span className={s.value}>H.264</span>
            <span className={s.hint}>NVENC, AMF, QuickSync or x264</span>
          </dd>
          <dt>Keyframe Interval</dt>
          <dd>
            <span className={s.value}>1 s</span>
            <span className={s.hint}>not 0 (auto), which is often 4 s or more</span>
          </dd>
          <dt>Audio Encoder</dt>
          <dd>
            <span className={s.value}>Opus</span>
          </dd>
        </dl>
      </li>
      <li className={s.step}>
        <h3 className={s.stepTitle}>
          Press <span className={s.mono}>Start Streaming</span> in OBS.
        </h3>
        {statusBox}
      </li>
    </ol>
  );

  const folded = (
    <details className={s.disclosure}>
      <summary>OBS settings for this key</summary>
      {steps(null)}
    </details>
  );

  const joinOn = (
    <section aria-labelledby="obs-join-on">
      <h2 className={s.boxTitle} id="obs-join-on">
        Your viewers join on
      </h2>
      <div className={s.codeRow}>
        <div className={s.codeBoxes} role="img" aria-label={`Code ${code}`}>
          {code.split('').map((ch, i) => (
            <span key={i} className={s.codeBox} aria-hidden="true">
              {ch}
            </span>
          ))}
        </div>
        <Button variant="secondary" onClick={() => copy('link', viewLink)}>
          <CopyIcon /> {copied === 'link' ? 'Copied' : 'Copy link'}
        </Button>
      </div>
    </section>
  );

  const causeBox = (reason: Cause) => (
    <div className={`${s.statusBox} ${s.errorBox}`} role="alert">
      <strong>{reason.cause}</strong>
      {reason.fix && <span className={s.fix}>{reason.fix}</span>}
    </div>
  );

  const footer = (
    <Footer
      keyActions={
        <>
          <Button variant="ghost" onClick={() => setConfirm('reset')} disabled={minting}>
            Reset key
          </Button>
          <Button variant="ghost" onClick={() => setConfirm('forget')}>
            Forget key
          </Button>
        </>
      }
    />
  );

  let body: ReactNode;
  switch (view.kind) {
    case 'live':
      body = (
        <LiveView
          status={status!}
          view={view}
          code={code}
          viewLink={viewLink}
          linkCopied={copied === 'link'}
          onCopyLink={() => copy('link', viewLink)}
          folded={folded}
        />
      );
      break;
    case 'stopped': {
      body = (
        <>
          <h1 className={s.title}>{view.title}</h1>
          <p className={s.text}>{view.body}</p>
          {causeBox(view.reason)}
          {view.retryAfterMs !== null && (
            <BackoffRetry deadline={readAt + view.retryAfterMs} onRetry={() => void retryNow()} />
          )}
          {joinOn}
          {folded}
        </>
      );
      break;
    }
    case 'ended':
      body = (
        <>
          <div role="status">
            <h1 className={s.title}>{view.title}</h1>
            <p className={s.text}>{view.body}</p>
          </div>
          {joinOn}
          {folded}
        </>
      );
      break;
    case 'refused':
    case 'malformed':
      body = (
        <>
          <h1 className={s.title}>Stream from OBS Studio</h1>
          {steps(causeBox(view.reason))}
          {joinOn}
        </>
      );
      break;
    case 'waiting':
      body = (
        <>
          <h1 className={s.title}>Stream from OBS Studio</h1>
          {steps(
            <div className={s.statusBox} role="status">
              <span className={s.statusHead}>
                <span className={s.pulse} aria-hidden="true" />
                {view.title}
              </span>
              <span className={s.fix}>{view.body}</span>
            </div>,
          )}
          {joinOn}
        </>
      );
      break;
  }

  return (
    <>
      {body}
      {mintErrorLine}
      {footer}
      {prompts}
    </>
  );
}

// S4's countdown and Retry now, while the backoff runs. The countdown ticks
// here, between polls; each poll re-bases it from retryAfterMs.
function BackoffRetry({ deadline, onRetry }: { deadline: number; onRetry: () => void }) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, []);
  return (
    <>
      <p className={s.text}>
        If OBS says it failed to connect, that is gawk turning it away for {fmtCountdown(deadline - now)} more,
        so viewers don’t freeze in a loop.
      </p>
      <div className={s.actions}>
        <Button onClick={onRetry}>Retry now</Button>
        <span className={s.hint}>once you’ve changed the setting</span>
      </div>
    </>
  );
}

function LiveView({
  status,
  view,
  code,
  viewLink,
  linkCopied,
  onCopyLink,
  folded,
}: {
  status: WhipStatus;
  view: Extract<ObsView, { kind: 'live' }>;
  code: string;
  viewLink: string;
  linkCopied: boolean;
  onCopyLink: () => void;
  folded: ReactNode;
}) {
  const res = status.resolution ? `${status.resolution.width}×${status.resolution.height}` : null;
  const fps = status.fps !== undefined ? `${Math.round(status.fps)} fps` : null;
  const video = [res, fps].filter(Boolean).join(' · ') || '—';
  const upload =
    status.bitrateKbps !== undefined
      ? `${(status.bitrateKbps / 1000).toFixed(1)} Mb/s${status.lossPct !== undefined ? ` · ${status.lossPct.toFixed(1)} % lost` : ''}`
      : '—';
  const gopWarn = view.warning !== null;
  return (
    <>
      <div className={s.liveBar}>
        <span className={b.liveBadge}>
          <span className={b.liveDot} aria-hidden="true" />
          LIVE
        </span>
        <span className={b.code}>
          <code>{code}</code>
          <IconButton label={linkCopied ? 'Copied' : 'Copy join link'} onClick={onCopyLink}>
            <CopyIcon />
          </IconButton>
        </span>
        {status.viewers !== undefined && (
          <span className={`${b.badge} ${b.watchingBadge}`}>
            <EyeIcon /> {fmtWatching(status.viewers)}
          </span>
        )}
      </div>
      <div role="status">
        <h1 className={s.title}>{view.title}</h1>
        {view.warning && (
          <div className={`${s.statusBox} ${s.warnBox}`} data-testid="obs-warning">
            <strong>{view.warning.cause}</strong>
            <span className={s.fix}>{view.warning.fix}</span>
          </div>
        )}
      </div>
      {/* The checks mean the gateway measured it: the frames, not the
          settings. */}
      <dl className={s.stats}>
        <div className={s.cell}>
          <dt>Video</dt>
          <dd>{video}</dd>
        </div>
        <div className={s.cell}>
          <dt>Codec</dt>
          <dd>
            {status.codec ?? '—'}
            {status.codec && <span className={s.check} aria-label="measured">✓</span>}
          </dd>
        </div>
        <div className={`${s.cell} ${gopWarn ? s.cellWarn : ''}`} data-testid="obs-cell-keyframes">
          <dt>Keyframes</dt>
          <dd>
            {status.gopMs !== undefined ? `every ${fmtSeconds(status.gopMs)} s` : '—'}
            {status.gopMs !== undefined && !gopWarn && (
              <span className={s.check} aria-label="measured">
                ✓
              </span>
            )}
          </dd>
        </div>
        <div className={s.cell}>
          <dt>Upload</dt>
          <dd>{upload}</dd>
        </div>
      </dl>
      <div className={s.actions}>
        <Button onClick={onCopyLink}>
          <CopyIcon /> {linkCopied ? 'Copied' : 'Copy link'}
        </Button>
        <a href={viewLink} target="_blank" rel="noopener noreferrer" className={s.watchLink}>
          Watch as a viewer ↗
        </a>
      </div>
      <p className={s.text}>Stop Streaming in OBS ends the broadcast. Next time, your code is {code} again.</p>
      {folded}
    </>
  );
}

function ConfirmModal({
  label,
  title,
  children,
  confirmButton,
  onCancel,
}: {
  label: string;
  title: string;
  children: ReactNode;
  confirmButton: ReactNode;
  onCancel: () => void;
}) {
  return (
    <>
      <div className={b.scrim} onClick={onCancel} />
      <div className={b.modalCenter}>
        <GlassPanel className={b.modal} role="alertdialog" aria-label={label}>
          <h2 className={b.modalTitle}>{title}</h2>
          {children}
          <div className={b.modalActions}>
            <Button variant="secondary" onClick={onCancel}>
              Cancel
            </Button>
            {confirmButton}
          </div>
        </GlassPanel>
      </div>
    </>
  );
}
