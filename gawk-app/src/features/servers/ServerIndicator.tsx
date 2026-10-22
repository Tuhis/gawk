// The in-session server indicator, rendered on the viewer and broadcaster
// screens themselves: link-borne sessions route straight there and never see
// the landing page, so this is where the "you are not on your default server"
// warning has to live. It renders before capture is granted or a secret
// entered and is not dismissible while a non-default resolution is active; on
// the default server with no link note to show it renders nothing at all.

import styles from './servers.module.css';
import { useTransportStore } from '../../state/transportStore';
import { relayHost } from '../../lib/relayUrl';
import { LocalRelaySurfaces } from '../pop/LocalRelayChip';
import { useHasLocalRelaySurface } from '../pop/useLocalRelaySurface';

export function ServerIndicator() {
  const resolvedSource = useTransportStore((s) => s.resolvedSource);
  const serverUrl = useTransportStore((s) => s.serverUrl);
  const relayLinkNote = useTransportStore((s) => s.relayLinkNote);
  const foreignTelemetryActive = useTransportStore((s) => s.foreignTelemetryActive);

  const popSurface = useHasLocalRelaySurface();
  const nonDefault = resolvedSource !== 'default';
  if (!nonDefault && relayLinkNote === null && !popSurface) return null;

  if (resolvedSource === 'pop' || (!nonDefault && relayLinkNote === null)) {
    // R74 (docs/76 §7.3): a session on a PoP shows the PoP's chip, not a
    // "you are not on your default server" warning: home chose it. On home,
    // only the local relay notes are left to say.
    return (
      <div
        className={`${styles.indicator} ${styles.indicatorNoteOnly} ${styles.indicatorInteractive}`}
        role="status"
        data-testid={resolvedSource === 'pop' ? 'server-indicator' : undefined}
      >
        <LocalRelaySurfaces />
        {foreignTelemetryActive && resolvedSource === 'pop' && (
          <span className={styles.indicatorDetail}>
            Diagnostics are shared with this server’s operator.
          </span>
        )}
      </div>
    );
  }

  if (!nonDefault) {
    // Quiet note only (invalid or disallowed ?relay=): the session runs on the
    // deployment's own relay and says why.
    return (
      <div className={`${styles.indicator} ${styles.indicatorNoteOnly}`} role="status">
        <span className={styles.indicatorDetail}>{relayLinkNote}</span>
        {popSurface && <LocalRelaySurfaces />}
      </div>
    );
  }

  return (
    <div className={styles.indicator} role="status" data-testid="server-indicator">
      <span className={styles.indicatorHost}>
        {resolvedSource === 'override' ? 'Using server from link: ' : 'Using server: '}
        {relayHost(serverUrl)}
      </span>
      {relayLinkNote !== null && <span className={styles.indicatorDetail}>{relayLinkNote}</span>}
      {popSurface && <LocalRelaySurfaces />}
      {/* Choosing the relay is the telemetry consent; this is the disclosure
          that makes it visible rather than silent. */}
      {foreignTelemetryActive && (
        <span className={styles.indicatorDetail}>
          Diagnostics are shared with this server’s operator.
        </span>
      )}
    </div>
  );
}
