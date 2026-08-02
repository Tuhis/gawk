import { create } from 'zustand';

import { getRelayUrl } from '../config';

// Server connection settings shared by the broadcast and view pages,
// persisted so a dev-cert hash pasted once survives reloads.
const LS_SERVER_URL = 'gawk.serverUrl';
const LS_CERT_HASH = 'gawk.certHashHex';
const LS_PUBLISH_SECRET = 'gawk.publishSecret';

// Precedence: the deployment's configured relay (gawk-app chart
// `config.relayUrl`, rendered into /config.js) > the reference deployment's
// own origin > local dev. A self-hosted install sets the chart value and its
// viewers connect with nothing to paste; without it every origin but
// gawk.ioio.fi fell through to localhost, which is only correct for `npm run
// dev`.
function defaultServerUrl(): string {
  const configured = getRelayUrl();
  if (configured) return configured;
  if (typeof window !== 'undefined' && window.location.hostname === 'gawk.ioio.fi') {
    return 'https://api.gawk.ioio.fi:4433';
  }
  return 'https://localhost:4433';
}

interface TransportSettingsState {
  serverUrl: string;
  // hex(SHA-256(cert DER)) from the gawk-server startup log; empty when the
  // server has a real certificate (or Chrome runs with the SPKI flag).
  certHashHex: string;
  // shared secret for publishing
  publishSecret: string;

  setServerUrl: (url: string) => void;
  setCertHashHex: (hash: string) => void;
  setPublishSecret: (secret: string) => void;
}

export const useTransportStore = create<TransportSettingsState>((set) => ({
  serverUrl: localStorage.getItem(LS_SERVER_URL) ?? defaultServerUrl(),
  certHashHex: localStorage.getItem(LS_CERT_HASH) ?? '',
  publishSecret: localStorage.getItem(LS_PUBLISH_SECRET) ?? '',

  setServerUrl: (serverUrl) => {
    localStorage.setItem(LS_SERVER_URL, serverUrl);
    set({ serverUrl });
  },
  setCertHashHex: (certHashHex) => {
    localStorage.setItem(LS_CERT_HASH, certHashHex);
    set({ certHashHex });
  },
  setPublishSecret: (publishSecret) => {
    localStorage.setItem(LS_PUBLISH_SECRET, publishSecret);
    set({ publishSecret });
  },
}));
