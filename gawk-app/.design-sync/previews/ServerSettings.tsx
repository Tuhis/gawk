import { ServerSettings, useTransportStore } from 'gawk-app';

// The frozen debug pages' relay fields, bound straight to the transport store.
const canvas: React.CSSProperties = {
  background: 'var(--bg)',
  color: 'var(--text)',
  fontFamily: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  minHeight: '100%',
  padding: '28px',
  maxWidth: '620px',
};

function withState(patch: Record<string, unknown>, disabled: boolean) {
  useTransportStore.setState(patch as never);
  return <ServerSettings disabled={disabled} />;
}

/** Editable, pointed at a local dev relay with its self-signed cert hash. */
export const LocalDevStack = () => (
  <div style={canvas}>
    {withState(
      {
        serverUrl: 'https://localhost:4433',
        certHashHex: '9f2c41a8be07d3155ec6a0b47f8d219c4e5b6a730f81cc92de4471a05b3e8d66',
        publishSecret: 'devsecret',
      },
      false,
    )}
  </div>
);

/** Disabled while a broadcast is live — the relay cannot change mid-session. */
export const LockedWhileLive = () => (
  <div style={canvas}>
    {withState(
      { serverUrl: 'https://api.gawk.ioio.fi:4433', certHashHex: '', publishSecret: '' },
      true,
    )}
  </div>
);
