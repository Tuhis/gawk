// @vitest-environment jsdom
import { cleanup, fireEvent, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { act, useEffect, useState } from 'react';
import { HashRouter, useNavigate, type NavigateFunction } from 'react-router';

import { ApiConsole } from './ApiConsole.tsx';
import { json, renderWithSession, stubSession, type ApiHandler } from '../testing/harness.tsx';
import { DOC } from '../testing/openapiFixture.ts';

// The console renders inside ApiView's HashRouter, the one Redoc 3 navigates
// when the reader clicks the sidebar. This stands in for Redoc: it hands the
// test the router's navigate, which is exactly what a sidebar click calls.
let navigate: NavigateFunction | null = null;
function Sidebar() {
  const nav = useNavigate();
  useEffect(() => {
    navigate = nav;
  });
  return null;
}

afterEach(() => {
  cleanup();
  navigate = null;
  window.history.replaceState(null, '', '/');
});

beforeEach(() => {
  window.history.replaceState(null, '', '/');
});

const fetchDocument = () => Promise.resolve(DOC);

function inRouter(node: React.ReactNode) {
  return (
    <HashRouter>
      <Sidebar />
      {node}
    </HashRouter>
  );
}

function mount(handler: ApiHandler = () => json({ ok: true }), onClose = () => undefined) {
  const session = stubSession(handler);
  renderWithSession(inRouter(<ApiConsole onClose={onClose} fetchDocument={fetchDocument} />), session);
  return session;
}

const operationSelect = () => screen.getByLabelText('Operation') as HTMLSelectElement;

async function choose(id: string) {
  await waitFor(() => expect(operationSelect().options.length).toBeGreaterThan(1));
  fireEvent.change(operationSelect(), { target: { value: id } });
}

describe('the API console (docs/49 D5, revised 2026-09-21)', () => {
  it('lists every operation of the served document and opens on the identity probe', async () => {
    mount();
    await choose('getMe');
    const labels = [...operationSelect().options].map((o) => o.textContent);
    expect(labels).toContain('GET /api/v1/me — Who am I');
    expect(labels).toContain('DELETE /api/v1/bans/{id} — Remove a ban');
    // The default is the first operation that needs a token, not the
    // unauthenticated document fetch listed before it.
    expect(operationSelect().value).toBe('getMe');
    expect(screen.getByText('role: operator')).toBeTruthy();
  });

  it('opens on the operation the docs route names', async () => {
    window.history.replaceState(null, '', '#/api/bans/createban');
    mount();
    await waitFor(() => expect(operationSelect().value).toBe('createBan'));
    expect((screen.getByLabelText(/Body/) as HTMLTextAreaElement).value).toContain('"ABC234"');
  });

  // Redoc's scroll spy rewrites the address bar as the reader scrolls, with
  // replaceState and without telling the router. That rewritten address is
  // what the reader is looking at, so it is what the drawer opens on.
  it('opens on the address bar, which the scroll spy rewrites behind the router', async () => {
    // The page is up on one route; the reader scrolls, and the scroll spy
    // rewrites the address bar without the router seeing it; THEN they open
    // the drawer — the order it happens in ApiView.
    window.history.replaceState(null, '', '#/api/bans');
    function Page() {
      const [open, setOpen] = useState(false);
      return inRouter(
        open ? (
          <ApiConsole onClose={() => undefined} fetchDocument={fetchDocument} />
        ) : (
          <button type="button" onClick={() => setOpen(true)}>
            Console
          </button>
        ),
      );
    }
    renderWithSession(<Page />, stubSession(() => json({})));
    act(() => window.history.replaceState(null, '', '#/api/webhooks/testwebhook'));
    fireEvent.click(screen.getByRole('button', { name: 'Console' }));
    await waitFor(() => expect(operationSelect().value).toBe('testWebhook'));
  });

  it('follows a sidebar click in Redoc', async () => {
    mount();
    await choose('getMe');
    act(() => navigate!('/api/bans/listbans'));
    await waitFor(() => expect(operationSelect().value).toBe('listBans'));
  });

  // Round-1 review finding: the router can sit on A while the scroll spy has
  // moved the address bar (and so the drawer) to X. Clicking A in the sidebar
  // again is a navigation to the SAME pathname — a new location, which the
  // drawer must still follow.
  it('follows a sidebar click to the route the router is already on', async () => {
    window.history.replaceState(null, '', '#/api/bans');
    function Page() {
      const [open, setOpen] = useState(false);
      return inRouter(
        open ? (
          <ApiConsole onClose={() => undefined} fetchDocument={fetchDocument} />
        ) : (
          <button type="button" onClick={() => setOpen(true)}>
            Console
          </button>
        ),
      );
    }
    renderWithSession(<Page />, stubSession(() => json({})));
    act(() => navigate!('/api/bans/listbans'));
    act(() => window.history.replaceState(window.history.state, '', '#/api/bans/createban'));
    fireEvent.click(screen.getByRole('button', { name: 'Console' }));
    await waitFor(() => expect(operationSelect().value).toBe('createBan'));
    act(() => navigate!('/api/bans/listbans'));
    await waitFor(() => expect(operationSelect().value).toBe('listBans'));
  });

  // Round-1 review finding: a sidebar click while a request is in flight
  // must not swap the operation under it — the response would land beside
  // another operation's caveats, with Send live again.
  it('does not follow the docs while a request is in flight', async () => {
    let release: (r: Response) => void = () => undefined;
    mount(() => new Promise<Response>((r) => (release = r)));
    await choose('getMe');
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    await screen.findByRole('button', { name: 'Sending…' });
    act(() => navigate!('/api/bans/listbans'));
    await new Promise((r) => setTimeout(r, 20));
    expect(operationSelect().value).toBe('getMe');
    await act(async () => release(json({ email: 'op@example.org' })));
    await screen.findByRole('button', { name: 'Send' });
    expect(operationSelect().value).toBe('getMe');
  });

  // The scroll spy's silent rewrite must NOT drag the drawer along: a
  // half-typed request should not change under the operator because they
  // scrolled to check a field.
  it('does not follow the scroll spy', async () => {
    mount();
    await choose('createBan');
    act(() => window.history.replaceState(null, '', '#/api/bans/listbans'));
    await new Promise((r) => setTimeout(r, 20));
    expect(operationSelect().value).toBe('createBan');
  });

  // The drawer's own "docs" link lands the router on the operation already
  // open; that is following the reader's own click back, not a new choice.
  it('keeps what was typed when the docs land on the operation already open', async () => {
    mount();
    await choose('createBan');
    fireEvent.change(screen.getByLabelText(/Body/), { target: { value: '{"reason":"typed"}' } });
    act(() => navigate!('/api/bans/createban'));
    await new Promise((r) => setTimeout(r, 20));
    expect((screen.getByLabelText(/Body/) as HTMLTextAreaElement).value).toBe('{"reason":"typed"}');
  });

  // Picking an operation in the drawer moves the reference to it, as a
  // sidebar click does: a router navigation, which Redoc scrolls to and the
  // Back button can undo. Opening the drawer does NOT move the reader.
  it('scrolls the docs to the operation picked in the console', async () => {
    window.history.replaceState(null, '', '#/api/bans');
    mount();
    await waitFor(() => expect(operationSelect().options.length).toBeGreaterThan(1));
    expect(window.location.hash).toBe('#/api/bans');
    fireEvent.change(operationSelect(), { target: { value: 'testWebhook' } });
    await waitFor(() => expect(window.location.hash).toBe('#/api/webhooks/testwebhook'));
    expect(operationSelect().value).toBe('testWebhook');
  });

  it('links back to the operation in the docs, inside the portal hash', async () => {
    mount();
    await choose('createBan');
    const link = screen.getByRole('link', { name: /docs/ });
    expect(link.getAttribute('href')).toBe('#/api/bans/createban');
  });

  // The property the whole design rests on: the request leaves through the
  // session — the ONE place a bearer is attached — with a RELATIVE path built
  // from the document, so it cannot go off-origin. No bare fetch, no URL field.
  it('sends a GET through the session with a relative api/v1 path, on one click', async () => {
    const session = mount(() => json({ email: 'op@example.org' }));
    await choose('getMe');
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));

    await screen.findByText('200');
    expect(session.calls).toHaveLength(1);
    expect(session.calls[0].path).toBe('api/v1/me');
    expect(session.calls[0].path.startsWith('/')).toBe(false);
    expect(session.calls[0].init.method).toBe('GET');
    expect(screen.getByText(/"email": "op@example.org"/)).toBeTruthy();
    expect(screen.queryByRole('textbox', { name: /url/i })).toBeNull();
  });

  it('never puts the token anywhere the operator can see it', async () => {
    const session = mount();
    await choose('getMe');
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    await screen.findByText('200');
    expect(document.body.textContent).not.toContain(session.accessToken());
  });

  it('carries query parameters that were filled and drops the empty optional ones', async () => {
    const session = mount(() => json({ bans: [] }));
    await choose('listBans');
    // A required query parameter left blank is refused before anything leaves.
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    expect(await screen.findByText('scope is required')).toBeTruthy();
    expect(session.calls).toHaveLength(0);

    fireEvent.change(screen.getByLabelText(/^scope/), { target: { value: 'x' } });
    fireEvent.change(screen.getByLabelText(/^state/), { target: { value: 'all' } });
    fireEvent.change(screen.getByLabelText(/^limit/), { target: { value: '5' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    await screen.findByText('200');
    expect(session.calls[0].path).toBe('api/v1/bans?scope=x&state=all&limit=5');
    // A sensitive response says so, in the document's own words.
    expect(screen.getByText(/Sensitive: A ban target may be a raw broadcast ID\./)).toBeTruthy();
  });

  it('asks once before a mutation, then sends the JSON body compactly', async () => {
    const session = mount(() => json({ id: 'b1' }, 201));
    await choose('createBan');
    const body = screen.getByLabelText(/Body/) as HTMLTextAreaElement;
    fireEvent.change(body, { target: { value: '{\n  "reason": "typed",\n  "target": {"type": "id", "value": "ABC234"}\n}' } });

    fireEvent.click(screen.getByRole('button', { name: 'Send…' }));
    // Nothing has left yet: the caution is on screen and the confirm button is red.
    expect(session.calls).toHaveLength(0);
    expect(screen.getByText(/it will act/)).toBeTruthy();

    fireEvent.click(screen.getByRole('button', { name: 'Send POST' }));
    await screen.findByText('201');
    expect(session.calls).toHaveLength(1);
    expect(session.calls[0].init.method).toBe('POST');
    expect((session.calls[0].init.headers as Record<string, string>)['Content-Type']).toBe('application/json');
    expect(session.calls[0].init.body).toBe('{"reason":"typed","target":{"type":"id","value":"ABC234"}}');
  });

  it('lets a confirm be cancelled', async () => {
    const session = mount();
    await choose('createBan');
    fireEvent.click(screen.getByRole('button', { name: 'Send…' }));
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByText(/it will act/)).toBeNull();
    expect(screen.getByRole('button', { name: 'Send…' })).toBeTruthy();
    expect(session.calls).toHaveLength(0);
  });

  it('refuses a body that is not JSON, and an empty path parameter, before anything leaves', async () => {
    const session = mount();
    await choose('createBan');
    fireEvent.change(screen.getByLabelText(/Body/), { target: { value: '{not json' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send…' }));
    expect(await screen.findByText(/The body is not valid JSON/)).toBeTruthy();
    expect(session.calls).toHaveLength(0);

    await choose('removeBan');
    fireEvent.click(screen.getByRole('button', { name: 'Send…' }));
    expect(await screen.findByText('id is required')).toBeTruthy();
    expect(session.calls).toHaveLength(0);
  });

  it('encodes a path parameter and shows a bodiless 204 as such', async () => {
    const session = mount(() => new Response(null, { status: 204 }));
    await choose('removeBan');
    fireEvent.change(screen.getByLabelText(/^id/), { target: { value: 'a b' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send…' }));
    fireEvent.click(screen.getByRole('button', { name: 'Send DELETE' }));
    await screen.findByText('204');
    expect(session.calls[0].path).toBe('api/v1/bans/a%20b');
    expect(screen.getByText('(no body)')).toBeTruthy();
  });

  it('shows the error envelope of a failed call in the failure colour', async () => {
    mount(() => json({ error: { code: 'not_found', message: 'no such ban' } }, 404));
    await choose('getMe');
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    const badge = await screen.findByText('404');
    expect(badge.className).toContain('badgeBad');
    expect(screen.getByText(/"message": "no such ban"/)).toBeTruthy();
  });

  it('copies a curl line with a $TOKEN placeholder, never the session token', async () => {
    const written: string[] = [];
    Object.assign(navigator, { clipboard: { writeText: (s: string) => (written.push(s), Promise.resolve()) } });
    const session = mount();
    await choose('createBan');
    fireEvent.click(screen.getByRole('button', { name: 'Copy as curl' }));
    await screen.findByText('Copied');
    expect(written).toHaveLength(1);
    expect(written[0]).toContain(`-H 'Authorization: Bearer $TOKEN'`);
    expect(written[0]).toContain('https://admin.gawk.example/api/v1/bans');
    expect(written[0]).not.toContain(session.accessToken());
  });

  it('resets what was typed when the operation changes', async () => {
    mount();
    await choose('createBan');
    fireEvent.change(screen.getByLabelText(/Body/), { target: { value: '{"reason":"typed"}' } });
    await choose('testWebhook');
    expect((screen.getByLabelText(/Body/) as HTMLTextAreaElement).value).toBe('{}');
  });

  it('closes on Escape and on the close button', async () => {
    const onClose = vi.fn();
    mount(undefined, onClose);
    await choose('getMe');
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(onClose).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole('button', { name: 'Close console' }));
    expect(onClose).toHaveBeenCalledTimes(2);
  });

  it('reports a contract that could not be loaded', async () => {
    const session = stubSession(() => json({}));
    renderWithSession(
      inRouter(
        <ApiConsole onClose={() => undefined} fetchDocument={() => Promise.reject(new Error('HTTP 503'))} />,
      ),
      session,
    );
    expect(await screen.findByText(/could not be loaded: HTTP 503/)).toBeTruthy();
  });
});
