/** A query result cell as text: objects as JSON, absence as nothing. */
export function cell(v: unknown): string {
  if (v === null || v === undefined) return '';
  if (typeof v === 'object') return JSON.stringify(v);
  return String(v);
}
