// localStorage access that never throws. Storage can be missing or refuse
// every call (blocked site data, private modes, quota), and a preference that
// cannot be remembered must only cost the memory of it, never the page.

export function readStored(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

// null removes the key.
export function writeStored(key: string, value: string | null): void {
  try {
    if (value === null) localStorage.removeItem(key);
    else localStorage.setItem(key, value);
  } catch {
    // The value still holds for this session.
  }
}
