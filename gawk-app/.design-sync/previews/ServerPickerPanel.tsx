import { ServerPickerPanel, useTransportStore } from 'gawk-app';

// The R37 server picker. Two things shape this preview:
//  1. The panel calls reloadFromStorage() on mount, so state poked straight
//     into the store is wiped. Seeding therefore goes through the store's own
//     addServer/selectServer actions, which persist — the same path the real
//     "Add a server" form takes.
//  2. Both network paths are injectable (probeFn / fetchFn), so the cells drive
//     them to fixed results instead of leaving every row spinning.
const canvas: React.CSSProperties = {
  background:
    'radial-gradient(circle at 40% 30%, #232f4c 0%, transparent 60%), var(--bg)',
  color: 'var(--text)',
  fontFamily: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  minHeight: '540px',
  position: 'relative',
};

const noop = () => {};

const SEED = [
  { label: 'Homelab', url: 'https://gawk.lan:4433', publishSecret: 'hunter2' },
  { label: "Friend's relay", url: 'https://relay.example.org:4433' },
];

// Deterministic probe: a different latency per host so the row dots show the
// coarse quality buckets rather than every row reading the same.
const rttByHost: Record<string, number> = {
  'gawk.lan:4433': 3,
  'relay.example.org:4433': 148,
};

const probeFn = (url: string) => {
  let host = url;
  try { host = new URL(url).host; } catch { /* keep the raw string */ }
  return Promise.resolve({
    state: 'ok' as const,
    rttMs: rttByHost[host] ?? 12,
    identity: { name: 'gawk relay', serverVersion: '0.41.0' },
  });
};

const directoryFetch = () =>
  Promise.resolve({
    ok: true,
    json: () =>
      Promise.resolve({
        version: 1,
        servers: [
          { label: 'Community EU', url: 'https://eu.relay.example', managed: true },
          { label: 'Community NA', url: 'https://na.relay.example', managed: true },
        ],
      }),
  } as unknown as Response);

// Seed through the public actions so reloadFromStorage() keeps them. Cells share
// an origin, so clear first — otherwise entries accumulate across captures.
function seed(select?: string) {
  const store = useTransportStore.getState();
  for (const s of store.servers.filter((e) => e.id !== 'default')) store.removeServer(s.id);
  const ids = SEED.map((entry) => useTransportStore.getState().addServer(entry));
  const wanted = select ? ids[SEED.findIndex((e) => e.label === select)] : null;
  useTransportStore.getState().selectServer(wanted ?? 'default');
  return null;
}

/** The saved-server list, each row carrying its own probe result. */
export const SavedServers = () => (
  <div style={canvas}>
    {seed()}
    <ServerPickerPanel onClose={noop} probeFn={probeFn} fetchFn={directoryFetch as typeof fetch} />
  </div>
);

/** A non-default relay selected — the picker marks which one the session uses. */
export const NonDefaultSelected = () => (
  <div style={canvas}>
    {seed('Homelab')}
    <ServerPickerPanel onClose={noop} probeFn={probeFn} fetchFn={directoryFetch as typeof fetch} />
  </div>
);
