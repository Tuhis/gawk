import { useCallback, useEffect, useId, useRef, useState } from 'react';
import { history as redocHistory } from 'redoc';

import {
  CONSOLE_DOCUMENT_URL,
  buildRequestPath,
  curlFor,
  hashForOperation,
  listOperations,
  operationFromHash,
  type Document,
  type Operation,
} from './apiConsole.ts';
import { AuthRedirect, type AuthSession } from '../auth/session.ts';
import { useSession } from '../auth/AuthContext.tsx';
import styles from './ApiConsole.module.css';
import ui from '../styles/ui.module.css';

/**
 * The API page's console: send one documented operation, as the signed-in
 * operator, and read what came back (R48, docs/49 D5 revised 2026-09-21).
 *
 * **In-house, not Swagger UI's "Try it out".** The first cut of the API page
 * had that button and was replaced by Redoc for how the page reads; this
 * brings the ability back without bringing back the thing it cost — a
 * third-party bundle holding the operator's access token. Nothing here sees
 * a token. The request goes through `AuthSession.authorizedFetch`, the ONE
 * place in the SPA a bearer header is attached (`client.ts`), with a path
 * built from the served document by `apiConsole.ts` — relative, `api/v1/...`,
 * so it cannot leave this origin whatever `-external-url` says. There is no
 * URL field for the same reason.
 *
 * A 403 from any operation is the session's "no operator role" 403:
 * `authorizedFetch` flips the whole portal to its forbidden page, as it does
 * for a view. That is the correct reading — the API has one role today — and
 * the console does not second-guess it.
 *
 * It lives beside Redoc rather than inside it: Redoc's DOM is generated and
 * not ours to add buttons to. The two are linked through the hash Redoc
 * writes (`router.ts`): the console opens on the operation the reader is
 * looking at, follows sidebar clicks, and offers a "docs" link back.
 */
