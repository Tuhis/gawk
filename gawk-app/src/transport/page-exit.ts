// Closes the page's open WebTransport sessions when the page goes away.
//
// A browser need not end a document's sessions when it unloads: in
// production a reloaded room tab kept its old room and voice sessions open
// until the whole tab closed, so the relay kept the old participant and the
// bridge its Mumble user, beside the reloaded tab's own. An explicit close()
// on pagehide sends the session close itself.
//
// A page the back/forward cache keeps has had its sessions closed by then,
// and nothing would reopen them (a local close() reports nothing to its
// owner), so such a page reloads when it is shown again.
//
// Window only: a worker has no pagehide; its sessions end with the worker.

export const pageExitHooks = { reload: (): void => window.location.reload() };

const open = new Set<WebTransport>();
let installed = false;
let closedForCache = false;

function onPageHide(e: PageTransitionEvent): void {
  if (open.size === 0) return;
  closedForCache = e.persisted;
  for (const wt of [...open]) {
    open.delete(wt);
    try {
      wt.close();
    } catch {
      // already closing
    }
  }
}

function onPageShow(e: PageTransitionEvent): void {
  if (!e.persisted || !closedForCache) return;
  closedForCache = false;
  pageExitHooks.reload();
}

// Registers an open session to be closed on pagehide; it leaves the set
// when it ends by any other way.
export function closeOnPageExit(wt: WebTransport): void {
  if (typeof window === 'undefined' || typeof window.addEventListener !== 'function') return;
  if (!installed) {
    installed = true;
    window.addEventListener('pagehide', onPageHide);
    window.addEventListener('pageshow', onPageShow);
  }
  open.add(wt);
  const forget = () => open.delete(wt);
  void (wt.closed as Promise<unknown> | undefined)?.then(forget, forget);
}
