import { useEffect, useRef } from 'react';
import SwaggerUIBundle from 'swagger-ui-dist/swagger-ui-es-bundle.js';
import 'swagger-ui-dist/swagger-ui.css';

import { useSession } from '../auth/AuthContext.tsx';
import { OPENAPI_URL, swaggerOptions } from './apiDocs.ts';
import ui from '../styles/ui.module.css';

/**
 * The API page (R48, docs/49 D5).
 *
 * It exists for one concrete moment: somebody about to write a bot wants to
 * try a call with their own token before writing any code, and an operator
 * wants to see what a token can and cannot reach. Reading the document is not
 * the same as executing against it, which is why this is Swagger UI and not
 * Redoc.
 *
 * **Everything here is served by this binary.** The bundle is embedded, not
 * loaded from a CDN — the portal's CSP is `default-src 'self'` and the
 * no-external-assets test enforces it as a build property — and the document
 * it renders is the one this same deployment serves at `/api/v1/openapi.json`.
 * No user-supplied spec is ever loaded.
 *
 * **This module is the whole reason the view is lazily imported.** Swagger UI
 * is ~1.6 MB of JavaScript; the moderation views must not pay for it, so it
 * lands in its own chunk that the entry chunk does not reference (`App.tsx`
 * imports this with `React.lazy`).
 */
export default function ApiView() {
  const session = useSession();
  const host = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const node = host.current;
    if (!node) return;
    // The token is read through a closure rather than captured, so a request
    // made after a silent refresh carries the CURRENT token rather than the
    // one that happened to be in hand when the page mounted. It is never
    // written anywhere — the in-memory holder stays the only copy (docs/42
    // §4.8 D17).
    const instance = SwaggerUIBundle(swaggerOptions(node, () => session.accessToken()));
    return () => {
      // Swagger UI has no teardown of its own; dropping our reference and
      // emptying the node is what keeps a re-navigation from stacking a second
      // copy of the whole UI under the first.
      void instance;
      node.replaceChildren();
    };
  }, [session]);

  return (
    <section>
      <div className={ui.head}>
        <h1>API</h1>
        <span className={ui.sub}>
          the contract this deployment serves —{' '}
          <a href={OPENAPI_URL} target="_blank" rel="noreferrer">
            openapi.json
          </a>
        </span>
      </div>
      <p className={ui.sub}>
        Requests you execute here are real: they run against this deployment, with your own
        token and your own roles. A kill is a kill.
      </p>
      <div ref={host} data-testid="swagger-ui" />
    </section>
  );
}