export function ApiConsole({
  onClose,
  fetchDocument = defaultFetchDocument,
}: {
  onClose: () => void;
  /** The served contract. A parameter so a test can hand in a small one. */
  fetchDocument?: () => Promise<Document>;
}) {
  const session = useSession();
  const [doc, setDoc] = useState<Document | null>(null);
  const [ops, setOps] = useState<Operation[]>([]);
  const [docError, setDocError] = useState<string | null>(null);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [values, setValues] = useState<Record<string, string>>({});
  const [body, setBody] = useState('');
  const [phase, setPhase] = useState<'idle' | 'confirm' | 'sending'>('idle');
  const [error, setError] = useState<string | null>(null);
  const [result, setResult] = useState<Result | null>(null);
  const [copied, setCopied] = useState(false);
  const selectRef = useRef<HTMLSelectElement>(null);
  const selectId = useId();

  const op = ops.find((o) => o.id === selectedId) ?? null;

  // Selecting an operation resets everything typed for the previous one: the
  // fields belong to the operation, and a stale body from `POST /bans` sent
  // to `POST /webhooks` is not a request anyone meant.
  const select = useCallback(
    (id: string, all: Operation[]) => {
      const next = all.find((o) => o.id === id);
      if (!next) return;
      setSelectedId(id);
      setValues({});
      setBody(next.bodyExample ?? '');
      setPhase('idle');
      setError(null);
      setResult(null);
      setCopied(false);
    },
    [],
  );

  // Load the contract once. Mount-only and it acts, so it reads nothing that
  // changes; `fetchDocument` is fixed for the life of the drawer.
  useEffect(() => {
    let cancelled = false;
    fetchDocument()
      .then((d) => {
        if (cancelled) return;
        const all = listOperations(d);
        setDoc(d);
        setOps(all);
        // Open on what the reader is looking at, else the first operation
        // that needs a token — the probe, which is the natural first send.
        const fromHash = operationFromHash(window.location.hash);
        const initial =
          (fromHash && all.find((o) => o.id === fromHash)) ??
          all.find((o) => o.roles.length > 0) ??
          all[0];
        if (initial) select(initial.id, all);
        selectRef.current?.focus();
      })
      .catch((err: unknown) => {
        if (!cancelled) setDocError(err instanceof Error ? err.message : String(err));
      });
    return () => {
      cancelled = true;
    };
  }, [fetchDocument, select]);

  // Follow the docs: a sidebar click in Redoc pushes `#tag/…/operation/<id>`
  // and its history service emits; so does a hashchange. The scroll spy's
  // `replaceState` emits nothing, so scrolling does not drag the console
  // along — which is right: a half-typed request should not change under
  // the operator because they scrolled to check a field.
  const opsRef = useRef(ops);
  useEffect(() => {
    opsRef.current = ops;
  });
  useEffect(() => {
    return redocHistory.subscribe(() => {
      const id = operationFromHash(window.location.hash);
      if (id) select(id, opsRef.current);
    });
  }, [select]);

  // Escape closes, unless a request is in flight — the same rule Dialog has,
  // for the same reason: the keyboard must not bypass a disabled state.
  const closeRef = useRef(onClose);
  const phaseRef = useRef(phase);
  useEffect(() => {
    closeRef.current = onClose;
    phaseRef.current = phase;
  });
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && phaseRef.current !== 'sending') closeRef.current();
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, []);

  const requestPath = (): string | null => {
    if (!op) return null;
    try {
      return buildRequestPath(op, values);
    } catch {
      return null;
    }
  };

  const validate = (): { path: string; payload: string | null } | null => {
    if (!op) return null;
    let path: string;
    try {
      path = buildRequestPath(op, values);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      return null;
    }
    let payload: string | null = null;
    if (op.bodyExample !== null) {
      try {
        // Re-serialised compactly: what the operator typed is checked, and
        // what travels is exactly what parsed.
        payload = JSON.stringify(JSON.parse(body));
      } catch (err) {
        setError(`The body is not valid JSON: ${err instanceof Error ? err.message : String(err)}`);
        return null;
      }
    }
    setError(null);
    return { path, payload };
  };

  const send = async () => {
    const req = validate();
    if (!req || !op) return;
    setPhase('sending');
    setResult(null);
    const init: RequestInit = { method: op.method };
    if (req.payload !== null) {
      init.headers = { 'Content-Type': 'application/json' };
      init.body = req.payload;
    }
    try {
      setResult(await perform(session, req.path, init));
    } catch (err) {
      // A redirect to the IdP is a navigation, not a failure to report.
      if (err instanceof AuthRedirect) return;
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setPhase('idle');
    }
  };

  // GET goes on the first click. Anything else asks once: this runs against
  // the live deployment as the signed-in operator, and a ban is a ban.
  const onSend = () => {
    if (!op) return;
    if (op.method === 'GET' || phase === 'confirm') {
      void send();
      return;
    }
    if (validate()) setPhase('confirm');
  };

  const copyCurl = async () => {
    if (!doc || !op) return;
    const path = requestPath();
    if (path === null) {
      setError('Fill in the path parameters first.');
      return;
    }
    const line = curlFor(doc, op, path, op.bodyExample !== null ? body : null);
    try {
      await navigator.clipboard.writeText(line);
      setCopied(true);
    } catch {
      setError('The clipboard is not available here; the curl line is shown below.');
      setResult({
        status: 0,
        ok: true,
        elapsedMs: 0,
        contentType: 'text/x-shellscript',
        body: line,
        empty: false,
      });
    }
  };

  const busy = phase === 'sending';
  const tags = [...new Set(ops.map((o) => o.tag))];

  return (
    <aside className={styles.drawer} aria-label="API console">
      <div className={styles.head}>
        <h2 className={styles.title}>Console</h2>
        <span className={ui.sub}>sends as you, to this deployment</span>
        <span className={ui.spacer} />
        <button type="button" onClick={onClose} disabled={busy} aria-label="Close console">
          ×
        </button>
      </div>

      {docError ? <p className={ui.error}>The contract could not be loaded: {docError}</p> : null}

      <div className={ui.field}>
        <label htmlFor={selectId}>Operation</label>
        <select
          id={selectId}
          ref={selectRef}
          value={selectedId ?? ''}
          disabled={busy || ops.length === 0}
          onChange={(e) => select(e.target.value, ops)}
        >
          {tags.map((tag) => (
            <optgroup key={tag} label={tag}>
              {ops
                .filter((o) => o.tag === tag)
                .map((o) => (
                  <option key={o.id} value={o.id}>
                    {o.method} {o.path} — {o.summary}
                  </option>
                ))}
            </optgroup>
          ))}
        </select>
      </div>

      {op ? (
        <>
          <div className={styles.meta}>
            <span className={`${styles.method} ${styles[op.method.toLowerCase()]}`}>{op.method}</span>
            <code className={styles.path}>{op.path}</code>
            <span className={ui.spacer} />
            <a href={hashForOperation(op)} className={styles.docsLink}>
              docs ↗
            </a>
          </div>
          <p className={styles.summary}>
            {op.summary}
            {op.roles.length > 0 ? (
              <span className={ui.badge}>role: {op.roles.join(', ')}</span>
            ) : (
              <span className={ui.badge}>no token needed</span>
            )}
          </p>

          {op.params.length > 0 ? (
            <div className={styles.params}>
              {op.params.map((p) => (
                <div key={`${p.in}:${p.name}`} className={ui.field}>
                  <label htmlFor={`${selectId}-${p.name}`}>
                    <span className={ui.mono}>{p.name}</span>
                    <span className={ui.dim}>
                      {' '}
                      {p.in}
                      {p.required ? ', required' : ''}
                    </span>
                  </label>
                  {p.enum ? (
                    <select
                      id={`${selectId}-${p.name}`}
                      value={values[p.name] ?? ''}
                      disabled={busy}
                      onChange={(e) => setValues({ ...values, [p.name]: e.target.value })}
                    >
                      <option value="">{p.hint ? `(${p.hint})` : '(not sent)'}</option>
                      {p.enum.map((v) => (
                        <option key={v} value={v}>
                          {v}
                        </option>
                      ))}
                    </select>
                  ) : (
                    <input
                      id={`${selectId}-${p.name}`}
                      type="text"
                      value={values[p.name] ?? ''}
                      placeholder={p.hint}
                      disabled={busy}
                      onChange={(e) => setValues({ ...values, [p.name]: e.target.value })}
                    />
                  )}
                  {p.description ? <p className={ui.caveat}>{p.description}</p> : null}
                </div>
              ))}
            </div>
          ) : null}

          {op.bodyExample !== null ? (
            <div className={ui.field}>
              <label htmlFor={`${selectId}-body`}>
                Body <span className={ui.dim}>application/json</span>
              </label>
              <textarea
                id={`${selectId}-body`}
                className={styles.body}
                value={body}
                rows={Math.min(14, Math.max(4, body.split('\n').length + 1))}
                spellCheck={false}
                disabled={busy}
                onChange={(e) => setBody(e.target.value)}
              />
              <p className={ui.caveat}>
                Pre-filled with the documented example.{' '}
                <button
                  type="button"
                  className={styles.linkButton}
                  disabled={busy || body === op.bodyExample}
                  onClick={() => setBody(op.bodyExample ?? '')}
                >
                  Reset
                </button>
              </p>
            </div>
          ) : null}

          {phase === 'confirm' ? (
            <div className={ui.warning}>
              This sends <strong>{op.method}</strong> to this deployment as you, and it will act.
            </div>
          ) : null}

          <div className={ui.actions}>
            {phase === 'confirm' ? (
              <>
                <button type="button" className={ui.danger} onClick={onSend}>
                  Send {op.method}
                </button>
                <button type="button" onClick={() => setPhase('idle')}>
                  Cancel
                </button>
              </>
            ) : (
              <button type="button" className={ui.primary} onClick={onSend} disabled={busy}>
                {busy ? 'Sending…' : op.method === 'GET' ? 'Send' : 'Send…'}
              </button>
            )}
            <span className={ui.spacer} />
            <button type="button" onClick={() => void copyCurl()} disabled={busy}>
              {copied ? 'Copied' : 'Copy as curl'}
            </button>
          </div>

          {error ? <p className={ui.error}>{error}</p> : null}

          {result ? (
            <div className={styles.response} aria-live="polite">
              <div className={styles.responseHead}>
                {result.status > 0 ? (
                  <span className={result.ok ? ui.badgeOk : ui.badgeBad}>{result.status}</span>
                ) : null}
                <span className={ui.dim}>
                  {result.status > 0 ? `${result.elapsedMs} ms` : 'curl'}
                  {result.contentType ? ` · ${result.contentType.split(';')[0]}` : ''}
                </span>
              </div>
              {op.sensitive && result.status > 0 && result.ok ? (
                <p className={ui.caveat}>Sensitive: {op.sensitive}</p>
              ) : null}
              <pre className={styles.pre}>{result.empty ? '(no body)' : result.body}</pre>
            </div>
          ) : null}
        </>
      ) : null}
    </aside>
  );
}

