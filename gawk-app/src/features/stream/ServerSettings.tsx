import { useState } from 'react';

import styles from './stream.module.css';
import { useTransportStore } from '../../state/transportStore';

interface Props {
  disabled: boolean;
}

export function ServerSettings({ disabled }: Props) {
  const serverUrl = useTransportStore((s) => s.serverUrl);
  const certHashHex = useTransportStore((s) => s.certHashHex);
  const publishSecret = useTransportStore((s) => s.publishSecret);
  const setServerUrl = useTransportStore((s) => s.setServerUrl);
  const setCertHashHex = useTransportStore((s) => s.setCertHashHex);
  const setPublishSecret = useTransportStore((s) => s.setPublishSecret);
  // The store normalizes (and replaces an invalid URL with the default), so a
  // half-typed URL lives here until blur/Enter. null = not editing: the field
  // shows the store's value, including changes made elsewhere.
  const [urlDraft, setUrlDraft] = useState<string | null>(null);

  const commitUrl = () => {
    if (urlDraft === null) return;
    setServerUrl(urlDraft);
    setUrlDraft(null);
  };

  return (
    <div className={styles.settings}>
      <div className={styles.field}>
        <label htmlFor="server-url">Server URL</label>
        <input
          id="server-url"
          value={urlDraft ?? serverUrl}
          onChange={(e) => setUrlDraft(e.target.value)}
          onBlur={commitUrl}
          onKeyDown={(e) => {
            if (e.key === 'Enter') commitUrl();
          }}
          disabled={disabled}
          placeholder="https://localhost:4433"
          spellCheck={false}
        />
      </div>
      <div className={styles.field}>
        <label htmlFor="cert-hash">Dev cert hash (hex; empty for a real cert)</label>
        <input
          id="cert-hash"
          value={certHashHex}
          onChange={(e) => setCertHashHex(e.target.value)}
          disabled={disabled}
          placeholder="cert_hash_hex from gawk-server startup log"
          spellCheck={false}
        />
      </div>
      <div className={styles.field}>
        <label htmlFor="publish-secret">Publish Secret (for broadcasters)</label>
        <input
          id="publish-secret"
          type="password"
          value={publishSecret}
          onChange={(e) => setPublishSecret(e.target.value)}
          disabled={disabled}
          placeholder="shared secret configured server-side"
          spellCheck={false}
        />
      </div>
    </div>
  );
}
