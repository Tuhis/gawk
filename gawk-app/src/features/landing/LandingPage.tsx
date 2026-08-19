import { useState } from 'react';
import styles from './landing.module.css';
import { CodeInput } from './CodeInput';
import { Button } from '../../ui/Button';
import { GlassPanel } from '../../ui/GlassPanel';
import { isValidBroadcastId } from '../../lib/broadcastId';
import { SOURCE_URL } from '../../config';
import { ServerChip } from '../servers/ServerChip';

// The front door (docs/10 J2). Segmented code entry is the hero; a smaller
// "start a stream" affordance sits below. A friend handed a #/view/<id> link
// never sees this page.
export function LandingPage() {
  const [code, setCode] = useState('');
  const valid = isValidBroadcastId(code);

  const join = (id: string = code) => {
    if (isValidBroadcastId(id)) window.location.hash = `#/view/${id}`;
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

      {/* R37 (docs/40 §4.3): the server chip — quiet on the default relay so
          join-by-code reads exactly as before; hidden when the deployment
          disallows custom relays. */}
      <div className={styles.chipRow}>
        <ServerChip />
      </div>

      {/* R23 (docs/29): terms reachable from the front door, unobtrusively —
          joined since the repository went public by a source link, which is the
          same quiet weight and the only outbound link on the page. */}
      <footer className={styles.foot}>
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
