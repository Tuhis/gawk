import { ServerIndicator, useTransportStore } from 'gawk-app';

// ServerIndicator reads the transport store and renders NOTHING on the default
// server with no link note — that is the whole point of it (it must not change
// the viewer/broadcaster screens in the normal case). So each cell drives the
// store into the state it is meant to document before rendering.
const canvas: React.CSSProperties = {
  background: 'var(--bg)',
  color: 'var(--text)',
  fontFamily: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  minHeight: '100%',
  padding: '32px',
};

function withState(patch: Record<string, unknown>) {
  useTransportStore.setState(patch as never);
  return <ServerIndicator />;
}

/** A session routed to a relay named in the join link (`?relay=`). */
export const FromLink = () => (
  <div style={canvas}>
    {withState({
      resolvedSource: 'override',
      serverUrl: 'https://relay.example.org:4433',
      relayLinkNote: null,
      foreignTelemetryActive: false,
    })}
  </div>
);

/**
 * The same, plus the telemetry disclosure: choosing the relay IS the telemetry
 * consent, so this makes it visible rather than silent (docs/40 D16).
 */
export const ForeignTelemetry = () => (
  <div style={canvas}>
    {withState({
      resolvedSource: 'override',
      serverUrl: 'https://relay.example.org:4433',
      relayLinkNote: null,
      foreignTelemetryActive: true,
    })}
  </div>
);

/** A server the user picked themselves, with a note riding beside it. */
export const SelectedWithNote = () => (
  <div style={canvas}>
    {withState({
      resolvedSource: 'selected',
      serverUrl: 'https://gawk.lan:4433',
      relayLinkNote: 'Saved server “Homelab”.',
      foreignTelemetryActive: false,
    })}
  </div>
);

/**
 * Note-only: the `?relay=` in the link was invalid or disallowed, so the
 * session runs on the deployment's own relay and says why.
 */
export const NoteOnly = () => (
  <div style={canvas}>
    {withState({
      resolvedSource: 'default',
      serverUrl: 'https://api.gawk.ioio.fi:4433',
      relayLinkNote: 'The server in that link isn’t allowed here — using this deployment’s relay.',
      foreignTelemetryActive: false,
    })}
  </div>
);
