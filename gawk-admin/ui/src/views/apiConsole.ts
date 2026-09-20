// The console's model: what the API page's "Console" drawer knows about the
// contract, computed from the served OpenAPI document (R48, docs/49 D5,
// revised 2026-09-21 — OA5).
//
// Pure functions, no React and no network, because the two properties worth
// testing are properties of this module: the request that leaves the browser
// is built from the DOCUMENT (never from a URL a user could type), and the
// only path it can produce is a relative `api/v1/...` one — the same shape
// `client.ts` uses, for the same sub-path reason, and the shape that makes
// "could the token go off-origin?" a question with no answer needed.
//
// It reads the subset of OpenAPI 3.1 the contract actually uses: `paths` →
// method → `parameters` (path and query, schemas inline or `$ref`'d into
// `components/schemas`), `requestBody.content['application/json'].example`,
// `x-gawk-roles`, and `x-gawk-sensitive` on the responses. Anything else in
// the document is Redoc's to render.

import { OPENAPI_URL } from './apiDocs.ts';

export type Method = 'GET' | 'POST' | 'PUT' | 'DELETE';

const METHODS: readonly Method[] = ['GET', 'POST', 'PUT', 'DELETE'];

/** The operations a user can send from the browser. Order is the document's. */
export interface Operation {
  id: string;
  method: Method;
  /** The path template as documented, e.g. `/api/v1/bans/{id}`. */
  path: string;
  tag: string;
  summary: string;
  roles: string[];
  params: Param[];
  /** The documented request example, pretty-printed; null when the operation has no body. */
  bodyExample: string | null;
  /** The `x-gawk-sensitive-reason` of any response marked sensitive, or null. */
  sensitive: string | null;
}

export interface Param {
  name: string;
  in: 'path' | 'query';
  required: boolean;
  description: string;
  /** Enumerated values, when the schema names them; a `<select>` renders these. */
  enum: string[] | null;
  /** What an empty field means, as text for a placeholder: a default, or a format. */
  hint: string;
}

/** The part of the served document this module reads. Everything is optional: the document is data. */
export interface Document {
  servers?: { url?: string }[];
  /** The top-level tag list, whose order is the sidebar's (and the picker's). */
  tags?: { name?: string }[];
  paths?: Record<string, Record<string, unknown>>;
  components?: Record<string, unknown>;
}

interface RawParam {
  name?: string;
  in?: string;
  required?: boolean;
  description?: string;
  schema?: Record<string, unknown>;
  $ref?: string;
}

interface RawOperation {
  operationId?: string;
  summary?: string;
  tags?: string[];
  parameters?: RawParam[];
  requestBody?: { content?: Record<string, { example?: unknown }> };
  responses?: Record<string, { 'x-gawk-sensitive'?: boolean; 'x-gawk-sensitive-reason'?: string }>;
  'x-gawk-roles'?: string[];
}

/**
 * Follows a local `$ref` (`#/components/schemas/X`) into the document.
 * Anything else — a remote reference, a broken pointer — resolves to `{}`,
 * which renders as a plain text field: the console must not fail to build
 * because one schema is fancier than it understands.
 */
export function resolveRef(doc: Document, value: unknown): Record<string, unknown> {
  if (!value || typeof value !== 'object') return {};
  const ref = (value as { $ref?: unknown }).$ref;
  if (typeof ref !== 'string') return value as Record<string, unknown>;
  if (!ref.startsWith('#/')) return {};
  let node: unknown = doc;
  for (const segment of ref.slice(2).split('/')) {
    if (!node || typeof node !== 'object') return {};
    node = (node as Record<string, unknown>)[segment.replace(/~1/g, '/').replace(/~0/g, '~')];
  }
  return node && typeof node === 'object' ? (node as Record<string, unknown>) : {};
}

function paramOf(doc: Document, raw: RawParam): Param | null {
  const p = raw.$ref ? (resolveRef(doc, raw) as RawParam) : raw;
  if (!p.name || (p.in !== 'path' && p.in !== 'query')) return null;
  const schema = resolveRef(doc, p.schema ?? {});
  const values = Array.isArray(schema.enum) ? schema.enum.map(String) : null;
  const hints: string[] = [];
  if (schema.default !== undefined) hints.push(`default ${String(schema.default)}`);
  else if (typeof schema.format === 'string') hints.push(schema.format);
  else if (typeof schema.type === 'string') hints.push(schema.type);
  if (typeof schema.maximum === 'number') hints.push(`≤ ${schema.maximum}`);
  return {
    name: p.name,
    in: p.in,
    // Path parameters are required by the standard whatever the field says.
    required: p.in === 'path' || p.required === true,
    description: p.description ?? '',
    enum: values,
    hint: hints.join(', '),
  };
}

/**
 * Every operation in the document — including the unauthenticated ones,
 * which are as sendable as any other — grouped in the order of the
 * document's top-level `tags` list, and by path within a tag.
 *
 * Grouped by tag rather than left in path order because the served copy
 * has NO path order to keep: it is YAML converted to JSON by Go, and
 * `paths` comes out sorted alphabetically, which put `/api/v1/bans` first
 * and made "List bans" the default operation. The tag list is the one
 * ordering the author wrote down, and it is the order Redoc's sidebar
 * shows, so the picker and the sidebar agree.
 */
