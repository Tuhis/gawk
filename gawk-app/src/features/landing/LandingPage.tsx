import { useState } from 'react';
import styles from './landing.module.css';
import { CodeInput } from './CodeInput';
import { Button } from '../../ui/Button';
import { GlassPanel } from '../../ui/GlassPanel';
import { isValidBroadcastId } from '../../lib/broadcastId';

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

      {/* R23 (docs/29): terms reachable from the front door, unobtrusively. */}
      <a className={styles.terms} href="#/terms">
        Terms of use
      </a>
    </div>
  );
}
