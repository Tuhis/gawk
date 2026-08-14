// R37 (docs/40 §4.3, F2): the in-session server indicator. Rendered on the
// viewer and broadcaster screens themselves — link-borne sessions route
// straight there and never see the landing page, so this is where the
// "you are not on your default server" warning has to live. It renders
// before capture is granted or a secret entered and is not dismissible
// while a non-default resolution is active; on the default server (and with
// no link note to show) it renders nothing at all, keeping both screens
// exactly as they were.

import styles from './servers.module.css';
import { useTransportStore } from '../../state/transportStore';

function hostOf(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

export function ServerIndicator() {
  const resolvedSource = useTransportStore((s) => s.resolvedSource);
  const serverUrl = useTransportStore((s) => s.serverUrl);
  const relayLinkNote = useTransportStore((s) => s.relayLinkNote);
  const foreignTelemetryActive = useTransportStore((s) => s.foreignTelemetryActive);

  const nonDefault = resolvedSource !== 'default';
  if (!nonDefault && relayLinkNote === null) return null;

  if (!nonDefault) {
    // Quiet note only (invalid or disallowed ?relay= — docs/40 §4.2): the
    // session runs on the deployment's own relay and says why.
    return (
      <div className={`${styles.indicator} ${styles.indicatorNoteOnly}`} role="status">
        <span className={styles.indicatorDetail}>{relayLinkNote}</span>
      </div>
    );
  }

  return (
    <div className={styles.indicator} role="status" data-testid="server-indicator">
      <span className={styles.indicatorHost}>
        {resolvedSource === 'override' ? 'Using server from link: ' : 'Using server: '}
        {hostOf(serverUrl)}
      </span>
      {relayLinkNote !== null && <span className={styles.indicatorDetail}>{relayLinkNote}</span>}
      {/* D16: choosing the relay is the telemetry consent; this is the
          disclosure that makes it visible rather than silent. */}
      {foreignTelemetryActive && (
        <span className={styles.indicatorDetail}>
          Diagnostics are shared with this server’s operator.
        </span>
      )}
    </div>
  );
}
