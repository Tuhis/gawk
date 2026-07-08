const startTime = performance.now();

function stamp(): string {
  const t = (performance.now() - startTime).toFixed(1);
  return `[${t.padStart(9, ' ')}ms]`;
}

export const log = {
  info: (...args: unknown[]) => console.info(stamp(), ...args),
  warn: (...args: unknown[]) => console.warn(stamp(), ...args),
  error: (...args: unknown[]) => console.error(stamp(), ...args),
};
