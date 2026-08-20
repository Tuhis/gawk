// @vitest-environment jsdom
import { cleanup, fireEvent, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { WebhooksView } from './WebhooksView.tsx';
import type { Webhook } from '../api/types.ts';
import { bodyOf, json, renderWithSession, stubSession } from '../testing/harness.tsx';

afterEach(cleanup);

const FROM_CONFIG: Webhook = {
  name: 'ntfy-oncall',
  url: 'https://ntfy.example/gawk',
  enabled: true,
  source: 'config',
};

const FROM_PORTAL: Webhook = {
  id: 'e2b0f6f0-0000-4000-8000-000000000001',
  name: 'discord',
  url: 'https://discord.example/hook',
  enabled: true,
  source: 'ui',
};

function mount(webhooks: Webhook[], extra?: (path: string, init: RequestInit) => Response) {
  const session = stubSession((path, init) => {
    if (path === 'api/v1/webhooks' && (init.method ?? 'GET') === 'GET')
      return json({ webhooks });
    if (extra) return extra(path, init);
    return json({ ok: true, status: 200 });
  });
  renderWithSession(<WebhooksView />, session);
  return session;
}

function rowFor(name: string): HTMLElement {
  const cell = screen.getByText(name);
  const row = cell.closest('tr');
  if (!row) throw new Error(`no row for ${name}`);
  return row;
}

describe('the merged list (§4.10, D9)', () => {
  it('renders both sources with their provenance visible', async () => {
    mount([FROM_CONFIG, FROM_PORTAL]);
    expect(await screen.findByText('ntfy-oncall')).toBeTruthy();
    expect(within(rowFor('ntfy-oncall')).getByText('from config')).toBeTruthy();
    expect(within(rowFor('discord')).getByText('portal')).toBeTruthy();
  });

  it('locks a config-sourced row: no edit, no delete', async () => {
    mount([FROM_CONFIG, FROM_PORTAL]);
    await screen.findByText('ntfy-oncall');
    const locked = within(rowFor('ntfy-oncall'));
    expect((locked.getByRole('button', { name: 'Edit' }) as HTMLButtonElement).disabled).toBe(
      true,
    );
    expect((locked.getByRole('button', { name: 'Delete' }) as HTMLButtonElement).disabled).toBe(
      true,
    );
    // The reason is on the control, not buried in a doc: the secret lives in a
    // Kubernetes Secret and the portal does not own this row.
    expect(locked.getByRole('button', { name: 'Edit' }).getAttribute('title')).toMatch(
      /chart values/i,
    );
  });

  it('leaves a portal-created row fully editable', async () => {
    mount([FROM_CONFIG, FROM_PORTAL]);
    await screen.findByText('discord');
    const editable = within(rowFor('discord'));
    expect((editable.getByRole('button', { name: 'Edit' }) as HTMLButtonElement).disabled).toBe(
      false,
    );
    expect((editable.getByRole('button', { name: 'Delete' }) as HTMLButtonElement).disabled).toBe(
      false,
    );
  });

  it('never displays a secret, for either source', async () => {
    mount([FROM_CONFIG, FROM_PORTAL]);
    await screen.findByText('discord');
    fireEvent.click(within(rowFor('discord')).getByRole('button', { name: 'Edit' }));
    const secret = screen.getByLabelText(/Signing secret/) as HTMLInputElement;
    // The API returns no secret (§4.7), so there is nothing to prefill — and
    // the field is a password field, not a text one.
    expect(secret.value).toBe('');
    expect(secret.type).toBe('password');
  });
});

describe('test-send (§4.10)', () => {
  it('works for a config-sourced webhook', async () => {
    const session = mount([FROM_CONFIG, FROM_PORTAL]);
    await screen.findByText('ntfy-oncall');
    fireEvent.click(within(rowFor('ntfy-oncall')).getByRole('button', { name: 'Send test' }));

    await waitFor(() => {
      expect(
        session.calls.some((c) => c.path === 'api/v1/webhooks/ntfy-oncall/test'),
      ).toBe(true);
    });
    expect(await within(rowFor('ntfy-oncall')).findByText(/delivered/)).toBeTruthy();
  });

  it('works for a portal-created webhook', async () => {
    const session = mount([FROM_CONFIG, FROM_PORTAL]);
    await screen.findByText('discord');
    fireEvent.click(within(rowFor('discord')).getByRole('button', { name: 'Send test' }));
    await waitFor(() => {
      expect(session.calls.some((c) => c.path === 'api/v1/webhooks/discord/test')).toBe(true);
    });
  });

  it('shows a failed delivery rather than swallowing it', async () => {
    mount([FROM_CONFIG], (path) =>
      path.endsWith('/test')
        ? json({ ok: false, status: 502, error: 'connection refused' })
        : json({}),
    );
    await screen.findByText('ntfy-oncall');
    fireEvent.click(screen.getByRole('button', { name: 'Send test' }));
    expect(await screen.findByText(/failed: connection refused/)).toBeTruthy();
  });
});

describe('CRUD on portal-created webhooks (§4.7)', () => {
  it('creates one, sending the secret write-only', async () => {
    const session = mount([]);
    await waitFor(() => expect(session.calls.length).toBeGreaterThan(0));
    fireEvent.click(screen.getByRole('button', { name: 'Add webhook' }));
    fireEvent.change(screen.getByLabelText(/Name/), { target: { value: 'pager' } });
    fireEvent.change(screen.getByLabelText('URL'), {
      target: { value: 'https://pager.example/hook' },
    });
    fireEvent.change(screen.getByLabelText(/Signing secret/), { target: { value: 's3cret' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => {
      expect(
        session.calls.some((c) => c.path === 'api/v1/webhooks' && c.init.method === 'POST'),
      ).toBe(true);
    });
    const post = session.calls.find(
      (c) => c.path === 'api/v1/webhooks' && c.init.method === 'POST',
    );
    expect(bodyOf(post!)).toEqual({
      name: 'pager',
      url: 'https://pager.example/hook',
      enabled: true,
      secret: 's3cret',
    });
  });

  it('omits the secret on edit when the field is left blank, so it is kept', async () => {
    const session = mount([FROM_PORTAL], (_path, init) =>
      init.method === 'PUT' ? json({ ...FROM_PORTAL, url: 'https://new.example' }) : json({}),
    );
    await screen.findByText('discord');
    fireEvent.click(within(rowFor('discord')).getByRole('button', { name: 'Edit' }));
    fireEvent.change(screen.getByLabelText('URL'), {
      target: { value: 'https://new.example' },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => {
      expect(session.calls.some((c) => c.init.method === 'PUT')).toBe(true);
    });
    const put = session.calls.find((c) => c.init.method === 'PUT');
    expect(put!.path).toBe(`api/v1/webhooks/${FROM_PORTAL.id}`);
    expect(bodyOf(put!)).toEqual({
      name: 'discord',
      url: 'https://new.example',
      enabled: true,
    });
  });

  it('deletes one', async () => {
    const session = mount([FROM_PORTAL], (_path, init) =>
      init.method === 'DELETE' ? new Response(null, { status: 204 }) : json({}),
    );
    await screen.findByText('discord');
    fireEvent.click(within(rowFor('discord')).getByRole('button', { name: 'Delete' }));
    await waitFor(() => {
      expect(
        session.calls.some(
          (c) => c.init.method === 'DELETE' && c.path === `api/v1/webhooks/${FROM_PORTAL.id}`,
        ),
      ).toBe(true);
    });
  });

  it('surfaces 409 source_immutable if the server ever disagrees about a row’s source', async () => {
    // The UI locks config rows, so this should be unreachable from the page.
    // It is asserted anyway because the server is the authority (D9), and a
    // refusal the operator cannot see is a refusal they will retry forever.
    const session = mount([{ ...FROM_CONFIG, id: 'wrongly-editable', source: 'ui' }], (_path, init) =>
      init.method === 'DELETE'
        ? json(
            {
              error: {
                code: 'source_immutable',
                message: 'ntfy-oncall is defined in the chart values',
              },
            },
            409,
          )
        : json({}),
    );
    await screen.findByText('ntfy-oncall');
    fireEvent.click(within(rowFor('ntfy-oncall')).getByRole('button', { name: 'Delete' }));
    expect(await screen.findByText(/defined in the chart values/)).toBeTruthy();
    expect(session.calls.some((c) => c.init.method === 'DELETE')).toBe(true);
  });
});
