export function fmt(n: number, digits = 1): string {
  return Number.isFinite(n) ? n.toFixed(digits) : '—';
}

// Nullable-friendly variants for the R9 overlays: connection stats are null
// wherever the browser doesn't expose them, and render as "—".

export function fmtOr(n: number | null | undefined, digits = 1): string {
  return n == null ? '—' : fmt(n, digits);
}

export function fmtInt(n: number | null | undefined): string {
  return n == null || !Number.isFinite(n) ? '—' : String(Math.round(n));
}

// Bits per second, human-scaled ("4.2 Mbps"). Callers with bytes/s multiply
// by 8 themselves so the unit at the call site is explicit.
export function fmtBits(bitsPerSec: number | null | undefined): string {
  if (bitsPerSec == null || !Number.isFinite(bitsPerSec)) return '—';
  if (bitsPerSec >= 1_000_000) return `${(bitsPerSec / 1_000_000).toFixed(1)} Mbps`;
  if (bitsPerSec >= 1_000) return `${(bitsPerSec / 1_000).toFixed(0)} kbps`;
  return `${bitsPerSec.toFixed(0)} bps`;
}
