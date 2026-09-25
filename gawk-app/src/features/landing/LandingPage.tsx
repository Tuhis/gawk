import { useState } from 'react';
import styles from './landing.module.css';
import { CodeInput } from './CodeInput';
import { Button } from '../../ui/Button';
import { GlassPanel } from '../../ui/GlassPanel';
import { isValidBroadcastId } from '../../lib/broadcastId';
import { SITE_DOWNLOAD_URL, SITE_URL, SOURCE_URL } from '../../config';
import { ServerChip } from '../servers/ServerChip';

// The front door. Segmented code entry is the hero; a smaller
// "start a stream" affordance sits below. A friend handed a #/view/<id> link
// never sees this page.
export function LandingPage() {
  const [code, setCode] = useState('');
  const valid = isValidBroadcastId(code);

  // A typed code may name a room or a broadcast; the #/join/ resolver asks
  // the relay and lands on whichever it is. No "start a room" here on
  // purpose: rooms are made from a running broadcast.
  const join = (id: string = code) => {
    if (isValidBroadcastId(id)) window.location.hash = `#/join/${id}`;
  };

  return (
    <div className={styles.root}>
      <div className={styles.bg} aria-hidden="true" />
      <GlassPanel className={styles.card}>
        <div className={styles.brand}>gawk</div>
        <h1 className={styles.prompt}>Join a stream</h1>

        <CodeInput
          value={code}
          onChange={setCode}
          onComplete={(id) => join(id)}
          onEnter={() => join()}
          autoFocus
        />
        <p className={styles.hint}>enter the 6-character code</p>

        <Button className={styles.join} disabled={!valid} onClick={() => join()}>
          Join
        </Button>

        <div className={styles.divider}>
          <span>or</span>
        </div>

        <button className={styles.startLink} onClick={() => (window.location.hash = '#/broadcast')}>
          Start a stream <span aria-hidden="true">→</span>
        </button>
      </GlassPanel>

      {/* The server chip: quiet on the default relay, hidden when the
          deployment disallows custom relays. */}
      <div className={styles.chipRow}>
        <ServerChip />
      </div>

      {/* Unobtrusive links, all the same quiet weight. "Get the app" is here
          because the native apps are the one thing this UI may send someone
          to fetch; the outbound links open a new tab so the join card is
          never lost. */}
      <footer className={styles.foot}>
        <a href={SITE_URL} target="_blank" rel="noopener noreferrer">
          About
        </a>
        <span className={styles.footSep} aria-hidden="true">
          ·
        </span>
        <a href={SITE_DOWNLOAD_URL} target="_blank" rel="noopener noreferrer">
          Get the app
        </a>
        <span className={styles.footSep} aria-hidden="true">
          ·
        </span>
        <a href="#/terms">Terms of use</a>
        <span className={styles.footSep} aria-hidden="true">
          ·
        </span>
        <a href={SOURCE_URL} target="_blank" rel="noopener noreferrer">
          GitHub
        </a>
      </footer>
    </div>
  );
}