export function listOperations(doc: Document): Operation[] {
  const out: Operation[] = [];
  for (const [path, methods] of Object.entries(doc.paths ?? {})) {
    for (const method of METHODS) {
      const raw = methods[method.toLowerCase()] as RawOperation | undefined;
      if (!raw || typeof raw !== 'object') continue;
      const example = raw.requestBody?.content?.['application/json']?.example;
      let sensitive: string | null = null;
      for (const response of Object.values(raw.responses ?? {})) {
        if (response?.['x-gawk-sensitive']) {
          sensitive = response['x-gawk-sensitive-reason'] ?? 'May contain raw identifiers.';
          break;
        }
      }
      out.push({
        id: raw.operationId ?? `${method} ${path}`,
        method,
        path,
        tag: raw.tags?.[0] ?? 'other',
        summary: raw.summary ?? '',
        roles: raw['x-gawk-roles'] ?? [],
        params: (raw.parameters ?? [])
          .map((p) => paramOf(doc, p))
          .filter((p): p is Param => p !== null),
        bodyExample: raw.requestBody
          ? JSON.stringify(example === undefined ? {} : example, null, 2)
          : null,
        sensitive,
      });
    }
  }
  const order = new Map((doc.tags ?? []).map((t, i) => [t.name ?? '', i]));
  const rank = (op: Operation) => order.get(op.tag) ?? order.size;
  // Stable, so operations within a tag keep their path order.
  return out.map((op, i) => [op, i] as const)
    .sort(([a, i], [b, j]) => rank(a) - rank(b) || i - j)
    .map(([op]) => op);
}

/**
 * The request path for an operation and the values typed into it: RELATIVE,
 * `api/v1/...` with no leading slash, exactly like `client.ts`'s `BASE`.
 *
 * Path values are percent-encoded per segment. Query parameters travel only
 * when they hold something — an empty field means "not sent", never
 * `?limit=`. Throws when a path parameter is empty: the alternative is a
 * request for `bans/` that the server would answer with a misleading 404.
 */
export function buildRequestPath(op: Operation, values: Record<string, string>): string {
  let path = op.path;
  for (const p of op.params) {
    if (p.in !== 'path') continue;
    const value = values[p.name] ?? '';
    if (value === '') throw new Error(`${p.name} is required`);
    path = path.split(`{${p.name}}`).join(encodeURIComponent(value));
  }
  const qs = new URLSearchParams();
  for (const p of op.params) {
    if (p.in !== 'query') continue;
    const value = values[p.name] ?? '';
    if (value !== '') qs.set(p.name, value);
  }
  const query = qs.toString();
  // The document's paths are root-absolute (`/api/v1/...`); the page is not
  // necessarily at the root. Dropping the slash is what makes the two agree.
  return path.replace(/^\//, '') + (query ? `?${query}` : '');
}

/**
 * The same request as a `curl` line against this deployment's public base
 * URL, with `$TOKEN` where the bearer goes — the shape self-hosting §9.8
 * teaches, and what a bot author ends up writing. Never the actual token: the
 * line is meant to be pasted into a terminal, a chat, a ticket.
 */
export function curlFor(doc: Document, op: Operation, requestPath: string, body: string | null): string {
  const base = (doc.servers?.[0]?.url ?? '').replace(/\/$/, '');
  const url = base ? `${base}/${requestPath}` : `/${requestPath}`;
  const parts = ['curl'];
  if (op.method !== 'GET') parts.push('-X', op.method);
  parts.push('-H', shellQuote('Authorization: Bearer $TOKEN'));
  if (body !== null) {
    parts.push('-H', shellQuote('Content-Type: application/json'));
    parts.push('-d', shellQuote(body));
  }
  parts.push(shellQuote(url));
  return parts.join(' ');
}

/** Single quotes, so `$TOKEN` survives untouched and JSON needs no escaping beyond `'`. */
function shellQuote(s: string): string {
  return `'${s.replace(/'/g, `'\\''`)}'`;
}

/**
 * The operation a Redoc fragment names — `#tag/bans/operation/createBan` —
 * or null. Redoc owns `location.hash` while the page is mounted
 * (`router.ts`), so this is how the console learns which operation the
 * reader is looking at.
 */
export function operationFromHash(hash: string): string | null {
  const m = /(?:^|\/)operation\/([^/?]+)/.exec(decodeURIComponent(hash.replace(/^#/, '')));
  return m ? m[1] : null;
}

/** The Redoc fragment for an operation — a link the console can offer back to the docs. */
export function hashForOperation(op: Operation): string {
  return `#tag/${encodeURIComponent(op.tag)}/operation/${encodeURIComponent(op.id)}`;
}

/** Where the console reads the contract from: the same relative document Redoc renders. */
export const CONSOLE_DOCUMENT_URL = OPENAPI_URL;
