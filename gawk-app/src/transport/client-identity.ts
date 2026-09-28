// The client identity a dial carries for the relay's usage metrics (R59,
// docs/61 D1): ?app=web&os=…&browser=… on /publish and /subscribe. Coarse
// by design — the relay folds anything outside its vocabulary into "other",
// and nothing finer than this is ever sent.
//
// `browser` names the ENGINE, because that is what decides which gawk code
// path runs (MSTP or not, the WebKit datagram writer, the iOS MSE path): on
// iOS every browser is WebKit, so Chrome on an iPhone reports `safari`.

export type ClientOS = 'windows' | 'macos' | 'linux' | 'android' | 'ios' | 'chromeos';
export type ClientBrowser = 'chromium' | 'firefox' | 'safari';

export interface ClientIdentity {
  os?: ClientOS;
  browser?: ClientBrowser;
}

// Order matters: iOS and Android UAs also say "Mac OS X" and "Linux", and
// ChromeOS says "Linux" too.
export function detectClientIdentity(userAgent: string): ClientIdentity {
  const ua = userAgent;
  let os: ClientOS | undefined;
  if (/iPhone|iPad|iPod/.test(ua)) os = 'ios';
  else if (/Android/.test(ua)) os = 'android';
  else if (/CrOS/.test(ua)) os = 'chromeos';
  else if (/Windows/.test(ua)) os = 'windows';
  else if (/Macintosh|Mac OS X/.test(ua)) os = 'macos';
  else if (/Linux/.test(ua)) os = 'linux';

  let browser: ClientBrowser | undefined;
  if (os === 'ios') browser = 'safari';
  else if (/Firefox\//.test(ua)) browser = 'firefox';
  else if (/Chrome\/|Chromium\//.test(ua)) browser = 'chromium';
  else if (/Safari\//.test(ua)) browser = 'safari';

  return { os, browser };
}

// Adds the identity parameters to a dial URL. `navigator` exists on the main
// thread and in workers alike (the viewer pipeline may run in either).
export function appendClientIdentity(
  url: URL,
  userAgent: string = typeof navigator === 'undefined' ? '' : navigator.userAgent,
): void {
  const { os, browser } = detectClientIdentity(userAgent);
  url.searchParams.set('app', 'web');
  if (os) url.searchParams.set('os', os);
  if (browser) url.searchParams.set('browser', browser);
}