interface Result {
  status: number;
  ok: boolean;
  elapsedMs: number;
  contentType: string;
  body: string;
  empty: boolean;
}

/**
 * One request through the session, timed. Outside the component so the clock
 * reads are plainly in an event's future and not in a render (the purity
 * lint cannot tell an async handler from the render body).
 */
async function perform(session: AuthSession, path: string, init: RequestInit): Promise<Result> {
  const started = performance.now();
  const res = await session.authorizedFetch(path, init);
  const text = await res.text();
  return {
    status: res.status,
    ok: res.ok,
    elapsedMs: Math.round(performance.now() - started),
    contentType: res.headers.get('content-type') ?? '',
    body: prettify(text),
    empty: text.length === 0,
  };
}

/**
 * The contract, from the same relative URL Redoc renders. A bare `fetch` and
 * no bearer, deliberately: the document is unauthenticated (docs/49 D2), and
 * `client.ts`'s "no bare fetch" rule exists to keep the token in one place —
 * a request that carries none is not what it guards.
 */
async function defaultFetchDocument(): Promise<Document> {
  const res = await fetch(CONSOLE_DOCUMENT_URL, { headers: { Accept: 'application/json' } });
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  return (await res.json()) as Document;
}

function prettify(text: string): string {
  try {
    return JSON.stringify(JSON.parse(text), null, 2);
  } catch {
    return text;
  }
}
