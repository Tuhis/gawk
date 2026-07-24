import { useEffect, useState } from 'react';
import styles from './terms.module.css';
import { GlassPanel } from '../../ui/GlassPanel';
import { Button } from '../../ui/Button';
import { LeaveIcon } from '../../ui/Icons';
import { HOME } from '../../routing';
import { getTermsUrl } from '../../config';
import { BundledTerms } from './BundledTerms';
import { sanitizeTermsHtml } from './sanitize';

// R23 (docs/29 §4.2). The terms surface, reachable from every surface and
// gated behind nothing. Renders the bundled default unless the operator has
// set config.termsUrl, in which case that document is fetched on open,
// sanitized, and rendered instead — with the bundled default shown while the
// fetch is in flight and on any failure, so the page is never blank. The fetch
// happens here (on route open), never at app boot.
export function TermsPage() {
  const url = getTermsUrl();
  const [override, setOverride] = useState<string | null>(null);

  useEffect(() => {
    if (!url) return;
    let cancelled = false;
    void (async () => {
      try {
        const res = await fetch(url, { cache: 'no-cache' });
        if (!res.ok) return; // 404 / 5xx → keep the bundled default
        const clean = sanitizeTermsHtml(await res.text()).trim();
        if (!cancelled && clean) setOverride(clean);
      } catch {
        // network error → keep the bundled default
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [url]);

  return (
    <div className={styles.root}>
      <div className={styles.bg} aria-hidden="true" />
      <GlassPanel className={styles.card}>
        <div className={styles.head}>
          <div className={styles.brand}>gawk</div>
          <Button variant="ghost" onClick={() => (window.location.hash = HOME)}>
            <LeaveIcon /> Home
          </Button>
        </div>
        <article className={styles.doc}>
          {override != null ? (
            // Sanitized above (allowlist, DOMParser-inert) — the single
            // dangerouslySetInnerHTML the app has, and the reason sanitize.ts
            // is test-heavy.
            <div dangerouslySetInnerHTML={{ __html: override }} />
          ) : (
            <BundledTerms />
          )}
        </article>
      </GlassPanel>
    </div>
  );
}
