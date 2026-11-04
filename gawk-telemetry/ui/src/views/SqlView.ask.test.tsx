// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { AskAnswer, QueryStatus } from '../api/types.ts';

// R82 NQ3: the Ask box above the console (docs/84 §4.3).

const api = vi.hoisted(() => ({
  status: null as QueryStatus | null,
  ask: vi.fn<(q: string) => Promise<AskAnswer>>(),
  run: vi.fn(),
}));

vi.mock('../api/client.ts', async (orig) => ({
  ...(await orig<typeof import('../api/client.ts')>()),
  fetchQueryStatus: () => Promise.resolve(api.status),
  askQuery: (q: string) => api.ask(q),
  runQuery: (...a: unknown[]) => api.run(...a),
}));

const { SqlView } = await import('./SqlView.tsx');

const base: QueryStatus = { enabled: true, rowLimit: 5000, timeoutMs: 30000, views: [] };
const answer: AskAnswer = {
  interpretation: 'Viewer sessions per platform, with their median stall count.',
  sql: "SELECT os, count(*) AS sessions FROM rollups WHERE role = 'viewer' GROUP BY os",
  checks: 1,
  check: 'OK',
  provider: 'anthropic',
  model: 'claude-sonnet-5-5',
  elapsedMs: 5200,
};

beforeEach(() => {
  window.location.hash = '#/sql';
  api.ask.mockReset();
  api.run.mockReset();
});
afterEach(cleanup);

function editor(): HTMLTextAreaElement {
  return screen.getByPlaceholderText('SELECT … FROM rollups') as HTMLTextAreaElement;
}

describe('the Ask box', () => {
  it('is hidden without a provider', async () => {
    api.status = { ...base, ask: { enabled: false, provider: 'none' } };
    render(<SqlView />);
    await screen.findByPlaceholderText('SELECT … FROM rollups');
    expect(screen.queryByLabelText('question')).toBeNull();
    expect(screen.queryByText(/Ask is not available/)).toBeNull();
  });

  it('is hidden on a server that predates it', async () => {
    api.status = { ...base };
    render(<SqlView />);
    await screen.findByPlaceholderText('SELECT … FROM rollups');
    expect(screen.queryByLabelText('question')).toBeNull();
  });

  it('says "no model" and "no engine" differently', async () => {
    api.status = { ...base, ask: { enabled: false, provider: 'anthropic', reason: 'it needs the query engine' } };
    render(<SqlView />);
    expect(await screen.findByText(/Ask is not available here/)).not.toBeNull();
    expect(screen.getByText(/it needs the query engine/)).not.toBeNull();
    cleanup();

    api.status = { enabled: false, reason: 'no engine compiled in', rowLimit: 0, timeoutMs: 0, ask: { enabled: false, provider: 'anthropic' } };
    render(<SqlView />);
    expect(await screen.findByText(/No query engine is compiled into this build/)).not.toBeNull();
    expect(screen.getByText(/needs the engine too/)).not.toBeNull();
    expect(screen.queryByLabelText('question')).toBeNull();
  });

  it('puts the draft in the editor with its reading, and runs nothing', async () => {
    api.status = { ...base, ask: { enabled: true, provider: 'anthropic', model: 'claude-sonnet-5-5', external: true, endpoint: 'api.anthropic.com' } };
    api.ask.mockResolvedValue(answer);
    render(<SqlView />);
    const box = await screen.findByLabelText('question');
    expect(screen.getByText('api.anthropic.com')).not.toBeNull();

    fireEvent.change(editor(), { target: { value: 'SELECT 1' } });
    fireEvent.change(box, { target: { value: 'sessions per os for viewers' } });
    fireEvent.submit(box.closest('form')!);

    await waitFor(() => expect(editor().value).toBe(answer.sql));
    expect(api.ask).toHaveBeenCalledWith('sessions per os for viewers');
    expect(screen.getByText(/Viewer sessions per platform/)).not.toBeNull();
    expect(screen.getByText(/claude-sonnet-5-5$/)).not.toBeNull();
    expect(api.run).not.toHaveBeenCalled();
    // What the editor held stays one Back away.
    expect(decodeURIComponent(window.location.hash)).toContain(answer.sql.replaceAll(' ', '+'));
  });

  it('says when the draft does not bind', async () => {
    api.status = { ...base, ask: { enabled: true, provider: 'openai', model: 'qwen2.5-coder:7b' } };
    api.ask.mockResolvedValue({ ...answer, check: 'ERROR: Binder Error: Referenced column "plaform" not found' });
    render(<SqlView />);
    const box = await screen.findByLabelText('question');
    // An in-cluster model: nothing to say about the question leaving.
    expect(screen.queryByText(/Your question is sent to/)).toBeNull();
    fireEvent.change(box, { target: { value: 'q' } });
    fireEvent.submit(box.closest('form')!);
    expect(await screen.findByText(/This draft does not bind: Binder Error/)).not.toBeNull();
    expect(api.run).not.toHaveBeenCalled();
  });

  it('renders an error from the model as prose', async () => {
    api.status = { ...base, ask: { enabled: true, provider: 'anthropic', model: 'claude-sonnet-5-5' } };
    api.ask.mockRejectedValue(new Error('ask: the model did not produce a usable draft: it answered without SQL: I cannot tell.'));
    render(<SqlView />);
    const box = await screen.findByLabelText('question');
    fireEvent.change(editor(), { target: { value: 'SELECT 1' } });
    fireEvent.change(box, { target: { value: 'q' } });
    fireEvent.submit(box.closest('form')!);
    expect(await screen.findByText(/it answered without SQL: I cannot tell/)).not.toBeNull();
    expect(editor().value).toBe('SELECT 1');
    expect(api.run).not.toHaveBeenCalled();
  });
});
